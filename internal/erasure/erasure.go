// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package erasure is spec 032: the deletion of everything one space holds,
// bound to the route of internal/admin.
//
// The order is invariant 1 of spec 001 and spec 021's rule together. Open
// multiparts are aborted at the bucket first, because a session row is the
// only durable pointer to a multipart's parts and a row deleted ahead of a
// failed abort strands them. The rows then go in one transaction, with the
// record of the deletion appended last inside it, so the log cannot miss a
// deletion and cannot hold one that rolled back. The bytes go only after
// the commit, and only those no remaining row references: an object id is
// not a content hash, but a restored version and a copy can name the same
// object from two rows, and the reference check of spec 010 is the one
// statement that decides whether bytes may go.
//
// A byte deletion that fails is logged, and the object is then one no row
// references, which the reconciler's orphan pass (spec 010, pass 1) removes
// on a later run. The deletion call is not failed for it: the rows are
// already gone, so a repeat would find nothing to retry.
package erasure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"latere.ai/x/arca/internal/admin"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// batch is how many keys one bucket call carries.
const batch = 1000

// Database is the metadata store as the deletion needs it: a querier outside
// a transaction and one transaction at a time. *store.DB satisfies it.
type Database interface {
	Querier() store.Querier
	Tx(ctx context.Context, fn func(store.Querier) error) error
}

// Options is what the node hands this package.
type Options struct {
	DB     Database
	Bucket blob.Store
	// Prefix is the bucket prefix of spec 003.
	Prefix string
	// Log is the log of spec 010.
	Log events.Log
	// Rows is the query set. Nil takes the one over Postgres.
	Rows store.Erasure
	// Referenced answers whether any row still names an object, which is
	// what decides whether its bytes may go. Nil takes the one statement of
	// spec 010, [store.ObjectReferenced].
	Referenced func(ctx context.Context, q store.Querier, id object.ID) (bool, error)
}

// Eraser deletes spaces. It implements [admin.Eraser].
type Eraser struct {
	o Options
}

// New builds the eraser. It refuses to build without what it cannot delete
// through.
func New(o Options) (*Eraser, error) {
	for _, missing := range []struct {
		absent bool
		what   string
	}{
		{o.DB == nil, "no database"},
		{o.Bucket == nil, "no bucket"},
		{o.Log == nil, "no log to record the deletion in"},
	} {
		if missing.absent {
			return nil, errors.New("erasure: " + missing.what)
		}
	}
	if o.Rows == nil {
		o.Rows = store.NewErasure()
	}
	if o.Referenced == nil {
		o.Referenced = store.ObjectReferenced
	}
	return &Eraser{o: o}, nil
}

// Erase deletes everything the owner's space holds. A space that holds
// nothing answers zero counts and records nothing.
func (e *Eraser) Erase(ctx context.Context, owner string) (admin.Erased, error) {
	// The work outlives the caller's connection: a half-finished erasure is
	// one a repeat must finish, and nothing is gained by abandoning it.
	ctx = context.WithoutCancel(ctx)

	uploads, err := e.o.Rows.OpenUploads(ctx, e.o.DB.Querier(), owner)
	if err != nil {
		return admin.Erased{}, err
	}
	for _, u := range uploads {
		if err := e.o.Bucket.AbortMultipart(ctx, u.ObjectID.Key(e.o.Prefix), u.UploadID); err != nil {
			return admin.Erased{}, fmt.Errorf("erasure: discard the parts of an open upload of %q: %w", owner, err)
		}
	}

	var gone store.SpaceErased
	err = e.o.DB.Tx(ctx, func(q store.Querier) error {
		var err error
		if gone, err = e.o.Rows.Erase(ctx, q, owner, string(events.ActionSpaceDelete)); err != nil {
			return err
		}
		if gone.Total() == 0 {
			return nil
		}
		// Last in the transaction, after the rows it describes and after the
		// space's own log, which the line above emptied. The detail is the
		// counts and nothing that names a file.
		_, err = e.o.Log.Append(ctx, q, events.Event{
			Owner: owner, Action: events.ActionSpaceDelete, Actor: auth.CallerFrom(ctx).Subject,
			Detail: map[string]any{
				"files": gone.Files, "trashed_files": gone.Trashed, "versions": gone.Versions,
				"objects": len(gone.Objects),
				"stars":   gone.Stars, "shares": gone.Shares, "links": gone.Links,
				"stars_held": gone.StarsHeld, "shares_held": gone.SharesHeld,
				"uploads": gone.Uploads, "events": gone.Events,
			},
		})
		return err
	})
	if err != nil {
		return admin.Erased{}, err
	}

	return admin.Erased{
		Files: gone.Files, Trashed: gone.Trashed, Versions: gone.Versions,
		Objects: e.dropBytes(ctx, owner, gone.Objects),
		Stars:   gone.Stars, Shares: gone.Shares, Links: gone.Links,
		StarsHeld: gone.StarsHeld, SharesHeld: gone.SharesHeld,
		Uploads: gone.Uploads, Events: gone.Events, Usage: gone.Usage, Subject: gone.Subject,
	}, nil
}

// dropBytes deletes the objects no remaining row references, after the rows
// are gone, and answers how many went. A failure is logged and the object is
// left for the reconciler's orphan pass; it never fails the deletion.
func (e *Eraser) dropBytes(ctx context.Context, owner string, ids []object.ID) int64 {
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		referenced, err := e.o.Referenced(ctx, e.o.DB.Querier(), id)
		if err != nil {
			slog.WarnContext(ctx, "erasure: the references of an object could not be read; the reconciler removes it if nothing names it",
				"space", owner, "object", string(id), "err", err)
			continue
		}
		if !referenced {
			keys = append(keys, id.Key(e.o.Prefix))
		}
	}
	var dropped int64
	for part := range slices.Chunk(keys, batch) {
		if err := e.o.Bucket.DeleteMany(ctx, part); err != nil {
			slog.WarnContext(ctx, "erasure: the bytes of a deleted space were not removed; the reconciler removes them as orphans",
				"space", owner, "keys", len(part), "err", err)
			continue
		}
		dropped += int64(len(part))
	}
	return dropped
}
