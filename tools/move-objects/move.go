// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/tools/internal/manifest"
)

// Move is one run over one manifest.
type Move struct {
	// Bucket is the store holding both the source keys and the destinations.
	Bucket blob.Store
	// Prefix is what the destination keys are derived under.
	Prefix string
	// DryRun reads every destination and every source and writes nothing.
	DryRun bool
	// Concurrency bounds how many keys move at once.
	Concurrency int
	// VerifyBytes reads each destination back and digests it, which is the
	// only verification that holds at a store reporting no checksum of its
	// own. A dry run never reads bytes: there is no destination yet to read.
	VerifyBytes bool
	// VerifyMax is the largest object read back in full. Above it the run
	// reads VerifySample percent of the keys, chosen by a digest of the key
	// so the choice is the same on every run and spread over the manifest.
	VerifyMax int64
	// VerifySample is that percentage, 0 to 100.
	VerifySample int
}

// State is what became of one line.
type State int

// The four outcomes the report counts. Every line is exactly one of them.
const (
	// Copied is a destination this run wrote and read back.
	Copied State = iota
	// Skipped is a destination that already held the line's bytes, which is
	// what makes a killed run resumable and a finished one repeatable.
	Skipped
	// Mismatched is a destination holding other bytes than the line names.
	// Nothing is overwritten: an operator decides what a disagreement means.
	Mismatched
	// Failed is a line the store could not answer for, a missing source
	// among them.
	Failed
)

// Proof is how far a destination was proved to hold the object the manifest
// line names. A report that counted them together would let the weakest read
// as the strongest.
type Proof int

// The three proofs, weakest first.
const (
	// OnSize is the destination's length and the store's own copy, which is
	// all a line carries when its checksum is a digest no store reports and
	// the run read no bytes.
	OnSize Proof = iota
	// OnLabel is the store's label against the line's, which holds only
	// where the row carries a label a store reports for a whole object.
	OnLabel
	// OnBytes is the destination read back and digested against the line's
	// checksum. It is the one proof a store answering no checksum of its own
	// can give, and it is what criterion 4b of spec 019 rests on.
	OnBytes
)

// Outcome is one line's result.
type Outcome struct {
	// Entry is the manifest line.
	Entry manifest.Entry
	// Destination is the key the object id derives under the prefix.
	Destination string
	// State is what became of it.
	State State
	// Why names the disagreement or the failure, for a report an operator
	// acts on. It is empty for a copy and a skip.
	Why string
	// Verified is how far the destination was proved to be the object the
	// line names.
	Verified Proof
	// Unstamped says the destination is public in the rows and the store
	// holds no object ACLs, which spec 003 answers as ErrNotSupported and a
	// bucket policy covers instead.
	Unstamped bool
}

// Run moves every entry and answers one outcome per line, in the manifest's
// order. A key that fails does not stop the others: the point of the run is
// to leave an operator one list of what to look at, not the first thing that
// went wrong.
func (m *Move) Run(ctx context.Context, entries []manifest.Entry) []Outcome {
	outcomes := make([]Outcome, len(entries))
	lines := make(chan int)
	var workers sync.WaitGroup
	for range min(max(m.Concurrency, 1), max(len(entries), 1)) {
		workers.Go(func() {
			for i := range lines {
				outcomes[i] = m.one(ctx, entries[i])
			}
		})
	}
	for i := range entries {
		select {
		case lines <- i:
		case <-ctx.Done():
			// A cancelled run stops handing out work and reports what it
			// has, with the rest failed rather than silently absent.
			outcomes[i] = Outcome{Entry: entries[i], Destination: entries[i].ID.Key(m.Prefix),
				State: Failed, Why: ctx.Err().Error()}
		}
	}
	close(lines)
	workers.Wait()
	return outcomes
}

// one moves a single object: the destination first, so a run resumes on any
// store rather than on one whose conditional copy is honored.
func (m *Move) one(ctx context.Context, e manifest.Entry) Outcome {
	o := Outcome{Entry: e, Destination: e.ID.Key(m.Prefix)}
	if o.Destination == "" {
		o.State, o.Why = Failed, fmt.Sprintf("the object id %q derives no key", e.ID)
		return o
	}

	switch held, err := m.Bucket.Head(ctx, o.Destination); {
	case err == nil:
		// The destination is there. Whether it is the right object is the
		// same comparison a fresh copy is checked with, byte read included:
		// a resumed run proves what it skips, or the keys of a killed run
		// would be the only ones nobody ever checked.
		m.compare(&o, held)
		if o.State == Copied {
			m.verify(ctx, &o)
		}
		if o.State == Copied {
			o.State = Skipped
		}
		return o
	case !errors.Is(err, blob.ErrNotFound):
		o.State, o.Why = Failed, fmt.Sprintf("read the destination: %v", err)
		return o
	}

	if m.DryRun {
		return m.rehearse(ctx, o)
	}
	if _, err := m.Bucket.Copy(ctx, e.Key, o.Destination, blob.PutOptions{}); err != nil {
		switch {
		case errors.Is(err, blob.ErrNotFound):
			o.State, o.Why = Failed, fmt.Sprintf("the source is not in the bucket: %v", err)
		case errors.Is(err, blob.ErrPreconditionFailed):
			// Another writer took the destination between the read and the
			// copy. What it holds decides, exactly as it would have above.
			o.State, o.Why = Failed, fmt.Sprintf("the destination was written between the read and the copy: %v", err)
		default:
			o.State, o.Why = Failed, fmt.Sprintf("copy: %v", err)
		}
		return o
	}

	held, err := m.Bucket.Head(ctx, o.Destination)
	if err != nil {
		o.State, o.Why = Failed, fmt.Sprintf("read the destination back: %v", err)
		return o
	}
	m.compare(&o, held)
	if o.State != Copied {
		return o
	}
	m.verify(ctx, &o)
	if o.State != Copied {
		return o
	}
	m.stamp(ctx, &o)
	return o
}

// rehearse is the dry run's half: read the source, write nothing, and report
// what the run would do. The destination has already been read, so the counts
// a rehearsal prints are the counts the run prints.
func (m *Move) rehearse(ctx context.Context, o Outcome) Outcome {
	source, err := m.Bucket.Head(ctx, o.Entry.Key)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		o.State, o.Why = Failed, "the source is not in the bucket"
	case err != nil:
		o.State, o.Why = Failed, fmt.Sprintf("read the source: %v", err)
	case source.Size != o.Entry.Size:
		o.State, o.Why = Mismatched, fmt.Sprintf("the source holds %d bytes and the manifest says %d", source.Size, o.Entry.Size)
	case comparableLabel(o.Entry.Checksum):
		o.Verified = OnLabel
	case m.reads(o.Entry):
		// A rehearsal reads no bytes, because the destination it would read
		// is not written yet. What it can say is which keys the run will
		// read, which is what an operator sizes the run by.
		o.Verified = OnBytes
	}
	return o
}

// compare checks what the store holds against the line and records the
// verdict on the outcome.
//
// The size is compared always. The checksum is compared where both the line
// and the store's label are a digest of the whole object, which is thirty-two
// hexadecimal characters. A sha256 the predecessor computed for itself is not
// one, and neither is the composite label the store gives a destination
// assembled from ranges. Those lines are verified on their size and on the
// store's own copy, and the report says how many.
func (m *Move) compare(o *Outcome, held blob.Object) {
	if held.Size != o.Entry.Size {
		o.State, o.Why = Mismatched, fmt.Sprintf("the destination holds %d bytes and the manifest says %d", held.Size, o.Entry.Size)
		return
	}
	switch {
	case !comparableLabel(o.Entry.Checksum), !comparableLabel(held.ETag):
		// One of the two is not a digest of the whole object: the row's
		// checksum is a sha256 the predecessor computed, or the destination
		// was assembled from ranges and carries a composite label, which no
		// digest of the whole object ever equals. Comparing either would
		// fail a healthy copy. The byte read is what proves these.
		o.State, o.Verified = Copied, OnSize
	case held.ETag != o.Entry.Checksum:
		o.State, o.Why = Mismatched, fmt.Sprintf("the destination is labelled %q and the manifest says %q", held.ETag, o.Entry.Checksum)
	default:
		o.State, o.Verified = Copied, OnLabel
	}
}

// verify reads the destination back and digests it against the line's
// checksum, which is the one proof a store that reports no checksum of its own
// can give, and what criterion 4b of spec 019 rests on. Every store the family
// runs is such a store: neither the MinIO of the tier nor the Spaces of
// production answers a sha256 for an object.
//
// It runs where the line carries a sha256, which is what the predecessor
// stored for an object written in one piece. A composite label is not a digest
// of the bytes, so there is nothing to compare a read against and the read
// would cost the bytes and prove nothing.
func (m *Move) verify(ctx context.Context, o *Outcome) {
	if !m.reads(o.Entry) {
		return
	}
	digest, err := m.digest(ctx, o.Destination)
	switch {
	case err != nil:
		o.State, o.Why = Failed, fmt.Sprintf("read the destination's bytes back: %v", err)
	case digest != o.Entry.Checksum:
		o.State, o.Why = Mismatched, fmt.Sprintf("the destination's bytes digest to %s and the manifest says %s",
			digest, o.Entry.Checksum)
	default:
		o.Verified = OnBytes
	}
}

// digest streams one object through a sha256 and answers the hexadecimal
// digest. The bytes are never held: an object past the threshold is read at
// the reader's pace and dropped as it goes.
func (m *Move) digest(ctx context.Context, key string) (string, error) {
	rc, _, err := m.Bucket.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// reads reports whether this run digests one line's destination: every line
// whose checksum is a sha256 and whose object is at or under the threshold,
// and a sampled share of the larger ones.
//
// The sample is a digest of the key rather than a random draw, so two runs
// choose the same keys and a resumed run does not leave a hole where the first
// one read.
func (m *Move) reads(e manifest.Entry) bool {
	if !m.VerifyBytes {
		return false
	}
	if len(e.Checksum) != sha256HexLen || !isHex(e.Checksum) {
		return false
	}
	if e.Size <= m.VerifyMax {
		return true
	}
	if m.VerifySample <= 0 {
		return false
	}
	if m.VerifySample >= 100 {
		return true
	}
	sum := sha256.Sum256([]byte(e.Key))
	return int(binary.BigEndian.Uint32(sum[:4])%100) < m.VerifySample
}

// sha256HexLen is the length of a sha256 written in lowercase hexadecimal,
// which is the shape the predecessor stored for an object written in one
// piece.
const sha256HexLen = 64

// isHex reports whether every character is a lowercase hexadecimal digit,
// which is the spelling a digest is compared in.
func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

// stamp re-stamps a public destination, because a copy carries no ACL. A
// store that holds no object ACLs and serves publicity through a bucket
// policy instead answers ErrNotSupported, which spec 003 has a caller carry
// on from rather than fail on.
func (m *Move) stamp(ctx context.Context, o *Outcome) {
	if !o.Entry.Public {
		return
	}
	err := m.Bucket.SetPublic(ctx, o.Destination, true)
	switch {
	case err == nil:
	case errors.Is(err, blob.ErrNotSupported):
		o.Unstamped = true
	default:
		o.State, o.Why = Failed, fmt.Sprintf("stamp the destination public: %v", err)
	}
}

// comparableLabel reports whether a checksum from the rows has the shape of a
// label a store reports for an object written in one piece: thirty-two
// hexadecimal characters, which is what a destination's ETag is after a copy
// the store made in one call.
func comparableLabel(checksum string) bool {
	if len(checksum) != labelLen {
		return false
	}
	_, err := hex.DecodeString(checksum)
	return err == nil
}

// labelLen is the length of a single part label written in hexadecimal.
const labelLen = 32
