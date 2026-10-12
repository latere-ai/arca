// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The migration of spec 028 against a real Postgres. 0006_one_tree.up.sql
// moves every object a space held under workspaces/<slug>/ into an ordinary
// folder under files/ and drops the two tables spec 009 owned.
//
// Every case here seeds a database at migration 0005, which is the schema an
// installation is on when it meets this file, and then applies the rest. The
// statements below write the two dropped tables directly for that reason:
// no query set of this build knows them.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/object"
)

// beforeOneTree is the last migration before the one under test.
const beforeOneTree = 5

// seededAtFive opens a database with a schema of its own holding migrations
// 0001 through 0005 and nothing later, and answers the pool and the
// connection string the rest is applied through.
func seededAtFive(t *testing.T) (*DB, string) {
	t.Helper()
	url, reason := stackDatabase()
	if reason != "" {
		t.Skip(reason)
	}
	schema := fmt.Sprintf("onetree_%d_%d", time.Now().UnixNano(), os.Getpid())
	admin, err := Open(t.Context(), url)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Querier().Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		dropper, err := Open(ctx, url)
		if err != nil {
			t.Errorf("drop the schema: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Querier().Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop the schema: %v", err)
		}
	})

	scoped := withSearchPath(url, schema)
	if err := MigrateTo(scoped, beforeOneTree); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), scoped)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(db.Close)
	return db, scoped
}

// exec runs one seeding statement.
func exec(t *testing.T, db *DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.Querier().Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(statement), " "), err)
	}
}

// aWorkspace seeds one row of the table the migration drops and answers its
// id. deleted is nil for a live one.
func aWorkspace(t *testing.T, db *DB, owner, slug string, deleted *time.Time) string {
	t.Helper()
	var id string
	if err := db.Querier().QueryRow(t.Context(), `
		INSERT INTO workspaces (owner, slug, created_by, deleted_at)
		VALUES ($1, $2, $1, $3) RETURNING id`, owner, slug, deleted).Scan(&id); err != nil {
		t.Fatalf("seed the workspace %q: %v", slug, err)
	}
	return id
}

// anObject seeds one file row with a size and answers it as the database
// holds it, times included.
func anObject(t *testing.T, db *DB, owner, path string, size int64) File {
	t.Helper()
	row := aRow(owner, path)
	row.SizeBytes = size
	if created, err := NewFiles().Insert(t.Context(), db.Querier(), row); err != nil || !created {
		t.Fatalf("seed %q: %v, %v", path, created, err)
	}
	return readObject(t, db, owner, path)
}

// readObject reads one path, failing the test when it is not there.
func readObject(t *testing.T, db *DB, owner, path string) File {
	t.Helper()
	f, err := NewFiles().Get(t.Context(), db.Querier(), owner, path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return f
}

// pathsOf answers every path a space holds, live and trashed, sorted.
func pathsOf(t *testing.T, db *DB, owner string) []string {
	t.Helper()
	rows, err := db.Querier().Query(t.Context(), `SELECT path FROM files WHERE owner = $1 ORDER BY path`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		out = append(out, path)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// tableExists reports whether the schema under test holds a table.
func tableExists(t *testing.T, db *DB, table string) bool {
	t.Helper()
	var exists bool
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// moveEvent is one row the migration appended to a space's log.
type moveEvent struct {
	path   string
	actor  *string
	detail map[string]any
}

// movesOf answers the move events of a space, in the order they were
// appended.
func movesOf(t *testing.T, db *DB, owner string) []moveEvent {
	t.Helper()
	rows, err := db.Querier().Query(t.Context(),
		`SELECT path, actor, detail FROM events WHERE owner = $1 AND action = 'move' ORDER BY id`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []moveEvent
	for rows.Next() {
		var (
			e   moveEvent
			raw []byte
		)
		if err := rows.Scan(&e.path, &e.actor, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStoreOneTreeMovesAWorkspaceIntoFiles is criterion 4 of spec 028: every
// object of a live workspace is at files/workspaces/<slug>/ afterwards with
// its row id, its object id, its checksum, its size and its times as they
// were, so no bucket key changes, and the space's usage is the number it
// was. A workspace a writer is attached to moves the same way, and its
// attachment ends with its table.
func TestStoreOneTreeMovesAWorkspaceIntoFiles(t *testing.T) {
	db, url := seededAtFive(t)
	owner := aSpaceOf(t)

	build := aWorkspace(t, db, owner, "build", nil)
	exec(t, db, `UPDATE workspaces SET writer_holder = 'sbx_1', writer_expires_at = now() + interval '1 hour' WHERE id = $1`, build)
	exec(t, db, `
		INSERT INTO workspace_attachments (workspace_id, holder, subject, mode, manifest, expires_at)
		VALUES ($1, 'sbx_1', $2, 'rw', '[]', now() + interval '1 hour')`, build, owner)
	aWorkspace(t, db, owner, "empty", nil)

	before := map[string]File{
		"main.go":         anObject(t, db, owner, "workspaces/build/main.go", 100),
		"cmd/tool/run.go": anObject(t, db, owner, "workspaces/build/cmd/tool/run.go", 250),
		"100%_done.txt":   anObject(t, db, owner, "workspaces/build/100%_done.txt", 7),
	}
	beside := anObject(t, db, owner, "files/notes/todo.md", 40)
	exec(t, db, `INSERT INTO space_usage (owner, bytes) VALUES ($1, 397)`, owner)

	if err := Migrate(url); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}

	for rest, was := range before {
		now := readObject(t, db, owner, "files/workspaces/build/"+rest)
		if now.ID != was.ID || now.ObjectID != was.ObjectID || now.Checksum != was.Checksum ||
			now.SizeBytes != was.SizeBytes || now.CreatedBy != was.CreatedBy {
			t.Errorf("%s moved as %+v, was %+v", rest, now, was)
		}
		if !now.CreatedAt.Equal(was.CreatedAt) || !now.UpdatedAt.Equal(was.UpdatedAt) {
			t.Errorf("%s moved with the times %s and %s, was %s and %s",
				rest, now.CreatedAt, now.UpdatedAt, was.CreatedAt, was.UpdatedAt)
		}
		if now.DeletedAt != nil {
			t.Errorf("%s of a live workspace was trashed", rest)
		}
	}
	if now := readObject(t, db, owner, beside.Path); now.ID != beside.ID || !now.UpdatedAt.Equal(beside.UpdatedAt) {
		t.Errorf("an object outside the workspace changed: %+v", now)
	}
	want := []string{
		"files/notes/todo.md",
		"files/workspaces/build/100%_done.txt",
		"files/workspaces/build/cmd/tool/run.go",
		"files/workspaces/build/main.go",
	}
	if got := pathsOf(t, db, owner); !slices.Equal(got, want) {
		t.Errorf("the space holds %v, want %v", got, want)
	}

	var usage int64
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT bytes FROM space_usage WHERE owner = $1`, owner).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 397 {
		t.Errorf("the space's usage is %d after the migration, was 397", usage)
	}
	// The ledger and a recomputation over the rows still agree, so the
	// reconciliation pass has nothing to correct.
	var summed int64
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT COALESCE(SUM(size_bytes), 0) FROM files WHERE owner = $1`, owner).Scan(&summed); err != nil {
		t.Fatal(err)
	}
	if summed != 397 {
		t.Errorf("the rows of the space sum to %d, was 397", summed)
	}

	for _, table := range []string{"workspaces", "workspace_attachments"} {
		if tableExists(t, db, table) {
			t.Errorf("the migration left the table %s", table)
		}
	}
	// One move per workspace, the empty one included, with no actor.
	moves := movesOf(t, db, owner)
	if len(moves) != 2 {
		t.Fatalf("the log holds %d moves, want one per workspace: %+v", len(moves), moves)
	}
	for _, e := range moves {
		if e.actor != nil {
			t.Errorf("the move at %s names the actor %q", e.path, *e.actor)
		}
	}
	if e := moves[0]; e.path != "files/workspaces/build" || e.detail["from"] != "workspaces/build" ||
		e.detail["files"] != float64(3) || e.detail["trashed"] != false {
		t.Errorf("the move of the workspace is %+v", e)
	}
	if e := moves[1]; e.path != "files/workspaces/empty" || e.detail["files"] != float64(0) {
		t.Errorf("the move of the empty workspace is %+v", e)
	}
	if pending, err := Pending(t.Context(), db.Querier()); err != nil || len(pending) != 0 {
		t.Errorf("Pending after the migration = %v, %v", pending, err)
	}
}

// TestStoreOneTreeNeverOverwritesAPath is criterion 5: where the space
// already holds something at or under files/workspaces/<slug>, live or
// trashed, the workspace takes the next free folder, a folder another
// workspace's own name claims is not free, and no row that was there
// changes.
func TestStoreOneTreeNeverOverwritesAPath(t *testing.T) {
	db, url := seededAtFive(t)
	owner := aSpaceOf(t)

	for _, slug := range []string{"build", "build-2", "site", "docs"} {
		aWorkspace(t, db, owner, slug, nil)
		anObject(t, db, owner, "workspaces/"+slug+"/a.txt", 10)
	}
	// What the space already holds: a live object under the folder build
	// would take, a trashed one under the folder site would take, and a
	// grant on the folder docs would take.
	live := anObject(t, db, owner, "files/workspaces/build/keep.md", 5)
	trashed := anObject(t, db, owner, "files/workspaces/site/old.md", 6)
	exec(t, db, `UPDATE files SET deleted_at = now() WHERE id = $1`, trashed.ID)
	trashed = readObject(t, db, owner, trashed.Path)
	exec(t, db, `
		INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
		VALUES ($1, 'files/workspaces/docs', 'subject', 'https://issuer.example|carol', 'read', $1)`, owner)

	if err := Migrate(url); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}

	want := []string{
		"files/workspaces/build-2/a.txt", // the workspace named build-2 keeps its own name
		"files/workspaces/build-3/a.txt", // build finds build and build-2 taken
		"files/workspaces/build/keep.md",
		"files/workspaces/docs-2/a.txt",
		"files/workspaces/site-2/a.txt",
		"files/workspaces/site/old.md",
	}
	if got := pathsOf(t, db, owner); !slices.Equal(got, want) {
		t.Fatalf("the space holds %v, want %v", got, want)
	}
	if now := readObject(t, db, owner, live.Path); now.ID != live.ID || now.ObjectID != live.ObjectID ||
		now.DeletedAt != nil || !now.UpdatedAt.Equal(live.UpdatedAt) {
		t.Errorf("the live object that was there changed: %+v", now)
	}
	if now := readObject(t, db, owner, trashed.Path); now.ID != trashed.ID || now.DeletedAt == nil ||
		!now.DeletedAt.Equal(*trashed.DeletedAt) {
		t.Errorf("the trashed object that was there changed: %+v", now)
	}
	// The grant that was on files/workspaces/docs still names that folder
	// and covers nothing the workspace held.
	covering, err := NewShares().Covering(t.Context(), db.Querier(), owner, "files/workspaces/docs-2/a.txt")
	if err != nil || len(covering) != 0 {
		t.Errorf("a grant given for another folder covers the moved object: %+v, %v", covering, err)
	}
	var froms []string
	for _, e := range movesOf(t, db, owner) {
		froms = append(froms, e.detail["from"].(string)+" -> "+e.path)
	}
	slices.Sort(froms)
	if want := []string{
		"workspaces/build -> files/workspaces/build-3",
		"workspaces/build-2 -> files/workspaces/build-2",
		"workspaces/docs -> files/workspaces/docs-2",
		"workspaces/site -> files/workspaces/site-2",
	}; !slices.Equal(froms, want) {
		t.Errorf("the log says %v, want %v", froms, want)
	}
}

// TestStoreOneTreeTrashesADeletedWorkspace is criterion 6: what a soft
// deleted workspace held is in the trash at the workspace's own deletion
// time, so the owner keeps what is left of the window and the trash pass
// purges it at the moment the tombstone pass would have. Rows under
// workspaces/ that no workspace names are trashed at the migration's time.
func TestStoreOneTreeTrashesADeletedWorkspace(t *testing.T) {
	db, url := seededAtFive(t)
	owner := aSpaceOf(t)
	deletedAt := time.Now().Add(-10 * 24 * time.Hour).UTC().Truncate(time.Microsecond)

	aWorkspace(t, db, owner, "old", &deletedAt)
	kept := anObject(t, db, owner, "workspaces/old/report.pdf", 64)
	anObject(t, db, owner, "workspaces/old/data/rows.csv", 36)
	orphan := anObject(t, db, owner, "workspaces/gone/left.bin", 12)
	exec(t, db, `INSERT INTO space_usage (owner, bytes) VALUES ($1, 112)`, owner)
	started := time.Now().Add(-time.Minute)

	if err := Migrate(url); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}

	for _, path := range []string{"files/workspaces/old/report.pdf", "files/workspaces/old/data/rows.csv"} {
		row := readObject(t, db, owner, path)
		if row.DeletedAt == nil || !row.DeletedAt.Equal(deletedAt) {
			t.Errorf("%s is trashed at %v, want the workspace's own %s", path, row.DeletedAt, deletedAt)
		}
	}
	left := readObject(t, db, owner, "files/workspaces/gone/left.bin")
	if left.ID != orphan.ID || left.DeletedAt == nil || left.DeletedAt.Before(started) {
		t.Errorf("a row no workspace named is %+v, want it trashed at the migration's time", left)
	}

	// Trashed is not listed, and is in the trash for the rest of the window.
	files := NewFiles()
	listed, _, err := files.ListPrefix(t.Context(), db.Querier(), owner, "files/", "", 100, nil)
	if err != nil || len(listed) != 0 {
		t.Errorf("a listing answers %d of the trashed objects, %v", len(listed), err)
	}
	window := time.Now().Add(-30 * 24 * time.Hour)
	trash, err := files.ListTrash(t.Context(), db.Querier(), owner, TrashCursor{}, 10, window, nil)
	if err != nil || len(trash) != 3 {
		t.Fatalf("the trash holds %d entries, %v, want the three", len(trash), err)
	}
	// One object comes back by itself, with the id it always had.
	restored, err := files.Restore(t.Context(), db.Querier(), owner, "files/workspaces/old/report.pdf", window)
	if err != nil || !restored {
		t.Fatalf("restore one object: %v, %v", restored, err)
	}
	if now := readObject(t, db, owner, "files/workspaces/old/report.pdf"); now.DeletedAt != nil || now.ID != kept.ID {
		t.Errorf("the restored object is %+v", now)
	}
	// Past the window it is not restorable, which is the same moment the
	// workspace itself would have stopped being so: the deletion time plus
	// the retention.
	late := deletedAt.Add(time.Second)
	if restored, err := files.Restore(t.Context(), db.Querier(), owner, "files/workspaces/old/data/rows.csv", late); err != nil || restored {
		t.Errorf("an object past the workspace's own window was restored: %v, %v", restored, err)
	}

	var usage int64
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT bytes FROM space_usage WHERE owner = $1`, owner).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 112 {
		t.Errorf("the space's usage is %d after the migration, was 112", usage)
	}
	moves := movesOf(t, db, owner)
	if len(moves) != 2 {
		t.Fatalf("the log holds %d moves, want two: %+v", len(moves), moves)
	}
	for _, e := range moves {
		if e.detail["trashed"] != true {
			t.Errorf("the move at %s is not marked trashed: %+v", e.path, e.detail)
		}
	}
}

// TestStoreOneTreeCarriesWhatKeysOnThePath is criterion 7: a star, a grant,
// a public link and an open upload session on a workspace path resolve to
// the same object after the migration, and the history of a path follows
// it.
func TestStoreOneTreeCarriesWhatKeysOnThePath(t *testing.T) {
	db, url := seededAtFive(t)
	owner := aSpaceOf(t)
	carol := "https://issuer.example|carol"

	aWorkspace(t, db, owner, "build", nil)
	target := anObject(t, db, owner, "workspaces/build/docs/guide.md", 20)
	exec(t, db, `INSERT INTO stars (subject, owner, path) VALUES ($1, $2, 'workspaces/build/docs/guide.md')`, carol, owner)
	exec(t, db, `
		INSERT INTO file_versions (owner, path, version_no, object_id, size_bytes, checksum, created_by)
		VALUES ($1, 'workspaces/build/docs/guide.md', 1, $2, 9, $3, $1)`,
		owner, object.NewID(), strings.Repeat("b", 64))
	exec(t, db, `
		INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
		VALUES ($1, 'workspaces/build', 'subject', $2, 'write', $1),
		       ($1, 'workspaces/build/docs/guide.md', 'subject', $2, 'read', $1)`, owner, carol)
	exec(t, db, `
		INSERT INTO shares (owner, path_prefix, grantee_kind, permission, token, created_by)
		VALUES ($1, 'workspaces/build/docs', 'link', 'read', $2, $1)`, owner, "tok-"+string(object.NewID()))
	exec(t, db, `
		INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
		VALUES ($1, 'workspaces', 'subject', $2, 'read', $1)`, owner, "https://issuer.example|dave")
	session := object.NewID()
	exec(t, db, `
		INSERT INTO upload_sessions (owner, path, object_id, upload_id, declared_size, created_by, expires_at)
		VALUES ($1, 'workspaces/build/video.mp4', $2, 'upload-1', 4096, $1, now() + interval '1 day')`, owner, session)

	if err := Migrate(url); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}
	q := db.Querier()
	moved := "files/workspaces/build/docs/guide.md"

	stars, err := NewStars().List(t.Context(), q, carol, nil, StarCursor{}, 10, nil)
	if err != nil || len(stars) != 1 || stars[0].Path != moved || stars[0].File.SizeBytes != target.SizeBytes {
		t.Errorf("the star resolves to %+v, %v", stars, err)
	}
	history, err := NewVersions().List(t.Context(), q, owner, moved, 0, 10)
	if err != nil || len(history) != 1 || history[0].VersionNo != 1 {
		t.Errorf("the history of the moved path is %+v, %v", history, err)
	}
	covering, err := NewShares().Covering(t.Context(), q, owner, moved)
	if err != nil {
		t.Fatal(err)
	}
	var prefixes []string
	for _, g := range covering {
		prefixes = append(prefixes, g.GranteeKind+" "+g.PathPrefix+" "+g.Permission)
	}
	slices.Sort(prefixes)
	if want := []string{
		"link files/workspaces/build/docs read",
		"subject files/workspaces read",
		"subject files/workspaces/build write",
		"subject files/workspaces/build/docs/guide.md read",
	}; !slices.Equal(prefixes, want) {
		t.Errorf("the grants covering the moved object are %v, want %v", prefixes, want)
	}
	var sessionPath string
	if err := q.QueryRow(t.Context(),
		`SELECT path FROM upload_sessions WHERE object_id = $1`, session).Scan(&sessionPath); err != nil {
		t.Fatal(err)
	}
	if sessionPath != "files/workspaces/build/video.mp4" {
		t.Errorf("the open upload names %q", sessionPath)
	}
	// Nothing that keys on a path still names the removed plane.
	for table, column := range map[string]string{
		"files": "path", "file_versions": "path", "stars": "path",
		"upload_sessions": "path", "shares": "path_prefix",
	} {
		var left int
		if err := q.QueryRow(t.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = 'workspaces' OR `+column+` LIKE 'workspaces/%'`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Errorf("%s holds %d rows still under the removed plane", table, left)
		}
	}
}

// TestStoreOneTreeNarrowsAGrantItCannotCarry: a grant on the whole removed
// plane covered every workspace of the space, which is now the folder
// files/workspaces. Where the space already held something there, the grant
// would cover paths nobody shared, so it is revoked and not moved.
func TestStoreOneTreeNarrowsAGrantItCannotCarry(t *testing.T) {
	db, url := seededAtFive(t)
	owner := aSpaceOf(t)

	aWorkspace(t, db, owner, "build", nil)
	anObject(t, db, owner, "workspaces/build/a.txt", 10)
	anObject(t, db, owner, "files/workspaces/private/notes.md", 5)
	exec(t, db, `
		INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
		VALUES ($1, 'workspaces', 'subject', 'https://issuer.example|dave', 'read', $1)`, owner)

	if err := Migrate(url); err != nil {
		t.Fatalf("apply the migration: %v", err)
	}
	for _, path := range []string{"files/workspaces/private/notes.md", "files/workspaces/build/a.txt"} {
		covering, err := NewShares().Covering(t.Context(), db.Querier(), owner, path)
		if err != nil || len(covering) != 0 {
			t.Errorf("the grant on the removed plane covers %s: %+v, %v", path, covering, err)
		}
	}
	var status string
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT status FROM shares WHERE owner = $1`, owner).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != GrantRevoked {
		t.Errorf("the grant is %q, want it revoked", status)
	}
}

// TestStoreOneTreeIsAllOrNothing is criterion 8: a fault in the middle of
// the migration leaves every row and both tables as they were, and the same
// database then takes the migration whole.
func TestStoreOneTreeIsAllOrNothing(t *testing.T) {
	db, url := seededAtFive(t)
	owner := aSpaceOf(t)
	q := db.Querier()

	aWorkspace(t, db, owner, "alpha", nil)
	aWorkspace(t, db, owner, "beta", nil)
	anObject(t, db, owner, "workspaces/alpha/a.txt", 10)
	anObject(t, db, owner, "workspaces/beta/b.txt", 20)
	before := pathsOf(t, db, owner)

	// The fault: the log refuses the second move, which the migration
	// appends after the first workspace's rows have already been rewritten.
	exec(t, db, `
		CREATE FUNCTION refuse_second_move() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		    IF NEW.path = 'files/workspaces/beta' THEN
		        RAISE EXCEPTION 'the log refused the row';
		    END IF;
		    RETURN NEW;
		END $$`)
	exec(t, db, `CREATE TRIGGER refuse_second_move BEFORE INSERT ON events
		FOR EACH ROW EXECUTE FUNCTION refuse_second_move()`)

	err := Migrate(url)
	if err == nil {
		t.Fatal("the migration reported success over a fault")
	}
	if !strings.Contains(err.Error(), "the log refused the row") {
		t.Fatalf("the migration failed for another reason: %v", err)
	}
	if got := pathsOf(t, db, owner); !slices.Equal(got, before) {
		t.Errorf("a failed migration left %v, was %v", got, before)
	}
	for _, table := range []string{"workspaces", "workspace_attachments"} {
		if !tableExists(t, db, table) {
			t.Errorf("a failed migration dropped %s", table)
		}
	}
	if moves := movesOf(t, db, owner); len(moves) != 0 {
		t.Errorf("a failed migration left %d move events", len(moves))
	}

	// The migrator marks a failed step dirty and refuses to go on until an
	// operator has looked, which is its rule for every migration. With the
	// fault gone and the mark cleared, the same database takes the step.
	exec(t, db, `DROP TRIGGER refuse_second_move ON events`)
	if _, err := Pending(t.Context(), q); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Errorf("Pending after a failed migration = %v, want the dirty mark named", err)
	}
	exec(t, db, `UPDATE schema_migrations SET version = $1, dirty = false`, beforeOneTree)
	if err := Migrate(url); err != nil {
		t.Fatalf("the migration after the fault was removed: %v", err)
	}
	want := []string{"files/workspaces/alpha/a.txt", "files/workspaces/beta/b.txt"}
	if got := pathsOf(t, db, owner); !slices.Equal(got, want) {
		t.Errorf("the space holds %v, want %v", got, want)
	}
	if moves := movesOf(t, db, owner); len(moves) != 2 {
		t.Errorf("the log holds %d moves, want one per workspace", len(moves))
	}
	if tableExists(t, db, "workspaces") || tableExists(t, db, "workspace_attachments") {
		t.Error("the migration left a table it drops")
	}
}
