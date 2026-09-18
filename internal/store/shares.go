// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Grant is one row of the shares table: one grantee holding one permission
// on one subtree of one space (spec 008). A grant is data and not a
// decision. Nothing here reads a claim, and nothing here decides anything;
// the decision is spec 006's, and it reads these rows through the seams of
// internal/auth.
type Grant struct {
	// ID is the grant's identity, the handle a revoke names.
	ID string
	// Owner is the space the grant is on, the subject <issuer>|<sub>.
	Owner string
	// PathPrefix is the subtree the grant covers, a plane rooted path with
	// no trailing slash. It is matched by segment: files/reports covers
	// files/reports and files/reports/q3.pdf and does not cover
	// files/reports-archive.
	PathPrefix string
	// GranteeKind is GranteeSubject, GranteeLink, or GranteePublic.
	GranteeKind string
	// Grantee is the grantee subject, set on a subject grant and empty on a
	// token grant.
	Grantee string
	// Permission is the rung of the ladder the grant carries: read, write,
	// or manage. A token grant carries read.
	Permission string
	// Token is the capability, set on a link or public grant and empty on a
	// subject grant. It is a bearer secret: it is answered once, at
	// creation, and never listed (spec 015).
	Token string
	// Status is StatusActive or StatusRevoked.
	Status string
	// ExpiresAt is when the grant stops counting, nil for no expiry.
	ExpiresAt *time.Time
	// CreatedBy is the subject that created the grant.
	CreatedBy string
	// CreatedAt is when.
	CreatedAt time.Time
}

// The grantee kinds of spec 008. A subject is a person, a service, or an
// organization, all one subject string the authorizer names; a link and the
// public are nobody, and the token is the grantee. The kinds the predecessor
// carried for a role, a team, an organization, and an invited address are
// gone with the claims that produced them (spec 019).
const (
	GranteeSubject = "subject"
	GranteeLink    = "link"
	GranteePublic  = "public"
)

// The statuses of spec 008. A revoked row is kept so an audit can see the
// grant existed; the pending and denied statuses of the approval queue the
// predecessor held do not arrive (spec 019).
const (
	StatusActive  = "active"
	StatusRevoked = "revoked"
)

// Shares is the query set over the grants of every space, the token grants
// among them included. A handler takes the interface, so a test passes a
// fake; NewShares answers the one over Postgres.
//
// The token halves of the set are in links.go, beside the two reads of the
// files table a link's own routes need. They are one interface because they
// are one table: a link is a grant with a token instead of a grantee.
type Shares interface {
	// Create writes one grant and answers it with the id and the creation
	// time the database assigned.
	Create(ctx context.Context, q Querier, g Grant) (Grant, error)
	// Get reads one grant by its id, revoked ones included, so a revoke is
	// idempotent and an audit can read what was revoked. A grant that is not
	// there is pgx.ErrNoRows.
	Get(ctx context.Context, q Querier, id string) (Grant, error)
	// Revoke sets the status of one grant to revoked and stamps nothing
	// else. It answers false when there is no such grant. A grant that is
	// already revoked is a success: the end state is the goal.
	Revoke(ctx context.Context, q Querier, id string) (bool, error)
	// ListSpace answers one page of the grants on a space, ordered by id,
	// narrowed to one subtree when prefix is not empty. Revoked grants are
	// not answered.
	ListSpace(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]Grant, error)
	// ListGrantee answers one page of the live grants whose grantee is the
	// subject, ordered by id. It is the one query shares/with-me runs, and
	// it reads no membership anywhere.
	ListGrantee(ctx context.Context, q Querier, grantee, cursor string, limit int) ([]Grant, error)
	// Covering answers the active, unexpired grants on the space whose
	// prefix covers path, highest permission first. The prefixes of path are
	// the whole of the match, so files/reports does not cover
	// files/reports-archive by construction and not by a filter.
	Covering(ctx context.Context, q Querier, owner, path string) ([]Grant, error)

	// ByToken resolves a token to its grant, live ones only (links.go).
	ByToken(ctx context.Context, q Querier, token string) (Grant, error)
	// Live reads one live token grant of a space by its id (links.go).
	Live(ctx context.Context, q Querier, id, owner string) (Grant, error)
	// ListTokens answers one page of a space's token grants (links.go).
	ListTokens(ctx context.Context, q Querier, owner, cursor string, limit int) ([]Grant, error)
	// Subtree answers one page of the live paths a grant's prefix covers
	// (links.go).
	Subtree(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]File, error)
	// MarkPublic stamps or clears the public flag on one path (links.go).
	MarkPublic(ctx context.Context, q Querier, owner, path string, public bool) (File, error)
}

// shares is the query set over Postgres.
type shares struct{}

// NewShares answers the query set over Postgres.
func NewShares() Shares { return shares{} }

// grantColumns is the column list every read of a grant shares, so one scan
// function serves them all. The two nullable columns are read through
// coalesce, so a grant is one flat value and no caller handles a pointer to
// a string.
const grantColumns = `id, owner, path_prefix, grantee_kind, coalesce(grantee, ''),
	permission, coalesce(token, ''), status, expires_at, created_by, created_at`

// scanGrant reads one row of grantColumns.
func scanGrant(r pgx.Row) (Grant, error) {
	var g Grant
	err := r.Scan(&g.ID, &g.Owner, &g.PathPrefix, &g.GranteeKind, &g.Grantee,
		&g.Permission, &g.Token, &g.Status, &g.ExpiresAt, &g.CreatedBy, &g.CreatedAt)
	return g, err
}

// scanGrants reads a page of grants and closes the rows.
func scanGrants(ctx context.Context, q Querier, what, sql string, args ...any) ([]Grant, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	defer rows.Close()

	var page []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: %s: %w", what, err)
		}
		page = append(page, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	return page, nil
}

// Create writes one grant.
func (shares) Create(ctx context.Context, q Querier, g Grant) (Grant, error) {
	err := q.QueryRow(ctx, `
		INSERT INTO shares (owner, path_prefix, grantee_kind, grantee, permission,
		                    token, status, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+grantColumns,
		g.Owner, g.PathPrefix, g.GranteeKind, null(g.Grantee), g.Permission,
		null(g.Token), StatusActive, g.CreatedBy, g.ExpiresAt).Scan(
		&g.ID, &g.Owner, &g.PathPrefix, &g.GranteeKind, &g.Grantee,
		&g.Permission, &g.Token, &g.Status, &g.ExpiresAt, &g.CreatedBy, &g.CreatedAt)
	if err != nil {
		return Grant{}, classify(fmt.Sprintf("grant %q on %q of %q", g.Permission, g.PathPrefix, g.Owner), err)
	}
	return g, nil
}

// Get reads one grant by its id.
func (shares) Get(ctx context.Context, q Querier, id string) (Grant, error) {
	g, err := scanGrant(q.QueryRow(ctx, `SELECT `+grantColumns+` FROM shares WHERE id = $1`, id))
	if err != nil {
		return Grant{}, fmt.Errorf("store: read the grant %q: %w", id, err)
	}
	return g, nil
}

// Revoke sets one grant's status to revoked.
func (shares) Revoke(ctx context.Context, q Querier, id string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE shares SET status = $2 WHERE id = $1`, id, StatusRevoked)
	if err != nil {
		return false, classify(fmt.Sprintf("revoke the grant %q", id), err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListSpace answers one page of a space's grants.
//
// The page is keyset paginated on the id, which is the ordered column, so an
// insert during the walk shifts nothing. A prefix narrows the page to one
// subtree by exact match rather than by coverage: a caller listing the
// grants on a path asked about that path's own grants.
//
// The order and the cursor are both the id as text. A cursor is opaque text
// a client hands back, so the comparison that resumes a walk and the order
// that produced it have to be one comparison; ordering by the uuid and
// resuming by its text would be two, and they agree only under a collation
// nobody here chose.
func (shares) ListSpace(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]Grant, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the grants of %q: the page holds %d rows", owner, limit)
	}
	return scanGrants(ctx, q, fmt.Sprintf("list the grants of %q", owner), `
		SELECT `+grantColumns+`
		  FROM shares
		 WHERE owner = $1 AND status = $2 AND ($3 = '' OR path_prefix = $3) AND id::text > $4
		 ORDER BY id::text
		 LIMIT $5`, owner, StatusActive, prefix, cursor, limit)
}

// ListGrantee answers one page of the grants whose grantee is the subject.
func (shares) ListGrantee(ctx context.Context, q Querier, grantee, cursor string, limit int) ([]Grant, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list what is shared with %q: the page holds %d rows", grantee, limit)
	}
	return scanGrants(ctx, q, fmt.Sprintf("list what is shared with %q", grantee), `
		SELECT `+grantColumns+`
		  FROM shares
		 WHERE grantee_kind = $1 AND grantee = $2 AND status = $3
		   AND (expires_at IS NULL OR expires_at > now())
		   AND id::text > $4
		 ORDER BY id::text
		 LIMIT $5`, GranteeSubject, grantee, StatusActive, cursor, limit)
}

// Covering answers the grants on a space whose prefix covers path.
//
// One indexed statement fetches them: the prefixes of path are computed
// here and matched exactly, the filter on the status and the expiry is in
// the statement, and the order is the ladder, so the caller reads the
// highest permission first and stops.
func (shares) Covering(ctx context.Context, q Querier, owner, path string) ([]Grant, error) {
	prefixes := PrefixesOf(path)
	if len(prefixes) == 0 {
		return nil, nil
	}
	return scanGrants(ctx, q, fmt.Sprintf("the grants of %q covering %q", owner, path), `
		SELECT `+grantColumns+`
		  FROM shares
		 WHERE owner = $1 AND path_prefix = ANY($2) AND status = $3
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY CASE permission WHEN 'manage' THEN 3 WHEN 'write' THEN 2 ELSE 1 END DESC, id`,
		owner, prefixes, StatusActive)
}

// PrefixesOf lists the prefixes a grant may carry that cover path: the path
// itself and every ancestor of it, longest first. It is the segment rule of
// spec 008 written as a list, which is what makes the covering query an
// equality: files/reports/q3.pdf is covered by files/reports and by files,
// and files/reports-archive is not covered by files/reports because it is
// not in the list.
func PrefixesOf(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	out := []string{path}
	for {
		cut := strings.LastIndex(path, "/")
		if cut <= 0 {
			return out
		}
		path = path[:cut]
		out = append(out, path)
	}
}

// null renders an empty string as the SQL NULL the column holds for an
// absent grantee and an absent token. A token is unique, so every subject
// grant writing an empty string would collide on the second one.
func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
