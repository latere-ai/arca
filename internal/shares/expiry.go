// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"context"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/arca/internal/store"
)

// Pass 7 of spec 010's table, implemented by the spec that owns the rows it
// removes: a grant that expired long ago leaves, and an object whose last
// public grant expired stops being public.
//
// It is not a method of [Service], and the reason is the same as the
// tombstone pass of spec 009: a service refuses to build without an
// authorizer, this pass puts no question, and binding it to the service
// would keep it off the arcad reap process, which starts no verifier.
//
// Expiry needs no sweep to take effect. Covering and every token lookup
// filter on expires_at, so a grant stops granting the moment it expires
// whether or not this has run (spec 008). What a public grant put on stays
// until something takes it off, though: the row's mark sends the owner's
// reads to ARCA_PUBLIC_CDN_URL, and the bucket's ACL serves the object to
// anyone who has its URL. The first half of the pass takes both off once no
// live public grant names the object, which is what the revoke of the last
// one does. The second half removes a row that has granted nothing for a
// whole retention window, which is housekeeping and never a change to who
// may act on a space. Neither appends an event: the log carries what
// happened to a space, and the expiry is what happened, written on the grant
// when it was made.

// ExpiryOptions is what the node builds the pass from.
type ExpiryOptions struct {
	// DB is the pool and its transactions: a lapsed object's row is held
	// while the pass decides it. Required.
	DB Database
	// Store is the grants query set of spec 008. Required.
	Store store.Shares
	// Publisher is the bucket a lapsed object's ACL is taken off. A build
	// with none clears the row's mark and stamps nothing, as the service
	// does.
	Publisher Publisher
	// BucketPrefix is ARCA_BUCKET_PREFIX, which the key of an object
	// derives from (spec 003).
	BucketPrefix string
	// Window is how long an expired grant is kept before it leaves.
	// ARCA_TRASH_RETENTION is what the node passes: a revoked or expired
	// grant is kept for an audit the same time a deleted object is kept for
	// a restore, and spec 009 says there is no second retention setting.
	Window time.Duration
}

// Expiry clears the objects whose last public grant expired and removes the
// grants whose expiry has long passed.
type Expiry struct {
	publicity
	db     Database
	window time.Duration
}

// lapsedPage is how many lapsed objects one pass takes. Each is a bucket
// call with a transaction behind it, so the page is small, as pass 4's is:
// a backlog is worked through over several runs.
const lapsedPage = 100

// NewExpiry builds the pass. A window of no time would delete a grant in the
// same second it expired, which leaves an audit nothing to read.
func NewExpiry(o ExpiryOptions) (*Expiry, error) {
	switch {
	case o.Store == nil:
		return nil, errNoStore
	case o.DB == nil:
		return nil, errNoDatabase
	case o.Window <= 0:
		return nil, errors.New("shares: a window of no time, which removes a grant in the second it expires")
	}
	return &Expiry{
		store: o.Store, publisher: o.Publisher, bucketPrefix: o.BucketPrefix,
		db: o.DB, window: o.Window,
	}, nil
}

// Sweep runs the pass once and answers how many grants left plus how many
// objects it cleared.
//
// The purge runs first, so a dry run counts what a live run changes: the
// purge then reads the state the count read, and an object cleared in this
// run frees its grant for the next run's purge rather than this one's. A
// dry run counts and changes nothing, which this pass can do honestly: the
// condition the delete carries is the condition the count reads, and the
// objects it would clear are a query.
//
// A failed purge does not stop the clears, which are what a reader without
// a token still reaches, and every failure is answered joined.
func (e *Expiry) Sweep(ctx context.Context, q store.Querier, now time.Time, dry bool) (int, error) {
	before := now.Add(-e.window)
	if dry {
		n, err := e.store.Expired(ctx, q, before)
		if err != nil {
			return 0, err
		}
		lapsed, err := e.store.Lapsed(ctx, q, lapsedPage)
		if err != nil {
			return 0, err
		}
		return int(n) + len(lapsed), nil
	}
	purged, purgeErr := e.store.PurgeExpired(ctx, q, before)
	cleared, clearErr := e.unpublish(ctx, q)
	return int(purged) + cleared, errors.Join(purgeErr, clearErr)
}

// unpublish clears one page of lapsed objects. An object that will not
// clear is kept for the next run and does not take the rest of the page
// down with it.
func (e *Expiry) unpublish(ctx context.Context, q store.Querier) (int, error) {
	lapsed, err := e.store.Lapsed(ctx, q, lapsedPage)
	if err != nil {
		return 0, err
	}
	var (
		cleared int
		errs    []error
	)
	for _, f := range lapsed {
		done, err := e.clear(ctx, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("shares: clear the lapsed public object %s: %w", f.ObjectID, err))
			continue
		}
		if done {
			cleared++
		}
	}
	return cleared, errors.Join(errs...)
}

// clear takes one lapsed object's publicity off, the bucket first.
//
// The row stays marked until the bucket has answered, so a refused call
// leaves the object as the next run's Lapsed finds it, and the expired grant
// that names it is kept from the purge for as long as it is. The order is
// the reverse of withdraw's, and for the retry's sake: a revoke's retry is
// the caller's same request against a grant still live, while this pass's
// retry is the next read of Lapsed, which finds only a row still marked. A
// row cleared before a bucket that then refused, with a database that would
// not put the mark back, would be an object readable without a token that
// nothing would ever look at again.
//
// The row is then held and the other grants read again, as every write of a
// public grant does. A public link made after Lapsed read the row found it
// marked and stamped nothing, so the row stays marked and the ACL this took
// off is put back for that link. Otherwise the mark is cleared. It answers
// whether this call cleared the mark.
func (e *Expiry) clear(ctx context.Context, f store.File) (bool, error) {
	if err := e.stamp(ctx, f, false); err != nil {
		return false, err
	}
	// No id: any live public grant on the object holds it public.
	g := store.Grant{Owner: f.Owner, PathPrefix: f.Path, GranteeKind: store.GranteePublic}
	relinked, cleared := false, false
	err := e.db.Tx(ctx, func(q store.Querier) error {
		held, err := e.hold(ctx, q, g)
		if err != nil || !held.IsPublic {
			return err
		}
		if relinked, err = e.markedElsewhere(ctx, q, g); err != nil || relinked {
			return err
		}
		written, err := e.publicRow(ctx, q, g, false)
		cleared = written.ObjectID != ""
		return err
	})
	if err != nil {
		return false, fmt.Errorf("unmark the row: %w", err)
	}
	if relinked {
		if err := e.stamp(ctx, f, true); err != nil {
			return false, fmt.Errorf("put back the ACL a new public link holds: %w", err)
		}
	}
	return cleared, nil
}
