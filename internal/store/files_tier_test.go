// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of specs 005 and 007: the queries those specs own against a
// real Postgres. What it proves is what a fake cannot, which is what the SQL
// means: that a conditional write refuses the race it lost rather than
// overwriting, that a create-only write revives a trashed path and loses to
// a live one, that a version captures no bytes, that the trash window is a
// predicate and not a convention, and that an open upload session keeps its
// object out of the reaper's reach.
package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/arca/object"
)

// content answers a row with a checksum of its own, so a test can tell two
// writes of one path apart.
func content(owner, path, checksum string) File {
	f := aRow(owner, path)
	f.Checksum = strings.Repeat(checksum, 64)
	return f
}

func TestStoreTwoConditionalWritersOfOnePathLeaveOneWinner(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|cas"
	first := content(owner, "files/plan.md", "a")
	if err := files.Upsert(t.Context(), db.Querier(), first); err != nil {
		t.Fatal(err)
	}

	// Both writers read the same checksum and both write against it. The
	// condition is in the statement, so the second matches no row.
	var wins, refusals int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := content(owner, "files/plan.md", string(rune('b'+i)))
			next.ObjectID = object.NewID()
			ok, err := files.UpdateIfChecksum(t.Context(), db.Querier(), next, first.Checksum)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("the conditional write failed: %v", err)
				return
			}
			if ok {
				wins++
			} else {
				refusals++
			}
		}()
	}
	wg.Wait()
	if wins != 1 || refusals != 1 {
		t.Fatalf("two writers of one checksum left %d winners and %d refusals", wins, refusals)
	}
}

func TestStoreACreateOnlyWriteRevivesATrashedPathAndLosesToALiveOne(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|create-only"
	live := content(owner, "files/notes.md", "a")
	if err := files.Upsert(t.Context(), db.Querier(), live); err != nil {
		t.Fatal(err)
	}

	second := content(owner, "files/notes.md", "b")
	second.ObjectID = object.NewID()
	if ok, err := files.CreateOnly(t.Context(), db.Querier(), second); err != nil || ok {
		t.Fatalf("a create-only write onto a live row = %t, %v", ok, err)
	}

	if ok, err := files.SoftDelete(t.Context(), db.Querier(), owner, live.Path); err != nil || !ok {
		t.Fatalf("SoftDelete = %t, %v", ok, err)
	}
	if ok, err := files.CreateOnly(t.Context(), db.Querier(), second); err != nil || !ok {
		t.Fatalf("a create-only write onto a trashed row = %t, %v", ok, err)
	}
	back, err := files.Get(t.Context(), db.Querier(), owner, live.Path)
	if err != nil || back.DeletedAt != nil || back.Checksum != second.Checksum {
		t.Fatalf("the revived row is %+v, %v", back, err)
	}
	// The revived row keeps the subject that first wrote the path, which is
	// the column a create-only write must not take over.
	if back.CreatedBy != live.CreatedBy {
		t.Errorf("the revived row is credited to %q and the path was first written by %q", back.CreatedBy, live.CreatedBy)
	}
}

func TestStoreAVersionKeepsTheObjectTheFileHadAndCostsOneRow(t *testing.T) {
	db := tier(t)
	files, versions := NewFiles(), NewVersions()
	owner := "https://issuer.example|versions"
	first := content(owner, "files/plan.md", "a")
	if err := files.Upsert(t.Context(), db.Querier(), first); err != nil {
		t.Fatal(err)
	}
	if ok, err := versions.Capture(t.Context(), db.Querier(), owner, first.Path, ""); err != nil || !ok {
		t.Fatalf("Capture = %t, %v", ok, err)
	}
	second := content(owner, "files/plan.md", "b")
	second.ObjectID = object.NewID()
	if err := files.Upsert(t.Context(), db.Querier(), second); err != nil {
		t.Fatal(err)
	}
	if ok, err := versions.Capture(t.Context(), db.Querier(), owner, first.Path, ""); err != nil || !ok {
		t.Fatalf("the second Capture = %t, %v", ok, err)
	}

	page, err := versions.List(t.Context(), db.Querier(), owner, first.Path, 0, 10)
	if err != nil || len(page) != 2 {
		t.Fatalf("List = %d versions, %v", len(page), err)
	}
	if page[0].VersionNo != 1 || page[1].VersionNo != 2 {
		t.Fatalf("the numbers are %d and %d", page[0].VersionNo, page[1].VersionNo)
	}
	if page[0].ObjectID != first.ObjectID || page[1].ObjectID != second.ObjectID {
		t.Fatal("a version does not keep the object the file had")
	}
	// The bytes of a superseded version are still referenced, so nothing may
	// delete them.
	referenced, err := ObjectReferenced(t.Context(), db.Querier(), first.ObjectID)
	if err != nil || !referenced {
		t.Fatalf("the superseded object is referenced = %t, %v", referenced, err)
	}

	// A conditional capture against a checksum the row no longer has takes
	// nothing, which is what stops a lost race from writing history.
	if ok, err := versions.Capture(t.Context(), db.Querier(), owner, first.Path, first.Checksum); err != nil || ok {
		t.Fatalf("a capture conditioned on a stale checksum = %t, %v", ok, err)
	}

	removed, ok, err := versions.Delete(t.Context(), db.Querier(), owner, first.Path, 1)
	if err != nil || !ok || removed.ObjectID != first.ObjectID {
		t.Fatalf("Delete = %+v, %t, %v", removed, ok, err)
	}
	rest, err := versions.DeletePath(t.Context(), db.Querier(), owner, first.Path)
	if err != nil || len(rest) != 1 {
		t.Fatalf("DeletePath = %d versions, %v", len(rest), err)
	}
}

func TestStoreTheTrashIsNewestFirstAndTheWindowIsAPredicate(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|trash"
	for _, name := range []string{"a", "b", "c"} {
		f := content(owner, "files/"+name+".md", "a")
		if err := files.Upsert(t.Context(), db.Querier(), f); err != nil {
			t.Fatal(err)
		}
		if _, err := files.SoftDelete(t.Context(), db.Querier(), owner, f.Path); err != nil {
			t.Fatal(err)
		}
	}
	// Age one entry past every window a test would use.
	if _, err := db.Querier().Exec(t.Context(),
		`UPDATE files SET deleted_at = now() - interval '900 hours' WHERE owner = $1 AND path = $2`,
		owner, "files/a.md"); err != nil {
		t.Fatal(err)
	}

	window := time.Now().Add(-720 * time.Hour)
	page, err := files.ListTrash(t.Context(), db.Querier(), owner, TrashCursor{}, 10, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("the trash holds %d restorable entries", len(page))
	}
	if page[0].Path != "files/c.md" {
		t.Fatalf("the listing leads with %q and c.md was trashed last", page[0].Path)
	}
	// The second page resumes after the first entry and does not repeat it.
	next, err := files.ListTrash(t.Context(), db.Querier(), owner,
		TrashCursor{DeletedAt: *page[0].DeletedAt, Path: page[0].Path}, 10, window)
	if err != nil || len(next) != 1 || next[0].Path != page[1].Path {
		t.Fatalf("the second page is %v, %v", next, err)
	}

	if ok, err := files.Restore(t.Context(), db.Querier(), owner, "files/a.md", window); err != nil || ok {
		t.Fatalf("a restore past the window = %t, %v", ok, err)
	}
	if ok, err := files.Restore(t.Context(), db.Querier(), owner, "files/b.md", window); err != nil || !ok {
		t.Fatalf("a restore inside the window = %t, %v", ok, err)
	}
	back, err := files.Get(t.Context(), db.Querier(), owner, "files/b.md")
	if err != nil || back.DeletedAt != nil {
		t.Fatalf("the restored row is %+v, %v", back, err)
	}

	purged, err := files.PurgeTrash(t.Context(), db.Querier(), owner, "")
	if err != nil || len(purged) != 2 {
		t.Fatalf("PurgeTrash = %d rows, %v", len(purged), err)
	}
}

func TestStoreAStarFollowsAMoveAndLeavesTheListingWhenItsTargetIsTrashed(t *testing.T) {
	db := tier(t)
	files, stars := NewFiles(), NewStars()
	owner := "https://issuer.example|stars"
	subject := "https://issuer.example|reader"
	f := content(owner, "files/plan.md", "a")
	if err := files.Upsert(t.Context(), db.Querier(), f); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{subject, owner} {
		if err := stars.Add(t.Context(), db.Querier(), who, owner, f.Path); err != nil {
			t.Fatal(err)
		}
	}
	// Starring twice is one row.
	if err := stars.Add(t.Context(), db.Querier(), subject, owner, f.Path); err != nil {
		t.Fatal(err)
	}
	page, err := stars.List(t.Context(), db.Querier(), subject, StarCursor{}, 10)
	if err != nil || len(page) != 1 || page[0].File.Checksum != f.Checksum {
		t.Fatalf("List = %v, %v", page, err)
	}

	// The destination is starred by one of the two subjects already, so the
	// move has a collision to resolve and still leaves one row per subject.
	moved := content(owner, "files/archive/plan.md", "b")
	moved.ObjectID = object.NewID()
	if err := files.Upsert(t.Context(), db.Querier(), moved); err != nil {
		t.Fatal(err)
	}
	if err := stars.Add(t.Context(), db.Querier(), subject, owner, moved.Path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.HardDelete(t.Context(), db.Querier(), owner, moved.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := stars.Move(t.Context(), db.Querier(), owner, f.Path, moved.Path); err != nil {
		t.Fatal(err)
	}
	if ok, err := files.Move(t.Context(), db.Querier(), owner, f.Path, moved.Path); err != nil || !ok {
		t.Fatalf("Move = %t, %v", ok, err)
	}
	page, err = stars.List(t.Context(), db.Querier(), subject, StarCursor{}, 10)
	if err != nil || len(page) != 1 || page[0].Path != moved.Path {
		t.Fatalf("the star did not follow the move: %v, %v", page, err)
	}

	if _, err := files.SoftDelete(t.Context(), db.Querier(), owner, moved.Path); err != nil {
		t.Fatal(err)
	}
	page, err = stars.List(t.Context(), db.Querier(), subject, StarCursor{}, 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("a star on a trashed path is still listed: %v, %v", page, err)
	}
	if err := stars.Remove(t.Context(), db.Querier(), subject, owner, moved.Path); err != nil {
		t.Fatal(err)
	}
}
