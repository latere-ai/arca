// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// The trash of spec 005. A soft delete leaves the row in place with
// deleted_at set, so the bytes stay addressable and the path stays
// restorable for ARCA_TRASH_RETENTION. The window is a parameter of every
// statement here rather than a constant, so a test moves the deadline
// without moving the clock.

// TrashCursor is where a trash listing resumes: the deleted_at and the path
// of the last entry of the page before. The listing is newest first, so the
// pair is compared as a row and not as two conditions.
type TrashCursor struct {
	DeletedAt time.Time
	Path      string
}

// Restore clears deleted_at, returning the path to its space. It answers
// false when no trashed row inside the window holds the path, which is a
// path that was never trashed, one a live row has taken back, and one the
// reaper is about to purge.
func (files) Restore(ctx context.Context, q Querier, owner, path string, since time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE files SET deleted_at = NULL, updated_at = now()
		 WHERE owner = $1 AND path = $2 AND deleted_at IS NOT NULL AND deleted_at > $3`,
		owner, path, since)
	if err != nil {
		return false, classify(fmt.Sprintf("restore %q of %q", path, owner), err)
	}
	return tag.RowsAffected() == 1, nil
}

// RestoreByID clears deleted_at on the one trashed row of a space the id
// names, and answers the row it restored.
//
// It is the id-addressed arm of the restore, which the administrative route
// of spec 012 reaches: that route takes an id rather than a path, because one
// id names either a trashed object or a soft deleted workspace and the answer
// says which it was. The condition is the same as Restore's, expressed once
// per address: the row is the space's, it is trashed, and it is inside the
// window.
//
// The restore and the read are one statement. A read followed by a write
// would let a purge land between them and restore a row that is already
// gone, and RETURNING is what makes the caller's answer the row the database
// actually changed.
//
// A string that is not an identifier at all names no row, which is the same
// answer a wrong id gets (spec 001, invariant 6).
func (files) RestoreByID(ctx context.Context, q Querier, owner, id string, since time.Time) (File, bool, error) {
	f, err := scanFile(q.QueryRow(ctx, `
		UPDATE files SET deleted_at = NULL, updated_at = now()
		 WHERE owner = $1 AND id = $2 AND deleted_at IS NOT NULL AND deleted_at > $3
		RETURNING `+fileColumns, owner, id, since))
	switch err = noSuchID(err); {
	case missing(err):
		return File{}, false, nil
	case err != nil:
		return File{}, false, classify(fmt.Sprintf("restore %q of %q", id, owner), err)
	default:
		return f, true, nil
	}
}

// ListTrash answers one page of a space's trash, newest first, of the rows
// still inside the retention window. A row past it is the reaper's and is
// not offered to a caller that could not restore it.
//
// The page is keyset paginated on the pair the order is by, so an entry
// trashed during the walk does not shift the page a caller is reading.
func (files) ListTrash(ctx context.Context, q Querier, owner string, cursor TrashCursor, limit int, since time.Time) ([]File, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the trash of %q: the page holds %d rows", owner, limit)
	}
	// The zero cursor is the first page. A time far in the future is above
	// every row, so one statement serves both.
	after := cursor.DeletedAt
	if after.IsZero() {
		after = time.Unix(1<<40, 0).UTC()
	}
	rows, err := q.Query(ctx, `
		SELECT `+fileColumns+`
		  FROM files
		 WHERE owner = $1 AND deleted_at IS NOT NULL AND deleted_at > $2
		   AND (deleted_at, path) < ($3, $4)
		 ORDER BY deleted_at DESC, path DESC
		 LIMIT $5`, owner, since, after, cursor.Path, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list the trash of %q: %w", owner, err)
	}
	defer rows.Close()

	page := make([]File, 0, limit)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list the trash of %q: %w", owner, err)
		}
		page = append(page, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the trash of %q: %w", owner, err)
	}
	return page, nil
}

// PurgeTrash removes trashed rows now and answers what it removed, so the
// caller drops their bytes after the commit. An empty path empties the
// space's trash; a path names one entry.
//
// Rows past the window go too. They are the reaper's to collect, and a
// caller emptying its trash is asking for everything in it to be gone rather
// than for a subset the retention rule happens to still offer.
func (files) PurgeTrash(ctx context.Context, q Querier, owner, path string) ([]File, error) {
	rows, err := q.Query(ctx, `
		DELETE FROM files
		 WHERE owner = $1 AND deleted_at IS NOT NULL AND ($2 = '' OR path = $2)
		RETURNING `+fileColumns, owner, path)
	if err != nil {
		return nil, fmt.Errorf("store: purge the trash of %q: %w", owner, err)
	}
	defer rows.Close()

	var purged []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: purge the trash of %q: %w", owner, err)
		}
		purged = append(purged, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: purge the trash of %q: %w", owner, err)
	}
	return purged, nil
}
