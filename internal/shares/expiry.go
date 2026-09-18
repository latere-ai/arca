// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"context"
	"errors"
	"time"

	"latere.ai/x/arca/internal/store"
)

// Pass 7 of spec 010's table, implemented by the spec that owns the rows it
// removes: a grant that expired long ago leaves.
//
// It is not a method of [Service], and the reason is the same as the
// tombstone pass of spec 009: a service refuses to build without an
// authorizer, this pass puts no question, and binding it to the service
// would keep it off the arcad reap process, which starts no verifier.
//
// Expiry needs no sweep to take effect. Covering and every token lookup
// filter on expires_at, so a grant stops granting the moment it expires
// whether or not this has run (spec 008); what this removes is a row that
// has granted nothing for a whole retention window. It is housekeeping and
// never a change to who may act on a space, so it appends no event: the
// log carries what happened to a space, and nothing happened here.

// ExpiryOptions is what the node builds the pass from.
type ExpiryOptions struct {
	// Store is the grants query set of spec 008.
	Store store.Shares
	// Window is how long an expired grant is kept before it leaves.
	// ARCA_TRASH_RETENTION is what the node passes: a revoked or expired
	// grant is kept for an audit the same time a deleted object is kept for
	// a restore, and spec 009 says there is no second retention setting.
	Window time.Duration
}

// Expiry removes the grants whose expiry has long passed.
type Expiry struct {
	store  store.Shares
	window time.Duration
}

// NewExpiry builds the pass. A window of no time would delete a grant in the
// same second it expired, which leaves an audit nothing to read.
func NewExpiry(o ExpiryOptions) (*Expiry, error) {
	switch {
	case o.Store == nil:
		return nil, errNoStore
	case o.Window <= 0:
		return nil, errors.New("shares: a window of no time, which removes a grant in the second it expires")
	}
	return &Expiry{store: o.Store, window: o.Window}, nil
}

// Sweep runs the pass once and answers how many grants left.
//
// A dry run counts and changes nothing, which this pass can do honestly: the
// condition the delete carries is the condition the count reads, so the
// number reported is the number a live run removes.
func (e *Expiry) Sweep(ctx context.Context, q store.Querier, now time.Time, dry bool) (int, error) {
	before := now.Add(-e.window)
	if dry {
		n, err := e.store.Expired(ctx, q, before)
		return int(n), err
	}
	n, err := e.store.PurgeExpired(ctx, q, before)
	return int(n), err
}
