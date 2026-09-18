// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

// File is one live or trashed path in a space. The bytes are not here: the
// object id is, and spec 003 derives the key from it, so a move touches no
// bucket key.
type File struct {
	// ID is the row's own identifier.
	ID string
	// Owner is the space, the subject <issuer>|<sub>.
	Owner string
	// Path is plane rooted: files/… or workspaces/….
	Path string
	// ObjectID names the bytes.
	ObjectID object.ID
	// CreatedBy is the subject that first wrote the path.
	CreatedBy string
	// ContentType is what a read answers.
	ContentType string
	// SizeBytes is the object's length.
	SizeBytes int64
	// Checksum is the digest or the store's label, and ChecksumKind says
	// which.
	Checksum     string
	ChecksumKind object.ChecksumKind
	// IsPublic is set by spec 008 and never inferred from a path.
	IsPublic bool
	// DeletedAt is the trash: nil is live.
	DeletedAt *time.Time
	// CreatedAt and UpdatedAt are the row's own times.
	CreatedAt, UpdatedAt time.Time
}

// Files is the query set over one space's paths. A handler takes the
// interface, so a test passes a fake; NewFiles answers the one over
// Postgres.
type Files interface {
	// Get reads one path, live or trashed. A path that is not there is
	// pgx.ErrNoRows and nothing else.
	Get(ctx context.Context, q Querier, owner, path string) (File, error)
	// Insert writes a path that does not exist yet, answering false when
	// the path is already taken rather than overwriting it.
	Insert(ctx context.Context, q Querier, f File) (created bool, err error)
	// UpdateIfChecksum replaces the content of a live path when its
	// checksum is still the one the caller read, which is the compare and
	// swap a conditional write answers with (spec 005).
	UpdateIfChecksum(ctx context.Context, q Querier, f File, ifMatch string) (bool, error)
	// Move renames a live path, answering false when there is nothing to
	// move and ErrConflict when the destination is taken.
	Move(ctx context.Context, q Querier, owner, from, to string) (bool, error)
	// ListPrefix answers one page of live paths under a prefix, ordered by
	// path, with the cursor the next page starts after.
	ListPrefix(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]File, string, error)

	// The write arms, the two deletes and the trash of spec 005, in
	// files_write.go and files_trash.go, where the comment on each says what
	// it is for.
	//
	// Insert and UpdateIfChecksum stay beside them. Insert writes a path
	// nothing holds at all, which is what spec 004 built first; CreateOnly
	// is the arm If-None-Match: * reaches, which also revives a trashed
	// path, so the two are not one statement.
	GetForUpdate(ctx context.Context, q Querier, owner, path string) (File, error)
	Upsert(ctx context.Context, q Querier, f File) error
	CreateOnly(ctx context.Context, q Querier, f File) (created bool, err error)
	SoftDelete(ctx context.Context, q Querier, owner, path string) (bool, error)
	HardDelete(ctx context.Context, q Querier, owner, path string) (File, bool, error)
	Restore(ctx context.Context, q Querier, owner, path string, since time.Time) (bool, error)
	ListTrash(ctx context.Context, q Querier, owner string, cursor TrashCursor, limit int, since time.Time) ([]File, error)
	PurgeTrash(ctx context.Context, q Querier, owner, path string) ([]File, error)
}

// files is the query set over Postgres.
type files struct{}

// NewFiles answers the query set over Postgres.
func NewFiles() Files { return files{} }

// fileColumns is the column list every read of a file shares, so one scan
// function serves them all.
const fileColumns = `id, owner, path, object_id, created_by, content_type, size_bytes,
	checksum, checksum_kind, is_public, deleted_at, created_at, updated_at`

// scanFile reads one row of fileColumns.
func scanFile(row pgx.Row) (File, error) {
	var f File
	err := row.Scan(&f.ID, &f.Owner, &f.Path, &f.ObjectID, &f.CreatedBy, &f.ContentType,
		&f.SizeBytes, &f.Checksum, &f.ChecksumKind, &f.IsPublic, &f.DeletedAt, &f.CreatedAt, &f.UpdatedAt)
	return f, err
}

// Get reads one path.
func (files) Get(ctx context.Context, q Querier, owner, path string) (File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM files WHERE owner = $1 AND path = $2`, owner, path))
	if err != nil {
		return File{}, fmt.Errorf("store: read %q of %q: %w", path, owner, err)
	}
	return f, nil
}

// Insert writes a path that does not exist yet.
func (files) Insert(ctx context.Context, q Querier, f File) (bool, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO files (owner, path, object_id, created_by, content_type, size_bytes,
		                   checksum, checksum_kind, is_public)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (owner, path) DO NOTHING
		RETURNING id`,
		f.Owner, f.Path, f.ObjectID, f.CreatedBy, f.ContentType, f.SizeBytes,
		f.Checksum, f.ChecksumKind, f.IsPublic).Scan(&id)
	switch {
	case missing(err):
		// The path is taken. The caller writes a version or refuses; it
		// does not overwrite, because the bytes of the two writes are two
		// objects.
		return false, nil
	case err != nil:
		return false, classify(fmt.Sprintf("write %q of %q", f.Path, f.Owner), err)
	default:
		return true, nil
	}
}

// UpdateIfChecksum replaces the content of a live path when its checksum is
// still the one the caller read. The condition is in the statement, so a
// lost race is a false and never an overwrite.
func (files) UpdateIfChecksum(ctx context.Context, q Querier, f File, ifMatch string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE files
		   SET object_id = $3, content_type = $4, size_bytes = $5, checksum = $6,
		       checksum_kind = $7, updated_at = now()
		 WHERE owner = $1 AND path = $2 AND checksum = $8 AND deleted_at IS NULL`,
		f.Owner, f.Path, f.ObjectID, f.ContentType, f.SizeBytes, f.Checksum, f.ChecksumKind, ifMatch)
	if err != nil {
		return false, classify(fmt.Sprintf("replace %q of %q", f.Path, f.Owner), err)
	}
	return tag.RowsAffected() == 1, nil
}

// Move renames a live path.
func (files) Move(ctx context.Context, q Querier, owner, from, to string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE files SET path = $3, updated_at = now()
		 WHERE owner = $1 AND path = $2 AND deleted_at IS NULL`, owner, from, to)
	if err != nil {
		return false, classify(fmt.Sprintf("move %q of %q", from, owner), err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListPrefix answers one page of live paths under a prefix. The page is
// keyset paginated on the ordered column: the cursor is the last path of the
// page before, so an insert during the walk shifts nothing.
func (files) ListPrefix(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]File, string, error) {
	if limit <= 0 {
		return nil, "", fmt.Errorf("store: list %q of %q: the page holds %d rows", prefix, owner, limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+fileColumns+`
		  FROM files
		 WHERE owner = $1 AND path LIKE $2 ESCAPE '\' AND path > $3 AND deleted_at IS NULL
		 ORDER BY path
		 LIMIT $4`, owner, likePrefix(prefix), cursor, limit)
	if err != nil {
		return nil, "", fmt.Errorf("store: list %q of %q: %w", prefix, owner, err)
	}
	defer rows.Close()

	page := make([]File, 0, limit)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: list %q of %q: %w", prefix, owner, err)
		}
		page = append(page, f)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list %q of %q: %w", prefix, owner, err)
	}
	next := ""
	if len(page) == limit {
		next = page[len(page)-1].Path
	}
	return page, next, nil
}

// likePrefix renders a prefix as a LIKE pattern, so a path holding a percent
// or an underscore matches itself and not everything.
func likePrefix(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return escaped + "%"
}
