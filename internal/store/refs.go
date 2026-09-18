// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"

	"latere.ai/x/arca/object"
)

// objectReferencedSQL is the union the sweep of spec 010 deletes bytes
// against. It names every table of the schema that holds an object id, and
// TestObjectReferencedNamesEveryTableThatHoldsAnObjectID holds it there: a
// table added with the column and left out of this statement is bytes
// deleted under a live row.
//
// The third member is upload_sessions, which spec 007 creates. An incomplete
// multipart's parts are invisible to object listing, so the session row is
// the only durable pointer to them. The service Arca replaces left that
// table out of the same check while calling it "the single invariant
// deciding whether a blob may be deleted", and what kept the omission out of
// reach there was the reaper's twenty-four hour grace window rather than the
// invariant.
const objectReferencedSQL = `
	SELECT EXISTS (SELECT 1 FROM files WHERE object_id = $1)
	    OR EXISTS (SELECT 1 FROM file_versions WHERE object_id = $1)
	    OR EXISTS (SELECT 1 FROM upload_sessions WHERE object_id = $1)`

// ObjectReferenced reports whether any row still points at the object id.
// It is the one statement deciding whether bytes may be deleted, so the
// reaper of spec 010, the version prune, and a handler that drops superseded
// bytes cannot drift apart and remove an object something still names.
func ObjectReferenced(ctx context.Context, q Querier, id object.ID) (bool, error) {
	var referenced bool
	if err := q.QueryRow(ctx, objectReferencedSQL, id).Scan(&referenced); err != nil {
		return false, fmt.Errorf("store: read the references of %s: %w", id, err)
	}
	return referenced, nil
}
