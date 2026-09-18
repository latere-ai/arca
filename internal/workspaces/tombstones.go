// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// Pass 6 of spec 010's table, implemented by the spec that owns the rows it
// removes: a workspace soft deleted past ARCA_TRASH_RETENTION leaves for
// good, with the subtree it held.
//
// It is not a method of [Service]. A service refuses to build without an
// authorizer, because every handler decides through one; this pass registers
// no handler and puts no question, so binding it to the service would keep
// it off the arcad reap process, which starts no verifier. The pass that
// does not run reports nothing, and a table with no row for it reads exactly
// like a pass that found nothing.

// tombstonePage is how many tombstones one pass takes. Each is a subtree
// delete and a bucket call, so the page is small and a backlog is worked
// through over several runs.
const tombstonePage = 100

// errRestored is a tombstone somebody brought back between the page this
// pass read and the statement that would have ended it. It is not a failure:
// the transaction rolls back, the workspace keeps its rows, and the pass
// reports one fewer purge.
var errRestored = errors.New("workspaces: the tombstone was restored before it could be purged")

// TombstoneOptions is what the node builds the pass from. It is the
// service's own wiring less the authorizer, which this pass has no question
// to ask of.
type TombstoneOptions struct {
	// DB is the transaction a purge runs in.
	DB Database
	// Workspaces and Objects are the rows the purge removes: the tombstone
	// itself, and the subtree of spec 005 it held.
	Workspaces store.Workspaces
	Objects    Objects
	// Bucket and Prefix are where the bytes go, after the rows.
	Bucket blob.Store
	Prefix string
	// Retention is ARCA_TRASH_RETENTION, how long a tombstone stays
	// restorable. It is the trash's window and not a second setting: a
	// deleted workspace and a trashed object are recoverable for the same
	// time (spec 009).
	Retention time.Duration
	// Ledger is the log and the usage counter of spec 010. Nil writes
	// nothing.
	Ledger Ledger
}

// Tombstones purges what a soft delete left behind.
type Tombstones struct {
	db         Database
	workspaces store.Workspaces
	objects    Objects
	bucket     blob.Store
	prefix     string
	retention  time.Duration
	ledger     Ledger
}

// NewTombstones builds the pass. It refuses what it would otherwise reach
// through a nil pointer on the first run, and refuses a window of no time:
// a retention of zero purges a workspace deleted a moment ago, which is the
// restore window closing before anybody could use it.
func NewTombstones(o TombstoneOptions) (*Tombstones, error) {
	for _, missing := range []struct {
		absent bool
		what   string
	}{
		{o.DB == nil, "no database"},
		{o.Workspaces == nil, "no workspace queries"},
		{o.Objects == nil, "no file plane"},
		{o.Bucket == nil, "no bucket"},
	} {
		if missing.absent {
			return nil, &missingSeam{what: missing.what}
		}
	}
	if o.Retention <= 0 {
		return nil, &missingSeam{what: "a retention of no time, which purges a workspace deleted a moment ago"}
	}
	t := &Tombstones{
		db: o.DB, workspaces: o.Workspaces, objects: o.Objects,
		bucket: o.Bucket, prefix: o.Prefix, retention: o.Retention, ledger: o.Ledger,
	}
	if t.ledger == nil {
		t.ledger = silent{}
	}
	return t, nil
}

// Sweep runs the pass once and answers how many tombstones it ended.
//
// A dry run counts and changes nothing, which this pass can do honestly: the
// working set is a read, so the number it reports is the number a live run
// would purge.
//
// One tombstone that will not purge does not take the page down with it. Its
// rows stay and the next run tries again, which is what a reconciler does
// with a store that is briefly unwell.
func (t *Tombstones) Sweep(ctx context.Context, q store.Querier, now time.Time, dry bool) (int, error) {
	gone, err := t.workspaces.Tombstones(ctx, q, now.Add(-t.retention), tombstonePage)
	if err != nil {
		return 0, err
	}
	if dry {
		return len(gone), nil
	}
	var (
		purged int
		errs   []error
	)
	for _, ws := range gone {
		switch err := t.purge(ctx, ws); {
		case errors.Is(err, errRestored):
			continue
		case err != nil:
			slog.WarnContext(ctx, "workspaces: a tombstone would not purge",
				"workspace", ws.ID, "space", ws.Owner, "error", err)
			errs = append(errs, err)
		default:
			purged++
		}
	}
	return purged, errors.Join(errs...)
}

// purge ends one tombstone.
//
// The conditional delete of the row comes first and is the guard: a restore
// that landed after the page was read matches it out, and the whole
// transaction rolls back rather than removing the subtree of a workspace
// somebody just brought back.
//
// The rows go before the keys, which is invariant 1 of spec 001's order for
// a delete: a failure between them leaves bytes pass 1 finds, rather than a
// row whose object is gone.
func (t *Tombstones) purge(ctx context.Context, ws store.Workspace) error {
	root := Root(ws.Slug)
	var (
		freed   []object.ID
		removed int
		bytes   int64
	)
	err := t.db.Tx(ctx, func(q store.Querier) error {
		ended, err := t.workspaces.Purge(ctx, q, ws.ID)
		if err != nil {
			return err
		}
		if !ended {
			return errRestored
		}
		dropped, held, err := t.objects.DropSubtree(ctx, q, ws.Owner, root)
		if err != nil {
			return err
		}
		removed, bytes = len(dropped), held
		// What may go is what no row still names: bytes another path or
		// another space's version carries stay where they are.
		freed, err = t.objects.Unreferenced(ctx, q, dropped)
		if err != nil {
			return err
		}
		if err := t.ledger.Release(ctx, q, ws.Owner, bytes); err != nil {
			return err
		}
		// The purge event is the last thing said about the workspace, and it
		// is written in the transaction that ends it: a record of what an
		// installation no longer holds may not go missing (spec 010).
		return t.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: root, Action: ActionPurge,
			Detail: map[string]any{"slug": ws.Slug, "files": removed, "bytes": bytes},
		})
	})
	if err != nil {
		return err
	}
	removeBytes(ctx, t.bucket, t.prefix, freed)
	return nil
}
