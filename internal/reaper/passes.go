// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package reaper

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// orphanBytes is pass 1, invariant 1's cleanup: a put writes the bucket
// before the database, so a crash between the two leaves bytes nothing
// points at.
//
// A key seen for the first time inside the grace window is left alone, which
// is what stops the pass from deleting bytes whose row is one statement away
// from existing. The referenced set is the union internal/store holds,
// which is three tables from spec 007 and not just files: an incomplete
// multipart's parts are invisible to a listing, so the session row is their
// only pointer.
func (r *Reconciler) orphanBytes(ctx context.Context, q store.Querier, f *Findings, _ spaces) error {
	now, seen, startAfter := r.opts.Now(), map[string]bool{}, ""
	for {
		listing, err := r.opts.Bucket.List(ctx, r.opts.Prefix, startAfter, page)
		if err != nil {
			return fmt.Errorf("list the bucket: %w", err)
		}
		if len(listing.Keys) == 0 {
			break
		}
		startAfter = listing.Keys[len(listing.Keys)-1]
		for _, key := range listing.Keys {
			seen[key] = true
			if err := r.orphan(ctx, q, key, now, f); err != nil {
				return err
			}
		}
		// The page ends on the store's truncation flag and never on a short
		// result: a store may answer fewer keys than the maximum while more
		// remain, and stopping there would leave orphans unswept.
		if !listing.Truncated {
			break
		}
	}
	// A candidate that vanished from the bucket on its own stops being one,
	// so the window is armed afresh if the key ever comes back.
	maps.DeleteFunc(r.candidates, func(key string, _ time.Time) bool { return !seen[key] })
	return nil
}

// mine answers the object a key names, and false for a key this
// installation did not write. A key it cannot read is a key it must not
// delete: several installations share one bucket by taking a prefix each,
// and a key in another shape under this one is somebody's.
func (r *Reconciler) mine(key string) (object.ID, bool) {
	id, err := object.ParseKey(r.opts.Prefix, key)
	return id, err == nil
}

// orphan decides one key.
func (r *Reconciler) orphan(ctx context.Context, q store.Querier, key string, now time.Time, f *Findings) error {
	id, ok := r.mine(key)
	if !ok {
		return nil
	}
	referenced, err := store.ObjectReferenced(ctx, q, id)
	if err != nil {
		return err
	}
	if referenced {
		delete(r.candidates, key)
		return nil
	}
	first, known := r.candidates[key]
	if !known {
		r.candidates[key] = now
	}
	if !known || now.Sub(first) < OrphanGrace {
		f.Add(KindOrphanCandidate, Found, 1)
		return nil
	}

	f.Add(KindOrphanObject, Found, 1)
	if r.opts.DryRun {
		return nil
	}
	if err := r.opts.Bucket.Delete(ctx, key); err != nil {
		// The bytes stay, the candidate stays, and the next run tries
		// again. A growing found beside a flat repaired is spec 018's
		// alert.
		f.Add(KindOrphanObject, Deferred, 1)
		r.opts.Log.WarnContext(ctx, "the orphan was not deleted", "object", string(id), "err", err)
		return nil
	}
	delete(r.candidates, key)
	f.Add(KindOrphanObject, Repaired, 1)
	return nil
}

// orphanRows is pass 2, invariant 2's alarm. A row whose bytes the bucket
// does not hold is a lie the database is telling, and the right answer is a
// server error on read and a finding an operator looks at, never a quiet
// delete of the only record that something existed. This pass deletes
// nothing, in a dry run or otherwise.
func (r *Reconciler) orphanRows(ctx context.Context, q store.Querier, f *Findings, _ spaces) error {
	var errs []error
	for _, table := range []string{"files", "file_versions"} {
		if err := r.missingBytes(ctx, q, table, f); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", table, err))
		}
	}
	return errors.Join(errs...)
}

// zeroID is the cursor the walk of one table starts at, below every id the
// column holds.
const zeroID = "00000000-0000-0000-0000-000000000000"

// missingBytes walks one table's rows and asks the bucket for each object.
// The walk is keyset paginated on the object id, so an insert during it
// shifts nothing.
func (r *Reconciler) missingBytes(ctx context.Context, q store.Querier, table string, f *Findings) error {
	cursor := zeroID
	for {
		// table is one of two constants of this package and never a value a
		// caller chose, so it is the one part of a statement here that is
		// not a numbered parameter.
		rows, err := q.Query(ctx, `
			SELECT owner, object_id FROM `+table+`
			 WHERE object_id > $1 ORDER BY object_id LIMIT $2`, cursor, page)
		if err != nil {
			return err
		}
		type row struct {
			owner string
			id    object.ID
		}
		var held []row
		for rows.Next() {
			var x row
			if err := rows.Scan(&x.owner, &x.id); err != nil {
				rows.Close()
				return err
			}
			held = append(held, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(held) == 0 {
			return nil
		}
		cursor = string(held[len(held)-1].id)

		for _, x := range held {
			switch _, err := r.opts.Bucket.Head(ctx, x.id.Key(r.opts.Prefix)); {
			case errors.Is(err, blob.ErrNotFound):
				f.Add(KindMissingBytes, Found, 1)
				// The path is not on the line: a path carries what a person
				// called their file (spec 018).
				r.opts.Log.ErrorContext(ctx, "the bucket does not hold the object a row names",
					"space", x.owner, "object", string(x.id), "table", table)
			case err != nil:
				return err
			}
		}
		if len(held) < page {
			return nil
		}
	}
}

// purgeTrash is pass 5: a trashed object past ARCA_TRASH_RETENTION leaves
// for good.
//
// The rows go before the keys, which is invariant 1's order for a delete, so
// a failure between them leaves bytes pass 1 finds rather than a row whose
// object is gone. The ledger delta is applied in the transaction that
// removes the rows, so pass 10 has nothing to correct after a healthy run:
// the predecessor purged trash outside any transaction and kept no ledger at
// all.
func (r *Reconciler) purgeTrash(ctx context.Context, q store.Querier, f *Findings, changed spaces) error {
	const past = `files WHERE deleted_at IS NOT NULL AND deleted_at < $1`
	cutoff := r.opts.Now().Add(-r.opts.TrashRetention)
	if r.opts.DryRun {
		n, err := count(ctx, q, `SELECT count(*) FROM `+past, cutoff)
		f.Add(KindTrashPurged, Found, n)
		return err
	}

	type purged struct {
		owner, path string
		id          object.ID
		size        int64
	}
	var gone []purged
	err := r.opts.DB.Tx(ctx, func(tx store.Querier) error {
		rows, err := tx.Query(ctx, `DELETE FROM `+past+` RETURNING owner, path, object_id, size_bytes`, cutoff)
		if err != nil {
			return err
		}
		for rows.Next() {
			var x purged
			if err := rows.Scan(&x.owner, &x.path, &x.id, &x.size); err != nil {
				rows.Close()
				return err
			}
			gone = append(gone, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		freed := map[string]int64{}
		for _, x := range gone {
			freed[x.owner] += x.size
		}
		for _, owner := range slices.Sorted(maps.Keys(freed)) {
			if _, err := r.ledger.Release(ctx, tx, owner, freed[owner]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	f.Add(KindTrashPurged, Found, len(gone))
	f.Add(KindTrashPurged, Repaired, len(gone))
	keys := make([]string, 0, len(gone))
	for _, x := range gone {
		changed.touch(x.owner, KindTrashPurged, 1)
		// A purge event is the last thing said about an object, appended
		// after the rows are gone: the operation already happened.
		r.log.Note(ctx, q, events.Event{Owner: x.owner, Path: x.path, Action: events.ActionPurge,
			Detail: map[string]any{"size": x.size}})
		referenced, err := store.ObjectReferenced(ctx, q, x.id)
		if err != nil {
			return err
		}
		if !referenced {
			keys = append(keys, x.id.Key(r.opts.Prefix))
		}
	}
	return r.deleteKeys(ctx, keys, f)
}

// deleteKeys removes the bytes of rows that are already gone, in batches.
// A failure is a warning and not a failed run: the rows went first, so what
// is left is an orphan pass 1 sweeps.
func (r *Reconciler) deleteKeys(ctx context.Context, keys []string, f *Findings) error {
	for batch := range slices.Chunk(keys, page) {
		if err := r.opts.Bucket.DeleteMany(ctx, batch); err != nil {
			f.Add(KindOrphanObject, Deferred, len(batch))
			r.opts.Log.WarnContext(ctx, "the purged rows left their bytes behind", "keys", len(batch), "err", err)
		}
	}
	return nil
}

// pruneStars is pass 8: a star whose target has no row at all.
//
// A star on a trashed object is kept, because the object is restorable for
// the whole retention window and a star that vanished while its target was
// recoverable would be a second thing to restore. Spec 010's table says
// "no live files row" and criterion 16b says the restorable case keeps its
// star; the criterion is the narrower statement and is what this pass does.
func (r *Reconciler) pruneStars(ctx context.Context, q store.Querier, f *Findings, changed spaces) error {
	const stale = `stars s WHERE NOT EXISTS (
		SELECT 1 FROM files x WHERE x.owner = s.owner AND x.path = s.path)`
	if r.opts.DryRun {
		n, err := count(ctx, q, `SELECT count(*) FROM `+stale)
		f.Add(KindStarPruned, Found, n)
		return err
	}
	rows, err := q.Query(ctx, `DELETE FROM `+stale+` RETURNING s.owner`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return err
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	f.Add(KindStarPruned, Found, len(owners))
	f.Add(KindStarPruned, Repaired, len(owners))
	for _, owner := range owners {
		changed.touch(owner, KindStarPruned, 1)
	}
	return nil
}

// pruneLog is pass 9: the log is a tail and not an archive.
//
// It appends no event of its own. A pass that wrote a row every time it
// removed rows from the same table would be the one pass that grows what it
// prunes.
func (r *Reconciler) pruneLog(ctx context.Context, q store.Querier, f *Findings, _ spaces) error {
	before := r.opts.Now().Add(-LogRetention)
	if r.opts.DryRun {
		n, err := r.log.Older(ctx, q, before)
		f.Add(KindEventPruned, Found, int(n))
		return err
	}
	n, err := r.log.Prune(ctx, q, before)
	f.Add(KindEventPruned, Found, int(n))
	f.Add(KindEventPruned, Repaired, int(n))
	return err
}

// reconcileLedger is pass 10, and it runs last: every pass before it moves
// bytes, and a reconciliation over a moving ledger corrects numbers that
// were about to be right anyway.
//
// The correction is conditional on the value it read, so a correction racing
// a live charge loses and is recomputed on the next run instead of erasing
// the charge. A correction is a finding and not routine housekeeping: a
// ledger that keeps needing one has a write path that forgot its delta.
func (r *Reconciler) reconcileLedger(ctx context.Context, q store.Querier, f *Findings, changed spaces) error {
	cursor := ""
	for {
		ledger, err := r.ledger.Spaces(ctx, q, cursor, page)
		if err != nil {
			return err
		}
		if len(ledger) == 0 {
			return nil
		}
		cursor = ledger[len(ledger)-1].Owner
		for _, space := range ledger {
			if err := r.reconcileSpace(ctx, q, space, f, changed); err != nil {
				return err
			}
		}
		if len(ledger) < page {
			return nil
		}
	}
}

// reconcileSpace checks one row of the ledger and samples its size.
func (r *Reconciler) reconcileSpace(ctx context.Context, q store.Querier, space events.Space, f *Findings, changed spaces) error {
	if r.opts.Metrics != nil {
		// Once per run, per space, into a distribution and never into a
		// series of its own (spec 018).
		r.opts.Metrics.Usage(space.Bytes)
	}
	held, err := r.ledger.Recompute(ctx, q, space.Owner)
	if err != nil {
		return err
	}
	if held == space.Bytes {
		return nil
	}
	f.Add(KindUsageCorrected, Found, 1)
	r.opts.Log.WarnContext(ctx, "the ledger disagrees with the rows that hold the bytes",
		"space", space.Owner, "ledger", space.Bytes, "rows", held)
	if r.opts.DryRun {
		return nil
	}
	corrected, err := r.ledger.Correct(ctx, q, space.Owner, space.Bytes, held)
	if err != nil {
		return err
	}
	if !corrected {
		f.Add(KindUsageCorrected, Deferred, 1)
		return nil
	}
	f.Add(KindUsageCorrected, Repaired, 1)
	changed.touch(space.Owner, KindUsageCorrected, 1)
	return nil
}

// count answers one counting statement, which is how a dry run reports what
// a live run would change.
func count(ctx context.Context, q store.Querier, sql string, args ...any) (int, error) {
	var n int
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
