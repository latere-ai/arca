// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"

	"latere.ai/x/arca/object"
)

// The file-plane half a workspace needs. Files under a workspace root are
// ordinary objects of spec 005: they are listed, trashed, charged and shared
// by the same code paths every other object is. What is here is the four
// statements a workspace asks of them as a subtree rather than as a path,
// which no per-path query answers in one round trip: the manifest, the
// counters, the rename, and the drop a sync reconciles.
//
// The rename is the reason this is a subtree statement and not a loop. A
// move touches no bucket key, which is invariant 8 of spec 001: the key
// derives from the object id and the row carries the id, so renaming a
// workspace of a hundred thousand files is one UPDATE and zero calls to the
// bucket.

// WorkspaceFile is one live object under a workspace root as the manifest
// and the reconciliation read it. The path is the full plane-rooted path;
// what is relative to the root is the workspace package's rendering.
type WorkspaceFile struct {
	// Path is plane rooted: workspaces/<slug>/….
	Path string
	// Checksum is the digest or the store's label, as the row holds it.
	Checksum string
	// Size is the object's length in bytes.
	Size int64
	// ObjectID names the bytes. Materialize presigns the key this id
	// derives, and never a key recomputed from the path, because an object
	// assembled from parts carries a key the path does not predict.
	ObjectID object.ID
}

// WorkspaceObjects is what a workspace asks of the file plane. It is an
// interface so the handlers of spec 009 take a seam rather than a table:
// spec 005 owns what a put under a workspace root does, and this is the
// narrow half spec 009 reads and reconciles.
type WorkspaceObjects interface {
	// Manifest lists the live objects under a root prefix, ordered by path.
	Manifest(ctx context.Context, q Querier, owner, prefix string) ([]WorkspaceFile, error)
	// Stat counts the live objects under a root prefix and their bytes.
	Stat(ctx context.Context, q Querier, owner, prefix string) (files, bytes int64, err error)
	// MoveSubtree rewrites every path under one prefix to sit under
	// another, trashed rows included, and answers how many moved. It
	// reaches no bucket key.
	MoveSubtree(ctx context.Context, q Querier, owner, from, to string) (int64, error)
	// Drop removes the named live rows and answers the objects they held
	// and the bytes they counted, so the bytes can go after the rows and
	// the usage of spec 010 can be charged for what left.
	Drop(ctx context.Context, q Querier, owner string, paths []string) (freed []object.ID, bytes int64, err error)
	// Unreferenced narrows a set of objects to the ones no row still names,
	// which is the one question deciding whether bytes may be deleted. It
	// is the batched form of ObjectReferenced, because a sync that drops a
	// large tree asks it once and not once per file.
	Unreferenced(ctx context.Context, q Querier, ids []object.ID) ([]object.ID, error)
	// DropSubtree removes every row under a root prefix, the trashed ones
	// and the superseded contents included, and answers the objects they
	// held and the bytes they counted. It is what pass 6 of spec 010 runs
	// before it removes the tombstone itself.
	DropSubtree(ctx context.Context, q Querier, owner, prefix string) (freed []object.ID, bytes int64, err error)
}

// workspaceObjects is the query set over Postgres.
type workspaceObjects struct{}

// NewWorkspaceObjects answers the file-plane query set over Postgres.
func NewWorkspaceObjects() WorkspaceObjects { return workspaceObjects{} }

// Manifest lists the live objects under a root prefix.
func (workspaceObjects) Manifest(ctx context.Context, q Querier, owner, prefix string) ([]WorkspaceFile, error) {
	rows, err := q.Query(ctx, `
		SELECT path, checksum, size_bytes, object_id
		  FROM files
		 WHERE owner = $1 AND path LIKE $2 ESCAPE '\' AND deleted_at IS NULL
		 ORDER BY path`, owner, likePrefix(prefix))
	if err != nil {
		return nil, fmt.Errorf("store: read the manifest of %q: %w", prefix, err)
	}
	defer rows.Close()

	var out []WorkspaceFile
	for rows.Next() {
		var f WorkspaceFile
		if err := rows.Scan(&f.Path, &f.Checksum, &f.Size, &f.ObjectID); err != nil {
			return nil, fmt.Errorf("store: read the manifest of %q: %w", prefix, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the manifest of %q: %w", prefix, err)
	}
	return out, nil
}

// Stat counts the live objects under a root prefix and their bytes.
func (workspaceObjects) Stat(ctx context.Context, q Querier, owner, prefix string) (int64, int64, error) {
	var files, bytes int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		  FROM files
		 WHERE owner = $1 AND path LIKE $2 ESCAPE '\' AND deleted_at IS NULL`,
		owner, likePrefix(prefix)).Scan(&files, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count the objects under %q: %w", prefix, err)
	}
	return files, bytes, nil
}

// MoveSubtree rewrites every path under one prefix to sit under another.
//
// The bookmarks of spec 005 key on the path too, so they move with it or a
// star orphans. The grants of spec 008 key on a path prefix and move with it
// for the same reason; that statement arrives with the table, which this
// build does not carry.
func (workspaceObjects) MoveSubtree(ctx context.Context, q Querier, owner, from, to string) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE files SET path = $4 || substring(path FROM char_length($3) + 1), updated_at = now()
		 WHERE owner = $1 AND path LIKE $2 ESCAPE '\'`, owner, likePrefix(from), from, to)
	if err != nil {
		return 0, classify(fmt.Sprintf("move the objects under %q", from), err)
	}
	moved := tag.RowsAffected()
	if _, err := q.Exec(ctx, `
		UPDATE stars SET path = $4 || substring(path FROM char_length($3) + 1)
		 WHERE owner = $1 AND path LIKE $2 ESCAPE '\'`, owner, likePrefix(from), from, to); err != nil {
		return 0, classify(fmt.Sprintf("move the stars under %q", from), err)
	}
	return moved, nil
}

// Drop removes the named live rows and answers what they held.
func (workspaceObjects) Drop(ctx context.Context, q Querier, owner string, paths []string) ([]object.ID, int64, error) {
	if len(paths) == 0 {
		return nil, 0, nil
	}
	rows, err := q.Query(ctx, `
		DELETE FROM files
		 WHERE owner = $1 AND path = ANY($2) AND deleted_at IS NULL
		 RETURNING object_id, size_bytes`, owner, paths)
	if err != nil {
		return nil, 0, fmt.Errorf("store: drop %d objects of %q: %w", len(paths), owner, err)
	}
	defer rows.Close()

	freed := make([]object.ID, 0, len(paths))
	var bytes int64
	for rows.Next() {
		var id object.ID
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			return nil, 0, fmt.Errorf("store: drop %d objects of %q: %w", len(paths), owner, err)
		}
		freed = append(freed, id)
		bytes += size
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: drop %d objects of %q: %w", len(paths), owner, err)
	}
	return freed, bytes, nil
}

// DropSubtree removes every row under a root prefix and answers what they
// held.
//
// It is Drop's whole-subtree arm, and it differs from Drop in the two ways a
// purge differs from a sync: a sync drops the live paths a client no longer
// declares, and a purge ends the subtree, so a trashed row is taken too and
// nothing is left for a restore that can no longer happen. The superseded
// contents go with it, because the ledger of spec 010 counts files and
// file_versions together and a purge that left the history behind would
// leave bytes charged to a space that holds nothing.
//
// The bytes are the sum over both tables, so the caller releases exactly
// what a recomputation would no longer find.
func (workspaceObjects) DropSubtree(ctx context.Context, q Querier, owner, prefix string) ([]object.ID, int64, error) {
	rows, err := q.Query(ctx, `
		WITH gone AS (
		    DELETE FROM files
		     WHERE owner = $1 AND path LIKE $2 ESCAPE '\'
		    RETURNING object_id, size_bytes
		), history AS (
		    DELETE FROM file_versions
		     WHERE owner = $1 AND path LIKE $2 ESCAPE '\'
		    RETURNING object_id, size_bytes
		)
		SELECT object_id, size_bytes FROM gone
		 UNION ALL
		SELECT object_id, size_bytes FROM history`, owner, likePrefix(prefix))
	if err != nil {
		return nil, 0, fmt.Errorf("store: drop the objects under %q of %q: %w", prefix, owner, err)
	}
	defer rows.Close()

	var (
		freed []object.ID
		bytes int64
	)
	for rows.Next() {
		var id object.ID
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			return nil, 0, fmt.Errorf("store: drop the objects under %q of %q: %w", prefix, owner, err)
		}
		freed = append(freed, id)
		bytes += size
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: drop the objects under %q of %q: %w", prefix, owner, err)
	}
	return freed, bytes, nil
}

// Unreferenced narrows a set of objects to the ones no row still names.
//
// The union is the one ObjectReferenced reads, asked of many ids in one
// statement. It is two tables today and three from spec 007, which creates
// upload_sessions; both readings of the union move together, because bytes
// removed while a row still names them are bytes a reader cannot get back.
func (workspaceObjects) Unreferenced(ctx context.Context, q Querier, ids []object.ID) ([]object.ID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT candidate
		  FROM unnest($1::uuid[]) AS candidate
		 WHERE NOT EXISTS (SELECT 1 FROM files WHERE object_id = candidate)
		   AND NOT EXISTS (SELECT 1 FROM file_versions WHERE object_id = candidate)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: read the references of %d objects: %w", len(ids), err)
	}
	defer rows.Close()

	out := make([]object.ID, 0, len(ids))
	for rows.Next() {
		var id object.ID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read the references of %d objects: %w", len(ids), err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the references of %d objects: %w", len(ids), err)
	}
	return out, nil
}
