// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"

	"latere.ai/x/arca/object"
)

// The statements spec 032's deletion of a whole space is made of.
//
// Every statement names the space in its WHERE clause and nothing wider: the
// owner column, or the grantee and subject columns for what the space's owner
// holds on other spaces. The rows are never listed first and deleted by a
// second filter, so a space that gains a row between the two cannot lose
// someone else's.

// SpaceErased is what one erasure removed, one count per kind, and the
// objects whose rows it removed.
type SpaceErased struct {
	// Files is the live paths, Trashed the paths in the trash, and Versions
	// the superseded contents of both.
	Files, Trashed, Versions int64
	// Stars is the stars on the space's paths, whoever placed them.
	Stars int64
	// Shares is the grants to subjects on the space, and Links the grants
	// that are a token: links and public links.
	Shares, Links int64
	// StarsHeld and SharesHeld are what the space's owner holds on other
	// spaces, as the one who starred and as the grantee.
	StarsHeld, SharesHeld int64
	// Uploads is the open upload sessions.
	Uploads int64
	// Events is the log rows, the record of the deletion itself excepted.
	Events int64
	// Usage and Subject are 1 when the ledger row and the directory entry
	// existed, and 0 otherwise.
	Usage, Subject int64
	// Objects is the distinct objects the removed files and versions named.
	// Whether their bytes may go is the caller's to decide against the rows
	// that remain.
	Objects []object.ID
}

// Total is every row removed, which is zero for a space that held nothing.
func (e SpaceErased) Total() int64 {
	return e.Files + e.Trashed + e.Versions + e.Stars + e.Shares + e.Links +
		e.StarsHeld + e.SharesHeld + e.Uploads + e.Events + e.Usage + e.Subject
}

// OpenUpload is one multipart a space has open: the object it will become
// and the upload id the bucket knows it by.
type OpenUpload struct {
	ObjectID object.ID
	UploadID string
}

// Erasure is the query set of spec 032.
type Erasure interface {
	// OpenUploads reads the multiparts a space has open. The session row is
	// the only durable pointer to a multipart's parts, so a caller aborts
	// them at the bucket before the rows go.
	OpenUploads(ctx context.Context, q Querier, owner string) ([]OpenUpload, error)
	// Erase deletes everything the space holds, in a transaction the caller
	// owns, and answers what it removed. The log rows go last, so a caller
	// appends the record of the deletion after them. Rows of the log whose
	// action is keep stay.
	Erase(ctx context.Context, q Querier, owner, keep string) (SpaceErased, error)
}

// erasure is the query set over Postgres.
type erasure struct{}

// NewErasure answers the query set over Postgres.
func NewErasure() Erasure { return erasure{} }

// OpenUploads reads the multiparts a space has open.
func (erasure) OpenUploads(ctx context.Context, q Querier, owner string) ([]OpenUpload, error) {
	rows, err := q.Query(ctx, `SELECT object_id, upload_id FROM upload_sessions WHERE owner = $1`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: read the open uploads of %q: %w", owner, err)
	}
	defer rows.Close()
	var out []OpenUpload
	for rows.Next() {
		var u OpenUpload
		if err := rows.Scan(&u.ObjectID, &u.UploadID); err != nil {
			return nil, fmt.Errorf("store: read the open uploads of %q: %w", owner, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the open uploads of %q: %w", owner, err)
	}
	return out, nil
}

// Erase deletes everything the space holds.
func (erasure) Erase(ctx context.Context, q Querier, owner, keep string) (SpaceErased, error) {
	var out SpaceErased
	seen := map[object.ID]bool{}
	note := func(id object.ID) {
		if !seen[id] {
			seen[id] = true
			out.Objects = append(out.Objects, id)
		}
	}
	fail := func(what string, err error) (SpaceErased, error) {
		return SpaceErased{}, fmt.Errorf("store: erase the %s of %q: %w", what, owner, err)
	}

	rows, err := q.Query(ctx,
		`DELETE FROM files WHERE owner = $1 RETURNING object_id, deleted_at IS NOT NULL`, owner)
	if err != nil {
		return fail("files", err)
	}
	for rows.Next() {
		var (
			id      object.ID
			trashed bool
		)
		if err := rows.Scan(&id, &trashed); err != nil {
			rows.Close()
			return fail("files", err)
		}
		note(id)
		if trashed {
			out.Trashed++
		} else {
			out.Files++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("files", err)
	}

	rows, err = q.Query(ctx, `DELETE FROM file_versions WHERE owner = $1 RETURNING object_id`, owner)
	if err != nil {
		return fail("versions", err)
	}
	for rows.Next() {
		var id object.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fail("versions", err)
		}
		note(id)
		out.Versions++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("versions", err)
	}

	rows, err = q.Query(ctx, `DELETE FROM shares WHERE owner = $1 RETURNING grantee_kind`, owner)
	if err != nil {
		return fail("shares", err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			return fail("shares", err)
		}
		if kind == "subject" {
			out.Shares++
		} else {
			out.Links++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail("shares", err)
	}

	// What the space's owner holds on other spaces comes after the space's
	// own rows, so a grant or a star on one's own space is counted once.
	for _, step := range []struct {
		what string
		sql  string
		into *int64
	}{
		{"stars", `DELETE FROM stars WHERE owner = $1`, &out.Stars},
		{"held stars", `DELETE FROM stars WHERE subject = $1`, &out.StarsHeld},
		{"held shares", `DELETE FROM shares WHERE grantee = $1`, &out.SharesHeld},
		{"upload sessions", `DELETE FROM upload_sessions WHERE owner = $1`, &out.Uploads},
		{"usage", `DELETE FROM space_usage WHERE owner = $1`, &out.Usage},
		{"directory entry", `DELETE FROM subjects WHERE subject = $1`, &out.Subject},
	} {
		tag, err := q.Exec(ctx, step.sql, owner)
		if err != nil {
			return fail(step.what, err)
		}
		*step.into = tag.RowsAffected()
	}

	tag, err := q.Exec(ctx, `DELETE FROM events WHERE owner = $1 AND action <> $2`, owner, keep)
	if err != nil {
		return fail("events", err)
	}
	out.Events = tag.RowsAffected()
	return out, nil
}
