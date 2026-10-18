// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The folder move of spec 030 against a real Postgres: what the statements
// a folder move runs mean over real rows. A fake proves what each binds;
// only the database proves that a rename keeps every other column, that a
// trashed row holds a destination, that a star and a version follow their
// path, and which grants the move carries and which it leaves behind.
package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// moveFixture seeds one space with a folder, what is beside it, and a grant
// on every prefix a case reads, and answers the space and the grants by
// prefix.
func moveFixture(t *testing.T, db *DB) (string, map[string]Grant) {
	t.Helper()
	owner := aSpaceOf(t)
	for path, size := range map[string]int64{
		"files/notes/a.md":          4,
		"files/notes/deep/b.md":     6,
		"files/notes/secret/s.md":   2,
		"files/notes/old.md":        3,
		"files/notes-archive/c.md":  2,
		"files/archive/existing.md": 1,
	} {
		reaperFile(t, db, owner, path, size)
	}
	reaperTrash(t, db, owner, "files/notes/old.md", time.Now())
	grants := map[string]Grant{}
	for _, prefix := range []string{
		"files", "files/notes", "files/notes/deep", "files/notes/a.md", "files/notes/old.md",
		"files/notes/secret", "files/notes-archive",
	} {
		grants[prefix] = grant(t, db, subjectGrant(owner, prefix, bobby, "read"))
	}
	return owner, grants
}

// prefixOf reads where a grant is now.
func prefixOf(t *testing.T, db *DB, g Grant) string {
	t.Helper()
	back, err := NewShares().Get(t.Context(), db.Querier(), g.ID)
	if err != nil {
		t.Fatalf("read the grant on %q back: %v", g.PathPrefix, err)
	}
	return back.PathPrefix
}

// TestStoreAFolderMoveRenamesTheRowsAndWhatKeysOnThem is criteria 1, 2, 3
// and 11 of spec 030 in Postgres: the live rows under the folder take their
// new paths with every other column kept, their history and their stars go
// with them, a trashed row stays, and the grants under the folder follow
// except the one on the path of the object that stayed.
func TestStoreAFolderMoveRenamesTheRowsAndWhatKeysOnThem(t *testing.T) {
	db := tier(t)
	q := db.Querier()
	files, versions, stars, shares := NewFiles(), NewVersions(), NewStars(), NewShares()
	owner, grants := moveFixture(t, db)
	before := map[string]File{}
	for _, path := range []string{"files/notes/a.md", "files/notes/deep/b.md", "files/notes/secret/s.md"} {
		row, err := files.Get(t.Context(), q, owner, path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = row
	}
	if ok, err := versions.Capture(t.Context(), q, owner, "files/notes/a.md", ""); err != nil || !ok {
		t.Fatalf("Capture = %t, %v", ok, err)
	}
	// Alice starred the source and, once, the destination; Bobby starred the
	// source alone.
	for _, s := range []struct{ subject, path string }{
		{alice, "files/notes/a.md"}, {alice, "files/archive/notes/a.md"}, {bobby, "files/notes/a.md"},
	} {
		if err := stars.Add(t.Context(), q, s.subject, owner, s.path); err != nil {
			t.Fatal(err)
		}
	}

	moves := []Rename{
		{From: "files/notes/a.md", To: "files/archive/notes/a.md"},
		{From: "files/notes/deep/b.md", To: "files/archive/notes/deep/b.md"},
		{From: "files/notes/secret/s.md", To: "files/archive/notes/secret/s.md"},
		// A trashed row is not renamed, even when it is named.
		{From: "files/notes/old.md", To: "files/archive/notes/old.md"},
	}
	dests := make([]string, 0, len(moves))
	for _, m := range moves {
		dests = append(dests, m.To)
	}
	if path, taken, err := files.Taken(t.Context(), q, owner, dests); err != nil || taken {
		t.Fatalf("a free destination reads as taken: %q, %t, %v", path, taken, err)
	}
	var counts [4]int64
	err := db.Tx(t.Context(), func(q Querier) error {
		var err error
		if counts[0], err = files.MoveMany(t.Context(), q, owner, moves); err != nil {
			return err
		}
		if counts[1], err = versions.MoveMany(t.Context(), q, owner, moves); err != nil {
			return err
		}
		if counts[2], err = stars.MoveMany(t.Context(), q, owner, moves); err != nil {
			return err
		}
		counts[3], err = shares.MoveUnder(t.Context(), q, owner, "files/notes", "files/archive/notes", nil)
		return err
	})
	// Three live rows, one version, a star of each subject, and four grants:
	// the folder's, the two on folders inside it, and the one on an object
	// that moved.
	if err != nil || counts != [4]int64{3, 1, 2, 4} {
		t.Fatalf("the move renamed %v rows, versions, stars and grants: %v", counts, err)
	}

	for from, row := range before {
		to := "files/archive/" + from[len("files/"):]
		moved, err := files.Get(t.Context(), q, owner, to)
		if err != nil {
			t.Fatalf("%q is not at %q: %v", from, to, err)
		}
		if moved.ID != row.ID || moved.ObjectID != row.ObjectID || moved.SizeBytes != row.SizeBytes ||
			moved.Checksum != row.Checksum || !moved.CreatedAt.Equal(row.CreatedAt) || moved.DeletedAt != nil {
			t.Errorf("%q moved as %+v from %+v", from, moved, row)
		}
		if _, err := files.Get(t.Context(), q, owner, from); err == nil {
			t.Errorf("%q still holds a row", from)
		}
	}
	if trashed, err := files.Get(t.Context(), q, owner, "files/notes/old.md"); err != nil || trashed.DeletedAt == nil {
		t.Errorf("the trashed row is %+v, %v; it stays where it was", trashed, err)
	}
	if history, err := versions.List(t.Context(), q, owner, "files/archive/notes/a.md", 0, 10); err != nil || len(history) != 1 {
		t.Errorf("the history at the new path is %v, %v", history, err)
	}
	for _, subject := range []string{alice, bobby} {
		page, err := stars.List(t.Context(), q, subject, []string{owner}, StarCursor{}, 10, nil)
		if err != nil || len(page) != 1 || page[0].Path != "files/archive/notes/a.md" {
			t.Errorf("the stars of %s are %v, %v, want one on the new path", subject, page, err)
		}
	}
	for prefix, want := range map[string]string{
		"files":               "files",
		"files/notes":         "files/archive/notes",
		"files/notes/deep":    "files/archive/notes/deep",
		"files/notes/a.md":    "files/archive/notes/a.md",
		"files/notes/secret":  "files/archive/notes/secret",
		"files/notes/old.md":  "files/notes/old.md",
		"files/notes-archive": "files/notes-archive",
	} {
		if got := prefixOf(t, db, grants[prefix]); got != want {
			t.Errorf("the grant on %q is on %q, want %q", prefix, got, want)
		}
	}
}

// TestStoreAFolderMoveLeavesTheGrantsAnOmissionKeeps: under an omission a
// grant at, under or above the omitted subtree stays, at the source end and
// at the destination end, and a grant on the path of an object at the
// folder's own path stays with that object.
func TestStoreAFolderMoveLeavesTheGrantsAnOmissionKeeps(t *testing.T) {
	db := tier(t)
	q := db.Querier()
	shares := NewShares()

	t.Run("at the source end", func(t *testing.T) {
		owner, grants := moveFixture(t, db)
		if _, err := shares.MoveUnder(t.Context(), q, owner, "files/notes", "files/archive/notes", []string{"files/notes/secret"}); err != nil {
			t.Fatal(err)
		}
		for prefix, want := range map[string]string{
			"files/notes":        "files/notes",
			"files/notes/secret": "files/notes/secret",
			"files/notes/deep":   "files/archive/notes/deep",
		} {
			if got := prefixOf(t, db, grants[prefix]); got != want {
				t.Errorf("the grant on %q is on %q, want %q", prefix, got, want)
			}
		}
	})
	t.Run("at the destination end", func(t *testing.T) {
		owner, grants := moveFixture(t, db)
		if _, err := shares.MoveUnder(t.Context(), q, owner, "files/notes", "files/archive/notes", []string{"files/archive/notes/deep"}); err != nil {
			t.Fatal(err)
		}
		for prefix, want := range map[string]string{
			"files/notes":        "files/notes",
			"files/notes/deep":   "files/notes/deep",
			"files/notes/secret": "files/archive/notes/secret",
		} {
			if got := prefixOf(t, db, grants[prefix]); got != want {
				t.Errorf("the grant on %q is on %q, want %q", prefix, got, want)
			}
		}
	})
	t.Run("an object at the folder's own path", func(t *testing.T) {
		owner, grants := moveFixture(t, db)
		reaperFile(t, db, owner, "files/notes", 5)
		if _, err := shares.MoveUnder(t.Context(), q, owner, "files/notes", "files/archive/notes", nil); err != nil {
			t.Fatal(err)
		}
		if got := prefixOf(t, db, grants["files/notes"]); got != "files/notes" {
			t.Errorf("the grant on the object at the folder's own path moved to %q", got)
		}
		if got := prefixOf(t, db, grants["files/notes/deep"]); got != "files/archive/notes/deep" {
			t.Errorf("the grant on a folder inside moved to %q", got)
		}
	})
}

// TestStoreAFolderMoveRefusesADestinationAnyRowHolds is criterion 4 in
// Postgres: a trashed row holds its path, an object the move would move away
// holds its path until it does, the check names the first in path order,
// and a rename onto a held path is a conflict that leaves every row where
// it was.
func TestStoreAFolderMoveRefusesADestinationAnyRowHolds(t *testing.T) {
	db := tier(t)
	q := db.Querier()
	files, versions := NewFiles(), NewVersions()
	owner, _ := moveFixture(t, db)
	reaperFile(t, db, owner, "files/archive/notes/old.md", 1)
	reaperTrash(t, db, owner, "files/archive/notes/old.md", time.Now())

	// A trashed row at a destination is taken.
	path, taken, err := files.Taken(t.Context(), q, owner, []string{"files/archive/notes/z.md", "files/archive/notes/old.md"})
	if err != nil || !taken || path != "files/archive/notes/old.md" {
		t.Fatalf("Taken = %q, %t, %v, want the trashed row", path, taken, err)
	}
	// The destination is the folder's parent: files/notes/deep moves to
	// files/notes, and the live row at files/notes/a.md is a source of
	// nothing here but holds a destination.
	path, taken, err = files.Taken(t.Context(), q, owner, []string{"files/notes/b.md", "files/notes/a.md"})
	if err != nil || !taken || path != "files/notes/a.md" {
		t.Fatalf("Taken = %q, %t, %v, want the live row", path, taken, err)
	}

	// Without the check, the rename meets the constraint and the
	// transaction leaves everything as it was.
	err = db.Tx(t.Context(), func(q Querier) error {
		_, err := files.MoveMany(t.Context(), q, owner, []Rename{
			{From: "files/notes/a.md", To: "files/archive/notes/a.md"},
			{From: "files/notes/deep/b.md", To: "files/archive/existing.md"},
		})
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a rename onto a held path = %v", err)
	}
	live, _, err := files.ListPrefix(t.Context(), q, owner, "files/", "", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pathsOfFiles(live), []string{
		"files/archive/existing.md", "files/notes-archive/c.md", "files/notes/a.md",
		"files/notes/deep/b.md", "files/notes/secret/s.md",
	}; !slices.Equal(got, want) {
		t.Errorf("after a refused rename the space holds %v, want %v", got, want)
	}

	// A history that already holds a version number at the destination is a
	// conflict too.
	for _, path := range []string{"files/notes/a.md", "files/archive/existing.md"} {
		if ok, err := versions.Capture(t.Context(), q, owner, path, ""); err != nil || !ok {
			t.Fatalf("Capture %q = %t, %v", path, ok, err)
		}
	}
	if _, err := versions.MoveMany(t.Context(), q, owner, []Rename{{From: "files/notes/a.md", To: "files/archive/existing.md"}}); !errors.Is(err, ErrConflict) {
		t.Errorf("a history moved onto one with the same version number = %v", err)
	}
}
