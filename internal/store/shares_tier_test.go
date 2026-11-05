// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of the grants and links of spec 008: the queries of
// shares.go and links.go against a real Postgres. What it proves is what a
// fake cannot: that the narrowed schema applies and refuses what spec 019
// removed, that the covering query matches on segments, that a revoke and an
// expiry take effect on the next read with no sweep in between, and that a
// token is unique across every space.
package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// alice owns the space every case below grants on, and bobby is the grantee.
const (
	alice = "https://issuer.example|alice"
	bobby = "https://issuer.example|bobby"
)

// grant writes one grant of the case's shape and fails the test when the
// database refuses it.
func grant(t *testing.T, db *DB, g Grant) Grant {
	t.Helper()
	written, err := NewShares().Create(t.Context(), db.Querier(), g)
	if err != nil {
		t.Fatalf("grant %q on %q: %v", g.Permission, g.PathPrefix, err)
	}
	return written
}

// subjectGrant is one grant to a subject.
func subjectGrant(owner, prefix, grantee, permission string) Grant {
	return Grant{
		Owner: owner, PathPrefix: prefix, GranteeKind: GranteeSubject,
		Grantee: grantee, Permission: permission, CreatedBy: owner,
	}
}

// tokenGrant is one grant whose token is its grantee.
func tokenGrant(owner, prefix, kind, token string) Grant {
	return Grant{
		Owner: owner, PathPrefix: prefix, GranteeKind: kind,
		Permission: "read", Token: token, CreatedBy: owner,
	}
}

func TestStoreSharesTheTableIsTheNarrowedOne(t *testing.T) {
	db := tier(t)
	q := db.Querier()

	var exists bool
	if err := q.QueryRow(t.Context(),
		`SELECT to_regclass(current_schema() || '.shares') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("the migrations did not create shares")
	}

	// The kinds and the statuses spec 019 removes are refused by the
	// schema, so a build that still wrote one would not start writing it.
	for _, refused := range []struct {
		what string
		sql  string
		args []any
	}{
		{"a grant to an address", `INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
			VALUES ($1, 'files/x', 'email', 'person@example.test', 'read', $1)`, []any{alice}},
		{"a grant to a role", `INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, created_by)
			VALUES ($1, 'files/x', 'role', 'admin', 'read', $1)`, []any{alice}},
		{"a grant awaiting approval", `INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, status, created_by)
			VALUES ($1, 'files/x', 'subject', $2, 'read', 'pending', $1)`, []any{alice, bobby}},
		{"a token grant that writes", `INSERT INTO shares (owner, path_prefix, grantee_kind, permission, token, created_by)
			VALUES ($1, 'files/x', 'link', 'write', 'w0uld-be', $1)`, []any{alice}},
		{"a subject grant with a token", `INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, token, created_by)
			VALUES ($1, 'files/x', 'subject', $2, 'read', 'b0th', $1)`, []any{alice, bobby}},
		{"a token grant with a grantee", `INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission, token, created_by)
			VALUES ($1, 'files/x', 'link', $2, 'read', 'als0-b0th', $1)`, []any{alice, bobby}},
	} {
		if _, err := q.Exec(t.Context(), refused.sql, refused.args...); err == nil {
			t.Errorf("the schema accepted %s", refused.what)
		}
	}
}

func TestStoreSharesCoveringIsActiveUnexpiredAndMatchedBySegment(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	plane := grant(t, db, subjectGrant(alice, "files", bobby, "read"))
	subtree := grant(t, db, subjectGrant(alice, "files/reports", bobby, "manage"))
	grant(t, db, subjectGrant(alice, "files/reports-archive", bobby, "manage"))
	expired := subjectGrant(alice, "files/reports", bobby, "write")
	expired.ExpiresAt = &past
	grant(t, db, expired)
	revoked := grant(t, db, subjectGrant(alice, "files/reports", bobby, "write"))
	if _, err := shares.Revoke(t.Context(), db.Querier(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	living := subjectGrant(alice, "files/reports", bobby, "read")
	living.ExpiresAt = &future
	grant(t, db, living)

	covering, err := shares.Covering(t.Context(), db.Querier(), alice, "files/reports/q3.pdf")
	if err != nil {
		t.Fatalf("Covering: %v", err)
	}
	var ids []string
	for _, g := range covering {
		ids = append(ids, g.ID)
		if g.PathPrefix == "files/reports-archive" {
			t.Error("a grant on files/reports-archive covers files/reports")
		}
		if g.ID == expired.ID || g.ID == revoked.ID {
			t.Errorf("a grant that is %s covers the path", g.Status)
		}
	}
	if len(covering) != 3 {
		t.Fatalf("Covering answered %d grants: %v", len(covering), ids)
	}
	if covering[0].ID != subtree.ID {
		t.Errorf("the highest permission is not first: %v", ids)
	}
	if covering[0].Permission != "manage" || covering[len(covering)-1].Permission != "read" {
		t.Errorf("the page is not ordered by the ladder: %v", ids)
	}
	if !containsID(covering, plane.ID) {
		t.Error("a grant on the plane does not cover a path inside it")
	}

	// The sibling's own path is covered by its own grant and by the plane's,
	// which is the same rule read the other way.
	sibling, err := shares.Covering(t.Context(), db.Querier(), alice, "files/reports-archive/q3.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(sibling) != 2 {
		t.Fatalf("the sibling is covered by %d grants", len(sibling))
	}
	// Another space's grants are not this space's.
	other, err := shares.Covering(t.Context(), db.Querier(), bobby, "files/reports/q3.pdf")
	if err != nil || len(other) != 0 {
		t.Fatalf("a space with no grants = %d, %v", len(other), err)
	}
}

func TestStoreSharesHoldersAreEachSubjectsRungOnThePath(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	past := time.Now().Add(-time.Hour)
	// zed sorts before bobby in byte order and after it in most locales, so
	// the order the page comes back in is the C collation's and no other.
	const (
		carol = "https://issuer.example|carol"
		zed   = "https://issuer.example|Zed"
	)

	grant(t, db, subjectGrant(alice, "files", bobby, "read"))
	grant(t, db, subjectGrant(alice, "files/reports", bobby, "manage"))
	grant(t, db, subjectGrant(alice, "files/reports/q3.pdf", zed, "write"))
	grant(t, db, subjectGrant(alice, "files/reports-archive", carol, "manage"))
	expired := subjectGrant(alice, "files/reports", carol, "write")
	expired.ExpiresAt = &past
	grant(t, db, expired)
	revoked := grant(t, db, subjectGrant(alice, "files/reports", carol, "read"))
	if _, err := shares.Revoke(t.Context(), db.Querier(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	grant(t, db, tokenGrant(alice, "files/reports", GranteeLink, "h0lders-l1nk"))
	grant(t, db, subjectGrant(bobby, "files/reports", carol, "manage"))
	// On the destination of a move: bobby writes there, zed holds nothing.
	grant(t, db, subjectGrant(alice, "files/inbox", bobby, "write"))
	grant(t, db, subjectGrant(alice, "files/inbox", carol, "read"))

	got, err := shares.Holders(t.Context(), db.Querier(), alice, "files/reports/q3.pdf", "", 10)
	if err != nil {
		t.Fatalf("Holders: %v", err)
	}
	want := []Holding{{Grantee: zed, Permission: "write"}, {Grantee: bobby, Permission: "manage"}}
	if !slices.Equal(got, want) {
		t.Errorf("the holders of the file are %v, want %v", got, want)
	}

	// A move names both ends: a holder of one end alone is no holder, and
	// one of both holds the lower rung.
	moved, err := shares.Holders(t.Context(), db.Querier(), alice, "files/inbox/q3.pdf", "files/reports/q3.pdf", 10)
	if err != nil {
		t.Fatalf("Holders with a source: %v", err)
	}
	if want := []Holding{{Grantee: bobby, Permission: "write"}}; !slices.Equal(moved, want) {
		t.Errorf("the holders of both ends are %v, want %v", moved, want)
	}

	// The limit is applied after the order, so the page is the first by
	// subject.
	first, err := shares.Holders(t.Context(), db.Querier(), alice, "files/reports/q3.pdf", "", 1)
	if err != nil || len(first) != 1 || first[0] != (Holding{Grantee: zed, Permission: "write"}) {
		t.Errorf("a page of one is %v, %v", first, err)
	}

	for _, c := range []struct{ owner, path, from string }{
		{bobby, "files/inbox/q3.pdf", ""},
		{alice, "", ""},
		{alice, "files/reports/q3.pdf", "/"},
	} {
		none, err := shares.Holders(t.Context(), db.Querier(), c.owner, c.path, c.from, 10)
		if err != nil || len(none) != 0 {
			t.Errorf("Holders(%q, %q, %q) = %v, %v", c.owner, c.path, c.from, none, err)
		}
	}
	if _, err := shares.Holders(t.Context(), db.Querier(), alice, "files/reports/q3.pdf", "", 0); err == nil {
		t.Error("a page of no rows was answered")
	}
}

func containsID(grants []Grant, id string) bool {
	for _, g := range grants {
		if g.ID == id {
			return true
		}
	}
	return false
}

func TestStoreSharesAPageWalksEachGrantOnceAndResumesWhereItStopped(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	for i := range 12 {
		grant(t, db, subjectGrant(alice, fmt.Sprintf("files/p%02d", i), bobby, "read"))
	}
	grant(t, db, subjectGrant(bobby, "files/elsewhere", alice, "read"))

	seen := map[string]int{}
	cursor := ""
	for range 12 {
		page, err := shares.ListSpace(t.Context(), db.Querier(), alice, "", cursor, 5, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, g := range page {
			seen[g.ID]++
			if g.Owner != alice {
				t.Errorf("the page of one space carries a grant of %q", g.Owner)
			}
		}
		cursor = page[len(page)-1].ID
	}
	if len(seen) != 12 {
		t.Fatalf("the walk saw %d of twelve grants", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("%s was walked %d times", id, count)
		}
	}

	narrowed, err := shares.ListSpace(t.Context(), db.Querier(), alice, "files/p03", "", 100, nil)
	if err != nil || len(narrowed) != 1 {
		t.Fatalf("a page narrowed to one subtree = %d, %v", len(narrowed), err)
	}
}

func TestStoreSharesWithMeIsTheGranteeIndexAndNoMembership(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	mine := grant(t, db, subjectGrant(alice, "files/reports", bobby, "write"))
	grant(t, db, subjectGrant(alice, "files/private", alice, "manage"))
	expired := subjectGrant(alice, "files/stale", bobby, "read")
	past := time.Now().Add(-time.Minute)
	expired.ExpiresAt = &past
	grant(t, db, expired)

	page, err := shares.ListGrantee(t.Context(), db.Querier(), bobby, "", 100, nil)
	if err != nil {
		t.Fatalf("ListGrantee: %v", err)
	}
	if len(page) != 1 || page[0].ID != mine.ID {
		t.Fatalf("what is shared with the grantee is %+v", page)
	}

	// A revoke takes effect on the next read, with no sweep in between.
	if _, err := shares.Revoke(t.Context(), db.Querier(), mine.ID); err != nil {
		t.Fatal(err)
	}
	page, err = shares.ListGrantee(t.Context(), db.Querier(), bobby, "", 100, nil)
	if err != nil || len(page) != 0 {
		t.Fatalf("after the revoke the grantee holds %+v, %v", page, err)
	}
	// The row is kept, so an audit can see the grant existed.
	held, err := shares.Get(t.Context(), db.Querier(), mine.ID)
	if err != nil || held.Status != GrantRevoked {
		t.Fatalf("the revoked row is %+v, %v", held, err)
	}
}

func TestStoreLinksATokenResolvesUntilItIsRevokedOrExpires(t *testing.T) {
	db := tier(t)
	shares := NewShares()
	live := grant(t, db, tokenGrant(alice, "files/reports", GranteeLink, "t0ken-live"))
	expired := tokenGrant(alice, "files/reports", GranteePublic, "t0ken-expired")
	past := time.Now().Add(-time.Second)
	expired.ExpiresAt = &past
	grant(t, db, expired)

	got, err := shares.ByToken(t.Context(), db.Querier(), "t0ken-live")
	if err != nil || got.ID != live.ID {
		t.Fatalf("ByToken = %+v, %v", got, err)
	}
	for _, token := range []string{"t0ken-expired", "t0ken-guessed"} {
		if _, err := shares.ByToken(t.Context(), db.Querier(), token); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("the token %q resolved: %v", token, err)
		}
	}
	if _, err := shares.Live(t.Context(), db.Querier(), live.ID, alice); err != nil {
		t.Fatalf("Live: %v", err)
	}
	if _, err := shares.Live(t.Context(), db.Querier(), live.ID, bobby); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("the link answered for a space that does not own it: %v", err)
	}

	if _, err := shares.Revoke(t.Context(), db.Querier(), live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.ByToken(t.Context(), db.Querier(), "t0ken-live"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a revoked token still resolves: %v", err)
	}
	if _, err := shares.Live(t.Context(), db.Querier(), live.ID, alice); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a revoked link is still live: %v", err)
	}
}

func TestStoreLinksATokenIsUniqueAcrossEverySpace(t *testing.T) {
	db := tier(t)
	grant(t, db, tokenGrant(alice, "files/reports", GranteeLink, "t0ken-shared"))
	_, err := NewShares().Create(t.Context(), db.Querier(), tokenGrant(bobby, "files/other", GranteeLink, "t0ken-shared"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a second grant of one token = %v", err)
	}
}

func TestStoreLinksTheSubtreeIsThePrefixAndNotItsSibling(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	for _, path := range []string{
		"files/reports", "files/reports/q3.pdf", "files/reports/q4/summary.md",
		"files/reports-archive/2025.pdf", "files/elsewhere.txt",
	} {
		if _, err := files.Insert(t.Context(), db.Querier(), aRow(alice, path)); err != nil {
			t.Fatal(err)
		}
	}
	trashed := aRow(alice, "files/reports/gone.md")
	if _, err := files.Insert(t.Context(), db.Querier(), trashed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Querier().Exec(t.Context(),
		`UPDATE files SET deleted_at = now() WHERE owner = $1 AND path = $2`, alice, trashed.Path); err != nil {
		t.Fatal(err)
	}

	page, err := NewShares().Subtree(t.Context(), db.Querier(), alice, "files/reports", "", 100, nil)
	if err != nil {
		t.Fatalf("Subtree: %v", err)
	}
	var paths []string
	for _, f := range page {
		paths = append(paths, f.Path)
	}
	want := []string{"files/reports", "files/reports/q3.pdf", "files/reports/q4/summary.md"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("the subtree is %v, want %v", paths, want)
	}

	// The prefix that names one object lists that object alone.
	one, err := NewShares().Subtree(t.Context(), db.Querier(), alice, "files/elsewhere.txt", "", 100, nil)
	if err != nil || len(one) != 1 || one[0].Path != "files/elsewhere.txt" {
		t.Fatalf("a prefix naming one object = %+v, %v", one, err)
	}
}

func TestStoreLinksMarkPublicIsARowAndNeverAPath(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	row := aRow(alice, "files/reports/q3.pdf")
	if _, err := files.Insert(t.Context(), db.Querier(), row); err != nil {
		t.Fatal(err)
	}
	shares := NewShares()
	marked, err := shares.MarkPublic(t.Context(), db.Querier(), alice, row.Path, true)
	if err != nil || !marked.IsPublic || marked.ObjectID != row.ObjectID {
		t.Fatalf("MarkPublic = %+v, %v", marked, err)
	}
	held, err := files.Get(t.Context(), db.Querier(), alice, row.Path)
	if err != nil || !held.IsPublic {
		t.Fatalf("the row holds %+v, %v", held, err)
	}
	if _, err := shares.MarkPublic(t.Context(), db.Querier(), alice, row.Path, false); err != nil {
		t.Fatal(err)
	}
	if held, err = files.Get(t.Context(), db.Querier(), alice, row.Path); err != nil || held.IsPublic {
		t.Fatalf("the row is still public: %+v, %v", held, err)
	}
	// A path nothing wrote is not marked, and the caller tells that from a
	// fault.
	if _, err := shares.MarkPublic(t.Context(), db.Querier(), alice, "files/avatar", true); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("marking a path that is not there = %v", err)
	}
}

// TestStoreLinksAHoldWaitsAndThenReadsTheRevokeBeforeIt is what the revoke
// of one of two public links on an object stands on: a second transaction
// that holds the row waits for the first to end, and then reads the grant
// the first revoked as revoked. Without the wait both would read the other
// as live, and both would leave the object public.
func TestStoreLinksAHoldWaitsAndThenReadsTheRevokeBeforeIt(t *testing.T) {
	db := tier(t)
	row := aRow(alice, "files/reports/q3.pdf")
	if _, err := NewFiles().Insert(t.Context(), db.Querier(), row); err != nil {
		t.Fatal(err)
	}
	first := grant(t, db, tokenGrant(alice, row.Path, GranteePublic, "t0ken-first"))
	second := grant(t, db, tokenGrant(alice, row.Path, GranteePublic, "t0ken-second"))
	shares := NewShares()

	held, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- db.Tx(t.Context(), func(q Querier) error {
			if _, err := shares.Hold(t.Context(), q, alice, row.Path); err != nil {
				return err
			}
			if _, err := shares.Revoke(t.Context(), q, first.ID); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-holder:
		t.Fatalf("the first transaction ended before it held the row: %v", err)
	}

	type read struct {
		grants []Grant
		err    error
	}
	after := make(chan read, 1)
	go func() {
		var r read
		r.err = db.Tx(t.Context(), func(q Querier) error {
			if _, err := shares.Hold(t.Context(), q, alice, row.Path); err != nil {
				return err
			}
			var err error
			r.grants, err = shares.Covering(t.Context(), q, alice, row.Path)
			return err
		})
		after <- r
	}()
	select {
	case r := <-after:
		close(release)
		t.Fatalf("the second hold did not wait for the first: %+v, %v", r.grants, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatalf("the first transaction: %v", err)
	}
	r := <-after
	if r.err != nil {
		t.Fatalf("the second transaction: %v", r.err)
	}
	if containsID(r.grants, first.ID) || !containsID(r.grants, second.ID) {
		t.Errorf("after the wait the covering read holds %+v", r.grants)
	}

	// A path that is not there holds nothing, and the caller tells that from
	// a fault.
	if _, err := shares.Hold(t.Context(), db.Querier(), alice, "files/avatar"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("holding a path that is not there = %v", err)
	}
}
