// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
)

// The write arms of spec 005's put, and the two deletes. Each is one
// statement, and which one a handler reaches is decided by the precondition
// the request carried: none, If-None-Match: *, or If-Match.
//
// A path holds at most one row, live or trashed, because the unique
// constraint of spec 004 covers both. A write to a trashed path therefore
// revives that row rather than inserting beside it, which is what keeps a
// revived path's version history attached to it.

// GetForUpdate reads a path inside a transaction and holds the row until the
// transaction ends. It is the first statement of every write: the row it
// answers is the pre-image the version capture, the ledger delta, and the
// precondition are all read from, and the lock is what stops two writers of
// one path from computing one version number.
//
// A path with no row locks nothing and answers pgx.ErrNoRows, which is not
// an error to a caller creating the path.
func (files) GetForUpdate(ctx context.Context, q Querier, owner, path string) (File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM files WHERE owner = $1 AND path = $2 FOR UPDATE`, owner, path))
	if err != nil {
		return File{}, fmt.Errorf("store: hold %q of %q: %w", path, owner, err)
	}
	return f, nil
}

// Upsert writes the content of a path, whatever is there, which is the arm a
// request with no precondition reaches. A trashed row is revived in place.
//
// Two columns the statement does not touch on an overwrite: created_by stays
// with the subject that first wrote the path, and is_public stays as spec
// 008 set it, because publicity is a decision about the path and not a
// property of the bytes at it.
func (files) Upsert(ctx context.Context, q Querier, f File) error {
	_, err := q.Exec(ctx, `
		INSERT INTO files (owner, path, object_id, created_by, content_type, size_bytes,
		                   checksum, checksum_kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner, path) DO UPDATE
		   SET object_id = $3, content_type = $5, size_bytes = $6, checksum = $7,
		       checksum_kind = $8, deleted_at = NULL, updated_at = now()`,
		f.Owner, f.Path, f.ObjectID, f.CreatedBy, f.ContentType, f.SizeBytes,
		f.Checksum, f.ChecksumKind)
	if err != nil {
		return classify(fmt.Sprintf("write %q of %q", f.Path, f.Owner), err)
	}
	return nil
}

// CreateOnly writes a path that no live row holds, which is the arm
// If-None-Match: * reaches. It answers false when a live row holds the path.
//
// The conflict arm is scoped to trashed rows, so a create-only write still
// revives a trashed path and a live row matches nothing: exactly one of two
// racing creators commits and the other reads its refusal from the row count
// rather than from a pre-read it could have lost since.
func (files) CreateOnly(ctx context.Context, q Querier, f File) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO files (owner, path, object_id, created_by, content_type, size_bytes,
		                   checksum, checksum_kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner, path) DO UPDATE
		   SET object_id = $3, content_type = $5, size_bytes = $6, checksum = $7,
		       checksum_kind = $8, deleted_at = NULL, updated_at = now()
		 WHERE files.deleted_at IS NOT NULL`,
		f.Owner, f.Path, f.ObjectID, f.CreatedBy, f.ContentType, f.SizeBytes,
		f.Checksum, f.ChecksumKind)
	if err != nil {
		return false, classify(fmt.Sprintf("create %q of %q", f.Path, f.Owner), err)
	}
	return tag.RowsAffected() == 1, nil
}

// SoftDelete puts a live path in the trash: the row leaves every listing,
// read, share and link, and the bytes stay where they are. It answers false
// when there is no live row to trash.
func (files) SoftDelete(ctx context.Context, q Querier, owner, path string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE files SET deleted_at = now(), updated_at = now()
		 WHERE owner = $1 AND path = $2 AND deleted_at IS NULL`, owner, path)
	if err != nil {
		return false, classify(fmt.Sprintf("trash %q of %q", path, owner), err)
	}
	return tag.RowsAffected() == 1, nil
}

// HardDelete removes the row of a path, live or trashed, and answers what it
// removed so the caller can drop the bytes afterwards. Rows go before bytes
// (spec 001, invariant 1), so a failure between the two leaves bytes the
// reaper finds and never a row without them.
func (files) HardDelete(ctx context.Context, q Querier, owner, path string) (File, bool, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`DELETE FROM files WHERE owner = $1 AND path = $2 RETURNING `+fileColumns, owner, path))
	switch {
	case missing(err):
		return File{}, false, nil
	case err != nil:
		return File{}, false, fmt.Errorf("store: remove %q of %q: %w", path, owner, err)
	default:
		return f, true, nil
	}
}
