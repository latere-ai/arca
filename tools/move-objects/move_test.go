// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the label a store reports for an object written in one piece, reproduced
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/object"
	"latere.ai/x/arca/tools/internal/manifest"
)

// The prefix every case moves under, which is decision 2 of spec 019: the
// bucket prefix stays what the predecessor wrote.
const prefix = "drive/"

// The ids the cases mint, in the canonical text a key derives from.
const (
	idNotes  = "0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f"
	idLogo   = "0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a20"
	idGone   = "0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a21"
	notesKey = "drive/u-1/files/notes.md"
	logoKey  = "drive/u-1/files/logo.png"
	goneKey  = "drive/u-1/files/gone.md"
)

// bucket is a store with the objects a case seeds, wrapped so a case counts
// what the move called.
type bucket struct {
	*blob.Counting
	inner *blob.Memory
}

// newBucket seeds one object per key and answers the store.
func newBucket(t *testing.T, keys map[string][]byte) *bucket {
	t.Helper()
	inner := blob.NewMemory()
	for key, body := range keys {
		if _, err := inner.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), blob.PutOptions{}); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}
	return &bucket{Counting: blob.NewCounting(inner), inner: inner}
}

// label is what a store reports for an object written in one piece, which is
// the one checksum shape a move can compare a line against.
func label(body []byte) string {
	sum := md5.Sum(body) //nolint:gosec // the store's own label, reproduced
	return hex.EncodeToString(sum[:])
}

// sha is the digest the predecessor stored for an object written in one
// piece, which is what the byte check compares a destination against.
func sha(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// entry is one manifest line whose checksum is the store's own label, which
// is the rung below the byte check.
func entry(key, id string, body []byte, public bool) manifest.Entry {
	return manifest.Entry{Key: key, ID: object.ID(id), Size: int64(len(body)), Checksum: label(body), Public: public}
}

// byteEntry is one manifest line whose checksum is a sha256, which is what
// almost every row of the predecessor carries and what the byte check reads.
func byteEntry(key, id string, body []byte, public bool) manifest.Entry {
	e := entry(key, id, body, public)
	e.Checksum = sha(body)
	return e
}

// moved runs the move the way the command runs it, with the byte check on and
// at its default bounds, and answers the outcomes by source key.
func moved(t *testing.T, b *bucket, dryRun bool, entries ...manifest.Entry) map[string]Outcome {
	t.Helper()
	return movedBy(t, b, &Move{
		Bucket: b, Prefix: prefix, DryRun: dryRun, Concurrency: 4,
		VerifyBytes: true, VerifyMax: DefaultVerifyMax, VerifySample: DefaultVerifySample,
	}, entries...)
}

// movedBy runs one move a case shaped itself.
func movedBy(t *testing.T, b *bucket, m *Move, entries ...manifest.Entry) map[string]Outcome {
	t.Helper()
	byKey := map[string]Outcome{}
	for _, o := range m.Run(t.Context(), entries) {
		byKey[o.Entry.Key] = o
	}
	// A move never deletes. The source keys go at the sunset of spec 019.
	if n := b.Calls(blob.MethodDelete) + b.Calls(blob.MethodDeleteMany); n != 0 {
		t.Errorf("the move deleted %d times", n)
	}
	return byKey
}

func TestAMoveCopiesEveryKeyToItsObjectIDsKey(t *testing.T) {
	notes, logo := []byte("the bytes of one note"), []byte("the bytes of one logo")
	b := newBucket(t, map[string][]byte{notesKey: notes, logoKey: logo})

	out := moved(t, b, false, entry(notesKey, idNotes, notes, false), entry(logoKey, idLogo, logo, false))
	for key, o := range out {
		if o.State != Copied {
			t.Fatalf("%s is %s: %s", key, name(o.State), o.Why)
		}
	}
	// The bytes are readable at the key the id derives, which is the whole
	// of criterion 4's byte half.
	for id, body := range map[string][]byte{idNotes: notes, idLogo: logo} {
		key := object.ID(id).Key(prefix)
		held, ok := b.inner.Bytes(key)
		if !ok || !bytes.Equal(held, body) {
			t.Errorf("%s holds %q", key, held)
		}
	}
	// The source is untouched.
	if held, ok := b.inner.Bytes(notesKey); !ok || !bytes.Equal(held, notes) {
		t.Error("the move changed a source key")
	}
	if n := b.Calls(blob.MethodCopy); n != 2 {
		t.Errorf("the move made %d copies for two keys", n)
	}
}

func TestADestinationThatIsAlreadyThereIsASkip(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	line := entry(notesKey, idNotes, notes, false)

	if o := moved(t, b, false, line)[notesKey]; o.State != Copied {
		t.Fatalf("the first run is %s: %s", name(o.State), o.Why)
	}
	// A killed run resumes, and a finished run repeats, by reading the
	// destination rather than by trusting a conditional copy: the store the
	// stack pins honours none.
	second := moved(t, b, false, line)[notesKey]
	if second.State != Skipped {
		t.Fatalf("the second run is %s: %s", name(second.State), second.Why)
	}
	if n := b.Calls(blob.MethodCopy); n != 1 {
		t.Errorf("the second run copied again: %d copies for one key", n)
	}
}

func TestADestinationHoldingOtherBytesIsAMismatchAndIsNotOverwritten(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	held := []byte("the bytes of something else entirely")
	destination := object.ID(idNotes).Key(prefix)
	if _, err := b.inner.Put(t.Context(), destination, bytes.NewReader(held), int64(len(held)), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	o := moved(t, b, false, entry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Mismatched {
		t.Fatalf("the run is %s: %s", name(o.State), o.Why)
	}
	if !strings.Contains(o.Why, fmt.Sprint(len(held))) {
		t.Errorf("the reason does not name what the destination holds: %q", o.Why)
	}
	if got, _ := b.inner.Bytes(destination); !bytes.Equal(got, held) {
		t.Error("the mismatch overwrote the destination")
	}
	if n := b.Calls(blob.MethodCopy); n != 0 {
		t.Errorf("a mismatch copied %d times", n)
	}
}

func TestADestinationOfTheRightSizeAndAnotherLabelIsAMismatch(t *testing.T) {
	notes := []byte("the bytes of one note")
	other := []byte("the bytes of one NOTE")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	destination := object.ID(idNotes).Key(prefix)
	if _, err := b.inner.Put(t.Context(), destination, bytes.NewReader(other), int64(len(other)), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	o := moved(t, b, false, entry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Mismatched || !strings.Contains(o.Why, "labelled") {
		t.Fatalf("the run is %s: %s", name(o.State), o.Why)
	}
}

func TestALineWithNeitherALabelNorADigestIsVerifiedOnItsSize(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	// The composite label of an object the predecessor assembled from parts.
	// It is not a label a copy's destination carries and it is not a digest
	// of the bytes, so neither rung above the size applies.
	line := entry(notesKey, idNotes, notes, false)
	line.Checksum = strings.Repeat("a", 32) + "-3"

	o := moved(t, b, false, line)[notesKey]
	if o.State != Copied || o.Verified != OnSize {
		t.Fatalf("the run is %s, proved %v: %s", name(o.State), o.Verified, o.Why)
	}
	if comparableLabel(line.Checksum) {
		t.Error("a composite label was read as comparable")
	}
	if !comparableLabel(label(notes)) {
		t.Error("a single part label was not read as comparable")
	}
}

func TestADestinationAssembledFromRangesIsVerifiedOnItsSize(t *testing.T) {
	// A source above the API's single copy maximum moves range by range, and
	// the store then labels the destination the way it labels an upload in
	// parts. No digest of the whole object equals that label, so a line that
	// carries one is verified on its size rather than failed.
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	destination := object.ID(idNotes).Key(prefix)
	uploadID, err := b.inner.CreateMultipart(t.Context(), destination, blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	etag, err := b.inner.UploadPart(destination, uploadID, 1, notes)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := b.inner.CompleteMultipart(t.Context(), destination, uploadID, []blob.Part{{Number: 1, ETag: etag}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(assembled.ETag, "-1") {
		t.Fatalf("the destination is labelled %q, which is not composite", assembled.ETag)
	}

	o := moved(t, b, false, entry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Skipped || o.Verified != OnSize {
		t.Fatalf("the run is %s, proved %v: %s", name(o.State), o.Verified, o.Why)
	}
}

// TestTheByteCheckProvesWhatNoStoreLabelCan is criterion 4b of spec 019 at a
// store that answers no checksum of its own, which is every store the family
// runs: the destination is read back and digested against the row's sha256.
func TestTheByteCheckProvesWhatNoStoreLabelCan(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})

	o := moved(t, b, false, byteEntry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Copied || o.Verified != OnBytes {
		t.Fatalf("the run is %s, proved %v: %s", name(o.State), o.Verified, o.Why)
	}
	if n := b.Calls(blob.MethodGet); n != 1 {
		t.Errorf("the byte check read the destination %d times", n)
	}
	// A resumed run proves what it skips, or the keys of a killed run would
	// be the only ones nobody ever checked.
	second := moved(t, b, false, byteEntry(notesKey, idNotes, notes, false))[notesKey]
	if second.State != Skipped || second.Verified != OnBytes {
		t.Fatalf("the resumed run is %s, proved %v: %s", name(second.State), second.Verified, second.Why)
	}
}

// TestADestinationWhoseBytesAreWrongIsAMismatch is the check earning its
// keep: the size and the store's own copy both hold, and the bytes do not.
func TestADestinationWhoseBytesAreWrongIsAMismatch(t *testing.T) {
	notes := []byte("the bytes of one note")
	other := []byte("the bytes of one NOTE")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	// The destination is there, the right length, and wrong. Only a read of
	// the bytes tells them apart.
	destination := object.ID(idNotes).Key(prefix)
	if _, err := b.inner.Put(t.Context(), destination, bytes.NewReader(other), int64(len(other)), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	o := moved(t, b, false, byteEntry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Mismatched || !strings.Contains(o.Why, "digest") {
		t.Fatalf("the run is %s: %s", name(o.State), o.Why)
	}
	if !strings.Contains(o.Why, sha(notes)) {
		t.Errorf("the reason does not name the checksum the manifest carries: %q", o.Why)
	}
	if got, _ := b.inner.Bytes(destination); !bytes.Equal(got, other) {
		t.Error("the mismatch overwrote the destination")
	}
}

func TestADestinationTheByteCheckCannotReadFails(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	b.FailNth(blob.MethodGet, 1, errors.New("the store went away"))

	o := moved(t, b, false, byteEntry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Failed || !strings.Contains(o.Why, "read the destination's bytes back") {
		t.Fatalf("the run is %s: %s", name(o.State), o.Why)
	}
}

func TestTheByteCheckIsOptedOutOfAndNeverIntoAndBoundsItsReads(t *testing.T) {
	notes := []byte("the bytes of one note")
	line := byteEntry(notesKey, idNotes, notes, false)
	for _, c := range []struct {
		name  string
		move  Move
		proof Proof
		reads int
	}{
		{"the default reads the bytes", Move{VerifyBytes: true, VerifyMax: DefaultVerifyMax, VerifySample: DefaultVerifySample}, OnBytes, 1},
		{"-verify-bytes=false reads none", Move{}, OnSize, 0},
		{"an object past the threshold is sampled out", Move{VerifyBytes: true, VerifyMax: 1, VerifySample: 0}, OnSize, 0},
		{"an object past the threshold is sampled in", Move{VerifyBytes: true, VerifyMax: 1, VerifySample: 100}, OnBytes, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBucket(t, map[string][]byte{notesKey: notes})
			m := c.move
			m.Bucket, m.Prefix, m.Concurrency = b, prefix, 1
			o := movedBy(t, b, &m, line)[notesKey]
			if o.State != Copied || o.Verified != c.proof {
				t.Fatalf("the run is %s, proved %v: %s", name(o.State), o.Verified, o.Why)
			}
			if got := b.Calls(blob.MethodGet); got != c.reads {
				t.Errorf("the run read %d destinations, want %d", got, c.reads)
			}
		})
	}
}

// TestTheSampleIsTheSameOnEveryRun holds the choice to a digest of the key
// rather than a draw, so a resumed run does not leave a hole where the first
// one read.
func TestTheSampleIsTheSameOnEveryRun(t *testing.T) {
	m := &Move{VerifyBytes: true, VerifyMax: 1, VerifySample: 50}
	chosen := 0
	for i := range 200 {
		e := manifest.Entry{Key: fmt.Sprintf("drive/u-1/files/%d", i), Size: 1 << 30, Checksum: sha([]byte{byte(i)})}
		first := m.reads(e)
		if first != m.reads(e) {
			t.Fatalf("%q was chosen differently twice", e.Key)
		}
		if first {
			chosen++
		}
	}
	// Half of two hundred, give or take what a digest spreads to.
	if chosen < 70 || chosen > 130 {
		t.Errorf("a half sample chose %d of 200", chosen)
	}
}

func TestAPublicRowIsStampedAndAStoreWithoutACLsIsCounted(t *testing.T) {
	logo := []byte("the bytes of one public logo")
	b := newBucket(t, map[string][]byte{logoKey: logo})

	o := moved(t, b, false, entry(logoKey, idLogo, logo, true))[logoKey]
	if o.State != Copied || o.Unstamped {
		t.Fatalf("the run is %s, unstamped %v: %s", name(o.State), o.Unstamped, o.Why)
	}
	// A copy carries no ACL, so the destination is stamped after it.
	if !b.inner.Public(object.ID(idLogo).Key(prefix)) {
		t.Error("the destination was not stamped public")
	}
	if b.inner.Public(logoKey) {
		t.Error("the move stamped the source")
	}

	// A store that holds no object ACLs answers ErrNotSupported, which spec
	// 003 has a caller carry on from. It is counted, not failed.
	unstamping := newBucket(t, map[string][]byte{logoKey: logo})
	unstamping.FailNth(blob.MethodSetPublic, 1, blob.ErrNotSupported)
	u := moved(t, unstamping, false, entry(logoKey, idLogo, logo, true))[logoKey]
	if u.State != Copied || !u.Unstamped {
		t.Fatalf("a store without ACLs made the move %s: %s", name(u.State), u.Why)
	}
}

func TestAStampTheStoreRefusesForAnotherReasonFails(t *testing.T) {
	logo := []byte("the bytes of one public logo")
	b := newBucket(t, map[string][]byte{logoKey: logo})
	b.FailNth(blob.MethodSetPublic, 1, errors.New("the store went away"))

	o := moved(t, b, false, entry(logoKey, idLogo, logo, true))[logoKey]
	if o.State != Failed || !strings.Contains(o.Why, "stamp") {
		t.Fatalf("the run is %s: %s", name(o.State), o.Why)
	}
}

func TestAMissingSourceFailsAndTheOtherKeysStillMove(t *testing.T) {
	notes := []byte("the bytes of one note")
	gone := []byte("the bytes nobody kept")
	b := newBucket(t, map[string][]byte{notesKey: notes})

	out := moved(t, b, false, entry(goneKey, idGone, gone, false), entry(notesKey, idNotes, notes, false))
	if o := out[goneKey]; o.State != Failed || !strings.Contains(o.Why, "not in the bucket") {
		t.Fatalf("a missing source is %s: %s", name(o.State), o.Why)
	}
	// A failed key does not stop the others: the point is one list of what
	// to look at, not the first thing that went wrong.
	if o := out[notesKey]; o.State != Copied {
		t.Fatalf("the key beside the failure is %s: %s", name(o.State), o.Why)
	}
}

func TestAStoreThatCannotBeReadOrWrittenFails(t *testing.T) {
	notes := []byte("the bytes of one note")
	for _, c := range []struct {
		name   string
		method string
		nth    int
		err    error
		says   string
	}{
		{"the destination cannot be read", blob.MethodHead, 1, errors.New("the store went away"), "read the destination"},
		{"the copy is refused", blob.MethodCopy, 1, errors.New("the store went away"), "copy:"},
		{"the destination cannot be read back", blob.MethodHead, 2, errors.New("the store went away"), "read the destination back"},
		{"another writer took the destination", blob.MethodCopy, 1, blob.ErrPreconditionFailed, "between the read and the copy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBucket(t, map[string][]byte{notesKey: notes})
			b.FailNth(c.method, c.nth, c.err)
			o := moved(t, b, false, entry(notesKey, idNotes, notes, false))[notesKey]
			if o.State != Failed || !strings.Contains(o.Why, c.says) {
				t.Fatalf("the run is %s: %s", name(o.State), o.Why)
			}
		})
	}
}

func TestADryRunReadsBothEndsAndWritesNothing(t *testing.T) {
	notes, logo := []byte("the bytes of one note"), []byte("the bytes of one logo")
	gone := []byte("the bytes nobody kept")
	b := newBucket(t, map[string][]byte{notesKey: notes, logoKey: logo})
	// The logo is already at its destination, so a rehearsal reports the
	// skip a run would report.
	if _, err := b.inner.Put(t.Context(), object.ID(idLogo).Key(prefix), bytes.NewReader(logo), int64(len(logo)), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	out := moved(t, b, true,
		entry(notesKey, idNotes, notes, false),
		entry(logoKey, idLogo, logo, false),
		entry(goneKey, idGone, gone, false))
	if o := out[notesKey]; o.State != Copied {
		t.Errorf("the key that would move is %s: %s", name(o.State), o.Why)
	}
	if o := out[logoKey]; o.State != Skipped {
		t.Errorf("the key already there is %s: %s", name(o.State), o.Why)
	}
	if o := out[goneKey]; o.State != Failed {
		t.Errorf("the missing source is %s: %s", name(o.State), o.Why)
	}
	if n := b.Calls(blob.MethodCopy) + b.Calls(blob.MethodPut) + b.Calls(blob.MethodSetPublic); n != 0 {
		t.Errorf("the dry run wrote %d times", n)
	}
	if _, held := b.inner.Bytes(object.ID(idNotes).Key(prefix)); held {
		t.Error("the dry run wrote a destination")
	}
}

func TestADryRunOverASourceOfAnotherSizeMismatches(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	line := entry(notesKey, idNotes, notes, false)
	line.Size = 3

	o := moved(t, b, true, line)[notesKey]
	if o.State != Mismatched || !strings.Contains(o.Why, "the source holds") {
		t.Fatalf("the rehearsal is %s: %s", name(o.State), o.Why)
	}
}

func TestADryRunThatCannotReadASourceFails(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	b.FailNth(blob.MethodHead, 2, errors.New("the store went away"))

	o := moved(t, b, true, entry(notesKey, idNotes, notes, false))[notesKey]
	if o.State != Failed || !strings.Contains(o.Why, "read the source") {
		t.Fatalf("the rehearsal is %s: %s", name(o.State), o.Why)
	}
}

func TestAnIDThatDerivesNoKeyFails(t *testing.T) {
	b := newBucket(t, map[string][]byte{notesKey: []byte("x")})
	o := moved(t, b, false, manifest.Entry{Key: notesKey, ID: "not-an-id", Size: 1})[notesKey]
	if o.State != Failed || !strings.Contains(o.Why, "derives no key") {
		t.Fatalf("the run is %s: %s", name(o.State), o.Why)
	}
}

func TestACancelledRunReportsTheKeysItDidNotReach(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	m := &Move{Bucket: b, Prefix: prefix, DryRun: false, Concurrency: 1}
	outcomes := m.Run(ctx, []manifest.Entry{entry(notesKey, idNotes, notes, false)})
	if outcomes[0].State != Failed {
		t.Fatalf("a cancelled run is %s", name(outcomes[0].State))
	}
}

func TestAMoveWithNoConcurrencyStillMovesOneKeyAtATime(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	m := &Move{Bucket: b, Prefix: prefix, Concurrency: 0}
	outcomes := m.Run(t.Context(), []manifest.Entry{entry(notesKey, idNotes, notes, false)})
	if outcomes[0].State != Copied {
		t.Fatalf("the run is %s: %s", name(outcomes[0].State), outcomes[0].Why)
	}
}

func TestAMoveOverNoKeysIsClean(t *testing.T) {
	b := newBucket(t, nil)
	m := &Move{Bucket: b, Prefix: prefix, Concurrency: 4}
	if outcomes := m.Run(t.Context(), nil); len(outcomes) != 0 {
		t.Fatalf("an empty manifest produced %d outcomes", len(outcomes))
	}
	if b.Total() != 0 {
		t.Errorf("an empty manifest reached the store %d times", b.Total())
	}
}
