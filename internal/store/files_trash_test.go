// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

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
