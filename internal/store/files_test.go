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
	page, cursor, err := NewFiles().ListPrefix(t.Context(), q, first.Owner, "files/notes/", "", 2, nil)
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
	if _, cursor, err := NewFiles().ListPrefix(t.Context(), short, first.Owner, "files/", "", 2, nil); err != nil || cursor != "" {
		t.Fatalf("a short page answered the cursor %q, %v", cursor, err)
	}
}

func TestAListingSurfacesEveryWayItCanFail(t *testing.T) {
	if _, _, err := NewFiles().ListPrefix(t.Context(), &fakeQuerier{}, "owner", "files/", "", 0, nil); err == nil {
		t.Error("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errors.New("the connection failed")}
	if _, _, err := NewFiles().ListPrefix(t.Context(), broken, "owner", "files/", "", 10, nil); err == nil {
		t.Error("a connection failure answered a page")
	}
	unreadable := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{
		func(...any) error { return errors.New("the row is not a file") },
	}}}
	if _, _, err := NewFiles().ListPrefix(t.Context(), unreadable, "owner", "files/", "", 10, nil); err == nil {
		t.Error("a row that could not be read answered a page")
	}
	interrupted := &fakeQuerier{rows: &fakeRows{err: errors.New("the walk was interrupted")}}
	if _, _, err := NewFiles().ListPrefix(t.Context(), interrupted, "owner", "files/", "", 10, nil); err == nil {
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

// TestASearchBindsTheTextEscaped: the text a person typed reaches the
// statement as a pattern that matches it anywhere and literally, the match
// is on the last segment with both sides lowered, and the page is keyset
// paginated on the path as a listing is (spec 029).
func TestASearchBindsTheTextEscaped(t *testing.T) {
	for text, want := range map[string]string{
		"plan":       `%plan%`,
		"50%_off":    `%50\%\_off%`,
		`back\slash`: `%back\\slash%`,
		"Überblick":  `%Überblick%`,
		"two words ": `%two words %`,
	} {
		if got := likeContains(text); got != want {
			t.Errorf("likeContains(%q) = %q, want %q", text, got, want)
		}
	}

	first, second := aFile(), aFile()
	second.Path = "files/notes/zebra.md"
	q := &fakeQuerier{rows: rowsOf(first, second)}
	page, cursor, err := NewFiles().SearchPrefix(t.Context(), q, first.Owner, "files/notes/", "50%", "files/a", 2, nil)
	if err != nil {
		t.Fatalf("SearchPrefix: %v", err)
	}
	if len(page) != 2 || cursor != second.Path {
		t.Fatalf("the page holds %d rows and the cursor is %q", len(page), cursor)
	}
	statement := q.statements[0]
	for _, want := range []string{
		"path LIKE $2 ESCAPE '\\'", "path > $3", "deleted_at IS NULL",
		"lower(regexp_replace(path, '^.*/', '')) LIKE lower($6) ESCAPE '\\'", "ORDER BY path", "LIMIT $4",
	} {
		if !strings.Contains(statement, want) {
			t.Errorf("the search does not carry %q:\n%s", want, statement)
		}
	}
	if args := q.args[0]; args[1] != `files/notes/%` || args[2] != "files/a" || args[5] != `%50\%%` {
		t.Errorf("the search bound %v", args)
	}
	short := &fakeQuerier{rows: rowsOf(first)}
	if _, cursor, err := NewFiles().SearchPrefix(t.Context(), short, first.Owner, "files/", "a", "", 2, nil); err != nil || cursor != "" {
		t.Fatalf("a short page of a search answered the cursor %q, %v", cursor, err)
	}
}

// TestRecentOrdersOnTheModificationAndBindsTheTextOnlyWhenThereIsOne: the
// newest first, ties in path order, one page, and the name match joins the
// statement only when a text was given, so no empty pattern is bound.
func TestRecentOrdersOnTheModificationAndBindsTheTextOnlyWhenThereIsOne(t *testing.T) {
	plain := &fakeQuerier{rows: rowsOf(aFile())}
	rows, err := NewFiles().RecentPrefix(t.Context(), plain, "owner", "files/", "", 10, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("RecentPrefix = %d rows, %v", len(rows), err)
	}
	statement := plain.statements[0]
	for _, want := range []string{"path LIKE $2 ESCAPE '\\'", "deleted_at IS NULL", "ORDER BY updated_at DESC, path", "LIMIT $3"} {
		if !strings.Contains(statement, want) {
			t.Errorf("the newest page does not carry %q:\n%s", want, statement)
		}
	}
	if strings.Contains(statement, "regexp_replace") || len(plain.args[0]) != 4 {
		t.Errorf("a newest page with no text matched a name: %v\n%s", plain.args[0], statement)
	}

	named := &fakeQuerier{rows: rowsOf(aFile())}
	if _, err := NewFiles().RecentPrefix(t.Context(), named, "owner", "files/", "Plan_", 10, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(named.statements[0], "LIKE lower($5) ESCAPE '\\'") || named.args[0][4] != `%Plan\_%` {
		t.Errorf("a newest page with a text bound %v:\n%s", named.args[0], named.statements[0])
	}
}

// TestASearchAndRecentSurfaceEveryWayTheyCanFail: a page of no rows, a
// connection that fails, a row that does not scan and a walk that stops are
// each an error and never a page.
func TestASearchAndRecentSurfaceEveryWayTheyCanFail(t *testing.T) {
	for name, run := range map[string]func(q Querier, limit int) error{
		"SearchPrefix": func(q Querier, limit int) error {
			_, _, err := NewFiles().SearchPrefix(t.Context(), q, "owner", "files/", "a", "", limit, nil)
			return err
		},
		"RecentPrefix": func(q Querier, limit int) error {
			_, err := NewFiles().RecentPrefix(t.Context(), q, "owner", "files/", "a", limit, nil)
			return err
		},
	} {
		if err := run(&fakeQuerier{}, 0); err == nil {
			t.Errorf("%s accepted a page of no rows", name)
		}
		if err := run(&fakeQuerier{queryErr: errors.New("the connection failed")}, 10); err == nil {
			t.Errorf("%s answered a page over a failed connection", name)
		}
		unreadable := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{
			func(...any) error { return errors.New("the row is not a file") },
		}}}
		if err := run(unreadable, 10); err == nil {
			t.Errorf("%s answered a page with a row it could not read", name)
		}
		if err := run(&fakeQuerier{rows: &fakeRows{err: errors.New("the walk was interrupted")}}, 10); err == nil {
			t.Errorf("%s answered an interrupted walk", name)
		}
	}
}
