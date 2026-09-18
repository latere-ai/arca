// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"latere.ai/x/arca/internal/store"
)

// sweepPage is how many expired sessions one pass takes. A session is a row
// with a bucket call behind it, so the page is small: a backlog is worked
// through over several runs rather than in one pass holding a connection
// open for thousands of aborts.
const sweepPage = 100

// Sweep is pass 4 of spec 010's table, implemented by the spec that owns the
// table it reads (reaper.Pass). A session past its deadline is a client that
// stopped: its parts are invisible to object listing, so the row is the only
// durable pointer to them and nothing else in either store could find them.
//
// The order is the abort's own. The parts go first and the row only once the
// store has taken them, so a failure between the two leaves the row for the
// next run rather than stranding parts for the life of the bucket. The
// declared bytes were charged when the session opened, and the same
// transaction that deletes the row gives them back.
//
// A dry run counts and changes nothing, which this pass can do honestly:
// what it would change is a query, not a write.
func (s *Service) Sweep(ctx context.Context, q store.Querier, now time.Time, dry bool) (int, error) {
	expired, err := s.sessions.Expired(ctx, q, now, sweepPage)
	if err != nil {
		return 0, err
	}
	if dry {
		return len(expired), nil
	}
	var (
		closed int
		errs   []error
	)
	for _, held := range expired {
		if err := s.close(ctx, held); err != nil {
			// The row is kept, so the next run tries again. A store that is
			// down must not take the rest of the page down with it.
			slog.WarnContext(ctx, "uploads: an expired session would not close",
				"session", held.ID, "owner", held.Owner, "error", err)
			errs = append(errs, err)
			continue
		}
		s.metrics.UploadSession(sessionExpired)
		closed++
	}
	return closed, errors.Join(errs...)
}

// Open answers how many sessions are open at that instant, which is what
// arca_upload_sessions_open reads at every scrape (spec 018). It changes
// nothing and holds no state: the number is the database's, because a
// session opened on one replica is open on all of them.
func (s *Service) Open(ctx context.Context, now time.Time) (int64, error) {
	return s.sessions.CountOpen(ctx, s.content.Querier(), now)
}
