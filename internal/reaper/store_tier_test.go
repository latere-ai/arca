// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 014 for the reconciler: internal/reaper against a
// real MinIO and a real Postgres. What it proves is what a fake cannot: that
// a put which failed after the bucket write leaves bytes the sweep finds,
// that a delete which failed after the row is gone leaves the same, and that
// a ledger row altered by hand is corrected against the rows that hold the
// bytes.
//
// It runs when E2E_DATABASE_URL and E2E_S3_ENDPOINT are set and skips
// otherwise, so a plain go test on a clean clone stays green with no
// services.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

const tierSpace = "https://issuer.example|tier"

// tier is one installation against the stack: a schema of its own, a bucket
// prefix of its own, and a clock the case moves. Both are removed when the
// test ends.
type tierRun struct {
	*Reconciler
	db     *store.DB
	bucket blob.Store
	prefix string
	now    time.Time
}

func tier(t *testing.T) *tierRun {
	t.Helper()
	url, endpoint := os.Getenv("E2E_DATABASE_URL"), os.Getenv("E2E_S3_ENDPOINT")
	if url == "" || endpoint == "" {
		t.Skip("set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)")
	}
	run := &tierRun{now: time.Now(), prefix: fmt.Sprintf("tier-reaper-%d-%d/", time.Now().UnixNano(), os.Getpid())}
	run.db = tierDatabase(t, url)
	run.bucket = tierBucket(t, endpoint, run.prefix)

	r, err := New(Options{
		DB: run.db, Bucket: run.bucket, Prefix: run.prefix, TrashRetention: 720 * time.Hour,
		Now: func() time.Time { return run.now },
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	run.Reconciler = r
	return run
}

// tierDatabase opens a schema of its own and applies the migrations.
func tierDatabase(t *testing.T, url string) *store.DB {
	t.Helper()
	schema := fmt.Sprintf("tier_reaper_%d_%d", time.Now().UnixNano(), os.Getpid())
	admin, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Querier().Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		dropper, err := store.Open(ctx, url)
		if err != nil {
			t.Errorf("drop the schema: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Querier().Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop the schema: %v", err)
		}
	})
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	scoped := url + separator + "search_path=" + schema
	if err := store.Migrate(scoped); err != nil {
		t.Fatalf("apply the migrations: %v", err)
	}
	db, err := store.Open(t.Context(), scoped)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// tierBucket opens the bucket under a prefix of its own and sweeps it
// afterwards, so the store holds nothing of the run when it ends.
func tierBucket(t *testing.T, endpoint, prefix string) blob.Store {
	t.Helper()
	bucket, err := blob.NewS3(t.Context(), blob.Options{
		Bucket:    envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint:  endpoint,
		Region:    "us-east-1",
		AccessKey: envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey: envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		page, err := bucket.List(ctx, prefix, "", 1000)
		if err != nil {
			t.Errorf("sweep the prefix: %v", err)
			return
		}
		if len(page.Keys) == 0 {
			return
		}
		if err := bucket.DeleteMany(ctx, page.Keys); err != nil {
			t.Errorf("sweep the prefix: %v", err)
		}
	})
	return bucket
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// put writes bytes under an object's key, which is what invariant 1 says a
// put does before the database hears about it.
func (r *tierRun) put(t *testing.T, id object.ID) string {
	t.Helper()
	key := id.Key(r.prefix)
	if _, err := r.bucket.Put(t.Context(), key, strings.NewReader("the bytes"), 9, blob.PutOptions{}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return key
}

// held reports whether the bucket still holds the key. A key that is gone is
// blob.ErrNotFound and nothing else: a throttle or an outage must never read
// as an object that is not there (spec 001, invariant 2).
func (r *tierRun) held(t *testing.T, key string) bool {
	t.Helper()
	_, err := r.bucket.Head(t.Context(), key)
	switch {
	case err == nil:
		return true
	case errors.Is(err, blob.ErrNotFound):
		return false
	default:
		t.Fatalf("head %s: %v", key, err)
		return false
	}
}

// row writes one live file row for the space.
func (r *tierRun) row(t *testing.T, path string, id object.ID, size int64) {
	t.Helper()
	if _, err := r.db.Querier().Exec(t.Context(), `
		INSERT INTO files (owner, path, object_id, created_by, size_bytes, checksum)
		VALUES ($1, $2, $3, $1, $4, $5)`,
		tierSpace, path, id, size, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

// sweep runs one sequence and fails the test when a pass did.
func (r *tierRun) sweep(t *testing.T) Findings {
	t.Helper()
	f, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("the run reported %v", err)
	}
	return f
}

// Criterion 11 of spec 010, and criterion 5 of spec 001: a put that failed
// after the bucket write leaves bytes with no row, and pass 1 reaps them
// after the grace window and not before.
func TestStoreAPutThatFailedAfterTheBucketWriteIsReapedAfterTheWindow(t *testing.T) {
	r := tier(t)
	key := r.put(t, object.NewID())

	if f := r.sweep(t); f.Count(KindOrphanCandidate, Found) != 1 {
		t.Fatalf("the first sweep found %v", f.Rows())
	}
	if !r.held(t, key) {
		t.Fatal("the bytes went inside the grace window")
	}

	r.now = r.now.Add(OrphanGrace + time.Minute)
	if f := r.sweep(t); f.Count(KindOrphanObject, Repaired) != 1 {
		t.Fatalf("the sweep past the window found %v", f.Rows())
	}
	if r.held(t, key) {
		t.Fatal("the orphan survived the window")
	}
	// And a second run over what the first left changes nothing.
	if f := r.sweep(t); f.Total(KindOrphanObject) != 0 {
		t.Fatalf("the settled sweep found %v", f.Rows())
	}
}

// Criterion 12 of spec 010, and criterion 6 of spec 001: a delete that
// failed after the row was removed leaves no visible object, and pass 1
// reaps the bytes.
func TestStoreADeleteThatFailedAfterTheRowIsReaped(t *testing.T) {
	r := tier(t)
	id := object.NewID()
	key := r.put(t, id)
	r.row(t, "files/q3.pdf", id, 9)

	// While the row is there the bytes are not an orphan, however long the
	// sweep waits.
	r.now = r.now.Add(2 * OrphanGrace)
	if f := r.sweep(t); f.Total(KindOrphanObject) != 0 || f.Total(KindOrphanCandidate) != 0 {
		t.Fatalf("a key a row names was reported as %v", f.Rows())
	}

	// The delete removed the row and then failed, which is the half state
	// invariant 1's order leaves.
	if _, err := r.db.Querier().Exec(t.Context(), `DELETE FROM files WHERE owner = $1`, tierSpace); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	r.now = r.now.Add(2 * OrphanGrace)
	if f := r.sweep(t); f.Count(KindOrphanObject, Repaired) != 1 {
		t.Fatalf("the sweep found %v", f.Rows())
	}
	if r.held(t, key) {
		t.Fatal("the bytes of a row that is gone survived")
	}
}

// Criterion 14 of spec 010 against the real stores: a row whose key the
// bucket does not hold is reported and never deleted.
func TestStoreARowWithoutItsBytesIsReportedAndKept(t *testing.T) {
	r := tier(t)
	r.row(t, "files/gone.pdf", object.NewID(), 9)

	if f := r.sweep(t); f.Count(KindMissingBytes, Found) != 1 {
		t.Fatalf("the sweep found %v", f.Rows())
	}
	var rows int
	if err := r.db.Querier().QueryRow(t.Context(),
		`SELECT count(*) FROM files WHERE owner = $1`, tierSpace).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("the pass deleted the only record that something existed")
	}
}

// Criterion 17 of spec 010 against Postgres: a ledger row altered by hand is
// corrected by pass 10 and reported as a finding, and a healthy run corrects
// nothing.
func TestStoreLedgerReconciles(t *testing.T) {
	r := tier(t)
	ledger := events.NewLedger()
	id := object.NewID()
	r.put(t, id)
	r.row(t, "files/q3.pdf", id, 100)
	if _, err := ledger.Charge(t.Context(), r.db.Querier(), tierSpace, 100, events.Unlimited()); err != nil {
		t.Fatal(err)
	}

	// A healthy installation corrects nothing.
	if f := r.sweep(t); f.Total(KindUsageCorrected) != 0 {
		t.Fatalf("a healthy run corrected %v", f.Rows())
	}

	// The row is altered by hand, which is the shape of a write path that
	// forgot its delta.
	if _, err := r.db.Querier().Exec(t.Context(),
		`UPDATE space_usage SET bytes = 4096 WHERE owner = $1`, tierSpace); err != nil {
		t.Fatal(err)
	}
	f := r.sweep(t)
	if f.Count(KindUsageCorrected, Found) != 1 || f.Count(KindUsageCorrected, Repaired) != 1 {
		t.Fatalf("the sweep reported %v", f.Rows())
	}
	held, err := ledger.Read(t.Context(), r.db.Querier(), tierSpace)
	if err != nil {
		t.Fatal(err)
	}
	if held != 100 {
		t.Fatalf("the ledger holds %d after the correction, and the rows hold 100", held)
	}
	// The correction is a finding, so the space hears about the run.
	page, err := events.NewLog().Tail(t.Context(), r.db.Querier(), events.Query{Owner: tierSpace, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var reaped bool
	for _, e := range page.Entries {
		reaped = reaped || e.Action == events.ActionReap
	}
	if !reaped {
		t.Fatalf("the log holds %+v, and a run that changed a space says so", page.Entries)
	}
}

// Criterion 16 of spec 010 against both stores: trash past the retention
// leaves the database and the bucket, lowers the ledger, and appears as a
// purge event.
func TestStoreTrashPastItsRetentionLeavesBothStores(t *testing.T) {
	r := tier(t)
	ledger := events.NewLedger()
	id := object.NewID()
	key := r.put(t, id)
	r.row(t, "files/old.pdf", id, 9)
	if _, err := r.db.Querier().Exec(t.Context(),
		`UPDATE files SET deleted_at = now() - interval '800 hours' WHERE owner = $1`, tierSpace); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Charge(t.Context(), r.db.Querier(), tierSpace, 9, events.Unlimited()); err != nil {
		t.Fatal(err)
	}

	f := r.sweep(t)
	if f.Count(KindTrashPurged, Repaired) != 1 {
		t.Fatalf("the sweep reported %v", f.Rows())
	}
	if r.held(t, key) {
		t.Fatal("the bytes of a purged row survived")
	}
	held, err := ledger.Read(t.Context(), r.db.Querier(), tierSpace)
	if err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("the ledger holds %d after the purge", held)
	}
	// The ledger moved with the rows, so pass 10 had nothing to correct.
	if f.Total(KindUsageCorrected) != 0 {
		t.Fatalf("a healthy purge left the ledger to correct: %v", f.Rows())
	}
	page, err := events.NewLog().Tail(t.Context(), r.db.Querier(), events.Query{Owner: tierSpace, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var purged bool
	for _, e := range page.Entries {
		purged = purged || e.Action == events.ActionPurge
	}
	if !purged {
		t.Fatalf("the log holds %+v, and a purge is the last thing said about an object", page.Entries)
	}
}
