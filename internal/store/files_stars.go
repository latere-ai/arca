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
	// they point at, ordered by space and path.
	List(ctx context.Context, q Querier, subject string, cursor StarCursor, limit int) ([]Star, error)
	// Move carries a path's stars to a new path, inside the transaction that
	// moves the file, so a bookmark follows what it bookmarked.
	Move(ctx context.Context, q Querier, owner, from, to string) (int64, error)
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
func (stars) List(ctx context.Context, q Querier, subject string, cursor StarCursor, limit int) ([]Star, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the stars of %q: the page holds %d rows", subject, limit)
	}
	rows, err := q.Query(ctx, `
		SELECT s.subject, s.owner, s.path, s.created_at,
		       f.content_type, f.size_bytes, f.checksum, f.checksum_kind, f.is_public, f.updated_at
		  FROM stars s
		  JOIN files f ON f.owner = s.owner AND f.path = s.path AND f.deleted_at IS NULL
		 WHERE s.subject = $1 AND (s.owner, s.path) > ($2, $3)
		 ORDER BY s.owner, s.path
		 LIMIT $4`, subject, cursor.Owner, cursor.Path, limit)
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
