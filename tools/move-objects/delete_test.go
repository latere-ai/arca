// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/object"
	"latere.ai/x/arca/tools/internal/manifest"
)

// sunset runs the move and the delete pass after it, the way the command runs
// them with -delete-sources, and answers the report and the verdict.
func sunset(t *testing.T, b *bucket, o options, entries ...manifest.Entry) (string, error) {
	t.Helper()
	o.manifest, o.bucket, o.prefix, o.deleteSources = "manifest.tsv", "arca-test", prefix, true
	o.verifyBytes, o.verifyMax, o.verifySample = true, DefaultVerifyMax, DefaultVerifySample
	if o.concurrency == 0 {
		o.concurrency = 4
	}
	var out strings.Builder
	err := move(t.Context(), o, prefix,
		&manifest.Manifest{Prefix: prefix, Entries: entries, Complete: true}, b, &out)
	return out.String(), err
}

// says fails the case unless the report holds every sentence.
func says(t *testing.T, report string, sentences ...string) {
	t.Helper()
	for _, sentence := range sentences {
		if !strings.Contains(report, sentence) {
			t.Errorf("the report holds no %q:\n%s", sentence, report)
		}
	}
}

func TestTheDeletePassRemovesTheSourceOfEveryVerifiedDestination(t *testing.T) {
	notes, logo := []byte("the bytes of one note"), []byte("the bytes of one logo")
	b := newBucket(t, map[string][]byte{notesKey: notes, logoKey: logo})

	out, err := sunset(t, b, options{},
		byteEntry(notesKey, idNotes, notes, false), byteEntry(logoKey, idLogo, logo, false))
	if err != nil {
		t.Fatalf("the sunset: %v\n%s", err, out)
	}
	says(t, out, "deleted "+notesKey, "deleted "+logoKey, "2 source keys deleted",
		"The source keys it deleted are gone")
	for _, key := range []string{notesKey, logoKey} {
		if _, held := b.inner.Bytes(key); held {
			t.Errorf("the source key %s is still in the bucket", key)
		}
	}
	// The destinations are what the bytes are readable at now, and they are
	// the only copy.
	for id, body := range map[string][]byte{idNotes: notes, idLogo: logo} {
		key := object.ID(id).Key(prefix)
		if held, ok := b.inner.Bytes(key); !ok || !bytes.Equal(held, body) {
			t.Errorf("%s holds %q", key, held)
		}
	}
	// One delete per key: the manifest's first field is the only list, and a
	// bulk delete would take a list a listing produced instead.
	if one, many := b.Calls(blob.MethodDelete), b.Calls(blob.MethodDeleteMany); one != 2 || many != 0 {
		t.Errorf("the pass made %d deletes and %d bulk deletes for two keys", one, many)
	}
}

func TestASourceWhoseDestinationDidNotVerifyIsKept(t *testing.T) {
	notes, logo := []byte("the bytes of one note"), []byte("the bytes of one logo")
	b := newBucket(t, map[string][]byte{notesKey: notes, logoKey: logo})
	// The logo's destination holds another object of another length, so the
	// move mismatches it; the third line's source was never in the bucket, so
	// its destination is missing when the pass reads the verdict.
	other := []byte("the bytes of something else entirely")
	destination := object.ID(idLogo).Key(prefix)
	if _, err := b.inner.Put(t.Context(), destination, bytes.NewReader(other), int64(len(other)), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	out, err := sunset(t, b, options{},
		byteEntry(notesKey, idNotes, notes, false),
		byteEntry(logoKey, idLogo, logo, false),
		byteEntry(goneKey, idGone, []byte("the bytes nobody wrote"), false))
	if err == nil {
		t.Fatalf("a run that could not verify every destination answered no error:\n%s", out)
	}
	says(t, out, "kept "+logoKey, "kept "+goneKey, "1 source key deleted",
		"No source of a key above was deleted")
	if strings.Contains(out, "deleted "+logoKey) || strings.Contains(out, "deleted "+goneKey) {
		t.Errorf("the report says it deleted a source it kept:\n%s", out)
	}
	// The source of a destination nobody proved is the only copy of those
	// bytes, and it stays.
	if held, ok := b.inner.Bytes(logoKey); !ok || !bytes.Equal(held, logo) {
		t.Error("the pass deleted the source of a destination that did not verify")
	}
	if _, held := b.inner.Bytes(notesKey); held {
		t.Error("the pass kept the source of a destination that verified")
	}
	if n := b.Calls(blob.MethodDelete); n != 1 {
		t.Errorf("the pass deleted %d keys, want the one that verified", n)
	}
}

func TestADryRunOfTheDeletePassDeletesNothing(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})

	out, err := sunset(t, b, options{dryRun: true}, byteEntry(notesKey, idNotes, notes, false))
	if err != nil {
		t.Fatalf("the rehearsal: %v\n%s", err, out)
	}
	says(t, out, "would delete "+notesKey, "1 source key would be deleted",
		"delete the source keys it names")
	if strings.Contains(out, "deleted "+notesKey) {
		t.Errorf("the rehearsal says it deleted a key:\n%s", out)
	}
	if n := b.Calls(blob.MethodDelete); n != 0 {
		t.Errorf("the rehearsal deleted %d keys", n)
	}
	if _, held := b.inner.Bytes(notesKey); !held {
		t.Error("the rehearsal removed a source key")
	}
}

func TestASourceTheStoreWillNotDeleteIsNotClean(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	b.FailNth(blob.MethodDelete, 1, errors.New("the store went away"))

	out, err := sunset(t, b, options{concurrency: 1}, byteEntry(notesKey, idNotes, notes, false))
	if err == nil {
		t.Fatalf("a delete the store refused answered no error:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not clean") {
		t.Errorf("the error is %v", err)
	}
	says(t, out, "not deleted "+notesKey, "the store went away", "0 source keys deleted",
		"1 the store would not delete")
	if _, held := b.inner.Bytes(notesKey); !held {
		t.Error("a delete the store refused removed the key anyway")
	}
}

func TestACancelledDeletePassKeepsEverySource(t *testing.T) {
	notes := []byte("the bytes of one note")
	b := newBucket(t, map[string][]byte{notesKey: notes})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s := &Sweep{Bucket: b, Concurrency: 1}
	deletions := s.Run(ctx, []Outcome{{Entry: byteEntry(notesKey, idNotes, notes, false), State: Copied}})
	if deletions[0].Fate != Kept {
		t.Fatalf("a cancelled pass deleted %s", deletions[0].Source)
	}
	if n := b.Calls(blob.MethodDelete); n != 0 {
		t.Errorf("a cancelled pass deleted %d keys", n)
	}
}

func TestADeletePassOverNoKeysDeletesNothing(t *testing.T) {
	b := newBucket(t, nil)
	s := &Sweep{Bucket: b, Concurrency: 0}
	if deletions := s.Run(t.Context(), nil); len(deletions) != 0 {
		t.Fatalf("an empty manifest produced %d deletions", len(deletions))
	}
	if b.Total() != 0 {
		t.Errorf("an empty manifest reached the store %d times", b.Total())
	}
}

func TestDeletingTheSourcesWithTheByteCheckOffIsRefused(t *testing.T) {
	// A source is the last other copy of the bytes under it, and a length is
	// not a proof. The two flags only make sense together.
	t.Setenv(BucketPrefixVar, "")
	path := writeManifest(t, "drive/")
	_, errs, code := command(t, "-manifest", path, "-bucket", "arca-test", "-prefix", "drive/",
		"-delete-sources", "-verify-bytes=false")
	if code != exitRefused {
		t.Fatalf("the command exited %d, want %d", code, exitRefused)
	}
	for _, reason := range []string{"-delete-sources", "-verify-bytes"} {
		if !strings.Contains(errs, reason) {
			t.Errorf("the refusal does not name %s:\n%s", reason, errs)
		}
	}
}

func TestTheReportCountsADeleteTakenOnALengthApart(t *testing.T) {
	// The byte check reaches every object under its threshold and a share of
	// the larger ones. What it did not reach was deleted on its length, and
	// the count says so rather than reading as a delete the bytes proved.
	r := NewReport("manifest.tsv", prefix, "arca-test", false)
	r.DeleteSources = true
	r.Deletions = []Deletion{
		{Source: notesKey, Fate: Deleted, Proof: OnBytes},
		{Source: logoKey, Fate: Deleted, Proof: OnSize},
	}
	out := rendered(r)
	says(t, out, "2 source keys deleted", "1 of the deleted were proved on their length alone")
}
