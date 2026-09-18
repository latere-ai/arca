// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// The query the administrative overview of spec 012 reads: one row per space
// that holds anything, with what it holds.
//
// Every other listing in this package reads one space. This one reads across
// all of them, which is why it is a single statement over the tables that
// hold a space's contents rather than a walk of the subject directory: a
// subject that made one request and stored nothing is not a space that holds
// anything, and the directory cannot tell the two apart.
//
// The counters are read from the tables that own them and not recomputed.
// Bytes is the ledger of spec 010 as the ledger holds it, which is the number
// a platform bills; the sum over the rows is what the reconciliation pass of
// that spec compares it against, and the two are allowed to differ until that
// pass runs. Summing here would hide the drift the pass exists to report.

// SpaceOverview is one space and what it holds, as one row of spec 012's
// overview. The link count is not here: the token grants are spec 008's
// table, and internal/admin counts them through a seam.
type SpaceOverview struct {
	// Owner is the space, the subject <issuer>|<sub>.
	Owner string
	// Bytes is the usage the ledger of spec 010 holds for the space.
	Bytes int64
	// Files is the live paths of the space, trash excluded.
	Files int64
	// TrashedBytes is the part of the usage that trashed paths hold, which
	// is what a reaper run would recover.
	TrashedBytes int64
	// Workspaces is the live workspaces of the space, the soft deleted ones
	// excluded.
	Workspaces int64
	// Leases is the workspaces of the space a writer holds right now. A
	// lease past its deadline is not held: the next attach takes it, so
	// counting it would report a state the next request disagrees with.
	Leases int64
	// LastWriteAt is when a path of the space last changed, nil for a space
	// that holds no path at all.
	LastWriteAt *time.Time
}

// Admin is the query set spec 012's overview reads. A handler takes the
// interface, so a test passes a fake; NewAdmin answers the one over Postgres.
type Admin interface {
	// Overview answers one page of the spaces that hold anything, ordered by
	// the subject, which is the page's cursor. A space every counter of which
	// is zero is left out: it holds nothing, and a page of such rows is a
	// page an administrator reads nothing from.
	Overview(ctx context.Context, q Querier, cursor string, limit int) ([]SpaceOverview, error)
}

// admin is the query set over Postgres.
type admin struct{}

// NewAdmin answers the query set over Postgres.
func NewAdmin() Admin { return admin{} }

// overviewSQL is the one statement the overview reads.
//
// The inner select derives the spaces from the three tables that hold
// something, then joins each table's counters onto them; the outer select
// drops a space whose counters are all zero and pages by the subject. The
// lease predicate is the workspace service's own rule, expressed once here
// so a lapsed lease is not counted as a held one.
const overviewSQL = `
SELECT owner, bytes, files, trashed_bytes, workspaces, leases, last_write_at
  FROM (
	SELECT s.owner                        AS owner,
	       COALESCE(u.bytes, 0)           AS bytes,
	       COALESCE(f.live, 0)            AS files,
	       COALESCE(f.trashed_bytes, 0)   AS trashed_bytes,
	       COALESCE(w.live, 0)            AS workspaces,
	       COALESCE(w.leases, 0)          AS leases,
	       f.last_write_at                AS last_write_at
	  FROM (
	          SELECT owner FROM space_usage WHERE bytes > 0
	    UNION SELECT owner FROM files
	    UNION SELECT owner FROM workspaces WHERE deleted_at IS NULL
	  ) s
	  LEFT JOIN space_usage u ON u.owner = s.owner
	  LEFT JOIN (
	    SELECT owner,
	           COUNT(*) FILTER (WHERE deleted_at IS NULL) AS live,
	           COALESCE(SUM(size_bytes) FILTER (WHERE deleted_at IS NOT NULL), 0) AS trashed_bytes,
	           MAX(updated_at) AS last_write_at
	      FROM files GROUP BY owner
	  ) f ON f.owner = s.owner
	  LEFT JOIN (
	    SELECT owner,
	           COUNT(*) FILTER (WHERE deleted_at IS NULL) AS live,
	           COUNT(*) FILTER (WHERE deleted_at IS NULL
	                              AND writer_holder IS NOT NULL
	                              AND writer_expires_at > now()) AS leases
	      FROM workspaces GROUP BY owner
	  ) w ON w.owner = s.owner
  ) space
 WHERE owner > $1
   AND (bytes > 0 OR files > 0 OR trashed_bytes > 0 OR workspaces > 0)
 ORDER BY owner
 LIMIT $2`

// Overview answers one page of the spaces that hold anything.
func (admin) Overview(ctx context.Context, q Querier, cursor string, limit int) ([]SpaceOverview, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: read the administrative overview: the page holds %d rows", limit)
	}
	rows, err := q.Query(ctx, overviewSQL, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read the administrative overview: %w", err)
	}
	defer rows.Close()

	page := make([]SpaceOverview, 0, limit)
	for rows.Next() {
		var s SpaceOverview
		if err := rows.Scan(&s.Owner, &s.Bytes, &s.Files, &s.TrashedBytes,
			&s.Workspaces, &s.Leases, &s.LastWriteAt); err != nil {
			return nil, fmt.Errorf("store: read the administrative overview: %w", err)
		}
		page = append(page, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the administrative overview: %w", err)
	}
	return page, nil
}
