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

// aVersion is one superseded content as the queries read and write it.
func aVersion(n int) Version {
	return Version{
		ID:           "0c1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e",
		Owner:        "https://issuer.example|9ab3",
		Path:         "files/notes/todo.md",
		VersionNo:    n,
		ObjectID:     object.NewID(),
		ContentType:  "text/markdown",
		SizeBytes:    int64(10 * n),
		Checksum:     strings.Repeat("c", 64),
		ChecksumKind: object.ChecksumSHA256,
		CreatedBy:    "https://issuer.example|9ab3",
		SupersededAt: time.Now(),
	}
}

// versionScan answers the scan of one version row, in the order of
// versionColumns.
func versionScan(v Version) func(dest ...any) error {
	return func(dest ...any) error {
		return assign(dest, []any{
			v.ID, v.Owner, v.Path, v.VersionNo, v.ObjectID, v.ContentType, v.SizeBytes,
			v.Checksum, v.ChecksumKind, v.CreatedBy, v.SupersededAt,
		})
	}
}

// versionRowsOf answers the rows of the versions the case named.
func versionRowsOf(vs ...Version) *fakeRows {
	rows := &fakeRows{}
	for _, v := range vs {
		rows.scans = append(rows.scans, versionScan(v))
	}
	return rows
}

func TestCaptureCopiesNoBytesAndCarriesThePreconditionWhenThereIsOne(t *testing.T) {
	plain := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 1")}
	ok, err := NewVersions().Capture(t.Context(), plain, "space", "files/a.md", "")
	if err != nil || !ok {
		t.Fatalf("Capture = %t, %v", ok, err)
	}
	statement := plain.statements[0]
	if !strings.Contains(statement, "f.object_id") {
		t.Fatalf("the capture does not keep the object the file had:\n%s", statement)
	}
	if strings.Contains(statement, "f.checksum = $3") {
		t.Fatalf("a capture with no precondition carries one:\n%s", statement)
	}
	if !strings.Contains(statement, "MAX(v.version_no)") {
		t.Fatalf("the version number is not read in the statement:\n%s", statement)
	}

	conditional := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 1")}
	if _, err := NewVersions().Capture(t.Context(), conditional, "space", "files/a.md", "abc"); err != nil {
		t.Fatalf("a conditional capture: %v", err)
	}
	if !strings.Contains(conditional.statements[0], "f.checksum = $3") {
		t.Fatalf("the precondition is not in the statement:\n%s", conditional.statements[0])
	}

	lost := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 0")}
	if ok, err := NewVersions().Capture(t.Context(), lost, "space", "files/a.md", "abc"); err != nil || ok {
		t.Fatalf("a capture that lost the race = %t, %v", ok, err)
	}
	refused := &fakeQuerier{execErr: conflictError}
	if _, err := NewVersions().Capture(t.Context(), refused, "space", "files/a.md", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("a unique violation surfaced as %v", err)
	}
}

func TestReadingOneVersionTellsAFaultFromAnAbsence(t *testing.T) {
	v := aVersion(3)
	found := &fakeQuerier{row: fakeRow{scan: versionScan(v)}}
	got, err := NewVersions().Get(t.Context(), found, v.Owner, v.Path, 3)
	if err != nil || got.VersionNo != 3 || got.ObjectID != v.ObjectID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewVersions().Get(t.Context(), absent, v.Owner, v.Path, 9); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a version that is not there = %v", err)
	}
}

func TestTheVersionListingIsOldestFirstAndResumesOnTheNumber(t *testing.T) {
	page := &fakeQuerier{rows: versionRowsOf(aVersion(1), aVersion(2))}
	got, err := NewVersions().List(t.Context(), page, "space", "files/a.md", 0, 10)
	if err != nil || len(got) != 2 || got[0].VersionNo != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}
	if !strings.Contains(page.statements[0], "version_no > $3") ||
		!strings.Contains(page.statements[0], "ORDER BY version_no") {
		t.Fatalf("the listing is not keyset on the number:\n%s", page.statements[0])
	}
	if _, err := NewVersions().List(t.Context(), page, "space", "files/a.md", 0, 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errFault}
	if _, err := NewVersions().List(t.Context(), broken, "space", "files/a.md", 0, 10); err == nil {
		t.Fatal("a failed listing reported success")
	}
	torn := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}}
	if _, err := NewVersions().List(t.Context(), torn, "space", "files/a.md", 0, 10); err == nil {
		t.Fatal("a row that would not scan reported success")
	}
	cut := &fakeQuerier{rows: &fakeRows{err: errFault}}
	if _, err := NewVersions().List(t.Context(), cut, "space", "files/a.md", 0, 10); err == nil {
		t.Fatal("a listing cut short reported success")
	}
}

func TestPruningAVersionAnswersWhatItRemoved(t *testing.T) {
	v := aVersion(2)
	removed := &fakeQuerier{row: fakeRow{scan: versionScan(v)}}
	got, ok, err := NewVersions().Delete(t.Context(), removed, v.Owner, v.Path, 2)
	if err != nil || !ok || got.ObjectID != v.ObjectID {
		t.Fatalf("Delete = %+v, %t, %v", got, ok, err)
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, ok, err := NewVersions().Delete(t.Context(), absent, v.Owner, v.Path, 2); err != nil || ok {
		t.Fatalf("Delete of a version that is not there = %t, %v", ok, err)
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, _, err := NewVersions().Delete(t.Context(), broken, v.Owner, v.Path, 2); err == nil {
		t.Fatal("a failed prune reported success")
	}
}

func TestRemovingAPathsHistoryAnswersEveryRowSoItsBytesCanFollow(t *testing.T) {
	all := &fakeQuerier{rows: versionRowsOf(aVersion(1), aVersion(2))}
	got, err := NewVersions().DeletePath(t.Context(), all, "space", "files/a.md")
	if err != nil || len(got) != 2 {
		t.Fatalf("DeletePath = %d rows, %v", len(got), err)
	}
	broken := &fakeQuerier{queryErr: errFault}
	if _, err := NewVersions().DeletePath(t.Context(), broken, "space", "files/a.md"); err == nil {
		t.Fatal("a failed removal reported success")
	}
	torn := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}}
	if _, err := NewVersions().DeletePath(t.Context(), torn, "space", "files/a.md"); err == nil {
		t.Fatal("a row that would not scan reported success")
	}
	cut := &fakeQuerier{rows: &fakeRows{err: errFault}}
	if _, err := NewVersions().DeletePath(t.Context(), cut, "space", "files/a.md"); !errors.Is(err, errFault) {
		t.Fatalf("a removal cut short answered %v", err)
	}
}

func TestAMoveCarriesTheHistoryWithThePath(t *testing.T) {
	moved := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 3")}
	n, err := NewVersions().Move(t.Context(), moved, "space", "files/a.md", "files/b.md")
	if err != nil || n != 3 {
		t.Fatalf("Move = %d, %v", n, err)
	}
	broken := &fakeQuerier{execErr: errFault}
	if _, err := NewVersions().Move(t.Context(), broken, "space", "files/a.md", "files/b.md"); err == nil {
		t.Fatal("a failed move reported success")
	}
}
