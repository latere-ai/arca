// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 014: internal/store against a real Postgres.
// What it proves is what a fake cannot: that the schema applies, that a
// unique constraint refuses a second write, that a transaction rolls back,
// and that a keyset walk under concurrent inserts returns each row once.
//
// It runs when E2E_DATABASE_URL is set and skips otherwise, so a plain
// go test on a clean clone stays green with no services. Every run creates
// a schema of its own and drops it, so two packages never share a migration
// state and nothing is left behind.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

// skipWithoutTheStack is the remediation a tier prints when the stack is
// not there. It names both variables and the command that starts them.
const skipWithoutTheStack = "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"

// stackDatabase answers the database's URL, or the reason to skip.
func stackDatabase() (string, string) {
	url := os.Getenv("E2E_DATABASE_URL")
	if url == "" || os.Getenv("E2E_S3_ENDPOINT") == "" {
		return "", skipWithoutTheStack
	}
	return url, ""
}

// tier opens a database with a schema of its own, applies the migrations,
// and drops the schema when the test ends.
func tier(t *testing.T) *DB {
	t.Helper()
	url, reason := stackDatabase()
	if reason != "" {
		t.Skip(reason)
	}
	schema := fmt.Sprintf("tier_%d_%d", time.Now().UnixNano(), os.Getpid())

	admin, err := Open(t.Context(), url)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Querier().Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	t.Cleanup(func() {
		dropper, err := Open(context.WithoutCancel(t.Context()), url)
		if err != nil {
			t.Errorf("drop the schema: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Querier().Exec(context.WithoutCancel(t.Context()), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop the schema: %v", err)
		}
	})

	scoped := withSearchPath(url, schema)
	if err := Migrate(scoped); err != nil {
		t.Fatalf("apply the migrations: %v", err)
	}
	db, err := Open(t.Context(), scoped)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// withSearchPath points a connection string at one schema, which is how a
// run keeps its tables to itself.
func withSearchPath(url, schema string) string {
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	return url + separator + "search_path=" + schema
}

// aRow is one file as the tier writes it.
func aRow(owner, path string) File {
	return File{
		Owner:        owner,
		Path:         path,
		ObjectID:     object.NewID(),
		CreatedBy:    owner,
		ContentType:  "text/markdown",
		SizeBytes:    11,
		Checksum:     strings.Repeat("a", 64),
		ChecksumKind: object.ChecksumSHA256,
	}
}

func TestStoreMigrationsApplyAndAreIdempotent(t *testing.T) {
	db := tier(t)
	q := db.Querier()
	for _, table := range []string{"subjects", "files", "file_versions", "stars"} {
		var exists bool
		if err := q.QueryRow(t.Context(),
			`SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("the migrations did not create %s", table)
		}
	}
	pending, err := Pending(t.Context(), q)
	if err != nil || len(pending) != 0 {
		t.Fatalf("Pending after the migrations = %v, %v", pending, err)
	}
	// A second run changes nothing, which is what a rolling deploy runs.
	if err := Migrate(withSearchPath(os.Getenv("E2E_DATABASE_URL"), schemaOf(t, db))); err != nil {
		t.Fatalf("the second migration run: %v", err)
	}
}

// schemaOf answers the schema the pool is pointed at.
func schemaOf(t *testing.T, db *DB) string {
	t.Helper()
	var schema string
	if err := db.Querier().QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestStoreATransactionLeavesOneRowOrNone(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|writer"

	boom := errors.New("the work refused")
	err := db.Tx(t.Context(), func(q Querier) error {
		if _, err := files.Insert(t.Context(), q, aRow(owner, "files/rolled-back.md")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v", err)
	}
	if _, err := files.Get(t.Context(), db.Querier(), owner, "files/rolled-back.md"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the rolled back row is still there: %v", err)
	}

	if err := db.Tx(t.Context(), func(q Querier) error {
		_, err := files.Insert(t.Context(), q, aRow(owner, "files/committed.md"))
		return err
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if _, err := files.Get(t.Context(), db.Querier(), owner, "files/committed.md"); err != nil {
		t.Fatalf("the committed row is not there: %v", err)
	}
}

func TestStoreARollbackOnACancelledContextReturnsTheConnectionUsable(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|cancelled"

	for range 8 {
		ctx, cancel := context.WithCancel(t.Context())
		err := db.Tx(ctx, func(q Querier) error {
			if _, err := files.Insert(ctx, q, aRow(owner, "files/never.md")); err != nil {
				return err
			}
			cancel()
			return errors.New("the caller hung up")
		})
		if err == nil {
			t.Fatal("Tx committed work whose caller hung up")
		}
	}
	// A connection handed back inside an open transaction poisons the next
	// caller of it, so the pool is asked for work afterwards.
	if _, err := files.Insert(t.Context(), db.Querier(), aRow(owner, "files/after.md")); err != nil {
		t.Fatalf("the pool is poisoned: %v", err)
	}
}

func TestStoreAUniqueViolationIsToldFromAFault(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|conflict"
	row := aRow(owner, "files/taken.md")

	created, err := files.Insert(t.Context(), db.Querier(), row)
	if err != nil || !created {
		t.Fatalf("Insert = %t, %v", created, err)
	}
	second := aRow(owner, "files/taken.md")
	created, err = files.Insert(t.Context(), db.Querier(), second)
	if err != nil || created {
		t.Fatalf("the second write of one path = %t, %v", created, err)
	}
	held, err := files.Get(t.Context(), db.Querier(), owner, "files/taken.md")
	if err != nil {
		t.Fatal(err)
	}
	if held.ObjectID != row.ObjectID {
		t.Fatal("the refused write replaced the object the path names")
	}
	if _, err := files.Get(t.Context(), db.Querier(), owner, "files/never-written.md"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a path that is not there = %v", err)
	}
	// A move onto a path that is taken is the same constraint, and it
	// surfaces as a conflict rather than as a fault.
	if _, err := files.Insert(t.Context(), db.Querier(), aRow(owner, "files/other.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Move(t.Context(), db.Querier(), owner, "files/other.md", "files/taken.md"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a move onto a path that is taken = %v", err)
	}
}

func TestStoreAConditionalReplaceLosesTheRaceRatherThanOverwriting(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|racers"
	row := aRow(owner, "files/contended.md")
	if _, err := files.Insert(t.Context(), db.Querier(), row); err != nil {
		t.Fatal(err)
	}

	// Two writers read the same checksum and write. One wins.
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := aRow(owner, "files/contended.md")
			next.Checksum = strings.Repeat(fmt.Sprint(i), 64)
			applied, err := files.UpdateIfChecksum(t.Context(), db.Querier(), next, row.Checksum)
			if err != nil {
				t.Errorf("UpdateIfChecksum: %v", err)
			}
			results <- applied
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for applied := range results {
		if applied {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of two writers applied their write", won)
	}
}

func TestStoreAnObjectIsReferencedByAnyTableThatNamesIt(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|refs"

	live := aRow(owner, "files/live.md")
	if _, err := files.Insert(t.Context(), db.Querier(), live); err != nil {
		t.Fatal(err)
	}
	referenced, err := ObjectReferenced(t.Context(), db.Querier(), live.ObjectID)
	if err != nil || !referenced {
		t.Fatalf("an object a file names = %t, %v", referenced, err)
	}

	// A version names bytes no live file does. Spec 005 owns the query set
	// that writes one; the tier writes the row itself.
	superseded := object.NewID()
	if _, err := db.Querier().Exec(t.Context(), `
		INSERT INTO file_versions (owner, path, version_no, object_id, size_bytes, checksum, created_by)
		VALUES ($1, $2, 1, $3, 11, $4, $1)`, owner, "files/live.md", superseded, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	referenced, err = ObjectReferenced(t.Context(), db.Querier(), superseded)
	if err != nil || !referenced {
		t.Fatalf("an object a version names = %t, %v", referenced, err)
	}

	referenced, err = ObjectReferenced(t.Context(), db.Querier(), object.NewID())
	if err != nil || referenced {
		t.Fatalf("an object nothing names = %t, %v", referenced, err)
	}
}

func TestStoreAKeysetWalkReturnsEachRowOnceUnderConcurrentInserts(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	owner := "https://issuer.example|walker"
	for i := range 20 {
		if _, err := files.Insert(t.Context(), db.Querier(), aRow(owner, fmt.Sprintf("files/walk/%03d.md", i))); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 20; page++ {
		rows, next, err := files.ListPrefix(t.Context(), db.Querier(), owner, "files/walk/", cursor, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			seen[row.Path]++
		}
		// An insert lands ahead of the walk between two pages, which is
		// what shifts an offset and leaves a keyset alone.
		if _, err := files.Insert(t.Context(), db.Querier(), aRow(owner, fmt.Sprintf("files/walk/%03d-late.md", page))); err != nil {
			t.Fatal(err)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for path, count := range seen {
		if count != 1 {
			t.Errorf("%s was walked %d times", path, count)
		}
	}
	for i := range 20 {
		if path := fmt.Sprintf("files/walk/%03d.md", i); seen[path] == 0 {
			t.Errorf("%s was never walked", path)
		}
	}
}

func TestStoreASubjectColumnHoldsWhatASubjectIs(t *testing.T) {
	db := tier(t)
	head := "https://issuer.example/realms/one|"
	subject := head + strings.Repeat("a:b/c", (512-len(head))/5+1)
	subject = subject[:512]
	if len(subject) != 512 {
		t.Fatalf("the subject under test is %d bytes", len(subject))
	}
	subjects := NewSubjects()
	if err := subjects.Touch(t.Context(), db.Querier(), subject, "person@example.test"); err != nil {
		t.Fatal(err)
	}
	// A second visit keeps the display it already had.
	if err := subjects.Touch(t.Context(), db.Querier(), subject, ""); err != nil {
		t.Fatal(err)
	}
	held, err := subjects.Get(t.Context(), db.Querier(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if held.Display != "person@example.test" || held.Subject != subject {
		t.Fatalf("the directory holds %+v", held)
	}
	if _, err := NewFiles().Insert(t.Context(), db.Querier(), aRow(subject, "files/long-subject.md")); err != nil {
		t.Fatalf("a space owned by a 512 byte subject: %v", err)
	}
}

// TestStoreTheTierSkipsWithoutTheStack is criterion 5 of spec 014: a tier
// whose variables are unset skips with the remediation in its message.
func TestStoreTheTierSkipsWithoutTheStack(t *testing.T) {
	t.Setenv("E2E_DATABASE_URL", "")
	if _, reason := stackDatabase(); reason != skipWithoutTheStack {
		t.Fatalf("the skip reason is %q", reason)
	}
	t.Setenv("E2E_DATABASE_URL", "postgres://arca@db/arca")
	t.Setenv("E2E_S3_ENDPOINT", "")
	if _, reason := stackDatabase(); reason == "" {
		t.Fatal("a tier ran with half the stack configured")
	}
}
