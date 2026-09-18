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
)

// The query sets spec 009 owns: a durable subtree of a space with at most
// one writer, and the attachments that hold that writer lease.
//
// The invariant of that spec lives in one statement here. A rw attach is a
// conditional update on the workspace row, and the second one matches no row
// and is a conflict. There is no lock taken in Go and no read before the
// write that a second process could interleave with, which is why two
// attaches arriving at once leave one lease and not two.

// Mode is how an attachment holds a workspace: a snapshot any number of
// readers take, or the one writer lease.
type Mode string

// The two modes, as the mode column constrains them.
const (
	// ModeRead takes a snapshot and touches no lease.
	ModeRead Mode = "ro"
	// ModeWrite takes the workspace's one writer lease.
	ModeWrite Mode = "rw"
)

// Valid reports whether m is one of the two modes.
func (m Mode) Valid() bool { return m == ModeRead || m == ModeWrite }

// AttachmentStatus is where an attachment is in its life.
type AttachmentStatus string

// The three statuses, as the status column constrains them.
const (
	// StatusActive is an attachment a sandbox still holds.
	StatusActive AttachmentStatus = "active"
	// StatusReleased is one the sandbox gave back.
	StatusReleased AttachmentStatus = "released"
	// StatusReaped is one whose time ran out before the sandbox released
	// it. Its unsynced work is lost, which is the price of a lease that
	// cannot be held forever.
	StatusReaped AttachmentStatus = "reaped"
)

// ErrBadCursor is a cursor that is not one of this listing's. A cursor is
// opaque and a client sends back what it read, so a value the listing could
// not have minted is a bad request and never a fault.
var ErrBadCursor = errors.New("store: the cursor is not one this listing minted")

// Workspace is one durable subtree of a space. The root prefix is derived
// from the slug and is never stored, so a rename moves rows and touches no
// bucket key.
type Workspace struct {
	// ID is the handle every route names.
	ID string
	// Owner is the space, the subject <issuer>|<sub>.
	Owner string
	// Slug is the name inside the space.
	Slug string
	// CreatedBy is the subject that created it.
	CreatedBy string
	// WriterHolder is the lease holder, nil while the lease is free.
	WriterHolder *string
	// WriterExpiresAt is when the lease lapses, nil while it is free.
	WriterExpiresAt *time.Time
	// LastSync is the boundary the newest snapshot was taken at.
	LastSync *time.Time
	// CreatedAt and UpdatedAt are the row's own times.
	CreatedAt, UpdatedAt time.Time
	// DeletedAt is the soft delete: nil while the workspace is live.
	DeletedAt *time.Time
}

// Attachment is one sandbox's session against one workspace.
type Attachment struct {
	// ID is the attachment's identity.
	ID string
	// WorkspaceID is the workspace it holds.
	WorkspaceID string
	// Holder is the sandbox or process that attached, opaque to Arca.
	Holder string
	// Subject is who it attached as.
	Subject string
	// Mode is ro or rw.
	Mode Mode
	// Status is where the attachment is in its life.
	Status AttachmentStatus
	// Manifest is the snapshot pinned at attach, as the JSON the column
	// holds. What the entries mean is the workspace package's.
	Manifest []byte
	// ExpiresAt is when the attachment lapses.
	ExpiresAt time.Time
	// CreatedAt is when it was taken, which is when its manifest was pinned.
	CreatedAt time.Time
	// ReleasedAt is when it was given back or reaped, nil while active.
	ReleasedAt *time.Time
}

// Workspaces is the query set over the workspace rows of one space. A
// handler takes the interface, so a test passes a fake; NewWorkspaces
// answers the one over Postgres.
type Workspaces interface {
	// Create writes a workspace whose slug the space does not hold,
	// answering ErrConflict when it does. The uniqueness is not conditional
	// on deleted_at, so a tombstone's slug collides too.
	Create(ctx context.Context, q Querier, w Workspace) (Workspace, error)
	// Get reads one workspace by its id, live or deleted. An id that names
	// no row, and a string that is not an id at all, are pgx.ErrNoRows.
	Get(ctx context.Context, q Querier, id string) (Workspace, error)
	// GetForUpdate reads one workspace and holds its row for the rest of
	// the transaction, so a check of the lease and the write that follows
	// it cannot be interleaved.
	GetForUpdate(ctx context.Context, q Querier, id string) (Workspace, error)
	// List answers one page of the live workspaces of a space, ordered by
	// id, after the cursor.
	List(ctx context.Context, q Querier, owner, cursor string, limit int) ([]Workspace, error)
	// ListDeleted answers one page of the soft deleted ones, in the same
	// order, which is what is still restorable.
	ListDeleted(ctx context.Context, q Querier, owner, cursor string, limit int) ([]Workspace, error)
	// Rename gives a live workspace whose lease is free another slug. It
	// answers false when the row moved under the caller and ErrConflict
	// when the space already holds the slug.
	Rename(ctx context.Context, q Querier, id, slug string) (bool, error)
	// SoftDelete stamps deleted_at on a live workspace whose lease is free.
	SoftDelete(ctx context.Context, q Querier, id string) (bool, error)
	// Restore clears deleted_at. It answers false for a workspace that is
	// not deleted, which the caller has already told from one that is gone.
	Restore(ctx context.Context, q Querier, id string) (bool, error)
	// TakeLease is the conditional update the writer lease rests on: it
	// matches a row whose lease is free or lapsed at now, so a second
	// attach arriving at once matches nothing and is a conflict.
	TakeLease(ctx context.Context, q Querier, id, holder string, now, until time.Time) (bool, error)
	// RenewLease stamps a new expiry and nothing else, for the holder that
	// still holds it.
	RenewLease(ctx context.Context, q Querier, id, holder string, until time.Time) (bool, error)
	// ReleaseLease frees a lease its holder still holds. It is scoped to
	// the holder, so a release that arrives after the lease moved on frees
	// nothing.
	ReleaseLease(ctx context.Context, q Querier, id, holder string) (bool, error)
	// StampSync records the boundary a completed sync took and answers it.
	StampSync(ctx context.Context, q Querier, id string) (time.Time, error)
	// ExpiredLeases answers the leases whose deadline had passed at now,
	// which is what the reaper of spec 010 sweeps. It changes nothing.
	ExpiredLeases(ctx context.Context, q Querier, now time.Time, limit int) ([]Workspace, error)
}

// Attachments is the query set over the attachments of a workspace.
type Attachments interface {
	// Insert opens an attachment. In a rw attach it runs in the same
	// transaction as TakeLease, so a lease and the attachment that holds it
	// are written together or not at all.
	Insert(ctx context.Context, q Querier, a Attachment) (Attachment, error)
	// Get reads one attachment of one workspace. An attachment of another
	// workspace is pgx.ErrNoRows, so an id cannot be used to read across.
	Get(ctx context.Context, q Querier, workspaceID, id string) (Attachment, error)
	// Release marks an active attachment released.
	Release(ctx context.Context, q Querier, id string) (bool, error)
	// Reap marks an active attachment reaped, which is what the sweep of
	// spec 010 does to one whose time ran out.
	Reap(ctx context.Context, q Querier, id string) (bool, error)
	// SetManifest rewrites an active attachment's pinned snapshot, which a
	// completed sync does so the same attachment can materialize again.
	SetManifest(ctx context.Context, q Querier, id string, manifest []byte) (bool, error)
	// Expired answers the active attachments whose time had passed at now,
	// which is the query the reaper of spec 010 calls. It changes nothing.
	Expired(ctx context.Context, q Querier, now time.Time, limit int) ([]Attachment, error)
}

// workspaces is the query set over Postgres.
type workspaces struct{}

// NewWorkspaces answers the workspace query set over Postgres.
func NewWorkspaces() Workspaces { return workspaces{} }

// attachments is the query set over Postgres.
type attachments struct{}

// NewAttachments answers the attachment query set over Postgres.
func NewAttachments() Attachments { return attachments{} }

// workspaceColumns is the column list every read of a workspace shares, so
// one scan function serves them all.
const workspaceColumns = `id, owner, slug, created_by, writer_holder, writer_expires_at,
	last_sync, created_at, updated_at, deleted_at`

// scanWorkspace reads one row of workspaceColumns.
func scanWorkspace(row pgx.Row) (Workspace, error) {
	var w Workspace
	err := row.Scan(&w.ID, &w.Owner, &w.Slug, &w.CreatedBy, &w.WriterHolder, &w.WriterExpiresAt,
		&w.LastSync, &w.CreatedAt, &w.UpdatedAt, &w.DeletedAt)
	return w, err
}

// Create writes a workspace whose slug the space does not hold.
func (workspaces) Create(ctx context.Context, q Querier, w Workspace) (Workspace, error) {
	created, err := scanWorkspace(q.QueryRow(ctx, `
		INSERT INTO workspaces (owner, slug, created_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (owner, slug) DO NOTHING
		RETURNING `+workspaceColumns, w.Owner, w.Slug, w.CreatedBy))
	switch {
	case missing(err):
		// The slug is taken, by a live workspace or by a tombstone that has
		// not been purged. Both are the same answer: the space already has a
		// workspace with that name.
		return Workspace{}, fmt.Errorf("store: create %q of %q: %w", w.Slug, w.Owner, ErrConflict)
	case err != nil:
		return Workspace{}, classify(fmt.Sprintf("create %q of %q", w.Slug, w.Owner), err)
	default:
		return created, nil
	}
}

// Get reads one workspace by its id.
func (workspaces) Get(ctx context.Context, q Querier, id string) (Workspace, error) {
	return readWorkspace(ctx, q, id, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = $1`)
}

// GetForUpdate reads one workspace and holds its row for the rest of the
// transaction. A rename and a delete read the lease and then write against
// it, and the row lock is what keeps the two steps one decision.
func (workspaces) GetForUpdate(ctx context.Context, q Querier, id string) (Workspace, error) {
	return readWorkspace(ctx, q, id, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = $1 FOR UPDATE`)
}

// readWorkspace runs one single-row read of a workspace by id.
func readWorkspace(ctx context.Context, q Querier, id, statement string) (Workspace, error) {
	w, err := scanWorkspace(q.QueryRow(ctx, statement, id))
	if err != nil {
		return Workspace{}, fmt.Errorf("store: read the workspace %q: %w", id, noSuchID(err))
	}
	return w, nil
}

// List answers one page of a space's live workspaces.
func (workspaces) List(ctx context.Context, q Querier, owner, cursor string, limit int) ([]Workspace, error) {
	return listWorkspaces(ctx, q, owner, cursor, limit, `deleted_at IS NULL`)
}

// ListDeleted answers one page of a space's soft deleted workspaces, which
// is what is still inside the restore window.
func (workspaces) ListDeleted(ctx context.Context, q Querier, owner, cursor string, limit int) ([]Workspace, error) {
	return listWorkspaces(ctx, q, owner, cursor, limit, `deleted_at IS NOT NULL`)
}

// listWorkspaces answers one keyset page of a space's workspaces. The cursor
// is the id of the last row of the page before, so an insert during the walk
// shifts nothing, and the page costs the same at row one and row one
// million.
func listWorkspaces(ctx context.Context, q Querier, owner, cursor string, limit int, state string) ([]Workspace, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list the workspaces of %q: the page holds %d rows", owner, limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+workspaceColumns+`
		  FROM workspaces
		 WHERE owner = $1 AND `+state+`
		   AND ($2::uuid IS NULL OR id > $2::uuid)
		 ORDER BY id
		 LIMIT $3`, owner, nullable(cursor), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list the workspaces of %q: %w", owner, noSuchCursor(err))
	}
	defer rows.Close()

	page := make([]Workspace, 0, limit)
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list the workspaces of %q: %w", owner, err)
		}
		page = append(page, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the workspaces of %q: %w", owner, noSuchCursor(err))
	}
	return page, nil
}

// Rename gives a live workspace whose lease is free another slug.
func (workspaces) Rename(ctx context.Context, q Querier, id, slug string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspaces SET slug = $2, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND writer_holder IS NULL`, id, slug)
	return changed(fmt.Sprintf("rename the workspace %q", id), tag, err)
}

// SoftDelete stamps deleted_at on a live workspace whose lease is free. A
// delete while the lease is held is refused: the writer's view of its own
// paths would change underneath it.
func (workspaces) SoftDelete(ctx context.Context, q Querier, id string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspaces SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL AND writer_holder IS NULL`, id)
	return changed(fmt.Sprintf("delete the workspace %q", id), tag, err)
}

// Restore clears deleted_at. It needs no collision guard: the uniqueness of
// (owner, slug) is not conditional on deleted_at, so the tombstone kept its
// slug reserved and no live workspace can have taken the name.
func (workspaces) Restore(ctx context.Context, q Querier, id string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspaces SET deleted_at = NULL, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NOT NULL`, id)
	return changed(fmt.Sprintf("restore the workspace %q", id), tag, err)
}

// TakeLease is the whole of the one-writer invariant.
//
// The condition is in the statement, so a lost race is a zero row count and
// never an overwrite: two attaches arriving at once leave one lease, and the
// loser learns it from the row count rather than from a read it took a
// moment earlier. A lapsed lease matches too, because a lease is time
// bounded and a holder past its deadline does not hold it; the reaper of
// spec 010 still marks that holder's attachment reaped and says so in the
// log, and this clause is what keeps a crashed sandbox from wedging a
// workspace until the sweep runs. The lapse is measured against the instant
// the caller names, which is the clock the expiry it writes came from, so
// one attach never reads two clocks.
func (workspaces) TakeLease(ctx context.Context, q Querier, id, holder string, now, until time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspaces SET writer_holder = $2, writer_expires_at = $3, updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL
		   AND (writer_holder IS NULL OR writer_expires_at <= $4)`, id, holder, until, now)
	return changed(fmt.Sprintf("take the lease of %q", id), tag, err)
}

// RenewLease stamps a new expiry and nothing else.
func (workspaces) RenewLease(ctx context.Context, q Querier, id, holder string, until time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspaces SET writer_expires_at = $3, updated_at = now()
		 WHERE id = $1 AND writer_holder = $2`, id, holder, until)
	return changed(fmt.Sprintf("renew the lease of %q", id), tag, err)
}

// ReleaseLease frees a lease its holder still holds.
func (workspaces) ReleaseLease(ctx context.Context, q Querier, id, holder string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspaces SET writer_holder = NULL, writer_expires_at = NULL, updated_at = now()
		 WHERE id = $1 AND writer_holder = $2`, id, holder)
	return changed(fmt.Sprintf("release the lease of %q", id), tag, err)
}

// StampSync records the boundary a completed sync took.
func (workspaces) StampSync(ctx context.Context, q Querier, id string) (time.Time, error) {
	var at time.Time
	err := q.QueryRow(ctx, `
		UPDATE workspaces SET last_sync = now(), updated_at = now()
		 WHERE id = $1 RETURNING last_sync`, id).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: stamp the sync of %q: %w", id, noSuchID(err))
	}
	return at, nil
}

// ExpiredLeases answers the leases whose deadline has passed. A lease can
// outlive the attachment that took it, so the reaper sweeps this list beside
// the attachment list rather than deriving one from the other.
func (workspaces) ExpiredLeases(ctx context.Context, q Querier, now time.Time, limit int) ([]Workspace, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: read the expired leases: the page holds %d rows", limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+workspaceColumns+`
		  FROM workspaces
		 WHERE writer_holder IS NOT NULL AND writer_expires_at < $1
		 ORDER BY writer_expires_at
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read the expired leases: %w", err)
	}
	defer rows.Close()

	out := make([]Workspace, 0, limit)
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("store: read the expired leases: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the expired leases: %w", err)
	}
	return out, nil
}

// attachmentColumns is the column list every read of an attachment shares.
const attachmentColumns = `id, workspace_id, holder, subject, mode, status, manifest,
	expires_at, created_at, released_at`

// scanAttachment reads one row of attachmentColumns.
func scanAttachment(row pgx.Row) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.Holder, &a.Subject, &a.Mode, &a.Status,
		&a.Manifest, &a.ExpiresAt, &a.CreatedAt, &a.ReleasedAt)
	return a, err
}

// Insert opens an attachment.
func (attachments) Insert(ctx context.Context, q Querier, a Attachment) (Attachment, error) {
	created, err := scanAttachment(q.QueryRow(ctx, `
		INSERT INTO workspace_attachments (workspace_id, holder, subject, mode, manifest, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+attachmentColumns,
		a.WorkspaceID, a.Holder, a.Subject, a.Mode, a.Manifest, a.ExpiresAt))
	if err != nil {
		return Attachment{}, classify(fmt.Sprintf("attach to the workspace %q", a.WorkspaceID), noSuchID(err))
	}
	return created, nil
}

// Get reads one attachment of one workspace.
func (attachments) Get(ctx context.Context, q Querier, workspaceID, id string) (Attachment, error) {
	a, err := scanAttachment(q.QueryRow(ctx,
		`SELECT `+attachmentColumns+` FROM workspace_attachments WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID))
	if err != nil {
		return Attachment{}, fmt.Errorf("store: read the attachment %q: %w", id, noSuchID(err))
	}
	return a, nil
}

// Release marks an active attachment released.
func (attachments) Release(ctx context.Context, q Querier, id string) (bool, error) {
	return endAttachment(ctx, q, id, StatusReleased)
}

// Reap marks an active attachment reaped.
func (attachments) Reap(ctx context.Context, q Querier, id string) (bool, error) {
	return endAttachment(ctx, q, id, StatusReaped)
}

// endAttachment moves an active attachment to one of the two end states.
func endAttachment(ctx context.Context, q Querier, id string, status AttachmentStatus) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspace_attachments SET status = $2, released_at = now()
		 WHERE id = $1 AND status = 'active'`, id, status)
	return changed(fmt.Sprintf("end the attachment %q", id), tag, err)
}

// SetManifest rewrites an active attachment's pinned snapshot.
func (attachments) SetManifest(ctx context.Context, q Querier, id string, manifest []byte) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE workspace_attachments SET manifest = $2
		 WHERE id = $1 AND status = 'active'`, id, manifest)
	return changed(fmt.Sprintf("pin the manifest of %q", id), tag, err)
}

// Expired answers the active attachments whose time has passed.
func (attachments) Expired(ctx context.Context, q Querier, now time.Time, limit int) ([]Attachment, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: read the expired attachments: the page holds %d rows", limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+attachmentColumns+`
		  FROM workspace_attachments
		 WHERE status = 'active' AND expires_at < $1
		 ORDER BY expires_at
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read the expired attachments: %w", err)
	}
	defer rows.Close()

	out := make([]Attachment, 0, limit)
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: read the expired attachments: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the expired attachments: %w", err)
	}
	return out, nil
}

// nullable renders an empty cursor as the SQL null the listing's condition
// reads, so the first page needs no second statement.
func nullable(cursor string) *string {
	if cursor == "" {
		return nil
	}
	return &cursor
}

// changed reports whether one conditional write matched its row. A string
// the database refused as not an identifier matched nothing, which is the
// same answer a wrong id gets, so it is a false and never a fault.
func changed(what string, tag pgconn.CommandTag, err error) (bool, error) {
	switch {
	case malformedText(err):
		return false, nil
	case err != nil:
		return false, classify(what, err)
	default:
		return tag.RowsAffected() == 1, nil
	}
}

// noSuchID maps a string the database refused as not an identifier onto the
// missing row it names. An id is opaque to a client, so a value that is not
// one names nothing, and answering not-found is the same answer a wrong id
// gets; a fault would be a 500 for a request that was merely wrong.
func noSuchID(err error) error {
	if malformedText(err) {
		return pgx.ErrNoRows
	}
	return err
}

// noSuchCursor maps the same refusal onto a bad cursor, which is what a
// listing's opaque value that did not come from this listing is.
func noSuchCursor(err error) error {
	if malformedText(err) {
		return ErrBadCursor
	}
	return err
}

// malformedText reports whether the database refused an argument as not the
// type the column holds, which is SQLSTATE 22P02.
func malformedText(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
