// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"sync"

	"latere.ai/x/arca/internal/blob"
)

// Sweep is the delete pass of step 5 of the sunset of spec 019: the source
// keys of a manifest, deleted after the move verified the destination each
// one was copied to.
//
// It is a pass of its own and never part of the copy. It reads the verdict
// the move took on every line, so no source is deleted before every
// destination of the run has been read back and, where the byte check runs,
// digested. One key is one DeleteObject: the manifest's first field is the
// only list, and a bulk delete of keys a listing produced would be a second
// list nobody wrote.
type Sweep struct {
	// Bucket is the store holding the source keys.
	Bucket blob.Store
	// DryRun names what the pass would delete and calls nothing.
	DryRun bool
	// Concurrency bounds how many keys are deleted at once.
	Concurrency int
}

// Fate is what became of one source key.
type Fate int

// The three fates. Every source key of the manifest is exactly one of them,
// and the one a zero value reads as is the one that deletes nothing.
const (
	// Kept is a source whose destination did not verify or is not there. The
	// bytes under it are the only copy left, so the pass leaves them.
	Kept Fate = iota
	// Deleted is a source whose destination this run verified, removed from
	// the bucket.
	Deleted
	// DeleteFailed is a source the store would not remove. The run is not
	// clean and an operator answers for the key.
	DeleteFailed
)

// Deletion is one source key's result.
type Deletion struct {
	// Source is the manifest's key, which is what the predecessor wrote.
	Source string
	// Fate is what became of it.
	Fate Fate
	// Proof is how far the destination was proved, carried from the move, so
	// a report never reads a delete taken on a length as one taken on the
	// bytes.
	Proof Proof
	// Why names the refusal or the failure. It is empty for a delete.
	Why string
}

// Run deletes the source of every verified destination and answers one
// deletion per line, in the manifest's order. A key the store will not remove
// does not stop the others: the point of the pass is one list of what is
// left, not the first thing that went wrong.
func (s *Sweep) Run(ctx context.Context, outcomes []Outcome) []Deletion {
	deletions := make([]Deletion, len(outcomes))
	lines := make(chan int)
	var workers sync.WaitGroup
	for range min(max(s.Concurrency, 1), max(len(outcomes), 1)) {
		workers.Go(func() {
			for i := range lines {
				deletions[i] = s.one(ctx, outcomes[i])
			}
		})
	}
	for i := range outcomes {
		lines <- i
	}
	close(lines)
	workers.Wait()
	return deletions
}

// one deletes a single source key, or says why it stays.
func (s *Sweep) one(ctx context.Context, o Outcome) Deletion {
	d := Deletion{Source: o.Entry.Key, Proof: o.Verified}
	if err := ctx.Err(); err != nil {
		// A cancelled pass deletes nothing more. Every line it has not reached
		// is kept, which is what a source key nobody deleted is.
		d.Why = err.Error()
		return d
	}
	if o.State != Copied && o.State != Skipped {
		d.Why = fmt.Sprintf("%s did not verify: %s", o.Destination, o.Why)
		return d
	}
	if s.DryRun {
		// A rehearsal reads the same verdict and calls nothing, so the keys it
		// names are the keys the run deletes.
		d.Fate = Deleted
		return d
	}
	if err := s.Bucket.Delete(ctx, o.Entry.Key); err != nil {
		d.Fate, d.Why = DeleteFailed, fmt.Sprintf("delete the source: %v", err)
		return d
	}
	d.Fate = Deleted
	return d
}
