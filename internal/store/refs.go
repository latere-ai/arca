// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"

	"latere.ai/x/arca/object"
)

// ObjectReferenced reports whether any row still points at the object id.
// It is the one statement deciding whether bytes may be deleted, so the
// reaper of spec 010, the version prune, and a handler that drops superseded
// bytes cannot drift apart and remove an object something still names.
//
// The union is two tables today and three from spec 007, which creates
// upload_sessions: an incomplete multipart's parts are invisible to a
// listing, so its session row is the only durable pointer to them.
func ObjectReferenced(ctx context.Context, q Querier, id object.ID) (bool, error) {
	var referenced bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM files WHERE object_id = $1)
		    OR EXISTS (SELECT 1 FROM file_versions WHERE object_id = $1)`, id).Scan(&referenced)
	if err != nil {
		return false, fmt.Errorf("store: read the references of %s: %w", id, err)
	}
	return referenced, nil
}
