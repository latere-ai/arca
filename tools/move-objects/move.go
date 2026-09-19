// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	// SizeOnly says the line carried no label this store's own could be
	// compared against, so the verification is the size and the store's own
	// copy and not a digest.
	SizeOnly bool
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
// store rather than on one whose conditional copy is honoured.
func (m *Move) one(ctx context.Context, e manifest.Entry) Outcome {
	o := Outcome{Entry: e, Destination: e.ID.Key(m.Prefix)}
	if o.Destination == "" {
		o.State, o.Why = Failed, fmt.Sprintf("the object id %q derives no key", e.ID)
		return o
	}

	switch held, err := m.Bucket.Head(ctx, o.Destination); {
	case err == nil:
		// The destination is there. Whether it is the right object is the
		// same comparison a fresh copy is checked with.
		m.compare(&o, held)
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
	default:
		o.SizeOnly = !comparableLabel(o.Entry.Checksum)
	}
	return o
}

// compare checks what the store holds against the line and records the
// verdict on the outcome.
//
// The size is compared always. The checksum is compared where the line
// carries a label the store's own can be read against, which is a digest of
// the whole object written as thirty-two hexadecimal characters. A sha256 the
// predecessor computed for itself is not one, and neither is the composite
// label of an object assembled from parts, because a copy relabels the
// destination as one piece. Those lines are verified on their size and on the
// store's own copy, and the report says how many.
func (m *Move) compare(o *Outcome, held blob.Object) {
	if held.Size != o.Entry.Size {
		o.State, o.Why = Mismatched, fmt.Sprintf("the destination holds %d bytes and the manifest says %d", held.Size, o.Entry.Size)
		return
	}
	if comparableLabel(o.Entry.Checksum) {
		if held.ETag != o.Entry.Checksum {
			o.State, o.Why = Mismatched, fmt.Sprintf("the destination is labelled %q and the manifest says %q", held.ETag, o.Entry.Checksum)
			return
		}
		o.State = Copied
		return
	}
	o.State, o.SizeOnly = Copied, true
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
