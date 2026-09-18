// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/object"
)

// Session is one open multipart upload (spec 007). The row is the only
// durable pointer to the upload's parts, which object listing does not show,
// so every statement here treats it as the record that may not be lost by
// accident.
type Session struct {
	// ID is the upload id a client holds and sends back.
	ID string
	// Owner is the space the completion will land the object in, and Path
	// the path.
	Owner, Path string
	// ObjectID is the key the parts assemble onto: the session's own object
	// and never the one it will replace.
	ObjectID object.ID
	// UploadID is the store's own multipart id.
	UploadID string
	// DeclaredSize is the promise the session was opened on, rechecked
	// against the assembled object at completion.
	DeclaredSize int64
	// ContentType is what the assembled object carries.
	ContentType string
	// CreatedBy is the subject that opened the session.
	CreatedBy string
	// CreatedAt is when it opened, and ExpiresAt when the reaper aborts it.
	CreatedAt, ExpiresAt time.Time
}

// Sessions is the query set over the open uploads of an installation.
type Sessions interface {
	// Insert opens a session and answers the id a client sends back.
	Insert(ctx context.Context, q Querier, s Session) (Session, error)
	// Get reads one session by its id. An id that names none, and an id
	// that is not an identifier at all, are both pgx.ErrNoRows: a client
	// that sends a word learns the same thing as one that sends a session
	// somebody else opened.
	Get(ctx context.Context, q Querier, id string) (Session, error)
	// Delete removes one session, answering false when it was already gone.
	Delete(ctx context.Context, q Querier, id string) (bool, error)
	// Expired answers the sessions past their deadline, oldest first, for
	// the reaper's fourth pass (spec 010). It is the query this spec
	// exposes; the sweep that runs it is that spec's.
	Expired(ctx context.Context, q Querier, at time.Time, limit int) ([]Session, error)
	// CountOpen answers how many sessions are open at that instant: started,
	// not yet completed or aborted, and not yet past their deadline. It is
	// the exact complement of Expired, and what arca_upload_sessions_open
	// reads (spec 018).
	CountOpen(ctx context.Context, q Querier, at time.Time) (int64, error)
}

// sessions is the query set over Postgres.
type sessions struct{}

// NewSessions answers the query set over Postgres.
func NewSessions() Sessions { return sessions{} }

// sessionColumns is the column list every read of a session shares.
const sessionColumns = `id, owner, path, object_id, upload_id, declared_size,
	content_type, created_by, created_at, expires_at`

// scanSession reads one row of sessionColumns.
func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.Owner, &s.Path, &s.ObjectID, &s.UploadID, &s.DeclaredSize,
		&s.ContentType, &s.CreatedBy, &s.CreatedAt, &s.ExpiresAt)
	return s, err
}

// Insert opens a session.
func (sessions) Insert(ctx context.Context, q Querier, s Session) (Session, error) {
	written, err := scanSession(q.QueryRow(ctx, `
		INSERT INTO upload_sessions (owner, path, object_id, upload_id, declared_size,
		                             content_type, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+sessionColumns,
		s.Owner, s.Path, s.ObjectID, s.UploadID, s.DeclaredSize,
		s.ContentType, s.CreatedBy, s.ExpiresAt))
	if err != nil {
		return Session{}, classify(fmt.Sprintf("open an upload of %q of %q", s.Path, s.Owner), err)
	}
	return written, nil
}

// Get reads one session by its id.
func (sessions) Get(ctx context.Context, q Querier, id string) (Session, error) {
	s, err := scanSession(q.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM upload_sessions WHERE id = $1`, id))
	switch {
	case notAnIdentifier(err):
		return Session{}, fmt.Errorf("store: read the upload %q: %w", id, pgx.ErrNoRows)
	case err != nil:
		return Session{}, fmt.Errorf("store: read the upload %q: %w", id, err)
	default:
		return s, nil
	}
}

// Delete removes one session.
func (sessions) Delete(ctx context.Context, q Querier, id string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM upload_sessions WHERE id = $1`, id)
	if err != nil {
		if notAnIdentifier(err) {
			return false, nil
		}
		return false, classify(fmt.Sprintf("close the upload %q", id), err)
	}
	return tag.RowsAffected() == 1, nil
}

// Expired answers the sessions past their deadline.
func (sessions) Expired(ctx context.Context, q Querier, at time.Time, limit int) ([]Session, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the expired uploads: the page holds %d rows", limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+sessionColumns+`
		  FROM upload_sessions
		 WHERE expires_at <= $1
		 ORDER BY expires_at
		 LIMIT $2`, at, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list the expired uploads: %w", err)
	}
	defer rows.Close()

	page := make([]Session, 0, limit)
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list the expired uploads: %w", err)
		}
		page = append(page, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the expired uploads: %w", err)
	}
	return page, nil
}

// CountOpen counts the sessions still open at that instant.
//
// A completed or aborted session has no row: both paths delete it inside the
// transaction that finishes them. What is left is a session still running or
// one the reaper has not swept yet, and the deadline tells the two apart.
func (sessions) CountOpen(ctx context.Context, q Querier, at time.Time) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM upload_sessions WHERE expires_at > $1`, at).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count the open uploads: %w", err)
	}
	return n, nil
}

// notAnIdentifier reports SQLSTATE 22P02, which is what a path parameter
// that is not a UUID reaches the database as. A word is not an id that names
// nothing, it is not an id at all, and both answer the caller the same thing
// (spec 001, invariant 6).
func notAnIdentifier(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
