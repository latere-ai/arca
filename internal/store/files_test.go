// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/object"
)

// aFile is one row as the queries read and write it, with a subject holding
// every character spec 004 says a subject column accepts.
func aFile() File {
	return File{
		ID:           "9c1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e",
		Owner:        "https://issuer.example/realms/one|user:7/agent",
		Path:         "files/notes/todo.md",
		ObjectID:     object.NewID(),
		CreatedBy:    "https://issuer.example/realms/one|user:7/agent",
		ContentType:  "text/markdown",
		SizeBytes:    42,
		Checksum:     strings.Repeat("a", 64),
		ChecksumKind: object.ChecksumSHA256,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
}

func TestReadingOnePathAnswersTheRowOrNoRowAtAll(t *testing.T) {
	want := aFile()
	q := &fakeQuerier{row: fakeRow{scan: fileScan(want)}}
	got, err := NewFiles().Get(t.Context(), q, want.Owner, want.Path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Path != want.Path || got.ObjectID != want.ObjectID || got.ChecksumKind != object.ChecksumSHA256 {
		t.Fatalf("Get = %+v", got)
	}
	if len(q.args[0]) != 2 || q.args[0][0] != want.Owner {
		t.Fatalf("the read bound %v", q.args[0])
	}

	missingRow := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewFiles().Get(t.Context(), missingRow, want.Owner, want.Path); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a path that is not there = %v", err)
	}
	broken := &fakeQuerier{row: failing(errors.New("the connection failed"))}
	if _, err := NewFiles().Get(t.Context(), broken, want.Owner, want.Path); errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a connection failure answered as a missing row: %v", err)
	}
}

func TestWritingAPathThatIsTakenIsNotAnOverwrite(t *testing.T) {
	f := aFile()
	created := &fakeQuerier{row: values("9c1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e")}
	if ok, err := NewFiles().Insert(t.Context(), created, f); err != nil || !ok {
		t.Fatalf("Insert = %t, %v", ok, err)
	}
	if !strings.Contains(created.statements[0], "ON CONFLICT (owner, path) DO NOTHING") {
		t.Fatalf("the write is not guarded:\n%s", created.statements[0])
	}
	taken := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if ok, err := NewFiles().Insert(t.Context(), taken, f); err != nil || ok {
		t.Fatalf("Insert onto a path that is taken = %t, %v", ok, err)
	}
	refused := &fakeQuerier{row: failing(conflictError)}
	if _, err := NewFiles().Insert(t.Context(), refused, f); !errors.Is(err, ErrConflict) {
		t.Fatalf("a unique violation surfaced as %v", err)
	}
}

func TestAConditionalReplaceReportsTheRaceItLost(t *testing.T) {
	f := aFile()
	applied := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	ok, err := NewFiles().UpdateIfChecksum(t.Context(), applied, f, strings.Repeat("b", 64))
	if err != nil || !ok {
		t.Fatalf("UpdateIfChecksum = %t, %v", ok, err)
	}
	if !strings.Contains(applied.statements[0], "checksum = $8") {
		t.Fatalf("the condition is not in the statement:\n%s", applied.statements[0])
	}
	stale := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewFiles().UpdateIfChecksum(t.Context(), stale, f, "stale"); err != nil || ok {
		t.Fatalf("a replace against a stale checksum = %t, %v", ok, err)
	}
	broken := &fakeQuerier{execErr: errors.New("the connection failed")}
	if _, err := NewFiles().UpdateIfChecksum(t.Context(), broken, f, "stale"); err == nil {
		t.Fatal("a connection failure answered as a lost race")
	}
}

func TestAMoveIsOneUpdateAndSaysWhenTheNameIsTaken(t *testing.T) {
	moved := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	if ok, err := NewFiles().Move(t.Context(), moved, "owner", "files/a", "files/b"); err != nil || !ok {
		t.Fatalf("Move = %t, %v", ok, err)
	}
	if strings.Count(moved.statements[0], "UPDATE") != 1 {
		t.Fatalf("a move is not one statement:\n%s", moved.statements[0])
	}
	absent := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewFiles().Move(t.Context(), absent, "owner", "files/a", "files/b"); err != nil || ok {
		t.Fatalf("a move of a path that is not there = %t, %v", ok, err)
	}
	taken := &fakeQuerier{execErr: conflictError}
	if _, err := NewFiles().Move(t.Context(), taken, "owner", "files/a", "files/b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a move onto a path that is taken = %v", err)
	}
}

func TestAListingIsKeysetPaginatedAndCarriesItsCursor(t *testing.T) {
	first, second := aFile(), aFile()
	second.Path = "files/notes/zebra.md"
	rows := rowsOf(first, second)
	q := &fakeQuerier{rows: rows}
	page, cursor, err := NewFiles().ListPrefix(t.Context(), q, first.Owner, "files/notes/", "", 2)
	if err != nil {
		t.Fatalf("ListPrefix: %v", err)
	}
	if len(page) != 2 || cursor != second.Path {
		t.Fatalf("the page holds %d rows and the cursor is %q", len(page), cursor)
	}
	if !rows.closed {
		t.Error("the rows were not closed")
	}
	if !strings.Contains(q.statements[0], "path > $3") || !strings.Contains(q.statements[0], "ORDER BY path") {
		t.Fatalf("the listing is not keyset paginated:\n%s", q.statements[0])
	}

	// A short page is the last one, so it carries no cursor.
	short := &fakeQuerier{rows: rowsOf(first)}
	if _, cursor, err := NewFiles().ListPrefix(t.Context(), short, first.Owner, "files/", "", 2); err != nil || cursor != "" {
		t.Fatalf("a short page answered the cursor %q, %v", cursor, err)
	}
}

func TestAListingSurfacesEveryWayItCanFail(t *testing.T) {
	if _, _, err := NewFiles().ListPrefix(t.Context(), &fakeQuerier{}, "owner", "files/", "", 0); err == nil {
		t.Error("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, _, err := NewFiles().ListPrefix(t.Context(), broken, "owner", "files/", "", 10); err == nil {
		t.Error("a connection failure answered a page")
	}
	unreadable := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{
		func(...any) error { return errors.New("the row is not a file") },
	}}}
	if _, _, err := NewFiles().ListPrefix(t.Context(), unreadable, "owner", "files/", "", 10); err == nil {
		t.Error("a row that could not be read answered a page")
	}
	interrupted := &fakeQuerier{rows: &fakeRows{err: errors.New("the walk was interrupted")}}
	if _, _, err := NewFiles().ListPrefix(t.Context(), interrupted, "owner", "files/", "", 10); err == nil {
		t.Error("an interrupted walk answered a page")
	}
}

func TestAPrefixMatchesItselfAndNotEverything(t *testing.T) {
	for prefix, want := range map[string]string{
		"files/":          `files/%`,
		"files/50%_off":   `files/50\%\_off%`,
		`files/back\lash`: `files/back\\lash%`,
	} {
		if got := likePrefix(prefix); got != want {
			t.Errorf("likePrefix(%q) = %q, want %q", prefix, got, want)
		}
	}
}
