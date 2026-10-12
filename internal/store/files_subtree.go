// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"strings"

	"latere.ai/x/arca/object"
)

// The statements spec 028 asks of a folder. A folder is a prefix: Arca
// stores no directory, so what a folder holds is every row whose path begins
// with the folder's path and a slash, and a statement over a folder is one
// statement over those rows at any size.
//
// Every statement here takes the subtrees an authorizer's answer left out
// (limits.omit of spec 028). A row at or under one of them is not read,
// trashed, removed or counted, so the caller acts on what it was allowed to
// enumerate and reports only that.

// under renders a folder as the LIKE pattern of everything inside it and
// never the folder's own path: files/notes covers files/notes/a and neither
// files/notes nor files/notes-archive.
func under(folder string) string { return likePrefix(strings.TrimSuffix(folder, "/") + "/") }

// notOmitted is the predicate that leaves the omitted subtrees out of an
// enumeration: the column is none of the prefixes the text array parameter
// holds and under none of them, matched on whole segments. An empty array
// leaves nothing out.
func notOmitted(column string, parameter int) string {
	return fmt.Sprintf(`NOT EXISTS (
		    SELECT 1 FROM unnest($%d::text[]) AS omitted(prefix)
		     WHERE %s = omitted.prefix OR starts_with(%s, omitted.prefix || '/'))`,
		parameter, column, column)
}

// omitted is the array parameter of notOmitted, never NULL, so the statement
// reads an answer that named no prefix as an empty list.
func omitted(omit []string) []string {
	if omit == nil {
		return []string{}
	}
	return omit
}

// Omitted reports whether path is one of the prefixes or under one of them,
// which is notOmitted's rule read in Go for a caller that holds the path
// already.
func Omitted(omit []string, path string) bool {
	for _, prefix := range omit {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// Subtree is what a statement over a folder changed: the rows, and the bytes
// they counted.
type Subtree struct {
	// Files is how many rows of files the statement took.
	Files int64
	// Bytes is what those rows held. For a drop it includes the superseded
	// contents that went with them, which is what the space's usage gives
	// back.
	Bytes int64
	// Objects are the object ids the rows named, for a drop, so the bytes go
	// after the rows.
	Objects []object.ID
}

// StatSubtree counts the live rows under a folder and their bytes, which is
// the size a copy's question carries before any row is read.
func (files) StatSubtree(ctx context.Context, q Querier, owner, folder string, omit []string) (Subtree, error) {
	var out Subtree
	err := q.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		  FROM files
		 WHERE owner = $1 AND path LIKE $2 ESCAPE '\' AND deleted_at IS NULL
		   AND `+notOmitted("path", 3),
		owner, under(folder), omitted(omit)).Scan(&out.Files, &out.Bytes)
	if err != nil {
		return Subtree{}, fmt.Errorf("store: count the objects under %q of %q: %w", folder, owner, err)
	}
	return out, nil
}

// TrashSubtree puts every live row under a folder in the trash, in one
// statement, and answers how many it trashed and their bytes. Each row is in
// the trash by itself and is restored one at a time.
func (files) TrashSubtree(ctx context.Context, q Querier, owner, folder string, omit []string) (Subtree, error) {
	var out Subtree
	err := q.QueryRow(ctx, `
		WITH trashed AS (
		    UPDATE files SET deleted_at = now(), updated_at = now()
		     WHERE owner = $1 AND path LIKE $2 ESCAPE '\' AND deleted_at IS NULL
		       AND `+notOmitted("path", 3)+`
		    RETURNING size_bytes
		)
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM trashed`,
		owner, under(folder), omitted(omit)).Scan(&out.Files, &out.Bytes)
	if err != nil {
		return Subtree{}, classify(fmt.Sprintf("trash the objects under %q of %q", folder, owner), err)
	}
	return out, nil
}

// DropSubtree removes every row under a folder, live and trashed, with the
// superseded contents of each, and answers what they held.
//
// The history goes with the rows because the ledger of spec 010 counts files
// and file_versions together: a drop that left the history behind would
// leave bytes charged to a space that holds nothing there. The bytes are the
// sum over both tables, so the caller releases exactly what a recomputation
// would no longer find, and the row count is of files alone.
func (files) DropSubtree(ctx context.Context, q Querier, owner, folder string, omit []string) (Subtree, error) {
	rows, err := q.Query(ctx, `
		WITH gone AS (
		    DELETE FROM files
		     WHERE owner = $1 AND path LIKE $2 ESCAPE '\'
		       AND `+notOmitted("path", 3)+`
		    RETURNING object_id, size_bytes
		), history AS (
		    DELETE FROM file_versions
		     WHERE owner = $1 AND path LIKE $2 ESCAPE '\'
		       AND `+notOmitted("path", 3)+`
		    RETURNING object_id, size_bytes
		)
		SELECT object_id, size_bytes, true FROM gone
		 UNION ALL
		SELECT object_id, size_bytes, false FROM history`,
		owner, under(folder), omitted(omit))
	if err != nil {
		return Subtree{}, fmt.Errorf("store: drop the objects under %q of %q: %w", folder, owner, err)
	}
	defer rows.Close()

	var out Subtree
	for rows.Next() {
		var (
			id   object.ID
			size int64
			row  bool
		)
		if err := rows.Scan(&id, &size, &row); err != nil {
			return Subtree{}, fmt.Errorf("store: drop the objects under %q of %q: %w", folder, owner, err)
		}
		out.Objects = append(out.Objects, id)
		out.Bytes += size
		if row {
			out.Files++
		}
	}
	if err := rows.Err(); err != nil {
		return Subtree{}, fmt.Errorf("store: drop the objects under %q of %q: %w", folder, owner, err)
	}
	return out, nil
}

// Unreferenced narrows a set of objects to the ones no row still names,
// which is the one question deciding whether bytes may be deleted. It is the
// batched form of ObjectReferenced and reads the same three tables, because
// a folder delete asks it once per thousand objects and not once per object.
func (files) Unreferenced(ctx context.Context, q Querier, ids []object.ID) ([]object.ID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, unreferencedSQL, ids)
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

// unreferencedSQL is objectReferencedSQL asked of many ids at once. The
// schema test holds both to every table that carries an object id, so the
// two readings of the union move together.
const unreferencedSQL = `
	SELECT candidate
	  FROM unnest($1::uuid[]) AS candidate
	 WHERE NOT EXISTS (SELECT 1 FROM files WHERE object_id = candidate)
	   AND NOT EXISTS (SELECT 1 FROM file_versions WHERE object_id = candidate)
	   AND NOT EXISTS (SELECT 1 FROM upload_sessions WHERE object_id = candidate)`
