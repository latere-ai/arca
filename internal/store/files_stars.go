// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// A star is a bookmark belonging to one subject and not a property of the
// file, so it lives in its own table and never appears in another reader's
// view (spec 005). The listing joins live rows, so a star whose target was
// trashed or removed drops out of it at once and the row is pruned later by
// the reaper's eighth pass.

// Star is one bookmark, and the live file it points at when the listing
// joined one.
type Star struct {
	// Subject is who starred.
	Subject string
	// Owner is the space of the starred path, and Path the path.
	Owner, Path string
	// CreatedAt is when the bookmark was made.
	CreatedAt time.Time
	// File is the live row the listing joined, zero on a read that joined
	// none.
	File File
}

// StarCursor is where a star listing resumes: the space and the path of the
// last entry of the page before. A subject stars paths across spaces, so the
// order is by the pair and the cursor is the pair.
type StarCursor struct {
	Owner, Path string
}

// Stars is the query set over one subject's bookmarks.
type Stars interface {
	// Add stars a path. It is idempotent: starring twice is one row.
	Add(ctx context.Context, q Querier, subject, owner, path string) error
	// Remove unstars a path. It is idempotent: unstarring nothing is not an
	// error.
	Remove(ctx context.Context, q Querier, subject, owner, path string) error
	// List answers one page of a subject's stars joined with the live rows
	// they point at, ordered by space and path. Owners narrows the page to
	// stars on those spaces; empty narrows nothing.
	List(ctx context.Context, q Querier, subject string, owners []string, cursor StarCursor, limit int, omit []string) ([]Star, error)
	// Move carries a path's stars to a new path, inside the transaction that
	// moves the file, so a bookmark follows what it bookmarked.
	Move(ctx context.Context, q Querier, owner, from, to string) (int64, error)
	// MoveMany carries the stars of each source path of a folder move to its
	// destination, by Move's rule (spec 030).
	MoveMany(ctx context.Context, q Querier, owner string, moves []Rename) (int64, error)
}

// stars is the query set over Postgres.
type stars struct{}

// NewStars answers the query set over Postgres.
func NewStars() Stars { return stars{} }

// Add stars a path.
func (stars) Add(ctx context.Context, q Querier, subject, owner, path string) error {
	_, err := q.Exec(ctx,
		`INSERT INTO stars (subject, owner, path) VALUES ($1, $2, $3)
		 ON CONFLICT (subject, owner, path) DO NOTHING`, subject, owner, path)
	if err != nil {
		return classify(fmt.Sprintf("star %q of %q", path, owner), err)
	}
	return nil
}

// Remove unstars a path.
func (stars) Remove(ctx context.Context, q Querier, subject, owner, path string) error {
	_, err := q.Exec(ctx,
		`DELETE FROM stars WHERE subject = $1 AND owner = $2 AND path = $3`, subject, owner, path)
	if err != nil {
		return classify(fmt.Sprintf("unstar %q of %q", path, owner), err)
	}
	return nil
}

// List answers one page of a subject's stars, joined with live rows.
//
// The narrowing is in the statement rather than applied to the page it
// returns, so a page is never short because a star outside the owners
// sorted into it. A nil slice binds as NULL, which the coalesce reads as
// the empty list that narrows nothing.
//
// A star whose path is at or under one of omit is left out (limits.omit of
// spec 028). The listing crosses spaces under one answer, so a prefix is
// read against the path of every star on the page, whichever space it is in.
func (stars) List(ctx context.Context, q Querier, subject string, owners []string, cursor StarCursor, limit int, omit []string) ([]Star, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the stars of %q: the page holds %d rows", subject, limit)
	}
	rows, err := q.Query(ctx, `
		SELECT s.subject, s.owner, s.path, s.created_at,
		       f.content_type, f.size_bytes, f.checksum, f.checksum_kind, f.is_public, f.updated_at
		  FROM stars s
		  JOIN files f ON f.owner = s.owner AND f.path = s.path AND f.deleted_at IS NULL
		 WHERE s.subject = $1 AND (s.owner, s.path) > ($2, $3)
		   AND (coalesce(cardinality($5::text[]), 0) = 0 OR s.owner = ANY($5::text[]))
		   AND `+notOmitted("s.path", 6)+`
		 ORDER BY s.owner, s.path
		 LIMIT $4`, subject, cursor.Owner, cursor.Path, limit, owners, omitted(omit))
	if err != nil {
		return nil, fmt.Errorf("store: list the stars of %q: %w", subject, err)
	}
	defer rows.Close()

	page := make([]Star, 0, limit)
	for rows.Next() {
		var s Star
		if err := rows.Scan(&s.Subject, &s.Owner, &s.Path, &s.CreatedAt,
			&s.File.ContentType, &s.File.SizeBytes, &s.File.Checksum, &s.File.ChecksumKind,
			&s.File.IsPublic, &s.File.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list the stars of %q: %w", subject, err)
		}
		s.File.Owner, s.File.Path = s.Owner, s.Path
		page = append(page, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the stars of %q: %w", subject, err)
	}
	return page, nil
}

// Move carries a path's stars to a new path.
//
// A subject that had starred the destination as well would collide, so the
// conflicting row is dropped first: one bookmark per subject and path is the
// table's shape, and a move that renamed one onto the other leaves one.
func (stars) Move(ctx context.Context, q Querier, owner, from, to string) (int64, error) {
	if _, err := q.Exec(ctx, `
		DELETE FROM stars
		 WHERE owner = $1 AND path = $3
		   AND subject IN (SELECT subject FROM stars WHERE owner = $1 AND path = $2)`,
		owner, from, to); err != nil {
		return 0, classify(fmt.Sprintf("move the stars of %q of %q", from, owner), err)
	}
	tag, err := q.Exec(ctx,
		`UPDATE stars SET path = $3 WHERE owner = $1 AND path = $2`, owner, from, to)
	if err != nil {
		return 0, classify(fmt.Sprintf("move the stars of %q of %q", from, owner), err)
	}
	return tag.RowsAffected(), nil
}

// MoveMany carries the stars of many paths to their new paths. A subject
// that had starred a destination as well as its source keeps one star, as
// Move leaves: the one on the destination is dropped first, so the rename
// meets no row of the same subject and path.
func (stars) MoveMany(ctx context.Context, q Querier, owner string, moves []Rename) (int64, error) {
	from, to := ends(moves)
	what := fmt.Sprintf("move the stars of %d paths of %q", len(moves), owner)
	if _, err := q.Exec(ctx, `
		DELETE FROM stars d
		 USING `+renames+`
		 WHERE d.owner = $1 AND d.path = m.dst
		   AND EXISTS (SELECT 1 FROM stars s WHERE s.owner = $1 AND s.path = m.src AND s.subject = d.subject)`,
		owner, from, to); err != nil {
		return 0, classify(what, err)
	}
	tag, err := q.Exec(ctx, `
		UPDATE stars s SET path = m.dst
		  FROM `+renames+`
		 WHERE s.owner = $1 AND s.path = m.src`,
		owner, from, to)
	if err != nil {
		return 0, classify(what, err)
	}
	return tag.RowsAffected(), nil
}
