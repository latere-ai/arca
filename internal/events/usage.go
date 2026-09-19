// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"fmt"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/store"
)

// Limit is the byte allowance one authorizer answer carried, and nothing
// else. Arca stores no limit: there is no column, no route, and no default,
// so a limit exists only for as long as the answer that carried it, which
// the decision cache of spec 006 bounds by its ttl.
//
// The zero value is no limit at all, which is what a self-hosted
// installation running the owner policy gets.
type Limit struct {
	bytes int64
	set   bool
}

// Unlimited is a space with no limit. Every write of any size is admitted.
func Unlimited() Limit { return Limit{} }

// LimitBytes is the limit an answer named.
func LimitBytes(n int64) Limit { return Limit{bytes: n, set: true} }

// Set reports whether the answer carried a limit.
func (l Limit) Set() bool { return l.set }

// Bytes answers the limit, meaningless when Set is false.
func (l Limit) Bytes() int64 { return l.bytes }

// LimitOf reads limits.quota_bytes off an authorizer's answer. An answer
// carrying no limits object, or a limits object naming no quota_bytes,
// leaves the space unlimited.
//
// The ttl is not read here. A decision lives as long as the client of spec
// 006 caches it, so the limit a handler passes in is the limit of the answer
// it is acting on and expires with it.
func LimitOf(d authz.Decision) (Limit, error) {
	var limits struct {
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if err := d.DecodeLimits(&limits); err != nil {
		return Unlimited(), fmt.Errorf("events: read limits.quota_bytes: %w", err)
	}
	if limits.QuotaBytes == nil {
		return Unlimited(), nil
	}
	return LimitBytes(*limits.QuotaBytes), nil
}

// OverLimitError is a charge the answer's limit does not admit. A handler
// answers it 413 quota_exceeded, with Used and Limit in the developer detail
// (spec 013).
type OverLimitError struct {
	// Owner is the space.
	Owner string
	// Used is what the space held before the charge.
	Used int64
	// Limit is what the answer allowed.
	Limit int64
	// Delta is what the write would have added.
	Delta int64
}

// Error is the developer sentence, which names the two figures a caller
// needs and no store, query, or key.
func (e *OverLimitError) Error() string {
	return fmt.Sprintf("events: the space holds %d bytes of the %d this answer allows, and the write adds %d",
		e.Used, e.Limit, e.Delta)
}

// Space is one row of the ledger.
type Space struct {
	// Owner is the space, the subject <issuer>|<sub>.
	Owner string
	// Bytes is what the ledger says it holds.
	Bytes int64
}

// Usage is what one space holds, as its owner reads it off a root listing of
// spec 005: the bytes the ledger counts and the live paths under them.
//
// Both are read from the tables that own them and neither is recomputed,
// which is the rule spec 012's overview states. The ledger is the number a
// platform bills; the sum over the rows is what the reconciliation pass
// compares it against, and the two are allowed to differ until that pass
// runs. Summing here would hide the drift the pass exists to report.
type Usage struct {
	// Bytes is the usage the ledger holds for the space.
	Bytes int64
	// Files is the live paths of the space, trash excluded.
	Files int64
}

// Ledger counts the bytes a space holds.
//
// Every delta is applied inside the transaction of the write that moves the
// bytes, and the comparison against the answer's limit reads the row in that
// same transaction. A ledger read or write that fails therefore takes the
// write down with it: a number that cannot be written is a number that stops
// being current, and the failure belongs to the caller (spec 015).
type Ledger interface {
	// Charge applies a signed delta and answers what the space holds
	// afterwards, refusing the charge when the limit the answer carried does
	// not admit it. A delta at or below zero is never refused, because a
	// full space must stay shrinkable.
	Charge(ctx context.Context, q store.Querier, owner string, delta int64, limit Limit) (int64, error)
	// Release gives bytes back. It is Charge of a negative delta by another
	// name, and it is never refused.
	Release(ctx context.Context, q store.Querier, owner string, bytes int64) (int64, error)
	// Read answers what the ledger says the space holds. A space with no row
	// holds nothing.
	Read(ctx context.Context, q store.Querier, owner string) (int64, error)
	// Usage answers the bytes and the live paths of one space, for the root
	// listing of spec 005 an owner reads its own usage off.
	Usage(ctx context.Context, q store.Querier, owner string) (Usage, error)
	// Recompute sums the rows that hold the space's bytes, which is what the
	// counter is checked against once per reaper run.
	Recompute(ctx context.Context, q store.Querier, owner string) (int64, error)
	// Correct writes a recomputed value where the row still holds the value
	// the caller read, so a correction racing a live charge loses and is
	// recomputed on the next run instead of erasing the charge.
	Correct(ctx context.Context, q store.Querier, owner string, was, now int64) (bool, error)
	// Spaces answers one page of the ledger, ordered by owner, for the pass
	// that walks every space.
	Spaces(ctx context.Context, q store.Querier, cursor string, limit int) ([]Space, error)
}

// ledger is the ledger over Postgres.
type ledger struct{}

// NewLedger answers the ledger over Postgres.
func NewLedger() Ledger { return ledger{} }

// Delta reports the bytes a write adds to the space it lands in, which is
// not always its size. Every write path routes its charge through here, so
// the two overwrite cases cannot diverge.
//
//	a new object            its size
//	a versioned overwrite   the new size, because the replaced object
//	                        survives as a version and the space grew by all
//	                        of it
//	a non-versioned         the new size less the size it replaced, because
//	overwrite               no second copy is kept
//
// replaced is zero when the write creates a path, which makes the two
// branches agree there.
func Delta(size, replaced int64, versioned bool) int64 {
	if versioned {
		return size
	}
	return size - replaced
}

// Charge applies a signed delta and answers the new total.
//
// The row is written first and compared afterwards, in one statement and one
// round trip, because the comparison has to read the number this write
// produced and not the number before it. A refusal leaves the charge in the
// caller's transaction, which the caller rolls back with the write it was
// refusing; a caller that charges outside a transaction has nothing to roll
// back and is the bug this contract names.
func (l ledger) Charge(ctx context.Context, q store.Querier, owner string, delta int64, limit Limit) (int64, error) {
	total, err := l.apply(ctx, q, owner, delta)
	if err != nil {
		return 0, err
	}
	if delta <= 0 || !limit.Set() || total <= limit.Bytes() {
		return total, nil
	}
	return total, &OverLimitError{Owner: owner, Used: total - delta, Limit: limit.Bytes(), Delta: delta}
}

// Release gives bytes back.
func (l ledger) Release(ctx context.Context, q store.Querier, owner string, bytes int64) (int64, error) {
	return l.apply(ctx, q, owner, -bytes)
}

// apply is the one statement every delta goes through: an upsert that adds a
// signed delta to the row and answers the new total.
//
// The total is clamped at zero, because the column refuses a negative and a
// delete must never be refused. A ledger that had drifted low and is clamped
// here disagrees with the rows that hold the bytes, which is exactly what the
// reconciliation pass of internal/reaper reports and corrects.
func (ledger) apply(ctx context.Context, q store.Querier, owner string, delta int64) (int64, error) {
	var total int64
	err := q.QueryRow(ctx, `
		INSERT INTO space_usage (owner, bytes) VALUES ($1, GREATEST(0, $2))
		ON CONFLICT (owner) DO UPDATE
		   SET bytes = GREATEST(0, space_usage.bytes + $2), updated_at = now()
		RETURNING bytes`, owner, delta).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("events: charge %d bytes to %q: %w", delta, owner, err)
	}
	return total, nil
}

// Read answers what the ledger says the space holds.
func (ledger) Read(ctx context.Context, q store.Querier, owner string) (int64, error) {
	var bytes int64
	err := q.QueryRow(ctx,
		`SELECT COALESCE((SELECT bytes FROM space_usage WHERE owner = $1), 0)`, owner).Scan(&bytes)
	if err != nil {
		return 0, fmt.Errorf("events: read the usage of %q: %w", owner, err)
	}
	return bytes, nil
}

// Usage answers the bytes and the live paths of one space, in one statement
// and one round trip, so a listing that reports them costs one query beside
// its page.
//
// The bytes are the ledger's row and the paths are counted off the rows,
// which is the split of spec 012's overview: one number is billed and the
// other is what the space looks like. A space with no ledger row holds
// nothing, and one with no path counts none.
func (ledger) Usage(ctx context.Context, q store.Querier, owner string) (Usage, error) {
	var u Usage
	err := q.QueryRow(ctx, `
		SELECT COALESCE((SELECT bytes FROM space_usage WHERE owner = $1), 0),
		       (SELECT COUNT(*) FROM files WHERE owner = $1 AND deleted_at IS NULL)`,
		owner).Scan(&u.Bytes, &u.Files)
	if err != nil {
		return Usage{}, fmt.Errorf("events: read what %q holds: %w", owner, err)
	}
	return u, nil
}

// Recompute sums the rows that hold the space's bytes.
//
// Three tables, and the third is not optional. An upload session is charged
// its declared bytes the moment it opens, so that a caller cannot hold a
// thousand sessions and fit them all under one limit (spec 010). A
// recomputation that summed only the two settled tables would answer less
// than the ledger holds, and the reaper's reconciliation would then write
// that lower number over the live charge: open a session, wait one reap
// interval, and the charge is gone. Repeat it and a space's usage never
// reflects what its sessions hold.
//
// This statement was written when spec 007's table did not exist yet and
// said so. It exists.
func (ledger) Recompute(ctx context.Context, q store.Querier, owner string) (int64, error) {
	var bytes int64
	err := q.QueryRow(ctx, `
		SELECT (SELECT COALESCE(SUM(size_bytes), 0)     FROM files           WHERE owner = $1)
		     + (SELECT COALESCE(SUM(size_bytes), 0)     FROM file_versions   WHERE owner = $1)
		     + (SELECT COALESCE(SUM(declared_size), 0)  FROM upload_sessions WHERE owner = $1)`,
		owner).Scan(&bytes)
	if err != nil {
		return 0, fmt.Errorf("events: recompute the usage of %q: %w", owner, err)
	}
	return bytes, nil
}

// Correct writes a recomputed value where the row still holds what the
// caller read.
func (ledger) Correct(ctx context.Context, q store.Querier, owner string, was, now int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE space_usage SET bytes = $3, updated_at = now()
		 WHERE owner = $1 AND bytes = $2`, owner, was, now)
	if err != nil {
		return false, fmt.Errorf("events: correct the usage of %q: %w", owner, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Spaces answers one page of the ledger.
func (ledger) Spaces(ctx context.Context, q store.Querier, cursor string, limit int) ([]Space, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("events: walk the ledger: the page holds %d rows", limit)
	}
	rows, err := q.Query(ctx, `
		SELECT owner, bytes FROM space_usage
		 WHERE owner > $1 ORDER BY owner LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("events: walk the ledger: %w", err)
	}
	defer rows.Close()

	page := make([]Space, 0, limit)
	for rows.Next() {
		var s Space
		if err := rows.Scan(&s.Owner, &s.Bytes); err != nil {
			return nil, fmt.Errorf("events: walk the ledger: %w", err)
		}
		page = append(page, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: walk the ledger: %w", err)
	}
	return page, nil
}
