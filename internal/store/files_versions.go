// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

// Version is a superseded content of a path. The bytes are not copied: the
// row keeps the object id the file carried before the overwrite, and the
// overwrite is already at a fresh id, so capturing a version costs one row
// whatever the file weighs (spec 005).
type Version struct {
	// ID is the row's own identifier.
	ID string
	// Owner is the space, and Path the path this content was at.
	Owner, Path string
	// VersionNo counts from 1 upward, per path.
	VersionNo int
	// ObjectID names the bytes, which are the bytes the file had.
	ObjectID object.ID
	// ContentType is what a read of this version answers.
	ContentType string
	// SizeBytes is the content's length.
	SizeBytes int64
	// Checksum is the digest or the store's label, and ChecksumKind says
	// which.
	Checksum     string
	ChecksumKind object.ChecksumKind
	// CreatedBy is the subject that wrote this content.
	CreatedBy string
	// SupersededAt is when the content stopped being current.
	SupersededAt time.Time
}

// Versions is the query set over the superseded contents of a path. Spec 005
// owns the table; this is the Go half of every statement that reaches it.
type Versions interface {
	// Capture moves the current row's metadata into the history, live or
	// trashed, and answers whether it captured one. ifMatch, when set,
	// conditions the capture on the same checksum the write that follows it
	// is conditioned on, so a lost race captures nothing.
	Capture(ctx context.Context, q Querier, owner, path, ifMatch string) (bool, error)
	// Get reads one version by its number. A version that is not there is
	// pgx.ErrNoRows and nothing else.
	Get(ctx context.Context, q Querier, owner, path string, n int) (Version, error)
	// List answers one page of a path's history, oldest first, keyset
	// paginated on the version number.
	List(ctx context.Context, q Querier, owner, path string, after, limit int) ([]Version, error)
	// Delete removes one version and answers what it removed, so the caller
	// can drop bytes nothing else references.
	Delete(ctx context.Context, q Querier, owner, path string, n int) (Version, bool, error)
	// DeletePath removes every version of a path and answers them, which is
	// what a hard delete and a purge take with them.
	DeletePath(ctx context.Context, q Querier, owner, path string) ([]Version, error)
	// Move carries a path's history to a new path, inside the transaction
	// that moves the file.
	Move(ctx context.Context, q Querier, owner, from, to string) (int64, error)
}

// versions is the query set over Postgres.
type versions struct{}

// NewVersions answers the query set over Postgres.
func NewVersions() Versions { return versions{} }

// versionColumns is the column list every read of a version shares.
const versionColumns = `id, owner, path, version_no, object_id, content_type, size_bytes,
	checksum, checksum_kind, created_by, superseded_at`

// scanVersion reads one row of versionColumns.
func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	err := row.Scan(&v.ID, &v.Owner, &v.Path, &v.VersionNo, &v.ObjectID, &v.ContentType,
		&v.SizeBytes, &v.Checksum, &v.ChecksumKind, &v.CreatedBy, &v.SupersededAt)
	return v, err
}

// Capture moves the current row's metadata into the history.
//
// The version number is the path's highest plus one, read inside the
// statement. Two writers of one path cannot both read the same number,
// because a write holds the file's row with GetForUpdate before it captures.
func (versions) Capture(ctx context.Context, q Querier, owner, path, ifMatch string) (bool, error) {
	statement := `
		INSERT INTO file_versions (owner, path, version_no, object_id, content_type,
		                           size_bytes, checksum, checksum_kind, created_by)
		SELECT f.owner, f.path,
		       COALESCE((SELECT MAX(v.version_no) FROM file_versions v
		                  WHERE v.owner = f.owner AND v.path = f.path), 0) + 1,
		       f.object_id, f.content_type, f.size_bytes, f.checksum, f.checksum_kind, f.created_by
		  FROM files f
		 WHERE f.owner = $1 AND f.path = $2`
	args := []any{owner, path}
	if ifMatch != "" {
		args = append(args, ifMatch)
		statement += ` AND f.checksum = $3`
	}
	tag, err := q.Exec(ctx, statement, args...)
	if err != nil {
		return false, classify(fmt.Sprintf("capture %q of %q", path, owner), err)
	}
	return tag.RowsAffected() == 1, nil
}

// Get reads one version by its number.
func (versions) Get(ctx context.Context, q Querier, owner, path string, n int) (Version, error) {
	v, err := scanVersion(q.QueryRow(ctx,
		`SELECT `+versionColumns+` FROM file_versions
		  WHERE owner = $1 AND path = $2 AND version_no = $3`, owner, path, n))
	if err != nil {
		return Version{}, fmt.Errorf("store: read version %d of %q of %q: %w", n, path, owner, err)
	}
	return v, nil
}

// List answers one page of a path's history, oldest first.
func (versions) List(ctx context.Context, q Querier, owner, path string, after, limit int) ([]Version, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the versions of %q of %q: the page holds %d rows", path, owner, limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+versionColumns+`
		  FROM file_versions
		 WHERE owner = $1 AND path = $2 AND version_no > $3
		 ORDER BY version_no
		 LIMIT $4`, owner, path, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list the versions of %q of %q: %w", path, owner, err)
	}
	defer rows.Close()

	page := make([]Version, 0, limit)
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list the versions of %q of %q: %w", path, owner, err)
		}
		page = append(page, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the versions of %q of %q: %w", path, owner, err)
	}
	return page, nil
}

// Delete removes one version.
func (versions) Delete(ctx context.Context, q Querier, owner, path string, n int) (Version, bool, error) {
	v, err := scanVersion(q.QueryRow(ctx,
		`DELETE FROM file_versions WHERE owner = $1 AND path = $2 AND version_no = $3
		 RETURNING `+versionColumns, owner, path, n))
	switch {
	case missing(err):
		return Version{}, false, nil
	case err != nil:
		return Version{}, false, fmt.Errorf("store: remove version %d of %q of %q: %w", n, path, owner, err)
	default:
		return v, true, nil
	}
}

// DeletePath removes every version of a path.
func (versions) DeletePath(ctx context.Context, q Querier, owner, path string) ([]Version, error) {
	rows, err := q.Query(ctx,
		`DELETE FROM file_versions WHERE owner = $1 AND path = $2 RETURNING `+versionColumns,
		owner, path)
	if err != nil {
		return nil, fmt.Errorf("store: remove the versions of %q of %q: %w", path, owner, err)
	}
	defer rows.Close()

	var removed []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("store: remove the versions of %q of %q: %w", path, owner, err)
		}
		removed = append(removed, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: remove the versions of %q of %q: %w", path, owner, err)
	}
	return removed, nil
}

// Move carries a path's history to a new path.
func (versions) Move(ctx context.Context, q Querier, owner, from, to string) (int64, error) {
	tag, err := q.Exec(ctx,
		`UPDATE file_versions SET path = $3 WHERE owner = $1 AND path = $2`, owner, from, to)
	if err != nil {
		return 0, classify(fmt.Sprintf("move the versions of %q of %q", from, owner), err)
	}
	return tag.RowsAffected(), nil
}
