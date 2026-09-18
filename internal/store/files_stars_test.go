// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/object"
)

// starScan answers the scan of one star row joined with its live file, in
// the order the listing selects.
func starScan(s Star) func(dest ...any) error {
	return func(dest ...any) error {
		return assign(dest, []any{
			s.Subject, s.Owner, s.Path, s.CreatedAt,
			s.File.ContentType, s.File.SizeBytes, s.File.Checksum, s.File.ChecksumKind,
			s.File.IsPublic, s.File.UpdatedAt,
		})
	}
}

// aStar is one bookmark and the live row it points at.
func aStar(owner, path string) Star {
	return Star{
		Subject:   "https://issuer.example|9ab3",
		Owner:     owner,
		Path:      path,
		CreatedAt: time.Now(),
		File: File{
			ContentType:  "text/markdown",
			SizeBytes:    11,
			Checksum:     strings.Repeat("d", 64),
			ChecksumKind: object.ChecksumSHA256,
			UpdatedAt:    time.Now(),
		},
	}
}

func TestStarringAndUnstarringAreIdempotent(t *testing.T) {
	added := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 1")}
	if err := NewStars().Add(t.Context(), added, "sub", "space", "files/a.md"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.Contains(added.statements[0], "DO NOTHING") {
		t.Fatalf("starring twice is not one row:\n%s", added.statements[0])
	}
	removed := &fakeQuerier{tag: pgconn.NewCommandTag("DELETE 0")}
	if err := NewStars().Remove(t.Context(), removed, "sub", "space", "files/a.md"); err != nil {
		t.Fatalf("Remove of a star that is not there: %v", err)
	}
	broken := &fakeQuerier{execErr: errFault}
	if err := NewStars().Add(t.Context(), broken, "sub", "space", "files/a.md"); err == nil {
		t.Fatal("a failed star reported success")
	}
	if err := NewStars().Remove(t.Context(), broken, "sub", "space", "files/a.md"); err == nil {
		t.Fatal("a failed unstar reported success")
	}
}

func TestTheStarListingJoinsLiveRowsAndResumesOnTheSpaceAndThePath(t *testing.T) {
	rows := &fakeRows{}
	for _, s := range []Star{aStar("space-a", "files/a.md"), aStar("space-b", "files/b.md")} {
		rows.scans = append(rows.scans, starScan(s))
	}
	listed := &fakeQuerier{rows: rows}
	page, err := NewStars().List(t.Context(), listed, "sub", StarCursor{}, 10)
	if err != nil || len(page) != 2 {
		t.Fatalf("List = %d rows, %v", len(page), err)
	}
	if page[0].File.Owner != page[0].Owner || page[0].File.Path != page[0].Path {
		t.Fatalf("the joined row does not carry the star's target: %+v", page[0])
	}
	statement := listed.statements[0]
	if !strings.Contains(statement, "f.deleted_at IS NULL") {
		t.Fatalf("a trashed target stays in the listing:\n%s", statement)
	}
	if !strings.Contains(statement, "(s.owner, s.path) > ($2, $3)") {
		t.Fatalf("the listing does not resume on the pair it orders by:\n%s", statement)
	}
	if _, err := NewStars().List(t.Context(), listed, "sub", StarCursor{}, 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errFault}
	if _, err := NewStars().List(t.Context(), broken, "sub", StarCursor{}, 10); err == nil {
		t.Fatal("a failed listing reported success")
	}
	torn := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}}
	if _, err := NewStars().List(t.Context(), torn, "sub", StarCursor{}, 10); err == nil {
		t.Fatal("a row that would not scan reported success")
	}
	cut := &fakeQuerier{rows: &fakeRows{err: errFault}}
	if _, err := NewStars().List(t.Context(), cut, "sub", StarCursor{}, 10); err == nil {
		t.Fatal("a listing cut short reported success")
	}
}

func TestAMoveCarriesTheStarsAndLeavesOnePerSubject(t *testing.T) {
	moved := &steps{tags: []pgconn.CommandTag{
		pgconn.NewCommandTag("DELETE 1"), pgconn.NewCommandTag("UPDATE 2"),
	}}
	n, err := NewStars().Move(t.Context(), moved, "space", "files/a.md", "files/b.md")
	if err != nil || n != 2 {
		t.Fatalf("Move = %d, %v", n, err)
	}
	if len(moved.statements) != 2 || !strings.HasPrefix(strings.TrimSpace(moved.statements[0]), "DELETE") {
		t.Fatalf("the colliding bookmark is not dropped first:\n%v", moved.statements)
	}
	firstFailed := &steps{errs: []error{errFault}}
	if _, err := NewStars().Move(t.Context(), firstFailed, "space", "files/a.md", "files/b.md"); err == nil {
		t.Fatal("a failed move reported success")
	}
	secondFailed := &steps{
		tags: []pgconn.CommandTag{pgconn.NewCommandTag("DELETE 0")},
		errs: []error{nil, errFault},
	}
	if _, err := NewStars().Move(t.Context(), secondFailed, "space", "files/a.md", "files/b.md"); err == nil {
		t.Fatal("a move whose rename failed reported success")
	}
}
