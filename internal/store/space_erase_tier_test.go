// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 032: the deletion of a whole space against a real
// Postgres. What it proves is what a fake cannot: that every statement is
// scoped to the one owner, that the counts equal what the fixtures hold,
// and that the other space is whole afterward.
package store

import (
	"reflect"
	"testing"

	"latere.ai/x/arca/object"
)

// erasedCount reads one count, for the checks of what is left.
func erasedCount(t *testing.T, db *DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Querier().QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// erasureFixture is the space the deletion removes and the one it spares,
// with every kind of row the deletion names, and the object both spaces'
// rows share, which is what keeps its bytes.
type erasureFixture struct {
	gone, spared string
	shared       object.ID
}

// seedErasure writes the two spaces.
func seedErasure(t *testing.T, db *DB) erasureFixture {
	t.Helper()
	fx := erasureFixture{
		gone: adminSpace(t, "gone"), spared: adminSpace(t, "spared"), shared: object.NewID(),
	}
	q, ctx := db.Querier(), t.Context()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := q.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	for _, owner := range []string{fx.gone, fx.spared} {
		adminFile(t, db, owner, "files/a.txt", 10)
		adminFile(t, db, owner, "files/b.txt", 20)
		adminTrash(t, db, owner, "files/b.txt")
		adminUsage(t, db, owner, 30)
		exec(`INSERT INTO subjects (subject, display) VALUES ($1, 'a person')`, owner)
		exec(`INSERT INTO events (owner, path, action) VALUES ($1, 'files/a.txt', 'put')`, owner)
		exec(`INSERT INTO file_versions (owner, path, version_no, object_id, size_bytes, checksum, created_by)
		      VALUES ($1, 'files/a.txt', 1, $2, 5, '9f2c', $1)`, owner, object.NewID())
		exec(`INSERT INTO upload_sessions (owner, path, object_id, upload_id, declared_size, created_by, expires_at)
		      VALUES ($1, 'files/big.bin', $2, 'u-1', 100, $1, now() + interval '1 day')`, owner, object.NewID())
		exec(`INSERT INTO stars (subject, owner, path) VALUES ($1, $1, 'files/a.txt')`, owner)
		exec(`INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
		      VALUES ($1, 'files', 'subject', $2, 'read', $1)`, owner, "https://issuer.example|someone-else")
		exec(`INSERT INTO shares (owner, path_prefix, grantee_kind, permission, token, created_by)
		      VALUES ($1, 'files', 'link', 'read', $2, $1)`, owner, "tok-"+owner)
	}
	// The grants and the stars each space's owner holds on the other.
	exec(`INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
	      VALUES ($1, 'files', 'subject', $2, 'read', $1)`, fx.spared, fx.gone)
	exec(`INSERT INTO stars (subject, owner, path) VALUES ($1, $2, 'files/a.txt')`, fx.gone, fx.spared)
	exec(`INSERT INTO stars (subject, owner, path) VALUES ($1, $2, 'files/a.txt')`, fx.spared, fx.gone)
	// A row in each space names one object, which is the case a reference
	// check exists for.
	exec(`UPDATE files SET object_id = $1 WHERE path = 'files/a.txt' AND owner IN ($2, $3)`, fx.shared, fx.gone, fx.spared)
	return fx
}

// TestStoreEraseDeletesOneSpaceAndNothingElse is criterion 4 of spec 032.
func TestStoreEraseDeletesOneSpaceAndNothingElse(t *testing.T) {
	db := tier(t)
	fx := seedErasure(t, db)
	// The spared space's own counts, read before and after.
	rows := map[string]string{
		"files":    `SELECT count(*) FROM files WHERE owner = $1`,
		"versions": `SELECT count(*) FROM file_versions WHERE owner = $1`,
		"sessions": `SELECT count(*) FROM upload_sessions WHERE owner = $1`,
		"shares":   `SELECT count(*) FROM shares WHERE owner = $1 OR grantee = $1`,
		"stars":    `SELECT count(*) FROM stars WHERE owner = $1 OR subject = $1`,
		"usage":    `SELECT count(*) FROM space_usage WHERE owner = $1`,
		"subjects": `SELECT count(*) FROM subjects WHERE subject = $1`,
		"events":   `SELECT count(*) FROM events WHERE owner = $1`,
	}
	before := map[string]int{}
	for name, sql := range rows {
		before[name] = erasedCount(t, db, sql, fx.spared)
	}

	var got SpaceErased
	err := db.Tx(t.Context(), func(q Querier) error {
		var err error
		got, err = NewErasure().Erase(t.Context(), q, fx.gone, "space_delete")
		return err
	})
	if err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}

	want := SpaceErased{
		Files: 1, Trashed: 1, Versions: 1,
		// The space's own star, and the star the other owner placed on it.
		Stars: 2,
		// One grant to a subject and one link, on the space.
		Shares: 1, Links: 1,
		// The star the space's owner placed on the other space, and the grant
		// the other space made to them.
		StarsHeld: 1, SharesHeld: 1,
		Uploads: 1, Events: 1, Usage: 1, Subject: 1,
	}
	got.Objects = nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the erasure counted %+v, want %+v", got, want)
	}
	for name, sql := range rows {
		if n := erasedCount(t, db, sql, fx.gone); n != 0 {
			t.Errorf("the deleted space still holds %d %s", n, name)
		}
	}
	// A star on the spared space's path by someone else is one the deleted
	// owner placed; the spared space's own are untouched.
	if n := erasedCount(t, db, `SELECT count(*) FROM stars WHERE subject = $1 AND owner = $1`, fx.spared); n != 1 {
		t.Errorf("the spared space's own star count is %d, want 1", n)
	}
	// Only what ties the spared space to the deleted one goes: the grant the
	// deleted owner held on it, and the two stars that crossed.
	lost := map[string]int{"shares": 1, "stars": 2}
	for name, sql := range rows {
		if after := erasedCount(t, db, sql, fx.spared); after != before[name]-lost[name] {
			t.Errorf("the spared space holds %d %s, want %d: only the deleted owner's rows go", after, name, before[name]-lost[name])
		}
	}
}

// TestStoreEraseNamesTheObjectsOnceAndTheReferenceCheckSeesTheOtherSpace is
// the half of criterion 5 a database settles: the object both spaces name is
// still referenced after the deletion, and the others are not.
func TestStoreEraseNamesTheObjectsOnceAndTheReferenceCheckSeesTheOtherSpace(t *testing.T) {
	db := tier(t)
	fx := seedErasure(t, db)
	var got SpaceErased
	if err := db.Tx(t.Context(), func(q Querier) error {
		var err error
		got, err = NewErasure().Erase(t.Context(), q, fx.gone, "space_delete")
		return err
	}); err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	// Two live and trashed files, one version: three distinct objects, of
	// which the shared one is named once.
	if len(got.Objects) != 3 {
		t.Errorf("the erasure named %d objects, want 3 distinct", len(got.Objects))
	}
	for _, id := range got.Objects {
		referenced, err := ObjectReferenced(t.Context(), db.Querier(), id)
		if err != nil {
			t.Fatalf("read the references: %v", err)
		}
		if referenced != (id == fx.shared) {
			t.Errorf("object %s referenced=%v after the deletion, want %v", id, referenced, id == fx.shared)
		}
	}
}

// TestStoreEraseKeepsTheRecordOfAnEarlierDeletion is part of criterion 7: the
// log's delete spares the action it is told to, so a repeat does not remove
// the record of the first call.
func TestStoreEraseKeepsTheRecordOfAnEarlierDeletion(t *testing.T) {
	db := tier(t)
	owner := adminSpace(t, "record")
	q := db.Querier()
	if _, err := q.Exec(t.Context(), `INSERT INTO events (owner, action) VALUES ($1, 'space_delete')`, owner); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := NewErasure().Erase(t.Context(), q, owner, "space_delete")
	if err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if got.Total() != 0 {
		t.Errorf("a repeat counted %+v, want nothing", got)
	}
	if n := erasedCount(t, db, `SELECT count(*) FROM events WHERE owner = $1`, owner); n != 1 {
		t.Errorf("the log holds %d rows, want the one record of the earlier deletion", n)
	}
}

// TestStoreEraseOfNothingCountsNothing is criterion 8's store half.
func TestStoreEraseOfNothingCountsNothing(t *testing.T) {
	db := tier(t)
	got, err := NewErasure().Erase(t.Context(), db.Querier(), adminSpace(t, "never"), "space_delete")
	if err != nil {
		t.Fatalf("the erasure of a space that held nothing failed: %v", err)
	}
	if got.Total() != 0 || len(got.Objects) != 0 {
		t.Errorf("a space that held nothing counted %+v", got)
	}
}

// TestStoreOpenUploadsReadsOnlyTheSpacesOwn: the multiparts to abort are the
// space's own.
func TestStoreOpenUploadsReadsOnlyTheSpacesOwn(t *testing.T) {
	db := tier(t)
	fx := seedErasure(t, db)
	open, err := NewErasure().OpenUploads(t.Context(), db.Querier(), fx.gone)
	if err != nil {
		t.Fatalf("read the open uploads: %v", err)
	}
	if len(open) != 1 || open[0].UploadID != "u-1" {
		t.Errorf("the space has the open uploads %+v, want its own one", open)
	}
}
