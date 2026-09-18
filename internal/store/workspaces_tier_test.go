// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 009: the workspace and attachment queries against a
// real Postgres. What it proves is what a fake cannot, and what spec 009's
// first criterion asks for by name: that two rw attaches arriving at once
// leave exactly one lease, decided by the database rather than by a lock in
// Go.
package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

// space is a subject of this run's own, so two tiers sharing a database do
// not share a space.
func space(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("https://issuer.example|%s-%d", t.Name(), time.Now().UnixNano())
}

// workspace creates one workspace of a space and answers it.
func workspace(t *testing.T, db *DB, owner, slug string) Workspace {
	t.Helper()
	w, err := NewWorkspaces().Create(t.Context(), db.Querier(), Workspace{
		Owner: owner, Slug: slug, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("create %q: %v", slug, err)
	}
	return w
}

// TestStoreTwoConcurrentWritersLeaveOneLease is criterion 1 of spec 009 and
// invariant 8 of spec 001: a real-database race, not a mock.
//
// Every writer runs the attach exactly as the handler does, the conditional
// update and the attachment insert in one transaction. The condition is in
// the statement, so the losers match no row; nothing in Go serialises them.
func TestStoreTwoConcurrentWritersLeaveOneLease(t *testing.T) {
	db := tier(t)
	owner := space(t)
	ws := workspace(t, db, owner, "build")

	const writers = 8
	now := time.Now()
	until := now.Add(time.Hour)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	took := make([]bool, writers)
	failed := make([]error, writers)
	for i := range writers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			holder := fmt.Sprintf("sbx_%d", i)
			failed[i] = db.Tx(t.Context(), func(q Querier) error {
				taken, err := NewWorkspaces().TakeLease(t.Context(), q, ws.ID, holder, now, until)
				if err != nil {
					return err
				}
				if !taken {
					return nil
				}
				_, err = NewAttachments().Insert(t.Context(), q, Attachment{
					WorkspaceID: ws.ID, Holder: holder, Subject: owner,
					Mode: ModeWrite, Manifest: []byte("[]"), ExpiresAt: until,
				})
				if err != nil {
					return err
				}
				took[i] = true
				return nil
			})
		}()
	}
	start.Done()
	done.Wait()

	winners := 0
	for i := range writers {
		if failed[i] != nil {
			t.Fatalf("writer %d failed: %v", i, failed[i])
		}
		if took[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d writers took the lease", winners, writers)
	}
	after, err := NewWorkspaces().Get(t.Context(), db.Querier(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.WriterHolder == nil || after.WriterExpiresAt == nil {
		t.Fatalf("the workspace holds no lease after the race: %+v", after)
	}
	// And exactly one attachment: the insert rides in the transaction the
	// conditional update lost or won, so a loser leaves no row behind.
	var attachments int
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM workspace_attachments WHERE workspace_id = $1`, ws.ID).Scan(&attachments); err != nil {
		t.Fatal(err)
	}
	if attachments != 1 {
		t.Fatalf("the race left %d attachments", attachments)
	}
}

// TestStoreManyReadersAttachAtOnceAndNoneTouchesTheLease is criterion 2: a
// read-only attach never reaches the lease, so there is no limit on how many
// exist at once.
func TestStoreManyReadersAttachAtOnceAndNoneTouchesTheLease(t *testing.T) {
	db := tier(t)
	owner := space(t)
	ws := workspace(t, db, owner, "build")

	const readers = 12
	until := time.Now().Add(time.Hour)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	failed := make([]error, readers)
	for i := range readers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, failed[i] = NewAttachments().Insert(t.Context(), db.Querier(), Attachment{
				WorkspaceID: ws.ID, Holder: fmt.Sprintf("sbx_%d", i), Subject: owner,
				Mode: ModeRead, Manifest: []byte("[]"), ExpiresAt: until,
			})
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range failed {
		if err != nil {
			t.Fatalf("reader %d failed: %v", i, err)
		}
	}
	after, err := NewWorkspaces().Get(t.Context(), db.Querier(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.WriterHolder != nil {
		t.Fatalf("a read-only attach took the lease: %+v", after.WriterHolder)
	}
	// A writer still reaches a workspace every reader is holding.
	taken, err := NewWorkspaces().TakeLease(t.Context(), db.Querier(), ws.ID, "sbx_w", time.Now(), until)
	if err != nil || !taken {
		t.Fatalf("the writer could not attach beside %d readers: %v, %v", readers, taken, err)
	}
}

// TestStoreASlugCollidesAcrossLiveAndDeletedRowsAlike is criterion 5: the
// uniqueness is not conditional on deleted_at, which is what makes restore
// guard-free.
func TestStoreASlugCollidesAcrossLiveAndDeletedRowsAlike(t *testing.T) {
	db := tier(t)
	owner := space(t)
	ws := workspace(t, db, owner, "build")

	if _, err := NewWorkspaces().Create(t.Context(), db.Querier(), Workspace{
		Owner: owner, Slug: "build", CreatedBy: owner,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a live slug taken twice = %v", err)
	}
	if ok, err := NewWorkspaces().SoftDelete(t.Context(), db.Querier(), ws.ID, time.Now()); err != nil || !ok {
		t.Fatalf("the delete = %v, %v", ok, err)
	}
	if _, err := NewWorkspaces().Create(t.Context(), db.Querier(), Workspace{
		Owner: owner, Slug: "build", CreatedBy: owner,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a tombstone's slug = %v", err)
	}
	// The tombstone therefore kept its name, and the restore takes it back
	// with nothing to collide against.
	if ok, err := NewWorkspaces().Restore(t.Context(), db.Querier(), ws.ID); err != nil || !ok {
		t.Fatalf("the restore = %v, %v", ok, err)
	}
	back, err := NewWorkspaces().Get(t.Context(), db.Querier(), ws.ID)
	if err != nil || back.DeletedAt != nil || back.Slug != "build" {
		t.Fatalf("the restored workspace is %+v, %v", back, err)
	}
	// Another space takes the same name: the uniqueness is per space.
	if _, err := NewWorkspaces().Create(t.Context(), db.Querier(), Workspace{
		Owner: space(t) + "-other", Slug: "build", CreatedBy: owner,
	}); err != nil {
		t.Fatalf("the same slug in another space = %v", err)
	}
}

// TestStoreTheWorkspaceQueriesRoundTripAgainstTheSchema drives every
// statement of the query set against the real columns, which is where a
// column that does not exist and a scan in the wrong order are found.
func TestStoreTheWorkspaceQueriesRoundTripAgainstTheSchema(t *testing.T) {
	db := tier(t)
	owner := space(t)
	ws := workspace(t, db, owner, "build")
	q := db.Querier()

	if ws.WriterHolder != nil || ws.LastSync != nil || ws.DeletedAt != nil {
		t.Fatalf("a fresh workspace is %+v", ws)
	}
	// A string that is not an id names no row rather than faulting.
	if _, err := NewWorkspaces().Get(t.Context(), q, "not-an-id"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a string that is not an id = %v", err)
	}
	if _, err := NewWorkspaces().List(t.Context(), q, owner, "not-a-cursor", 10); !errors.Is(err, ErrBadCursor) {
		t.Errorf("a cursor from nowhere = %v", err)
	}

	// The listing is keyset paginated on the id and pages without repeating
	// or skipping a row.
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		workspace(t, db, owner, slug)
	}
	seen := map[string]bool{}
	cursor := ""
	for range 10 {
		rows, err := NewWorkspaces().List(t.Context(), q, owner, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Errorf("the walk returned %s twice", row.ID)
			}
			seen[row.ID] = true
			cursor = row.ID
		}
		if len(rows) < 2 {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatalf("the walk saw %d of 4 workspaces", len(seen))
	}

	// The lease is renewed and released by its holder and by nobody else.
	now := time.Now()
	if taken, err := NewWorkspaces().TakeLease(t.Context(), q, ws.ID, "sbx_a", now, now.Add(time.Hour)); err != nil || !taken {
		t.Fatalf("TakeLease = %v, %v", taken, err)
	}
	if held, err := NewWorkspaces().RenewLease(t.Context(), q, ws.ID, "sbx_b", now.Add(2*time.Hour)); err != nil || held {
		t.Errorf("another holder renewed the lease: %v, %v", held, err)
	}
	if held, err := NewWorkspaces().RenewLease(t.Context(), q, ws.ID, "sbx_a", now.Add(2*time.Hour)); err != nil || !held {
		t.Errorf("the holder could not renew: %v, %v", held, err)
	}
	// A rename and a delete are refused while it is held.
	if ok, err := NewWorkspaces().Rename(t.Context(), q, ws.ID, "release", now); err != nil || ok {
		t.Errorf("a rename while the lease is held = %v, %v", ok, err)
	}
	if ok, err := NewWorkspaces().SoftDelete(t.Context(), q, ws.ID, now); err != nil || ok {
		t.Errorf("a delete while the lease is held = %v, %v", ok, err)
	}
	if freed, err := NewWorkspaces().ReleaseLease(t.Context(), q, ws.ID, "sbx_b"); err != nil || freed {
		t.Errorf("another holder released the lease: %v, %v", freed, err)
	}
	if freed, err := NewWorkspaces().ReleaseLease(t.Context(), q, ws.ID, "sbx_a"); err != nil || !freed {
		t.Errorf("the holder could not release: %v, %v", freed, err)
	}
	// And go through once the lease is free.
	if ok, err := NewWorkspaces().Rename(t.Context(), q, ws.ID, "release", now); err != nil || !ok {
		t.Fatalf("the rename = %v, %v", ok, err)
	}
	if _, err := NewWorkspaces().Create(t.Context(), q, Workspace{Owner: owner, Slug: "build", CreatedBy: owner}); err != nil {
		t.Fatalf("the freed slug = %v", err)
	}
	if _, err := NewWorkspaces().Rename(t.Context(), q, ws.ID, "alpha", now); !errors.Is(err, ErrConflict) {
		t.Errorf("a rename onto a taken slug = %v", err)
	}
	at, err := NewWorkspaces().StampSync(t.Context(), q, ws.ID)
	if err != nil || at.IsZero() {
		t.Fatalf("StampSync = %v, %v", at, err)
	}
	stamped, err := NewWorkspaces().Get(t.Context(), q, ws.ID)
	if err != nil || stamped.LastSync == nil {
		t.Fatalf("the boundary was not recorded: %+v, %v", stamped, err)
	}
}

// TestStoreAnAttachmentRoundTripsWithItsManifest drives the attachment set
// against the real JSONB column and the real foreign key.
func TestStoreAnAttachmentRoundTripsWithItsManifest(t *testing.T) {
	db := tier(t)
	owner := space(t)
	ws := workspace(t, db, owner, "build")
	q := db.Querier()
	manifest := []byte(`[{"path":"src/main.go","checksum":"4f9a","size":2814}]`)

	a, err := NewAttachments().Insert(t.Context(), q, Attachment{
		WorkspaceID: ws.ID, Holder: "sbx_a", Subject: owner,
		Mode: ModeWrite, Manifest: manifest, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if a.Status != StatusActive || a.ReleasedAt != nil || !strings.Contains(string(a.Manifest), "src/main.go") {
		t.Fatalf("the attachment is %+v", a)
	}
	read, err := NewAttachments().Get(t.Context(), q, ws.ID, a.ID)
	if err != nil || read.ID != a.ID {
		t.Fatalf("Get = %+v, %v", read, err)
	}
	// An attachment is read through its workspace, so an id from another
	// workspace names no row.
	elsewhere := workspace(t, db, owner, "release")
	if _, err := NewAttachments().Get(t.Context(), q, elsewhere.ID, a.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("an attachment read across workspaces = %v", err)
	}
	post := []byte(`[{"path":"bin/app","checksum":"11c0","size":5120000}]`)
	if ok, err := NewAttachments().SetManifest(t.Context(), q, a.ID, post); err != nil || !ok {
		t.Fatalf("SetManifest = %v, %v", ok, err)
	}
	until := time.Now().Add(2 * time.Hour)
	if ok, err := NewAttachments().SetExpiry(t.Context(), q, a.ID, until); err != nil || !ok {
		t.Fatalf("SetExpiry = %v, %v", ok, err)
	}
	moved, err := NewAttachments().Get(t.Context(), q, ws.ID, a.ID)
	if err != nil || !strings.Contains(string(moved.Manifest), "bin/app") || !moved.ExpiresAt.After(a.ExpiresAt) {
		t.Fatalf("the re-pinned attachment is %+v, %v", moved, err)
	}
	if ok, err := NewAttachments().Release(t.Context(), q, a.ID); err != nil || !ok {
		t.Fatalf("Release = %v, %v", ok, err)
	}
	// An attachment ends once. A second release, a re-pin and a renew all
	// find nothing to change.
	for name, run := range map[string]func() (bool, error){
		"release":    func() (bool, error) { return NewAttachments().Release(t.Context(), q, a.ID) },
		"reap":       func() (bool, error) { return NewAttachments().Reap(t.Context(), q, a.ID) },
		"set expiry": func() (bool, error) { return NewAttachments().SetExpiry(t.Context(), q, a.ID, until) },
		"re-pin":     func() (bool, error) { return NewAttachments().SetManifest(t.Context(), q, a.ID, post) },
	} {
		if ok, err := run(); err != nil || ok {
			t.Errorf("a %s of an ended attachment = %v, %v", name, ok, err)
		}
	}
	ended, err := NewAttachments().Get(t.Context(), q, ws.ID, a.ID)
	if err != nil || ended.Status != StatusReleased || ended.ReleasedAt == nil {
		t.Fatalf("the ended attachment is %+v, %v", ended, err)
	}
	// The foreign key carries the delete: purging a workspace takes its
	// attachments with it, which is what the reaper of spec 010 relies on.
	if _, err := q.Exec(t.Context(), `DELETE FROM workspaces WHERE id = $1`, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttachments().Get(t.Context(), q, ws.ID, a.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("an attachment outlived its workspace: %v", err)
	}
}

// TestStoreTheReaperReadsWhatOutlivedItsDeadline drives the two queries the
// sweep of spec 010 calls, against rows whose deadlines have passed.
func TestStoreTheReaperReadsWhatOutlivedItsDeadline(t *testing.T) {
	db := tier(t)
	owner := space(t)
	ws := workspace(t, db, owner, "build")
	q := db.Querier()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	stale, err := NewAttachments().Insert(t.Context(), q, Attachment{
		WorkspaceID: ws.ID, Holder: "sbx_a", Subject: owner,
		Mode: ModeWrite, Manifest: []byte("[]"), ExpiresAt: past,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttachments().Insert(t.Context(), q, Attachment{
		WorkspaceID: ws.ID, Holder: "sbx_b", Subject: owner,
		Mode: ModeRead, Manifest: []byte("[]"), ExpiresAt: future,
	}); err != nil {
		t.Fatal(err)
	}
	expired, err := NewAttachments().Expired(t.Context(), q, time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != stale.ID {
		t.Fatalf("the sweep read %+v", expired)
	}
	if ok, err := NewAttachments().Reap(t.Context(), q, stale.ID); err != nil || !ok {
		t.Fatalf("Reap = %v, %v", ok, err)
	}
	if again, err := NewAttachments().Expired(t.Context(), q, time.Now(), 100); err != nil || len(again) != 0 {
		t.Fatalf("a reaped attachment was swept twice: %+v, %v", again, err)
	}

	// A lease can outlive the attachment that took it, so the sweep reads
	// the workspace row beside the attachments.
	if taken, err := NewWorkspaces().TakeLease(t.Context(), q, ws.ID, "sbx_orphan", past.Add(-time.Hour), past); err != nil || !taken {
		t.Fatalf("TakeLease = %v, %v", taken, err)
	}
	leases, err := NewWorkspaces().ExpiredLeases(t.Context(), q, time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].ID != ws.ID {
		t.Fatalf("the lease sweep read %+v", leases)
	}
	if freed, err := NewWorkspaces().ReleaseLease(t.Context(), q, ws.ID, "sbx_orphan"); err != nil || !freed {
		t.Fatalf("ReleaseLease = %v, %v", freed, err)
	}
	if again, err := NewWorkspaces().ExpiredLeases(t.Context(), q, time.Now(), 100); err != nil || len(again) != 0 {
		t.Fatalf("a freed lease was swept twice: %+v, %v", again, err)
	}
	// A lapsed lease does not hold the workspace: the next writer takes it
	// without waiting for the sweep.
	if taken, err := NewWorkspaces().TakeLease(t.Context(), q, ws.ID, "sbx_a", past.Add(-time.Hour), past); err != nil || !taken {
		t.Fatal(err)
	}
	if taken, err := NewWorkspaces().TakeLease(t.Context(), q, ws.ID, "sbx_next", time.Now(), future); err != nil || !taken {
		t.Fatalf("a lapsed lease blocked the next writer: %v, %v", taken, err)
	}
}

// TestStoreASubtreeMovesItsRowsAndItsBookmarksAndReachesNoBucket is
// criterion 6 at the SQL layer: a rename is one statement per table and
// names no key.
func TestStoreASubtreeMovesItsRowsAndItsBookmarks(t *testing.T) {
	db := tier(t)
	owner := space(t)
	q := db.Querier()
	objects := NewWorkspaceObjects()

	held := seedFile(t, db, owner, "workspaces/build/src/main.go", 2814)
	seedFile(t, db, owner, "workspaces/build/bin/app", 5120000)
	seedFile(t, db, owner, "files/elsewhere.md", 10)
	if _, err := q.Exec(t.Context(),
		`INSERT INTO stars (subject, owner, path) VALUES ($1, $1, $2)`,
		owner, "workspaces/build/src/main.go"); err != nil {
		t.Fatal(err)
	}
	// A trashed row under the root follows the rename: it is still an object
	// of the workspace and its path has to keep naming it.
	if _, err := q.Exec(t.Context(),
		`UPDATE files SET deleted_at = now() WHERE owner = $1 AND path = $2`,
		owner, "workspaces/build/bin/app"); err != nil {
		t.Fatal(err)
	}

	files, bytes, err := objects.Stat(t.Context(), q, owner, "workspaces/build/")
	if err != nil || files != 1 || bytes != 2814 {
		t.Fatalf("Stat counted %d files and %d bytes: %v", files, bytes, err)
	}
	moved, err := objects.MoveSubtree(t.Context(), q, owner, "workspaces/build/", "workspaces/release/")
	if err != nil || moved != 2 {
		t.Fatalf("MoveSubtree moved %d rows: %v", moved, err)
	}
	after, err := objects.Manifest(t.Context(), q, owner, "workspaces/release/")
	if err != nil || len(after) != 1 || after[0].ObjectID != held {
		t.Fatalf("the moved subtree holds %+v, %v", after, err)
	}
	var starred string
	if err := q.QueryRow(t.Context(),
		`SELECT path FROM stars WHERE subject = $1`, owner).Scan(&starred); err != nil {
		t.Fatal(err)
	}
	if starred != "workspaces/release/src/main.go" {
		t.Errorf("the bookmark stayed at %q", starred)
	}
	// Nothing outside the root moved.
	outside, err := objects.Manifest(t.Context(), q, owner, "files/")
	if err != nil || len(outside) != 1 {
		t.Fatalf("the other plane holds %+v, %v", outside, err)
	}
	// A prefix holding a percent matches itself and not everything.
	seedFile(t, db, owner, "workspaces/100%/a", 1)
	odd, err := objects.Manifest(t.Context(), q, owner, "workspaces/100%/")
	if err != nil || len(odd) != 1 {
		t.Fatalf("a prefix with a percent matched %+v, %v", odd, err)
	}
}

// TestStoreASyncDropsTheRowsAndAnswersWhatMayGo drives the reconciliation
// half: one statement over a set of paths, and one batched reference check
// deciding which bytes may follow the rows.
func TestStoreASyncDropsTheRowsAndAnswersWhatMayGo(t *testing.T) {
	db := tier(t)
	owner := space(t)
	q := db.Querier()
	objects := NewWorkspaceObjects()

	dropped := seedFile(t, db, owner, "workspaces/build/bin/app", 8)
	kept := seedFile(t, db, owner, "workspaces/build/src/main.go", 12)
	// A second row names one of the objects, which is what a copy leaves.
	shared := seedFile(t, db, owner, "workspaces/build/bin/copy", 8)
	if _, err := q.Exec(t.Context(),
		`UPDATE files SET object_id = $3 WHERE owner = $1 AND path = $2`,
		owner, "workspaces/build/bin/copy", dropped); err != nil {
		t.Fatal(err)
	}

	freed, bytes, err := objects.Drop(t.Context(), q, owner, []string{"workspaces/build/bin/app"})
	if err != nil || len(freed) != 1 || bytes != 8 {
		t.Fatalf("Drop = %v, %d, %v", freed, bytes, err)
	}
	// The object is still named by the copy, so its bytes stay.
	unreferenced, err := objects.Unreferenced(t.Context(), q, freed)
	if err != nil {
		t.Fatalf("Unreferenced: %v", err)
	}
	if len(unreferenced) != 0 {
		t.Fatalf("bytes another row still names were offered for deletion: %v", unreferenced)
	}
	// Once the copy goes too, they may.
	if _, _, err := objects.Drop(t.Context(), q, owner, []string{"workspaces/build/bin/copy"}); err != nil {
		t.Fatal(err)
	}
	unreferenced, err = objects.Unreferenced(t.Context(), q, freed)
	if err != nil || len(unreferenced) != 1 || unreferenced[0] != dropped {
		t.Fatalf("Unreferenced = %v, %v", unreferenced, err)
	}
	// A version of a path still names its bytes, which is the second table
	// of the union.
	if _, err := q.Exec(t.Context(), `
		INSERT INTO file_versions (owner, path, version_no, object_id, size_bytes, checksum, created_by)
		VALUES ($1, $2, 1, $3, 12, 'old', $1)`, owner, "workspaces/build/src/main.go", kept); err != nil {
		t.Fatal(err)
	}
	if _, _, err := objects.Drop(t.Context(), q, owner, []string{"workspaces/build/src/main.go"}); err != nil {
		t.Fatal(err)
	}
	still, err := objects.Unreferenced(t.Context(), q, []object.ID{kept})
	if err != nil || len(still) != 0 {
		t.Fatalf("bytes a version still names were offered for deletion: %v, %v", still, err)
	}
	// Dropping nothing asks nothing.
	if ids, n, err := objects.Drop(t.Context(), q, owner, nil); err != nil || ids != nil || n != 0 {
		t.Fatalf("dropping nothing = %v, %d, %v", ids, n, err)
	}
	_ = shared
}

// seedFile writes one row of the file plane, which is what a put of spec 005
// leaves behind. The tiers of spec 009 stand in for that spec until it
// lands.
func seedFile(t *testing.T, db *DB, owner, path string, size int64) object.ID {
	t.Helper()
	id := object.NewID()
	created, err := NewFiles().Insert(t.Context(), db.Querier(), File{
		Owner: owner, Path: path, ObjectID: id, CreatedBy: owner,
		SizeBytes: size, Checksum: fmt.Sprintf("%064d", size),
		ChecksumKind: object.ChecksumSHA256,
	})
	if err != nil || !created {
		t.Fatalf("seed %q: %v, %v", path, created, err)
	}
	return id
}
