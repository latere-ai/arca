// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

// aFileRow is one live file as a case writes it.
func aFileRow(owner, path string) File {
	return File{
		Owner: owner, Path: path, ObjectID: object.NewID(), CreatedBy: owner,
		ContentType: "application/pdf", SizeBytes: 48213,
		Checksum: strings.Repeat("9", 64), ChecksumKind: object.ChecksumSHA256,
	}
}

// aLink is one token grant as a case writes it.
func aLink(owner, prefix, token string) Grant {
	return Grant{
		ID: "01J8LINK", Owner: owner, PathPrefix: prefix, GranteeKind: GranteeLink,
		Permission: "read", Token: token, Status: GrantActive, CreatedBy: owner,
	}
}

func TestByTokenResolvesALiveTokenAndNamesNoneInItsError(t *testing.T) {
	shares := NewShares()
	link := aLink("https://issuer.example|alice", "files/reports", "t0ken")
	q := &fakeQuerier{row: grantRow(link)}
	got, err := shares.ByToken(t.Context(), q, link.Token)
	if err != nil {
		t.Fatalf("ByToken: %v", err)
	}
	if got.ID != link.ID || got.PathPrefix != link.PathPrefix {
		t.Errorf("ByToken answered %+v", got)
	}
	if q.args[0][0] != link.Token {
		t.Errorf("ByToken bound %v", q.args[0])
	}
	// An unknown token, a revoked one, and an expired one are one answer,
	// so the handler above cannot tell them apart and neither can a caller.
	for _, want := range []string{"grantee_kind IN ('link', 'public')", "status = 'active'",
		"expires_at IS NULL OR expires_at > now()"} {
		if !strings.Contains(q.statements[0], want) {
			t.Errorf("the statement does not carry %q: %q", want, q.statements[0])
		}
	}
	q = &fakeQuerier{row: failing(pgx.ErrNoRows)}
	_, err = shares.ByToken(t.Context(), q, "guessed")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a token that resolves to nothing = %v", err)
	}
	if strings.Contains(err.Error(), "guessed") {
		t.Errorf("the error names the token: %q", err)
	}
}

func TestLiveReadsOneLinkOfOneSpace(t *testing.T) {
	shares := NewShares()
	link := aLink("https://issuer.example|alice", "files/reports", "t0ken")
	q := &fakeQuerier{row: grantRow(link)}
	got, err := shares.Live(t.Context(), q, link.ID, link.Owner)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if got.PathPrefix != link.PathPrefix {
		t.Errorf("Live answered %+v", got)
	}
	if args := q.args[0]; args[0] != link.ID || args[1] != link.Owner {
		t.Errorf("Live bound %v", args)
	}
	q = &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := shares.Live(t.Context(), q, link.ID, "https://issuer.example|bob"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a link of another space = %v", err)
	}
}

func TestListTokensPagesTheLinksOfOneSpace(t *testing.T) {
	shares := NewShares()
	link := aLink("https://issuer.example|alice", "files/reports", "t0ken")
	q := &fakeQuerier{rows: grantRowsOf(link)}
	page, err := shares.ListTokens(t.Context(), q, link.Owner, "01J8A", 25)
	if err != nil || len(page) != 1 {
		t.Fatalf("ListTokens = %+v, %v", page, err)
	}
	if args := q.args[0]; args[0] != link.Owner || args[1] != "01J8A" || args[2] != 25 {
		t.Errorf("ListTokens bound %v", args)
	}
	if _, err := shares.ListTokens(t.Context(), q, link.Owner, "", 0); err == nil {
		t.Error("a page of no rows answered no error")
	}
}

func TestSubtreeMatchesOnSegments(t *testing.T) {
	shares := NewShares()
	owner := "https://issuer.example|alice"
	q := &fakeQuerier{rows: rowsOf(aFileRow(owner, "files/reports/q3.pdf"))}
	page, err := shares.Subtree(t.Context(), q, owner, "files/reports", "", 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("Subtree = %+v, %v", page, err)
	}
	args := q.args[0]
	if args[1] != "files/reports" || args[2] != `files/reports/%` {
		t.Errorf("Subtree bound %v; the pattern carries the slash that keeps the match on segments", args)
	}
	if !strings.Contains(q.statements[0], "deleted_at IS NULL") {
		t.Errorf("Subtree lists what is trashed: %q", q.statements[0])
	}
	if _, err := shares.Subtree(t.Context(), q, owner, "files/reports", "", 0); err == nil {
		t.Error("a page of no rows answered no error")
	}
	boom := errors.New("the connection went away")
	if _, err := shares.Subtree(t.Context(), &fakeQuerier{queryErr: boom}, owner, "files", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed query = %v", err)
	}
	scanning := &fakeRows{scans: []func(...any) error{func(...any) error { return boom }}}
	if _, err := shares.Subtree(t.Context(), &fakeQuerier{rows: scanning}, owner, "files", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed scan = %v", err)
	}
	walking := &fakeRows{err: boom}
	if _, err := shares.Subtree(t.Context(), &fakeQuerier{rows: walking}, owner, "files", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed walk = %v", err)
	}
	if !walking.closed {
		t.Error("the rows were not closed")
	}
}

func TestMarkPublicWritesTheRowAndAnswersTheObjectItNames(t *testing.T) {
	shares := NewShares()
	owner := "https://issuer.example|alice"
	stored := aFileRow(owner, "files/reports/q3.pdf")
	stored.IsPublic = true
	q := &fakeQuerier{row: fakeRow{scan: fileScan(stored)}}
	got, err := shares.MarkPublic(t.Context(), q, owner, stored.Path, true)
	if err != nil {
		t.Fatalf("MarkPublic: %v", err)
	}
	if got.ObjectID != stored.ObjectID || !got.IsPublic {
		t.Errorf("MarkPublic answered %+v", got)
	}
	if args := q.args[0]; args[0] != owner || args[1] != stored.Path || args[2] != true {
		t.Errorf("MarkPublic bound %v", args)
	}
	q = &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := shares.MarkPublic(t.Context(), q, owner, "files/gone", false); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("marking a path that is not there = %v", err)
	}
}

// The overview of spec 012 counts a space's links through the same predicate
// every other token query carries, so the number it reports and the rows the
// link listing serves are one reading of what a live link is.
func TestCountingLinksReadsThePredicateEveryTokenQueryCarries(t *testing.T) {
	rows := &fakeRows{scans: []func(...any) error{
		func(dest ...any) error { return assign(dest, []any{"space-a", int64(3)}) },
		func(dest ...any) error { return assign(dest, []any{"space-b", int64(1)}) },
	}}
	q := &fakeQuerier{rows: rows}
	owners := []string{"space-a", "space-b", "space-c"}

	counts, err := NewShares().CountLinks(t.Context(), q, owners)
	if err != nil {
		t.Fatalf("CountLinks: %v", err)
	}
	if counts["space-a"] != 3 || counts["space-b"] != 1 {
		t.Fatalf("the counts are %v", counts)
	}
	// A space holding none is absent rather than zero: the caller reads a
	// map, and a row that says nothing is a row nobody needs.
	if _, named := counts["space-c"]; named {
		t.Errorf("a space that holds no link is a row of the answer: %v", counts)
	}
	statement := q.statements[0]
	if !strings.Contains(statement, liveToken) {
		t.Errorf("the count does not read the live-token predicate:\n%s", statement)
	}
	for _, want := range []string{"owner = ANY($1)", "GROUP BY owner"} {
		if !strings.Contains(statement, want) {
			t.Errorf("the statement does not carry %q:\n%s", want, statement)
		}
	}
	if !rows.closed {
		t.Error("the rows were not closed")
	}
}

// The page's owners go together. No owners is no query at all, so an
// overview whose page came back empty costs nothing.
func TestCountingNoSpacesReachesNoDatabase(t *testing.T) {
	q := &fakeQuerier{}
	counts, err := NewShares().CountLinks(t.Context(), q, nil)
	if err != nil || counts != nil {
		t.Fatalf("CountLinks over no spaces = %v, %v", counts, err)
	}
	if len(q.statements) != 0 {
		t.Errorf("a count of no spaces reached the database: %v", q.statements)
	}
}

func TestCountingLinksCarriesWhatTheDatabaseAnswered(t *testing.T) {
	owners := []string{"space"}
	if _, err := NewShares().CountLinks(t.Context(), &fakeQuerier{queryErr: errFault}, owners); !errors.Is(err, errFault) {
		t.Errorf("a failed count answered %v", err)
	}
	broken := &fakeRows{scans: []func(...any) error{func(...any) error { return errFault }}}
	if _, err := NewShares().CountLinks(t.Context(), &fakeQuerier{rows: broken}, owners); !errors.Is(err, errFault) {
		t.Errorf("a row that did not scan answered %v", err)
	}
	ended := &fakeRows{err: errFault}
	if _, err := NewShares().CountLinks(t.Context(), &fakeQuerier{rows: ended}, owners); !errors.Is(err, errFault) {
		t.Errorf("a page that ended in a failure answered %v", err)
	}
}
