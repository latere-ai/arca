// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
)

// The unit tier over pass 6 of spec 010: what a tombstone past the window
// takes with it, what a dry run reports, and what a restore arriving mid-pass
// keeps.

// retention is the window every case here works to, the same value the
// trash of spec 005 gets.
const retention = 720 * time.Hour

// tombstones builds the pass over the harness's stores.
func (h *harness) tombstones(t *testing.T, opts ...func(*TombstoneOptions)) *Tombstones {
	t.Helper()
	o := TombstoneOptions{
		DB: h.store, Workspaces: h.store, Objects: h.store,
		Bucket: h.bucket, Prefix: "arca/", Retention: retention, Ledger: h.ledger,
	}
	for _, opt := range opts {
		opt(&o)
	}
	pass, err := NewTombstones(o)
	if err != nil {
		t.Fatalf("the pass would not build: %v", err)
	}
	return pass
}

// buried creates a workspace, deletes it, and moves the tombstone past the
// window, so the pass reads it without the test waiting a month.
func (h *harness) buried(t *testing.T, slug string) Workspace {
	t.Helper()
	ws := h.create(t, slug)
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d: %s", got.code, got.body)
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	row := h.store.workspaces[ws.ID]
	long := time.Now().Add(-retention - time.Hour)
	row.DeletedAt = &long
	h.store.workspaces[ws.ID] = row
	return ws
}

func TestATombstonePastTheWindowTakesItsSubtreeItsBytesAndItsRow(t *testing.T) {
	h := newHarness(t)
	ws := h.buried(t, "build")
	inside := h.store.put(h.subject, Root("build")+"src/main.go", "4f9a", 2814)
	h.store.put(h.subject, "files/notes.md", "aaaa", 10)
	if _, err := h.bucket.Put(t.Context(), inside.Key("arca/"), strings.NewReader("package main"), 12, blob.PutOptions{}); err != nil {
		t.Fatalf("the bytes would not go in: %v", err)
	}

	n, err := h.tombstones(t).Sweep(t.Context(), nil, time.Now(), false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("the pass purged %d tombstones, want 1", n)
	}
	if _, err := h.store.Get(t.Context(), nil, ws.ID); err == nil {
		t.Error("the purge left the workspace row")
	}
	if held, _ := h.store.Manifest(t.Context(), nil, h.subject, Root("build")); len(held) != 0 {
		t.Errorf("the purge left %d objects under the root", len(held))
	}
	// A path outside the root is not this workspace's and stays.
	if held, _ := h.store.Manifest(t.Context(), nil, h.subject, "files/"); len(held) != 1 {
		t.Errorf("the purge took the objects outside the root with it, leaving %d", len(held))
	}
	// The bytes go after the rows, which is invariant 1's order for a
	// delete: the keys are dropped once the commit stands.
	if _, err := h.bucket.Head(t.Context(), inside.Key("arca/")); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the purge left the bytes in the bucket: %v", err)
	}
	if released := h.ledger.released[h.subject]; released != 2814 {
		t.Errorf("the purge released %d bytes, want 2814", released)
	}
	if !slices.Contains(h.ledger.actions(), ActionPurge) {
		t.Errorf("the purge appended %v and no purge row", h.ledger.actions())
	}
}

// A dry run reports what a live run would purge and changes neither store.
// This pass can say so honestly: its working set is a read.
func TestADryTombstoneSweepCountsAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	ws := h.buried(t, "build")
	h.store.put(h.subject, Root("build")+"src/main.go", "4f9a", 2814)

	n, err := h.tombstones(t).Sweep(t.Context(), nil, time.Now(), true)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("the dry run reported %d, want the one a live run would purge", n)
	}
	if _, err := h.store.Get(t.Context(), nil, ws.ID); err != nil {
		t.Error("the dry run removed the workspace row")
	}
	if held, _ := h.store.Manifest(t.Context(), nil, h.subject, Root("build")); len(held) != 1 {
		t.Errorf("the dry run removed %d objects", 1-len(held))
	}
	if len(h.ledger.actions()) != 0 {
		t.Errorf("the dry run appended %v", h.ledger.actions())
	}
}

// A tombstone inside the window is still restorable and is not the pass's.
func TestATombstoneInsideTheWindowIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d", got.code)
	}
	n, err := h.tombstones(t).Sweep(t.Context(), nil, time.Now(), false)
	if err != nil || n != 0 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if _, err := h.store.Get(t.Context(), nil, ws.ID); err != nil {
		t.Error("a tombstone inside the window was purged")
	}
}

// A restore that landed between the page and the statement that would have
// ended the tombstone matches the conditional delete out, and the whole
// transaction rolls back: an administrator's undo is not undone by the
// housekeeping behind it, and the run is not a failure either.
func TestARestoreThatArrivesMidPassKeepsItsRowsAndIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	ws := h.buried(t, "build")
	h.store.put(h.subject, Root("build")+"src/main.go", "4f9a", 2814)
	h.store.mu.Lock()
	row := h.store.workspaces[ws.ID]
	row.DeletedAt = nil
	h.store.workspaces[ws.ID] = row
	h.store.mu.Unlock()

	// The page is the one the read answered a moment ago; the restore lands
	// before the delete, which is what the fake's Purge now refuses.
	pass := h.tombstones(t)
	stale := store.Workspace{ID: ws.ID, Owner: h.subject, Slug: "build"}
	if err := pass.purge(t.Context(), stale); !errors.Is(err, errRestored) {
		t.Fatalf("the purge of a restored tombstone answered %v", err)
	}
	if held, _ := h.store.Manifest(t.Context(), nil, h.subject, Root("build")); len(held) != 1 {
		t.Errorf("the rolled-back purge left %d objects under the root, want 1", len(held))
	}
	if swept, err := pass.Sweep(t.Context(), nil, time.Now(), false); err != nil || swept != 0 {
		t.Fatalf("the sweep of a restored tombstone = %d, %v", swept, err)
	}
}

// One tombstone that will not purge does not take the page down with it. Its
// rows stay and the next run tries again, and the run is reported as a
// failure so an operator sees it.
func TestATombstoneThatWillNotPurgeLeavesTheRestOfThePageToRun(t *testing.T) {
	h := newHarness(t)
	h.buried(t, "build")
	h.store.fail = map[string]error{"DropSubtree": errors.New("the connection failed")}
	n, err := h.tombstones(t).Sweep(t.Context(), nil, time.Now(), false)
	if err == nil {
		t.Fatal("a failed purge was reported as a clean run")
	}
	if n != 0 {
		t.Errorf("the pass counted %d purges it did not make", n)
	}
	h.store.fail = map[string]error{"Tombstones": errors.New("the connection failed")}
	if _, err := h.tombstones(t).Sweep(t.Context(), nil, time.Now(), false); err == nil {
		t.Fatal("a failed read of the working set was reported as a clean run")
	}
}

// The pass refuses what it would otherwise reach through a nil pointer on
// the first run, and refuses a window of no time: a retention of zero purges
// a workspace deleted a moment ago.
func TestTheTombstonePassRefusesToBuildWithoutWhatItReaches(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name   string
		break_ func(*TombstoneOptions)
	}{
		{"no database", func(o *TombstoneOptions) { o.DB = nil }},
		{"no workspace queries", func(o *TombstoneOptions) { o.Workspaces = nil }},
		{"no file plane", func(o *TombstoneOptions) { o.Objects = nil }},
		{"no bucket", func(o *TombstoneOptions) { o.Bucket = nil }},
		{"no window", func(o *TombstoneOptions) { o.Retention = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := TombstoneOptions{
				DB: h.store, Workspaces: h.store, Objects: h.store,
				Bucket: h.bucket, Prefix: "arca/", Retention: retention,
			}
			tc.break_(&o)
			if _, err := NewTombstones(o); err == nil {
				t.Fatalf("a pass with %s was built anyway", tc.name)
			}
		})
	}
	// A build that binds no ledger writes nothing rather than branching at
	// every call site.
	pass, err := NewTombstones(TombstoneOptions{
		DB: h.store, Workspaces: h.store, Objects: h.store,
		Bucket: h.bucket, Prefix: "arca/", Retention: retention,
	})
	if err != nil {
		t.Fatalf("a pass with no ledger would not build: %v", err)
	}
	h.buried(t, "build")
	if n, err := pass.Sweep(t.Context(), nil, time.Now(), false); err != nil || n != 1 {
		t.Fatalf("the pass with no ledger = %d, %v", n, err)
	}
}
