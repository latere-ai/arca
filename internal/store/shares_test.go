// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// aGrant is one grant as a case writes it.
func aGrant(owner, prefix, grantee, permission string) Grant {
	return Grant{
		Owner: owner, PathPrefix: prefix, GranteeKind: GranteeSubject,
		Grantee: grantee, Permission: permission, CreatedBy: owner,
	}
}

// grantValues is one row of grantColumns, in its order.
func grantValues(g Grant) []any {
	return []any{
		g.ID, g.Owner, g.PathPrefix, g.GranteeKind, g.Grantee, g.Permission,
		g.Token, g.Status, g.ExpiresAt, g.CreatedBy, g.CreatedAt,
	}
}

// grantRow answers a row that scans to one grant.
func grantRow(g Grant) fakeRow {
	return fakeRow{scan: func(dest ...any) error { return assign(dest, grantValues(g)) }}
}

// grantRowsOf answers the rows of the grants a case named.
func grantRowsOf(gs ...Grant) *fakeRows {
	rows := &fakeRows{}
	for _, g := range gs {
		rows.scans = append(rows.scans, func(dest ...any) error { return assign(dest, grantValues(g)) })
	}
	return rows
}

func TestCreateBindsTheGrantAndReadsBackWhatTheDatabaseAssigned(t *testing.T) {
	expires := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	want := aGrant("https://issuer.example|alice", "files/reports", carol, "write")
	want.ExpiresAt = &expires
	stored := want
	stored.ID, stored.Status, stored.CreatedAt = "01J8GRANT", GrantActive, time.Unix(1, 0).UTC()

	q := &fakeQuerier{row: grantRow(stored)}
	got, err := NewShares().Create(t.Context(), q, want)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != stored.ID || got.Status != GrantActive {
		t.Errorf("Create answered %+v", got)
	}
	if !strings.Contains(q.statements[0], "INSERT INTO shares") {
		t.Errorf("the statement is %q", q.statements[0])
	}
	// The status is the statement's and not the caller's: a grant is born
	// active, and there is no other status a create may ask for.
	args := q.args[0]
	if args[0] != want.Owner || args[1] != want.PathPrefix || args[2] != GranteeSubject ||
		args[3] != want.Grantee || args[4] != "write" || args[6] != GrantActive {
		t.Errorf("Create bound %v", args)
	}
	if args[5] != nil {
		t.Errorf("a subject grant bound the token %v; a token that is not a token is NULL", args[5])
	}
}

func TestCreateTellsAConflictFromAFault(t *testing.T) {
	shares := NewShares()
	q := &fakeQuerier{row: failing(&pgconn.PgError{Code: "23505"})}
	if _, err := shares.Create(t.Context(), q, aGrant("a", "files/x", carol, "read")); !errors.Is(err, ErrConflict) {
		t.Errorf("a unique violation surfaced as %v", err)
	}
	boom := errors.New("the connection went away")
	q = &fakeQuerier{row: failing(boom)}
	_, err := shares.Create(t.Context(), q, aGrant("a", "files/x", carol, "read"))
	if !errors.Is(err, boom) || errors.Is(err, ErrConflict) {
		t.Errorf("a transient fault surfaced as %v", err)
	}
}

func TestGetReadsOneGrantAndTellsAMissingRow(t *testing.T) {
	want := aGrant("https://issuer.example|alice", "files/reports", carol, "manage")
	want.ID = "01J8GRANT"
	q := &fakeQuerier{row: grantRow(want)}
	got, err := NewShares().Get(t.Context(), q, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != want.ID || got.Permission != "manage" {
		t.Errorf("Get answered %+v", got)
	}
	if q.args[0][0] != want.ID {
		t.Errorf("Get bound %v", q.args[0])
	}
	// A revoked grant is read like any other, so a revoke is idempotent and
	// an audit can read what was revoked.
	if strings.Contains(q.statements[0], "status = ") {
		t.Errorf("Get filters the status: %q", q.statements[0])
	}
	q = &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewShares().Get(t.Context(), q, "nobody"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a grant that is not there = %v", err)
	}
}

func TestRevokeReportsWhetherThereWasAGrant(t *testing.T) {
	shares := NewShares()
	q := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	revoked, err := shares.Revoke(t.Context(), q, "01J8GRANT")
	if err != nil || !revoked {
		t.Fatalf("Revoke = %t, %v", revoked, err)
	}
	if args := q.args[0]; args[0] != "01J8GRANT" || args[1] != GrantRevoked {
		t.Errorf("Revoke bound %v", args)
	}
	// Nothing else is stamped: the predecessor wrote a resolver and a time
	// beside the status, and both belong to the approval queue spec 019
	// leaves behind.
	if strings.Contains(q.statements[0], "resolved") {
		t.Errorf("Revoke stamps more than the status: %q", q.statements[0])
	}
	q = &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if revoked, err := shares.Revoke(t.Context(), q, "nobody"); err != nil || revoked {
		t.Errorf("revoking nothing = %t, %v", revoked, err)
	}
	q = &fakeQuerier{execErr: errors.New("the connection went away")}
	if _, err := shares.Revoke(t.Context(), q, "01J8GRANT"); err == nil {
		t.Error("a failed revoke answered no error")
	}
}

func TestListSpacePagesTheGrantsOfOneSpace(t *testing.T) {
	shares := NewShares()
	first := aGrant("https://issuer.example|alice", "files/reports", carol, "read")
	first.ID = "01J8A"
	q := &fakeQuerier{rows: grantRowsOf(first)}
	page, err := shares.ListSpace(t.Context(), q, first.Owner, "files/reports", "01J89", 10)
	if err != nil {
		t.Fatalf("ListSpace: %v", err)
	}
	if len(page) != 1 || page[0].ID != first.ID {
		t.Fatalf("ListSpace answered %+v", page)
	}
	if args := q.args[0]; args[0] != first.Owner || args[1] != GrantActive ||
		args[2] != "files/reports" || args[3] != "01J89" || args[4] != 10 {
		t.Errorf("ListSpace bound %v", args)
	}
	// The order and the cursor are one comparison, so a walk resumes where
	// it stopped whatever the database's collation is.
	if !strings.Contains(q.statements[0], "ORDER BY id::text") ||
		!strings.Contains(q.statements[0], "id::text > $4") {
		t.Errorf("the statement orders and resumes differently: %q", q.statements[0])
	}
	if _, err := shares.ListSpace(t.Context(), q, first.Owner, "", "", 0); err == nil {
		t.Error("a page of no rows answered no error")
	}
}

func TestListGranteeReadsTheGranteeIndexAndNothingElse(t *testing.T) {
	shares := NewShares()
	g := aGrant("https://issuer.example|alice", "files/reports", carol, "write")
	g.ID = "01J8B"
	q := &fakeQuerier{rows: grantRowsOf(g)}
	page, err := shares.ListGrantee(t.Context(), q, carol, "", 100)
	if err != nil || len(page) != 1 {
		t.Fatalf("ListGrantee = %+v, %v", page, err)
	}
	if args := q.args[0]; args[0] != GranteeSubject || args[1] != carol || args[2] != GrantActive {
		t.Errorf("ListGrantee bound %v", args)
	}
	// An expired grant stops granting at the moment it expires, with no
	// sweep in between, so the filter is in the statement.
	if !strings.Contains(q.statements[0], "expires_at IS NULL OR expires_at > now()") {
		t.Errorf("the statement does not filter the expiry: %q", q.statements[0])
	}
	if _, err := shares.ListGrantee(t.Context(), q, carol, "", 0); err == nil {
		t.Error("a page of no rows answered no error")
	}
}

func TestCoveringAsksForThePrefixesOfThePathHighestFirst(t *testing.T) {
	shares := NewShares()
	manage := aGrant("https://issuer.example|alice", "files", carol, "manage")
	read := aGrant("https://issuer.example|alice", "files/reports", carol, "read")
	q := &fakeQuerier{rows: grantRowsOf(manage, read)}
	page, err := shares.Covering(t.Context(), q, manage.Owner, "files/reports/q3.pdf")
	if err != nil || len(page) != 2 {
		t.Fatalf("Covering = %+v, %v", page, err)
	}
	prefixes, ok := q.args[0][1].([]string)
	if !ok {
		t.Fatalf("Covering bound %T for the prefixes", q.args[0][1])
	}
	want := []string{"files/reports/q3.pdf", "files/reports", "files"}
	if !slices.Equal(prefixes, want) {
		t.Errorf("Covering asked for %v, want %v", prefixes, want)
	}
	if !strings.Contains(q.statements[0], "path_prefix = ANY($2)") {
		t.Errorf("the statement matches a prefix some other way: %q", q.statements[0])
	}
	if !strings.Contains(q.statements[0], "WHEN 'manage' THEN 3") {
		t.Errorf("the statement does not order by the ladder: %q", q.statements[0])
	}
	// A path in no plane names no prefix, so the query is not sent at all.
	page, err = shares.Covering(t.Context(), q, manage.Owner, "")
	if err != nil || page != nil {
		t.Errorf("Covering on no path = %+v, %v", page, err)
	}
	if len(q.statements) != 1 {
		t.Errorf("Covering on no path sent %d statements", len(q.statements))
	}
}

func TestAPageOfGrantsSurfacesEveryFailure(t *testing.T) {
	shares := NewShares()
	owner := "https://issuer.example|alice"
	boom := errors.New("the connection went away")
	if _, err := shares.ListSpace(t.Context(), &fakeQuerier{queryErr: boom}, owner, "", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed query = %v", err)
	}
	scanning := &fakeRows{scans: []func(...any) error{func(...any) error { return boom }}}
	if _, err := shares.ListSpace(t.Context(), &fakeQuerier{rows: scanning}, owner, "", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed scan = %v", err)
	}
	walking := &fakeRows{err: boom}
	if _, err := shares.ListSpace(t.Context(), &fakeQuerier{rows: walking}, owner, "", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed walk = %v", err)
	}
	if !walking.closed {
		t.Error("the rows were not closed")
	}
}

func TestPrefixesOfIsTheSegmentRule(t *testing.T) {
	for _, c := range []struct {
		path string
		want []string
	}{
		{"files/reports/q3.pdf", []string{"files/reports/q3.pdf", "files/reports", "files"}},
		{"files/reports", []string{"files/reports", "files"}},
		{"files", []string{"files"}},
		{"/files/reports/", []string{"files/reports", "files"}},
		{"", nil},
		{"/", nil},
	} {
		if got := PrefixesOf(c.path); !slices.Equal(got, c.want) {
			t.Errorf("PrefixesOf(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	// The rule read the other way: a sibling whose name begins with the
	// grant's is not covered, because it is in no prefix list of its own
	// that names the grant.
	if slices.Contains(PrefixesOf("files/reports-archive/q3.pdf"), "files/reports") {
		t.Error("files/reports covers files/reports-archive")
	}
}

func TestNullIsWhatAnAbsentColumnHolds(t *testing.T) {
	if got := null(""); got != nil {
		t.Errorf("null(\"\") = %v", got)
	}
	if got := null("t0ken"); got != "t0ken" {
		t.Errorf("null(%q) = %v", "t0ken", got)
	}
}

// carol is the grantee of the cases above: neither the owner of the space
// nor its creator.
const carol = "https://issuer.example|carol"

// Pass 7 of spec 010 counts and removes through one condition, so a dry run
// reports the rows the live run takes. A grant with no expiry is in neither:
// it grants until somebody revokes it.
func TestTheExpiredGrantsAreCountedAndRemovedByOneCondition(t *testing.T) {
	before := time.Now().Add(-720 * time.Hour)
	counted := &fakeQuerier{row: values(int64(4))}
	n, err := NewShares().Expired(t.Context(), counted, before)
	if err != nil || n != 4 {
		t.Fatalf("Expired = %d, %v", n, err)
	}
	purged := &fakeQuerier{tag: pgconn.NewCommandTag("DELETE 4")}
	gone, err := NewShares().PurgeExpired(t.Context(), purged, before)
	if err != nil || gone != 4 {
		t.Fatalf("PurgeExpired = %d, %v", gone, err)
	}
	if !strings.Contains(counted.statements[0], expiredGrants) ||
		!strings.Contains(purged.statements[0], expiredGrants) {
		t.Fatalf("the two halves read two conditions:\n%s\n%s", counted.statements[0], purged.statements[0])
	}
	if !strings.Contains(expiredGrants, "expires_at IS NOT NULL") {
		t.Errorf("a grant with no expiry is swept:\n%s", expiredGrants)
	}
	if counted.args[0][0] != before || purged.args[0][0] != before {
		t.Errorf("the halves bound %v and %v as the cutoff", counted.args[0][0], purged.args[0][0])
	}
}

func TestTheExpiredGrantsCarryWhatTheDatabaseAnswered(t *testing.T) {
	if _, err := NewShares().Expired(t.Context(), &fakeQuerier{row: failing(errFault)}, time.Now()); !errors.Is(err, errFault) {
		t.Errorf("a failed count answered %v", err)
	}
	if _, err := NewShares().PurgeExpired(t.Context(), &fakeQuerier{execErr: errFault}, time.Now()); !errors.Is(err, errFault) {
		t.Errorf("a failed purge answered %v", err)
	}
}
