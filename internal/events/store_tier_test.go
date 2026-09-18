// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 014 for the ledger and the log: internal/events
// against a real Postgres. What it proves is what a fake cannot: that the
// migration applies, that a charge and its comparison read one row in one
// transaction, that a refused charge leaves neither a row nor bytes on the
// counter, and that a keyset tail is gapless across a concurrent burst.
//
// It runs when E2E_DATABASE_URL is set and skips otherwise, so a plain go
// test on a clean clone stays green with no services.
package events

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// tier opens a database with a schema of its own, applies the migrations,
// and drops the schema when the test ends.
func tier(t *testing.T) *store.DB {
	t.Helper()
	url := os.Getenv("E2E_DATABASE_URL")
	if url == "" || os.Getenv("E2E_S3_ENDPOINT") == "" {
		t.Skip("set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)")
	}
	schema := fmt.Sprintf("tier_events_%d_%d", time.Now().UnixNano(), os.Getpid())

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

// TestStoreTheLedgerAndTheLogApplyOverTheNumbersTheirSpecsHaveNotFilled
// holds the numbering of spec 004 to what the migrator does with it. This
// spec's file is 0005 and 0002 through 0004 belong to specs 007, 008 and
// 009, so an empty database applies 0001 and then 0005 across the gap. The
// gap is also why a database migrated here before those three exist will
// never receive them: the migrator records one version, and this spec's
// Current state names the remediation.
func TestStoreTheLedgerAndTheLogApplyOverTheNumbersTheirSpecsHaveNotFilled(t *testing.T) {
	db := tier(t)
	for _, table := range []string{"space_usage", "events"} {
		var exists bool
		if err := db.Querier().QueryRow(t.Context(),
			`SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("the migrations left no %s table", table)
		}
	}
	var indexes int
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'events'`).
		Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	// The primary key, the tail's index, and the retention pass's.
	if indexes != 3 {
		t.Fatalf("the events table carries %d indexes", indexes)
	}
}

// Criteria 3 and 4 of spec 010 against Postgres: a write that lands exactly
// on the answer's limit is admitted, the next byte over is refused with the
// used and limit figures, and a delete on a space over the limit is
// admitted.
func TestStoreUsageAdmitsTheLimitAndRefusesTheByteAfterIt(t *testing.T) {
	db, ledger := tier(t), NewLedger()
	q := db.Querier()

	total, err := ledger.Charge(t.Context(), q, aSpace, 100, LimitBytes(100))
	if err != nil || total != 100 {
		t.Fatalf("a write onto the limit answered %d, %v", total, err)
	}
	_, err = ledger.Charge(t.Context(), q, aSpace, 1, LimitBytes(100))
	over, refused := AsOverLimit(err)
	if !refused {
		t.Fatalf("the byte after the limit answered %v", err)
	}
	if over.Used != 100 || over.Limit != 100 || over.Delta != 1 {
		t.Fatalf("the refusal reads %+v", over)
	}
	// A delete is never refused, and it works on a space the answer's limit
	// says is already too full.
	if _, err := ledger.Release(t.Context(), q, aSpace, 50); err != nil {
		t.Fatalf("a delete over the limit answered %v", err)
	}
	if held, err := ledger.Read(t.Context(), q, aSpace); err != nil || held != 51 {
		t.Fatalf("the ledger holds %d, %v", held, err)
	}
}

// Criterion 6 of spec 010 against Postgres: a refused charge leaves neither
// a row nor a charge, because the charge is applied in the write's own
// transaction and the refusal takes the write down with it.
func TestStoreUsageFailsClosed(t *testing.T) {
	db, ledger := tier(t), NewLedger()
	if _, err := ledger.Charge(t.Context(), db.Querier(), aSpace, 90, Unlimited()); err != nil {
		t.Fatal(err)
	}

	// The write path: the row and the charge in one transaction, refused by
	// the limit the answer carried.
	write := db.Tx(t.Context(), func(tx store.Querier) error {
		if err := insertFile(t, tx, aSpace, "files/too-big.pdf", 20); err != nil {
			return err
		}
		_, err := ledger.Charge(t.Context(), tx, aSpace, 20, LimitBytes(100))
		return err
	})
	if _, refused := AsOverLimit(write); !refused {
		t.Fatalf("the write answered %v", write)
	}

	held, err := ledger.Read(t.Context(), db.Querier(), aSpace)
	if err != nil {
		t.Fatal(err)
	}
	if held != 90 {
		t.Fatalf("the ledger holds %d after a refused write, and the charge should have rolled back with it", held)
	}
	var rows int
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT count(*) FROM files WHERE owner = $1`, aSpace).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a refused write left %d rows", rows)
	}
}

func TestStoreTheLedgerIsRecomputedFromTheRowsThatHoldTheBytes(t *testing.T) {
	db, ledger := tier(t), NewLedger()
	q := db.Querier()
	if err := insertFile(t, q, aSpace, "files/q3.pdf", 100); err != nil {
		t.Fatal(err)
	}
	if err := insertVersion(t, q, aSpace, "files/q3.pdf", 80); err != nil {
		t.Fatal(err)
	}
	// A space whose rows were written without a charge, which is the drift
	// pass 10 of spec 010 corrects.
	if _, err := ledger.Charge(t.Context(), q, aSpace, 0, Unlimited()); err != nil {
		t.Fatal(err)
	}

	held, err := ledger.Recompute(t.Context(), q, aSpace)
	if err != nil {
		t.Fatal(err)
	}
	if held != 180 {
		t.Fatalf("the recomputation reads %d, and the rows hold 180", held)
	}
	corrected, err := ledger.Correct(t.Context(), q, aSpace, 0, held)
	if err != nil || !corrected {
		t.Fatalf("the correction answered %v, %v", corrected, err)
	}
	// A correction racing a live charge loses: the row no longer holds what
	// the caller read.
	again, err := ledger.Correct(t.Context(), q, aSpace, 0, held)
	if err != nil || again {
		t.Fatalf("a correction on a row that moved answered %v, %v", again, err)
	}
}

func TestStoreTheLedgerNeverGoesBelowNothing(t *testing.T) {
	db, ledger := tier(t), NewLedger()
	// The column refuses a negative and a delete must never be refused, so
	// a release against a ledger that had drifted low clamps and leaves the
	// disagreement for pass 10 to report.
	total, err := ledger.Release(t.Context(), db.Querier(), aSpace, 4096)
	if err != nil || total != 0 {
		t.Fatalf("a release against an empty ledger answered %d, %v", total, err)
	}
}

// Criterion 9 of spec 010 against Postgres: the tail is gapless for a given
// cursor across a concurrent write burst. The walk runs after the burst, so
// every row the burst committed is visible to it and each is read once.
func TestStoreTheTailIsGaplessAcrossABurst(t *testing.T) {
	db, log := tier(t), NewLog()
	const writers, each = 8, 25

	var wg sync.WaitGroup
	appended := make(chan int64, writers*each)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range each {
				id, err := log.Append(t.Context(), db.Querier(), Event{
					Owner: aSpace, Action: ActionPut,
					Path:   fmt.Sprintf("files/%d-%d.txt", w, n),
					Detail: map[string]any{"size": n},
				})
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				appended <- id
			}
		}()
	}
	wg.Wait()
	close(appended)
	want := map[int64]bool{}
	for id := range appended {
		want[id] = true
	}

	read, cursor, pages := map[int64]bool{}, int64(0), 0
	for {
		page, err := log.Tail(t.Context(), db.Querier(), Query{Owner: aSpace, Cursor: cursor, Limit: 7})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			if e.ID <= cursor {
				t.Fatalf("the page holds %d after the cursor %d", e.ID, cursor)
			}
			if read[e.ID] {
				t.Fatalf("the walk read %d twice", e.ID)
			}
			read[e.ID] = true
		}
		pages++
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if len(read) != len(want) {
		t.Fatalf("the walk read %d of the %d rows the burst wrote", len(read), len(want))
	}
	for id := range want {
		if !read[id] {
			t.Fatalf("the walk never read %d", id)
		}
	}
	if pages < 2 {
		t.Fatalf("the walk took %d pages, and the burst is larger than one", pages)
	}
}

func TestStoreTheLogIsPrunedByAge(t *testing.T) {
	db, log := tier(t), NewLog()
	q := db.Querier()
	old, err := log.Append(t.Context(), q, Event{Owner: aSpace, Action: ActionPut})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(t.Context(),
		`UPDATE events SET created_at = now() - interval '40 days' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(t.Context(), q, Event{Owner: aSpace, Action: ActionReap}); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-30 * 24 * time.Hour)
	older, err := log.Older(t.Context(), q, before)
	if err != nil || older != 1 {
		t.Fatalf("the count answered %d, %v", older, err)
	}
	gone, err := log.Prune(t.Context(), q, before)
	if err != nil || gone != 1 {
		t.Fatalf("the prune answered %d, %v", gone, err)
	}
	page, err := log.Tail(t.Context(), q, Query{Owner: aSpace, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Action != ActionReap {
		t.Fatalf("the log holds %+v", page.Entries)
	}
}

func TestStoreTheLogRefusesNothingAndReadsBackTheDetail(t *testing.T) {
	db, log := tier(t), NewLog()
	q := db.Querier()
	for _, action := range Actions() {
		if _, err := log.Append(t.Context(), q, Event{
			Owner: aSpace, Action: action, Actor: aSpace,
			Detail: map[string]any{"kind": string(action)},
		}); err != nil {
			t.Fatalf("the column refused %q: %v", action, err)
		}
	}
	page, err := log.Tail(t.Context(), q, Query{Owner: aSpace, Limit: len(Actions())})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != len(Actions()) {
		t.Fatalf("the log holds %d of the %d actions", len(page.Entries), len(Actions()))
	}
	for i, e := range page.Entries {
		if e.Action != Actions()[i] || e.Detail["kind"] != string(Actions()[i]) {
			t.Fatalf("the row reads %+v", e)
		}
	}
	// A space the caller did not name is not in the page.
	empty, err := log.Tail(t.Context(), q, Query{Owner: "https://issuer.example|somebody-else", Limit: 10})
	if err != nil || len(empty.Entries) != 0 {
		t.Fatalf("another space's tail read %d rows, %v", len(empty.Entries), err)
	}
}

func TestStoreTheLedgerWalksEverySpaceOnce(t *testing.T) {
	db, ledger := tier(t), NewLedger()
	q := db.Querier()
	owners := []string{"https://issuer.example|a", "https://issuer.example|b", "https://issuer.example|c"}
	for i, owner := range owners {
		if _, err := ledger.Charge(t.Context(), q, owner, int64(i+1), Unlimited()); err != nil {
			t.Fatal(err)
		}
	}
	seen, cursor := map[string]int64{}, ""
	for {
		page, err := ledger.Spaces(t.Context(), q, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, space := range page {
			if _, twice := seen[space.Owner]; twice {
				t.Fatalf("the walk read %q twice", space.Owner)
			}
			seen[space.Owner] = space.Bytes
		}
		cursor = page[len(page)-1].Owner
	}
	if len(seen) != len(owners) || seen[owners[2]] != 3 {
		t.Fatalf("the walk read %v", seen)
	}
}

// insertFile writes one live row, which is what the recomputation sums.
func insertFile(t *testing.T, q store.Querier, owner, path string, size int64) error {
	t.Helper()
	_, err := q.Exec(t.Context(), `
		INSERT INTO files (owner, path, object_id, created_by, size_bytes, checksum)
		VALUES ($1, $2, $3, $1, $4, $5)`,
		owner, path, object.NewID(), size, strings.Repeat("a", 64))
	if err != nil {
		return errors.New("insert the file: " + err.Error())
	}
	return nil
}

// insertVersion writes one superseded content, which counts too: a space
// keeping copies of a large file is storing them.
func insertVersion(t *testing.T, q store.Querier, owner, path string, size int64) error {
	t.Helper()
	_, err := q.Exec(t.Context(), `
		INSERT INTO file_versions (owner, path, version_no, object_id, size_bytes, checksum, created_by)
		VALUES ($1, $2, 1, $3, $4, $5, $1)`,
		owner, path, object.NewID(), size, strings.Repeat("b", 64))
	if err != nil {
		return errors.New("insert the version: " + err.Error())
	}
	return nil
}
