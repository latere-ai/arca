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
		Permission: "read", Token: token, Status: StatusActive, CreatedBy: owner,
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
