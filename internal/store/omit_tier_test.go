// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// limits.omit of spec 028 against a real Postgres: what the predicate every
// enumerating statement carries means over real rows. A fake proves that a
// statement binds the omission; only the database proves that the page it
// answers holds nothing under an omitted prefix, that a sibling whose name
// begins with the prefix is not taken for part of it, and that a page stays
// full when rows are left out of it.
package store

import (
	"slices"
	"testing"
	"time"

	"latere.ai/x/arca/object"
)

// omitFixture seeds one space with objects inside and outside the prefix
// the cases omit, and answers the space.
func omitFixture(t *testing.T, db *DB) string {
	t.Helper()
	owner := aSpaceOf(t)
	for path, size := range map[string]int64{
		"files/team/plan.md":               4,
		"files/team/payroll/may.pdf":       7,
		"files/team/payroll/2026/june.pdf": 9,
		"files/team/payroll-notes.md":      5,
		"files/team/pay%roll/x.md":         2,
		"files/other.md":                   1,
	} {
		reaperFile(t, db, owner, path, size)
	}
	return owner
}

// omitting is the answer the cases read under, and its absence.
var omitting = []string{"files/team/payroll"}

// pathsOfFiles answers the paths of a page, in its order.
func pathsOfFiles(page []File) []string {
	out := make([]string, 0, len(page))
	for _, f := range page {
		out = append(out, f.Path)
	}
	return out
}

// TestStoreAnOmittedSubtreeIsLeftOutOfEveryEnumeration holds each statement
// that enumerates to the rule of spec 028: nothing at or under an omitted
// prefix is read, counted, trashed or removed, and with no omission every
// row is.
func TestStoreAnOmittedSubtreeIsLeftOutOfEveryEnumeration(t *testing.T) {
	db := tier(t)
	q := db.Querier()
	files := NewFiles()
	outside := []string{
		"files/other.md", "files/team/pay%roll/x.md", "files/team/payroll-notes.md", "files/team/plan.md",
	}

	t.Run("the listing and its pages", func(t *testing.T) {
		owner := omitFixture(t, db)
		page, _, err := files.ListPrefix(t.Context(), q, owner, "files/", "", 100, omitting)
		if err != nil || !slices.Equal(pathsOfFiles(page), outside) {
			t.Fatalf("the listing holds %v, %v, want %v", pathsOfFiles(page), err, outside)
		}
		whole, _, err := files.ListPrefix(t.Context(), q, owner, "files/", "", 100, nil)
		if err != nil || len(whole) != 6 {
			t.Fatalf("the listing with no omission holds %d rows, %v", len(whole), err)
		}
		// A page is as full as what may be read allows, and the cursor
		// resumes on a row that was read.
		var walked []string
		cursor := ""
		for range 4 {
			rows, next, err := files.ListPrefix(t.Context(), q, owner, "files/", cursor, 2, omitting)
			if err != nil {
				t.Fatal(err)
			}
			walked = append(walked, pathsOfFiles(rows)...)
			if next == "" {
				break
			}
			if len(rows) != 2 {
				t.Errorf("a page before the last holds %d rows, want 2", len(rows))
			}
			cursor = next
		}
		if !slices.Equal(walked, outside) {
			t.Errorf("the walk read %v, want %v", walked, outside)
		}
		// An entry that names one object leaves out that object, and an
		// entry with a LIKE wildcard in it matches itself and nothing else.
		one, _, err := files.ListPrefix(t.Context(), q, owner, "files/", "", 100,
			[]string{"files/team/plan.md", "files/team/pay%roll"})
		if err != nil || !slices.Equal(pathsOfFiles(one), []string{
			"files/other.md", "files/team/payroll-notes.md",
			"files/team/payroll/2026/june.pdf", "files/team/payroll/may.pdf",
		}) {
			t.Errorf("the listing holds %v, %v", pathsOfFiles(one), err)
		}
		stat, err := files.StatSubtree(t.Context(), q, owner, "files/team", omitting)
		if err != nil || stat.Files != 3 || stat.Bytes != 11 {
			t.Errorf("the count under the folder is %+v, %v, want 3 rows and 11 bytes", stat, err)
		}
	})

	t.Run("the trash", func(t *testing.T) {
		owner := omitFixture(t, db)
		window := time.Now().Add(-time.Hour)
		trashed, err := files.TrashSubtree(t.Context(), q, owner, "files/team", omitting)
		if err != nil || trashed.Files != 3 || trashed.Bytes != 11 {
			t.Fatalf("the folder delete trashed %+v, %v, want 3 rows and 11 bytes", trashed, err)
		}
		live, _, err := files.ListPrefix(t.Context(), q, owner, "files/", "", 100, nil)
		if err != nil || !slices.Equal(pathsOfFiles(live), []string{
			"files/other.md", "files/team/payroll/2026/june.pdf", "files/team/payroll/may.pdf",
		}) {
			t.Fatalf("the space holds %v live, %v", pathsOfFiles(live), err)
		}
		// Everything under the folder is trashed now, by a second delete
		// with no omission, so the trash holds rows on both sides.
		if _, err := files.TrashSubtree(t.Context(), q, owner, "files/team", nil); err != nil {
			t.Fatal(err)
		}
		listed, err := files.ListTrash(t.Context(), q, owner, TrashCursor{}, 100, window, omitting)
		if err != nil || len(listed) != 3 {
			t.Fatalf("the trash with the prefix omitted lists %v, %v", pathsOfFiles(listed), err)
		}
		for _, f := range listed {
			if Omitted(omitting, f.Path) {
				t.Errorf("the trash lists %s", f.Path)
			}
		}
		purged, err := files.PurgeTrash(t.Context(), q, owner, "", omitting)
		if err != nil || len(purged) != 3 {
			t.Fatalf("emptying the trash purged %v, %v", pathsOfFiles(purged), err)
		}
		left, err := files.ListTrash(t.Context(), q, owner, TrashCursor{}, 100, window, nil)
		if err != nil || len(left) != 2 {
			t.Fatalf("the trash holds %v after it was emptied, %v", pathsOfFiles(left), err)
		}
		one, err := files.PurgeTrash(t.Context(), q, owner, "files/team/payroll/may.pdf", omitting)
		if err != nil || len(one) != 1 {
			t.Errorf("a purge of one path inside the omitted subtree purged %v, %v", pathsOfFiles(one), err)
		}
	})

	t.Run("the permanent folder delete and the grants under it", func(t *testing.T) {
		owner := omitFixture(t, db)
		shares := NewShares()
		for _, prefix := range []string{
			"files", "files/team", "files/team/plan.md", "files/team/payroll",
			"files/team/payroll/2026", "files/team/payroll-notes.md", "files/other.md",
		} {
			if _, err := shares.Create(t.Context(), q, Grant{
				Owner: owner, PathPrefix: prefix, GranteeKind: GranteeSubject,
				Grantee: "https://issuer.example|carol", Permission: "read", CreatedBy: owner,
			}); err != nil {
				t.Fatalf("grant %q: %v", prefix, err)
			}
		}
		gone, err := files.DropSubtree(t.Context(), q, owner, "files/team", omitting)
		if err != nil || gone.Files != 3 || gone.Bytes != 11 || len(gone.Objects) != 3 {
			t.Fatalf("the drop answered %+v, %v", gone, err)
		}
		removed, err := shares.DeleteUnder(t.Context(), q, owner, "files/team", omitting)
		if err != nil {
			t.Fatal(err)
		}
		left, err := shares.ListSpace(t.Context(), q, owner, "", "", 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		var prefixes []string
		for _, g := range left {
			prefixes = append(prefixes, g.PathPrefix)
		}
		slices.Sort(prefixes)
		// Gone: the two grants under the folder that cover nothing left in
		// place. Kept: the ones outside the folder, the ones at or under the
		// omitted subtree, and the one on the folder itself, which still
		// covers that subtree.
		if want := []string{
			"files", "files/other.md", "files/team", "files/team/payroll", "files/team/payroll/2026",
		}; removed != 2 || !slices.Equal(prefixes, want) {
			t.Errorf("the delete removed %d grants and left %v, want 2 and %v", removed, prefixes, want)
		}
		// With no omission the folder's grants all go.
		if _, err := files.DropSubtree(t.Context(), q, owner, "files/team", nil); err != nil {
			t.Fatal(err)
		}
		if n, err := shares.DeleteUnder(t.Context(), q, owner, "files/team", nil); err != nil || n != 3 {
			t.Errorf("the delete with no omission removed %d grants, %v, want the three under the folder", n, err)
		}
	})

	t.Run("the stars", func(t *testing.T) {
		owner, other := omitFixture(t, db), omitFixture(t, db)
		stars := NewStars()
		carol := "https://issuer.example|" + string(object.NewID())
		for _, space := range []string{owner, other} {
			for _, path := range []string{"files/team/plan.md", "files/team/payroll/may.pdf", "files/team/payroll-notes.md"} {
				if err := stars.Add(t.Context(), q, carol, space, path); err != nil {
					t.Fatal(err)
				}
			}
		}
		page, err := stars.List(t.Context(), q, carol, nil, StarCursor{}, 100, omitting)
		if err != nil || len(page) != 4 {
			t.Fatalf("the stars with the prefix omitted are %d, %v, want two in each space", len(page), err)
		}
		for _, s := range page {
			if Omitted(omitting, s.Path) {
				t.Errorf("the listing holds the star on %s", s.Path)
			}
		}
		if whole, err := stars.List(t.Context(), q, carol, nil, StarCursor{}, 100, nil); err != nil || len(whole) != 6 {
			t.Errorf("the stars with no omission are %d, %v", len(whole), err)
		}
	})

	t.Run("the grants, the links and a link's listing", func(t *testing.T) {
		owner := omitFixture(t, db)
		shares := NewShares()
		carol := "https://issuer.example|" + string(object.NewID())
		for _, prefix := range []string{"files/team", "files/team/payroll", "files/team/payroll/2026", "files/team/payroll-notes.md"} {
			if _, err := shares.Create(t.Context(), q, Grant{
				Owner: owner, PathPrefix: prefix, GranteeKind: GranteeSubject,
				Grantee: carol, Permission: "read", CreatedBy: owner,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := shares.Create(t.Context(), q, Grant{
				Owner: owner, PathPrefix: prefix, GranteeKind: GranteeLink,
				Permission: "read", Token: "tok-" + string(object.NewID()), CreatedBy: owner,
			}); err != nil {
				t.Fatal(err)
			}
		}
		prefixesOf := func(page []Grant, err error) []string {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, g := range page {
				out = append(out, g.PathPrefix)
			}
			slices.Sort(out)
			return out
		}
		// A grant above the omitted subtree is listed: its own prefix is not
		// inside what the answer left out.
		kept := []string{"files/team", "files/team/payroll-notes.md"}
		if got := prefixesOf(shares.ListGrantee(t.Context(), q, carol, "", 100, omitting)); !slices.Equal(got, kept) {
			t.Errorf("what is shared with the grantee is %v, want %v", got, kept)
		}
		if got := prefixesOf(shares.ListTokens(t.Context(), q, owner, "", 100, omitting)); !slices.Equal(got, kept) {
			t.Errorf("the links of the space are %v, want %v", got, kept)
		}
		if got := prefixesOf(shares.ListSpace(t.Context(), q, owner, "", "", 100, omitting)); len(got) != 4 {
			t.Errorf("the grants of the space with the prefix omitted are %v, want the two prefixes twice", got)
		}
		if got := prefixesOf(shares.ListSpace(t.Context(), q, owner, "", "", 100, nil)); len(got) != 8 {
			t.Errorf("the grants of the space with no omission are %v", got)
		}
		listing, err := shares.Subtree(t.Context(), q, owner, "files/team", "", 100, omitting)
		if err != nil || !slices.Equal(pathsOfFiles(listing), []string{
			"files/team/pay%roll/x.md", "files/team/payroll-notes.md", "files/team/plan.md",
		}) {
			t.Errorf("a link's listing holds %v, %v", pathsOfFiles(listing), err)
		}
		if whole, err := shares.Subtree(t.Context(), q, owner, "files/team", "", 100, nil); err != nil || len(whole) != 5 {
			t.Errorf("a link's listing with no omission holds %d rows, %v", len(whole), err)
		}
	})
}
