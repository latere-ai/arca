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
)

// aTrashedFile is one row in the trash, deleted the given time ago.
func aTrashedFile(path string, ago time.Duration) File {
	f := aFile()
	f.Path = path
	at := time.Now().Add(-ago)
	f.DeletedAt = &at
	return f
}

func TestRestoringIsScopedToTheRetentionWindow(t *testing.T) {
	restored := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	since := time.Now().Add(-720 * time.Hour)
	ok, err := NewFiles().Restore(t.Context(), restored, "space", "files/a.md", since)
	if err != nil || !ok {
		t.Fatalf("Restore = %t, %v", ok, err)
	}
	statement := restored.statements[0]
	if !strings.Contains(statement, "deleted_at IS NOT NULL") || !strings.Contains(statement, "deleted_at > $3") {
		t.Fatalf("the restore is not scoped to the window:\n%s", statement)
	}
	if restored.args[0][2] != since {
		t.Fatalf("the restore bound %v as the window", restored.args[0][2])
	}
	expired := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewFiles().Restore(t.Context(), expired, "space", "files/a.md", since); err != nil || ok {
		t.Fatalf("Restore past the window = %t, %v", ok, err)
	}
	broken := &fakeQuerier{execErr: errFault}
	if _, err := NewFiles().Restore(t.Context(), broken, "space", "files/a.md", since); err == nil {
		t.Fatal("a failed restore reported success")
	}
}

func TestTheTrashListingIsNewestFirstAndResumesOnThePairItOrdersBy(t *testing.T) {
	newest := aTrashedFile("files/b.md", time.Hour)
	older := aTrashedFile("files/a.md", 2*time.Hour)
	first := &fakeQuerier{rows: rowsOf(newest, older)}
	page, err := NewFiles().ListTrash(t.Context(), first, "space", TrashCursor{}, 10, time.Now().Add(-720*time.Hour))
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(page) != 2 || page[0].Path != newest.Path {
		t.Fatalf("the page is %v", page)
	}
	if !strings.Contains(first.statements[0], "ORDER BY deleted_at DESC, path DESC") {
		t.Fatalf("the listing is not newest first:\n%s", first.statements[0])
	}
	// The first page compares against a time above every row, so one
	// statement serves the first page and every page after it.
	if at, ok := first.args[0][2].(time.Time); !ok || at.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("the first page bound %v as its cursor", first.args[0][2])
	}

	next := &fakeQuerier{rows: rowsOf(older)}
	cursor := TrashCursor{DeletedAt: *newest.DeletedAt, Path: newest.Path}
	if _, err := NewFiles().ListTrash(t.Context(), next, "space", cursor, 10, time.Time{}); err != nil {
		t.Fatalf("the second page: %v", err)
	}
	if next.args[0][2] != cursor.DeletedAt || next.args[0][3] != cursor.Path {
		t.Fatalf("the second page bound %v", next.args[0])
	}

	if _, err := NewFiles().ListTrash(t.Context(), first, "space", TrashCursor{}, 0, time.Time{}); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errFault}
	if _, err := NewFiles().ListTrash(t.Context(), broken, "space", TrashCursor{}, 10, time.Time{}); err == nil {
		t.Fatal("a failed listing reported success")
	}
	torn := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}}
	if _, err := NewFiles().ListTrash(t.Context(), torn, "space", TrashCursor{}, 10, time.Time{}); err == nil {
		t.Fatal("a row that would not scan reported success")
	}
	cut := &fakeQuerier{rows: &fakeRows{err: errFault}}
	if _, err := NewFiles().ListTrash(t.Context(), cut, "space", TrashCursor{}, 10, time.Time{}); err == nil {
		t.Fatal("a listing cut short reported success")
	}
}

func TestPurgingAnswersEveryRowItRemovedSoItsBytesCanFollow(t *testing.T) {
	one := aTrashedFile("files/a.md", time.Hour)
	other := aTrashedFile("files/b.md", 900*time.Hour)
	all := &fakeQuerier{rows: rowsOf(one, other)}
	purged, err := NewFiles().PurgeTrash(t.Context(), all, "space", "")
	if err != nil || len(purged) != 2 {
		t.Fatalf("PurgeTrash = %d rows, %v", len(purged), err)
	}
	// Emptying a trash takes what is past the window too: those rows are on
	// their way out either way, and a caller asking for an empty trash is
	// not asking for the subset retention still offers.
	if !strings.Contains(all.statements[0], `($2 = '' OR path = $2)`) {
		t.Fatalf("one path and the whole trash are not one statement:\n%s", all.statements[0])
	}
	broken := &fakeQuerier{queryErr: errFault}
	if _, err := NewFiles().PurgeTrash(t.Context(), broken, "space", ""); err == nil {
		t.Fatal("a failed purge reported success")
	}
	torn := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}}
	if _, err := NewFiles().PurgeTrash(t.Context(), torn, "space", ""); err == nil {
		t.Fatal("a row that would not scan reported success")
	}
	cut := &fakeQuerier{rows: &fakeRows{err: errFault}}
	if _, err := NewFiles().PurgeTrash(t.Context(), cut, "space", ""); !errors.Is(err, errFault) {
		t.Fatalf("a purge cut short answered %v", err)
	}
}

// The id-addressed arm of the restore, which the administrative route of
// spec 012 reaches. It answers the row it changed, so the caller reads what
// the database restored rather than what it read a moment before.
func TestRestoringByIDAnswersTheRowItBroughtBack(t *testing.T) {
	row := aTrashedFile("files/reports/q3.pdf", time.Hour)
	row.ID = "d0e6b0f8-0000-4000-8000-000000000001"
	live := row
	live.DeletedAt = nil
	q := &fakeQuerier{row: fakeRow{scan: fileScan(live)}}
	since := time.Now().Add(-720 * time.Hour)

	back, ok, err := NewFiles().RestoreByID(t.Context(), q, "space", row.ID, since)
	if err != nil || !ok {
		t.Fatalf("RestoreByID = %t, %v", ok, err)
	}
	if back.Path != row.Path || back.DeletedAt != nil {
		t.Fatalf("the restored row is %+v", back)
	}
	statement := q.statements[0]
	for _, want := range []string{"owner = $1", "id = $2", "deleted_at IS NOT NULL", "deleted_at > $3", "RETURNING"} {
		if !strings.Contains(statement, want) {
			t.Errorf("the statement does not carry %q:\n%s", want, statement)
		}
	}
	if q.args[0][0] != "space" || q.args[0][1] != row.ID || q.args[0][2] != since {
		t.Errorf("the restore bound %v", q.args[0])
	}
}

// An id of another space, one past the window, one that names nothing, and a
// string that is not an identifier at all are one answer: there is nothing to
// bring back. A word that is not an id names nothing, and answering a fault
// would make a wrong request a 500 (spec 001, invariant 6).
func TestRestoringByIDAnswersNothingForAnIDItCannotBringBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  fakeRow
	}{
		{"no such row", failing(pgx.ErrNoRows)},
		{"not an identifier", failing(&pgconn.PgError{Code: "22P02"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{row: tc.row}
			back, ok, err := NewFiles().RestoreByID(t.Context(), q, "space", "nope", time.Now())
			if err != nil || ok {
				t.Fatalf("RestoreByID = %+v, %t, %v", back, ok, err)
			}
		})
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, _, err := NewFiles().RestoreByID(t.Context(), broken, "space", "id", time.Now()); !errors.Is(err, errFault) {
		t.Fatalf("a failed restore answered %v", err)
	}
}
