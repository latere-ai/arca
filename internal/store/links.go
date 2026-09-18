// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
)

// The token half of the grants table, and the two reads of the files table a
// link's own routes need. A link is a grant with a token instead of a
// grantee, so these are queries over the table of shares.go against the same
// columns; they are here because the three routes of spec 008 that redeem a
// token are the only callers.
//
// Every read of a token filters the status and the expiry in the statement.
// An unknown token, a revoked one, and an expired one are therefore one
// answer, pgx.ErrNoRows, and the handler above answers not_found for all
// three without learning which it was.

// liveToken is the condition every token read carries: a token grant of a
// space, active, and not past its expiry.
const liveToken = `grantee_kind IN ('link', 'public') AND status = 'active'
	AND (expires_at IS NULL OR expires_at > now())`

// ByToken resolves a token to its grant.
func (shares) ByToken(ctx context.Context, q Querier, token string) (Grant, error) {
	g, err := scanGrant(q.QueryRow(ctx,
		`SELECT `+grantColumns+` FROM shares WHERE token = $1 AND `+liveToken, token))
	if err != nil {
		// The token is not named: it is a bearer secret, and spec 015 keeps
		// it out of every log line, this one included.
		return Grant{}, fmt.Errorf("store: resolve a link token: %w", err)
	}
	return g, nil
}

// Live reads one live token grant of a space by its id. It is what the link
// step of spec 006's flowchart asks: the grant a redemption already resolved
// is still live and still belongs to the space the question names.
func (shares) Live(ctx context.Context, q Querier, id, owner string) (Grant, error) {
	g, err := scanGrant(q.QueryRow(ctx,
		`SELECT `+grantColumns+` FROM shares WHERE id = $1 AND owner = $2 AND `+liveToken, id, owner))
	if err != nil {
		return Grant{}, fmt.Errorf("store: read the link %q of %q: %w", id, owner, err)
	}
	return g, nil
}

// ListTokens answers one page of a space's token grants. The token column is
// read like every other, and the caller drops it: a token is answered once,
// at creation, and never listed (spec 015).
func (shares) ListTokens(ctx context.Context, q Querier, owner, cursor string, limit int) ([]Grant, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the links of %q: the page holds %d rows", owner, limit)
	}
	return scanGrants(ctx, q, fmt.Sprintf("list the links of %q", owner), `
		SELECT `+grantColumns+`
		  FROM shares
		 WHERE owner = $1 AND `+liveToken+` AND id::text > $2
		 ORDER BY id::text
		 LIMIT $3`, owner, cursor, limit)
}

// CountLinks answers the live token grants each space named holds.
//
// It reads liveToken, the same condition every other token query carries, so
// the number the overview of spec 012 reports and the rows GET
// /v1/shares/links lists are one predicate read twice. A count with a
// predicate of its own would drift from the listing the first time either
// changed.
//
// A space holding none is left out rather than answered as zero: the caller
// reads a map and a missing key is zero, and a row per space that has no
// link is a row that says nothing.
func (shares) CountLinks(ctx context.Context, q Querier, owners []string) (map[string]int64, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT owner, COUNT(*)
		  FROM shares
		 WHERE owner = ANY($1) AND `+liveToken+`
		 GROUP BY owner`, owners)
	if err != nil {
		return nil, fmt.Errorf("store: count the links of %d spaces: %w", len(owners), err)
	}
	defer rows.Close()

	counts := make(map[string]int64, len(owners))
	for rows.Next() {
		var owner string
		var n int64
		if err := rows.Scan(&owner, &n); err != nil {
			return nil, fmt.Errorf("store: count the links of %d spaces: %w", len(owners), err)
		}
		counts[owner] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count the links of %d spaces: %w", len(owners), err)
	}
	return counts, nil
}

// Subtree answers one page of the live paths a grant's prefix covers: the
// path itself, where the prefix names one object, and everything under it.
//
// The slash in the pattern is what keeps the match on segments: a grant on
// files/reports lists files/reports and files/reports/q3.pdf and never
// files/reports-archive.
func (shares) Subtree(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]File, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list %q of %q: the page holds %d rows", prefix, owner, limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+fileColumns+`
		  FROM files
		 WHERE owner = $1 AND (path = $2 OR path LIKE $3 ESCAPE '\')
		   AND path > $4 AND deleted_at IS NULL
		 ORDER BY path
		 LIMIT $5`, owner, prefix, likePrefix(prefix+"/"), cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list %q of %q: %w", prefix, owner, err)
	}
	defer rows.Close()

	var page []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list %q of %q: %w", prefix, owner, err)
		}
		page = append(page, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list %q of %q: %w", prefix, owner, err)
	}
	return page, nil
}

// MarkPublic stamps or clears the public flag on one live path and answers
// the row it wrote, so the caller has the object id the bucket key derives
// from. A path that is not there is pgx.ErrNoRows.
//
// Publicity is a property of the object recorded in its row, derived from a
// grant and never from the path. The service Arca replaces read files/avatar
// and files/public/** as public by name; there is no such convention here
// (spec 008).
func (shares) MarkPublic(ctx context.Context, q Querier, owner, path string, public bool) (File, error) {
	f, err := scanFile(q.QueryRow(ctx, `
		UPDATE files SET is_public = $3, updated_at = now()
		 WHERE owner = $1 AND path = $2 AND deleted_at IS NULL
		 RETURNING `+fileColumns, owner, path, public))
	if err != nil {
		return File{}, fmt.Errorf("store: mark %q of %q public=%t: %w", path, owner, public, err)
	}
	return f, nil
}
