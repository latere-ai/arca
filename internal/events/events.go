// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package events holds the two records a space keeps of itself: the ledger
// that counts the bytes it holds and the append-only log of what happened to
// it. Both are spec 010's.
//
// The ledger is a counter, not a sum: every statement that moves bytes
// applies its delta to the space's row inside the transaction that moves
// them, so the commit that records a file and the commit that records its
// bytes are one commit. A counter can drift where a computed sum cannot,
// which is what the reconciliation pass of internal/reaper pays for.
//
// The log is a tail and not an archive. Every mutation appends one row after
// it succeeds, the vocabulary is the closed table below, and a consumer
// replays from any cursor it kept.
//
// Nothing here decides access. A limit reaches Arca on the authorizer's
// answer and lives for that answer's ttl; Arca stores none.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// Action is one member of the closed vocabulary of spec 010. The database
// holds the column without a constraint, so this table is the closure that
// every writer and every filter reads: adding an action is a change here and
// to the conformance rows of spec 017, and it is not a migration.
type Action string

// The eleven actions. The spec that appends each is in its comment.
const (
	// ActionPut is an object written, by a put or by a completed upload
	// session (specs 005 and 007).
	ActionPut Action = "put"
	// ActionMove is a path renamed (spec 005).
	ActionMove Action = "move"
	// ActionDelete is an object trashed or removed (spec 005).
	ActionDelete Action = "delete"
	// ActionRestore is an object brought back from trash or a version
	// brought forward (specs 005 and 009).
	ActionRestore Action = "restore"
	// ActionPurge is a trashed object or a tombstone leaving for good, which
	// is the last thing said about it (this spec's reaper).
	ActionPurge Action = "purge"
	// ActionAttach is an attachment opened on a workspace (spec 009).
	ActionAttach Action = "attach"
	// ActionRelease is an attachment given up (spec 009).
	ActionRelease Action = "release"
	// ActionSync is a workspace's post-state declared (spec 009).
	ActionSync Action = "sync"
	// ActionReap is a reconciliation pass that changed something in the
	// space (this spec's reaper).
	ActionReap Action = "reap"
	// ActionShareCreated is a grant made, a link included, with its kind in
	// the detail (spec 008).
	ActionShareCreated Action = "share_created"
	// ActionShareRevoked is a grant withdrawn (spec 008).
	ActionShareRevoked Action = "share_revoked"
)

// actions is the vocabulary in the order spec 010 lists it.
var actions = []Action{
	ActionPut, ActionMove, ActionDelete, ActionRestore, ActionPurge,
	ActionAttach, ActionRelease, ActionSync, ActionReap,
	ActionShareCreated, ActionShareRevoked,
}

// Actions answers the closed vocabulary, in the order spec 010 lists it. The
// caller receives a copy, so a filter that sorts its own view leaves the
// table alone.
func Actions() []Action { return slices.Clone(actions) }

// Valid reports whether a is in the vocabulary. A writer checks before it
// appends, because the column carries no constraint.
func (a Action) Valid() bool { return slices.Contains(actions, a) }

// ErrUnknownAction is an append of an action outside the closed table. It is
// a bug in the caller rather than a fault, so it is refused and not written.
var ErrUnknownAction = fmt.Errorf("events: the action is not in the vocabulary of spec 010")

// Event is one row of the log.
type Event struct {
	// ID is the cursor a consumer tails. It is assigned by the append and is
	// zero on the value a caller hands in.
	ID int64
	// Owner is the space the event happened to.
	Owner string
	// Path is what it happened to, empty for an event about the space
	// itself.
	Path string
	// Action is the member of the closed vocabulary.
	Action Action
	// Actor is the subject that caused it, empty for the reaper and for
	// anything else the server did on its own.
	Actor string
	// Detail is the finer grain a consumer reads when the action is not
	// enough: the size of a put, the kind of a share. It is the reason the
	// vocabulary can stay at eleven members.
	Detail map[string]any
	// At is when the row was written.
	At time.Time
}

// Log is the append-only record of what happened to a space.
//
// Every query takes a querier, so one function serves a caller inside a
// transaction and one outside it. The append is best effort for an ordinary
// mutation, which is what Note is for; an administrative mutation appends
// inside its own transaction with Append, because the record of what an
// administrator did may not go missing quietly (spec 012).
type Log interface {
	// Append writes one row and answers its id. The caller decides what a
	// failure means: inside a transaction it takes the mutation down with
	// it, and outside one it is a warning.
	Append(ctx context.Context, q store.Querier, e Event) (int64, error)
	// Note appends and swallows a failure into one warning, which is the
	// rule for an ordinary mutation: the operation already happened, and an
	// event is a notification about it.
	Note(ctx context.Context, q store.Querier, e Event)
	// Tail answers one page of a space's log, oldest first, keyset
	// paginated on the primary key.
	Tail(ctx context.Context, q store.Querier, t Query) (Page, error)
	// Prune removes the rows older than before and answers how many went.
	// The log is a tail, not an archive (this spec's pass 9).
	Prune(ctx context.Context, q store.Querier, before time.Time) (int64, error)
	// Older counts the rows Prune would remove, which is what the dry run
	// of that pass reports.
	Older(ctx context.Context, q store.Querier, before time.Time) (int64, error)
}

// Query selects one page of the tail.
type Query struct {
	// Owner is the space, the subject <issuer>|<sub>.
	Owner string
	// Cursor is the id the page starts after. Zero reads from the start of
	// the log the installation still holds.
	Cursor int64
	// Limit is how many rows the page holds at most.
	Limit int
}

// Page is one page of the tail.
type Page struct {
	// Entries are the rows, oldest first, never nil.
	Entries []Event
	// NextCursor is the id to send back to read the next page, zero when
	// this page is the end of the log. Its presence is the whole has-more
	// signal (spec 013).
	NextCursor int64
}

// eventLog is the log over Postgres.
type eventLog struct{}

// NewLog answers the log over Postgres.
func NewLog() Log { return eventLog{} }

// eventColumns is the column list every read of an event shares.
const eventColumns = `id, owner, path, action, actor, detail, created_at`

// Append writes one row and answers its id.
func (eventLog) Append(ctx context.Context, q store.Querier, e Event) (int64, error) {
	if !e.Action.Valid() {
		return 0, fmt.Errorf("events: append %q to %q: %w", e.Action, e.Owner, ErrUnknownAction)
	}
	detail, err := marshalDetail(administrative(ctx, e.Detail))
	if err != nil {
		return 0, fmt.Errorf("events: append %q to %q: %w", e.Action, e.Owner, err)
	}
	var id int64
	if err := q.QueryRow(ctx, `
		INSERT INTO events (owner, path, action, actor, detail)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		e.Owner, nullable(e.Path), string(e.Action), nullable(e.Actor), detail).Scan(&id); err != nil {
		return 0, fmt.Errorf("events: append %q to %q: %w", e.Action, e.Owner, err)
	}
	return id, nil
}

// Note appends and swallows a failure into one warning.
func (l eventLog) Note(ctx context.Context, q store.Querier, e Event) {
	if _, err := l.Append(ctx, q, e); err != nil {
		slog.WarnContext(ctx, "the event was not appended",
			"action", string(e.Action), "owner", e.Owner, "err", err)
	}
}

// Tail answers one page of a space's log.
//
// The page reads one row beyond the limit and drops it, so the cursor is
// present when a further page exists and absent on the last page, which is
// the only has-more signal a consumer has (spec 013).
func (eventLog) Tail(ctx context.Context, q store.Querier, t Query) (Page, error) {
	if t.Limit <= 0 {
		return Page{}, fmt.Errorf("events: tail %q: the page holds %d rows", t.Owner, t.Limit)
	}
	rows, err := q.Query(ctx, `
		SELECT `+eventColumns+`
		  FROM events
		 WHERE owner = $1 AND id > $2
		 ORDER BY id
		 LIMIT $3`, t.Owner, t.Cursor, t.Limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("events: tail %q: %w", t.Owner, err)
	}
	defer rows.Close()

	page := Page{Entries: make([]Event, 0, t.Limit)}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return Page{}, fmt.Errorf("events: tail %q: %w", t.Owner, err)
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("events: tail %q: %w", t.Owner, err)
	}
	if len(page.Entries) > t.Limit {
		page.Entries = page.Entries[:t.Limit]
		page.NextCursor = page.Entries[t.Limit-1].ID
	}
	return page, nil
}

// Prune removes the rows older than before.
func (eventLog) Prune(ctx context.Context, q store.Querier, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM events WHERE created_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("events: prune the log before %s: %w", before.UTC().Format(time.RFC3339), err)
	}
	return tag.RowsAffected(), nil
}

// Older counts the rows Prune would remove.
func (eventLog) Older(ctx context.Context, q store.Querier, before time.Time) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM events WHERE created_at < $1`, before).Scan(&n); err != nil {
		return 0, fmt.Errorf("events: count the log before %s: %w", before.UTC().Format(time.RFC3339), err)
	}
	return n, nil
}

// scanner is what a row and a page of rows both answer a scan through.
type scanner interface{ Scan(dest ...any) error }

// scanEvent reads one row of eventColumns.
func scanEvent(row scanner) (Event, error) {
	var (
		e      Event
		path   *string
		actor  *string
		detail []byte
	)
	if err := row.Scan(&e.ID, &e.Owner, &path, &e.Action, &actor, &detail, &e.At); err != nil {
		return Event{}, err
	}
	if path != nil {
		e.Path = *path
	}
	if actor != nil {
		e.Actor = *actor
	}
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &e.Detail); err != nil {
			return Event{}, fmt.Errorf("read the detail of event %d: %w", e.ID, err)
		}
	}
	return e, nil
}

// DetailAdmin is the key an event carries when neither ownership nor a
// covering grant explains the allow that caused it (spec 012).
const DetailAdmin = "admin"

// administrative adds the mark to a detail when the request that caused the
// event received such an allow. The decision path set it, one question
// earlier and in another package, so a route added later is recorded without
// being told to be and no writer can put the key on an event that did not
// earn it.
//
// The map is copied and never written into. Every writer passes a literal
// today, and a writer that reused one would find this key in it on the next
// call.
func administrative(ctx context.Context, detail map[string]any) map[string]any {
	if !auth.Administrative(ctx) {
		return detail
	}
	marked := make(map[string]any, len(detail)+1)
	maps.Copy(marked, detail)
	marked[DetailAdmin] = true
	return marked
}

// marshalDetail renders the detail, or nothing when there is none. A detail
// that will not encode refuses the append rather than writing a row whose
// detail is silently lost: the caller built it, so the caller hears about
// it.
func marshalDetail(detail map[string]any) ([]byte, error) {
	if len(detail) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("encode the detail: %w", err)
	}
	return b, nil
}

// nullable renders an empty string as the SQL null the column holds for an
// absent value, so a path that is not there reads back as absent rather than
// as the empty path.
func nullable(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
