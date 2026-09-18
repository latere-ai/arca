// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of the queries the seams of spec 010's passes 6 and 7 read,
// and of the two queries spec 012's bindings added: the id-addressed restore
// of a trashed object, the tombstone page and its conditional purge, the
// subtree drop that ends a workspace, the link count of the overview, and the
// grant hygiene of pass 7.
//
// What it proves is what a fake cannot: that the predicates mean over real
// rows what their Go halves say they mean, that a subtree delete takes the
// trash and the history with it, and that the link count reads exactly the
// rows a link listing serves.
package store

import (
	"testing"
	"time"

	"latere.ai/x/arca/object"
)

// reaperFile writes one live path of a space and answers the row.
func reaperFile(t *testing.T, db *DB, owner, path string, size int64) File {
	t.Helper()
	row := File{
		Owner: owner, Path: path, ObjectID: object.NewID(), CreatedBy: owner,
		SizeBytes: size, Checksum: "9f2c", ChecksumKind: object.ChecksumSHA256,
	}
	created, err := NewFiles().Insert(t.Context(), db.Querier(), row)
	if err != nil || !created {
		t.Fatalf("write %q: %v, %v", path, created, err)
	}
	back, err := NewFiles().Get(t.Context(), db.Querier(), owner, path)
	if err != nil {
		t.Fatalf("read %q back: %v", path, err)
	}
	return back
}

// reaperTrash moves a live path into the trash, at a deleted_at the case
// chose, so a window is driven without waiting for one.
func reaperTrash(t *testing.T, db *DB, owner, path string, at time.Time) {
	t.Helper()
	tag, err := db.Querier().Exec(t.Context(),
		`UPDATE files SET deleted_at = $3 WHERE owner = $1 AND path = $2`, owner, path, at)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("trash %q: %v, %d rows", path, err, tag.RowsAffected())
	}
}

// TestStoreRestoringByIDIsScopedToTheSpaceAndTheWindow is the file arm of
// spec 012's restore across owners as the database decides it: an id of
// another space and one past the retention window are both nothing to bring
// back, and a restore inside the window answers the row it changed.
func TestStoreRestoringByIDIsScopedToTheSpaceAndTheWindow(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner, stranger := wsSpace(t), wsSpace(t)
	window := time.Now().Add(-720 * time.Hour)

	row := reaperFile(t, db, owner, "files/reports/q3.pdf", 100)
	reaperTrash(t, db, owner, "files/reports/q3.pdf", time.Now())
	old := reaperFile(t, db, owner, "files/notes/old.txt", 50)
	reaperTrash(t, db, owner, "files/notes/old.txt", time.Now().Add(-800*time.Hour))
	live := reaperFile(t, db, owner, "files/plan.md", 10)

	for _, tc := range []struct {
		name  string
		owner string
		id    string
	}{
		{"another space's id", stranger, row.ID},
		{"an id that names nothing", owner, "8e1c0f00-0000-4000-8000-00000000dead"},
		{"a string that is not an id", owner, "not-a-uuid"},
		{"a row past the window", owner, old.ID},
		{"a live row", owner, live.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back, ok, err := files.RestoreByID(t.Context(), db.Querier(), tc.owner, tc.id, window)
			if err != nil || ok {
				t.Fatalf("RestoreByID = %+v, %t, %v", back, ok, err)
			}
		})
	}

	back, ok, err := files.RestoreByID(t.Context(), db.Querier(), owner, row.ID, window)
	if err != nil || !ok {
		t.Fatalf("RestoreByID = %t, %v", ok, err)
	}
	if back.Path != "files/reports/q3.pdf" || back.DeletedAt != nil {
		t.Fatalf("the restored row is %+v", back)
	}
	// The statement restores and reads in one round trip, so a second
	// restore of the same row finds nothing to do.
	if _, again, err := files.RestoreByID(t.Context(), db.Querier(), owner, row.ID, window); err != nil || again {
		t.Fatalf("a second restore = %t, %v", again, err)
	}
}

// TestStoreTheTombstonePageAndItsPurgeAgreeOnWhatIsPastTheWindow is pass 6's
// working set and its conditional end. A tombstone inside the window is
// nobody's, and a restore that lands between the two matches the delete out.
func TestStoreTheTombstonePageAndItsPurgeAgreeOnWhatIsPastTheWindow(t *testing.T) {
	db := tier(t)
	workspaces := NewWorkspaces()
	owner := wsSpace(t)
	cutoff := time.Now().Add(-720 * time.Hour)

	past := wsRow(t, db, owner, "past")
	recent := wsRow(t, db, owner, "recent")
	wsRow(t, db, owner, "live")
	for id, at := range map[string]time.Time{
		past.ID:   time.Now().Add(-800 * time.Hour),
		recent.ID: time.Now(),
	} {
		if _, err := db.Querier().Exec(t.Context(),
			`UPDATE workspaces SET deleted_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatalf("soft delete %q: %v", id, err)
		}
	}

	page, err := workspaces.Tombstones(t.Context(), db.Querier(), cutoff, 100)
	if err != nil {
		t.Fatalf("Tombstones: %v", err)
	}
	if len(page) != 1 || page[0].ID != past.ID {
		t.Fatalf("the page holds %v, want the one past the window", page)
	}

	// A restore lands before the purge, so the conditional delete matches no
	// row and the workspace somebody just brought back stays.
	if ok, err := workspaces.Restore(t.Context(), db.Querier(), past.ID); err != nil || !ok {
		t.Fatalf("the restore = %t, %v", ok, err)
	}
	if ended, err := workspaces.Purge(t.Context(), db.Querier(), past.ID); err != nil || ended {
		t.Fatalf("the purge of a restored tombstone = %t, %v", ended, err)
	}
	if _, err := workspaces.Get(t.Context(), db.Querier(), past.ID); err != nil {
		t.Fatalf("the restored workspace is gone: %v", err)
	}

	if _, err := db.Querier().Exec(t.Context(),
		`UPDATE workspaces SET deleted_at = now() WHERE id = $1`, past.ID); err != nil {
		t.Fatalf("soft delete again: %v", err)
	}
	if ended, err := workspaces.Purge(t.Context(), db.Querier(), past.ID); err != nil || !ended {
		t.Fatalf("the purge = %t, %v", ended, err)
	}
	if _, err := workspaces.Get(t.Context(), db.Querier(), past.ID); err == nil {
		t.Error("the purge left the workspace row")
	}
}

// TestStoreDroppingASubtreeTakesTheTrashAndTheHistoryWithIt: a purge ends a
// subtree, so a trashed row goes too, and the superseded contents go with
// it, because the ledger counts both tables and bytes left charged to a
// space that holds nothing is drift pass 10 would have to correct.
func TestStoreDroppingASubtreeTakesTheTrashAndTheHistoryWithIt(t *testing.T) {
	db := tier(t)
	owner := wsSpace(t)
	root := "workspaces/build/"

	live := reaperFile(t, db, owner, root+"src/main.go", 2814)
	reaperFile(t, db, owner, root+"src/old.go", 100)
	reaperTrash(t, db, owner, root+"src/old.go", time.Now())
	outside := reaperFile(t, db, owner, "files/notes.md", 10)
	// One superseded content of a path under the root, which spec 005 owns
	// and this tier writes directly.
	history := object.NewID()
	if _, err := db.Querier().Exec(t.Context(), `
		INSERT INTO file_versions (owner, path, version_no, object_id, size_bytes, checksum, created_by)
		VALUES ($1, $2, 1, $3, 40, '9f2c', $1)`, owner, root+"src/main.go", history); err != nil {
		t.Fatalf("write the history: %v", err)
	}

	freed, bytes, err := NewWorkspaceObjects().DropSubtree(t.Context(), db.Querier(), owner, root)
	if err != nil {
		t.Fatalf("DropSubtree: %v", err)
	}
	// Two file rows and one version, and the bytes of all three.
	if len(freed) != 3 || bytes != 2954 {
		t.Fatalf("the drop answered %d objects and %d bytes, want 3 and 2954", len(freed), bytes)
	}
	if _, err := NewFiles().Get(t.Context(), db.Querier(), owner, live.Path); err == nil {
		t.Error("the drop left a live row under the root")
	}
	if _, err := NewFiles().Get(t.Context(), db.Querier(), owner, outside.Path); err != nil {
		t.Errorf("the drop took a path outside the root: %v", err)
	}
	var versions int
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT count(*) FROM file_versions WHERE owner = $1`, owner).Scan(&versions); err != nil {
		t.Fatalf("count the history: %v", err)
	}
	if versions != 0 {
		t.Errorf("the drop left %d versions", versions)
	}
}

// TestStoreTheLinkCountIsTheLinkListingCounted: the overview of spec 012 and
// the link listing of spec 008 read one predicate, so a revoked or expired
// link is in neither.
func TestStoreTheLinkCountIsTheLinkListingCounted(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	owner, quiet := wsSpace(t), wsSpace(t)
	gone := time.Now().Add(-time.Hour)

	// A grant is created active, so the revoked row is written and then
	// revoked, which is the path a revoke takes in the service too.
	for _, g := range []Grant{
		{Owner: owner, PathPrefix: "files/a", GranteeKind: GranteeLink, Permission: "read",
			Token: "t-live-1", CreatedBy: owner},
		{Owner: owner, PathPrefix: "files/b", GranteeKind: GranteePublic, Permission: "read",
			Token: "t-live-2", CreatedBy: owner},
		{Owner: owner, PathPrefix: "files/d", GranteeKind: GranteeLink, Permission: "read",
			Token: "t-expired", ExpiresAt: &gone, CreatedBy: owner},
		{Owner: owner, PathPrefix: "files/e", GranteeKind: GranteeSubject, Grantee: "someone",
			Permission: "read", CreatedBy: owner},
	} {
		if _, err := shares.Create(t.Context(), db.Querier(), g); err != nil {
			t.Fatalf("write the grant on %q: %v", g.PathPrefix, err)
		}
	}
	revoked, err := shares.Create(t.Context(), db.Querier(), Grant{
		Owner: owner, PathPrefix: "files/c", GranteeKind: GranteeLink, Permission: "read",
		Token: "t-revoked", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("write the revoked link: %v", err)
	}
	if ok, err := shares.Revoke(t.Context(), db.Querier(), revoked.ID); err != nil || !ok {
		t.Fatalf("the revoke = %t, %v", ok, err)
	}

	counts, err := shares.CountLinks(t.Context(), db.Querier(), []string{owner, quiet})
	if err != nil {
		t.Fatalf("CountLinks: %v", err)
	}
	listed, err := shares.ListTokens(t.Context(), db.Querier(), owner, "", 100)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if counts[owner] != int64(len(listed)) || counts[owner] != 2 {
		t.Fatalf("the count is %d and the listing holds %d, want 2 of each", counts[owner], len(listed))
	}
	if _, named := counts[quiet]; named {
		t.Errorf("a space that holds no link is a row of the answer: %v", counts)
	}
}

// TestStoreTheGrantSweepTakesOnlyWhatExpiredAWholeWindowAgo is pass 7: a
// grant that stopped granting long ago leaves, and a revoked row that has
// not expired stays for the audit spec 008 keeps it for.
func TestStoreTheGrantSweepTakesOnlyWhatExpiredAWholeWindowAgo(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	owner := wsSpace(t)
	long := time.Now().Add(-800 * time.Hour)
	recently := time.Now().Add(-time.Hour)

	for _, g := range []Grant{
		{Owner: owner, PathPrefix: "files/a", GranteeKind: GranteeSubject, Grantee: "someone",
			Permission: "read", ExpiresAt: &long, CreatedBy: owner},
		{Owner: owner, PathPrefix: "files/b", GranteeKind: GranteeSubject, Grantee: "someone",
			Permission: "read", ExpiresAt: &recently, CreatedBy: owner},
	} {
		if _, err := shares.Create(t.Context(), db.Querier(), g); err != nil {
			t.Fatalf("write the grant on %q: %v", g.PathPrefix, err)
		}
	}
	// A revoked grant with no expiry is the audit's, and this sweep is not
	// about it: what leaves is what stopped granting on its own long ago.
	revoked, err := shares.Create(t.Context(), db.Querier(), Grant{
		Owner: owner, PathPrefix: "files/c", GranteeKind: GranteeSubject, Grantee: "someone",
		Permission: "read", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("write the revoked grant: %v", err)
	}
	if ok, err := shares.Revoke(t.Context(), db.Querier(), revoked.ID); err != nil || !ok {
		t.Fatalf("the revoke = %t, %v", ok, err)
	}
	cutoff := time.Now().Add(-720 * time.Hour)

	counted, err := shares.Expired(t.Context(), db.Querier(), cutoff)
	if err != nil {
		t.Fatalf("Expired: %v", err)
	}
	gone, err := shares.PurgeExpired(t.Context(), db.Querier(), cutoff)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	// The dry half and the working half read one condition, so they agree.
	if counted != 1 || gone != 1 {
		t.Fatalf("the count is %d and the purge took %d, want 1 of each", counted, gone)
	}
	left, err := shares.ListSpace(t.Context(), db.Querier(), owner, "", "", 100)
	if err != nil {
		t.Fatalf("ListSpace: %v", err)
	}
	if len(left) != 1 || left[0].PathPrefix != "files/b" {
		t.Fatalf("the space holds %v after the sweep", left)
	}
	var held int
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT count(*) FROM shares WHERE owner = $1`, owner).Scan(&held); err != nil {
		t.Fatalf("count the grants: %v", err)
	}
	if held != 2 {
		t.Errorf("the sweep left %d rows, want the recent expiry and the revoked one", held)
	}
}
