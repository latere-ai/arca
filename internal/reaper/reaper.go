// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package reaper reconciles the two stores of spec 001 and expires what has
// a deadline. It is spec 010's.
//
// The passes exist because the bucket and the database fail independently:
// a put writes the bucket first, so a crash between the two leaves bytes
// nothing points at, and a delete writes the database first, so a crash
// between the two leaves bytes whose row is already gone. Invariants 1 and 2
// say what each failure leaves behind, and the passes below are what removes
// or reports it.
//
// Every pass is idempotent and safe to run while another replica runs it,
// because every destructive statement is conditional on the state it read.
// There is no lease: two replicas issuing the same conditional delete give
// one deletion and one no-op, which is cheaper than a lock and removes the
// failure mode where a crashed holder stops housekeeping for everyone.
//
// A pass that fails does not stop the run. Each pass is independent, so
// stopping at the first failure would mean a bucket that is down keeps the
// ledger from ever reconciling; the run collects what failed and reports the
// run as a failure at the end.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"latere.ai/x/pkg/wait"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
)

// Kind is one thing the reconciler can find. It is the closed vocabulary
// spec 018 labels arca_reaper_findings_total with, one member per row of
// spec 010's pass table plus the two the predecessor's summary counted.
type Kind string

// The twelve kinds.
const (
	// KindOrphanObject is a key under the prefix that no row references
	// (pass 1).
	KindOrphanObject Kind = "orphan_object"
	// KindOrphanCandidate is such a key seen inside the grace window, which
	// is what stops the pass from deleting bytes whose row is one statement
	// away from existing (pass 1).
	KindOrphanCandidate Kind = "orphan_candidate"
	// KindMissingBytes is a row whose key the bucket does not hold: invariant
	// 2 broken, and the one finding a human always reads (pass 2).
	KindMissingBytes Kind = "missing_bytes"
	// KindWorkspacePurged is a soft deleted workspace past its window (pass
	// 6).
	KindWorkspacePurged Kind = "workspace_purged"
	// KindFilePurged is a row removed with the subtree its workspace held
	// (pass 6).
	KindFilePurged Kind = "file_purged"
	// KindTrashPurged is a trashed object past its retention (pass 5).
	KindTrashPurged Kind = "trash_purged"
	// KindShareExpired is a grant revoked for age (pass 7).
	KindShareExpired Kind = "share_expired"
	// KindEventPruned is a log row past thirty days (pass 9).
	KindEventPruned Kind = "event_pruned"
	// KindStarPruned is a star whose target no longer exists (pass 8).
	KindStarPruned Kind = "star_pruned"
	// KindVersionPruned is a superseded content removed by the retention
	// spec 005 owns.
	KindVersionPruned Kind = "version_pruned"
	// KindUploadAborted is an upload session idle past its ttl (pass 4).
	KindUploadAborted Kind = "upload_aborted"
	// KindUsageCorrected is a ledger row that disagreed with the rows
	// holding its bytes (pass 10). A healthy installation reports none: a
	// ledger that keeps needing one has a write path that forgot its delta.
	KindUsageCorrected Kind = "usage_corrected"
	// KindLeaseExpired is an attachment past its expiry, whose writer lease
	// the pass cleared (pass 3). It is the one member spec 018's table does
	// not list: that table has a counter of its own for the expiry,
	// arca_lease_expiries_total, and a pass whose findings no run reports is
	// a pass an operator cannot see run at all.
	KindLeaseExpired Kind = "lease_expired"
)

// kinds is the vocabulary in the order spec 018 lists it, with the
// thirteenth member above at the end.
var kinds = []Kind{
	KindOrphanObject, KindOrphanCandidate, KindMissingBytes, KindWorkspacePurged,
	KindFilePurged, KindTrashPurged, KindShareExpired, KindEventPruned,
	KindStarPruned, KindVersionPruned, KindUploadAborted, KindUsageCorrected,
	KindLeaseExpired,
}

// Kinds answers the closed vocabulary, in the order spec 018 lists it.
func Kinds() []Kind { return slices.Clone(kinds) }

// Outcome is what a count says happened to what the pass found.
type Outcome string

// The three outcomes, which are spec 018's label values.
const (
	// Found is what the pass saw.
	Found Outcome = "found"
	// Repaired is what it resolved.
	Repaired Outcome = "repaired"
	// Deferred is what it saw and left, which is a finding an operator
	// reads: a growing found beside a flat repaired is the alert.
	Deferred Outcome = "deferred"
)

// Findings is what one run found, one count per kind and outcome.
//
// A dry run fills it with exactly what a live run would, which is the whole
// point of a dry run: the predecessor's dry run skipped three of its passes
// before counting, so what it printed was not what a run would do.
type Findings struct{ counts map[finding]int }

// finding keys one count.
type finding struct {
	Kind    Kind
	Outcome Outcome
}

// Row is one count, for a caller that renders the table.
type Row struct {
	Kind    Kind
	Outcome Outcome
	Count   int
}

// Add records n of one kind and outcome. A zero or negative n records
// nothing, so a pass that found nothing leaves no row.
func (f *Findings) Add(kind Kind, outcome Outcome, n int) {
	if n <= 0 {
		return
	}
	if f.counts == nil {
		f.counts = map[finding]int{}
	}
	f.counts[finding{kind, outcome}] += n
}

// Count answers one count.
func (f Findings) Count(kind Kind, outcome Outcome) int { return f.counts[finding{kind, outcome}] }

// Total answers every count of one kind.
func (f Findings) Total(kind Kind) int {
	var n int
	for _, outcome := range []Outcome{Found, Repaired, Deferred} {
		n += f.counts[finding{kind, outcome}]
	}
	return n
}

// Rows answers the counts in the vocabulary's order, so a log line and a
// test read the same table.
func (f Findings) Rows() []Row {
	rows := make([]Row, 0, len(f.counts))
	for _, kind := range kinds {
		for _, outcome := range []Outcome{Found, Repaired, Deferred} {
			if n := f.counts[finding{kind, outcome}]; n > 0 {
				rows = append(rows, Row{kind, outcome, n})
			}
		}
	}
	return rows
}

// attributes renders the findings for one log line.
func (f Findings) attributes() []any {
	attrs := make([]any, 0, len(f.counts)*2)
	for _, row := range f.Rows() {
		attrs = append(attrs, string(row.Kind)+"_"+string(row.Outcome), row.Count)
	}
	return attrs
}

// Metrics is where a run's counters go. Spec 018 owns the registry and the
// names; this is the seam it binds, so this package registers nothing and a
// process that exports nothing still runs the passes.
//
// No method takes a space. Usage is published in aggregate and never as a
// series per space, because a space is addressed by a subject and a subject
// on a scrape endpoint is a name anybody who can scrape the namespace reads
// (spec 018).
type Metrics interface {
	// Finding records n of one kind and outcome.
	Finding(kind Kind, outcome Outcome, n int)
	// Usage observes one space's bytes, once per run.
	Usage(bytes int64)
	// Run records one finished sequence and how long it took.
	Run(ok bool, took time.Duration)
}

// Pass is a reconciliation pass over a table a later spec creates.
//
// Four of spec 010's ten passes read tables that are not in the schema yet:
// the attachments of spec 009, the upload sessions of spec 007, the
// workspaces of spec 009, and the grants of spec 008. Each arrives as an
// implementation of this interface, built by the spec that creates its
// table and given to the reconciler here; a pass the reconciler was not
// given does not run and reports nothing.
type Pass interface {
	// Sweep runs the pass once and answers how many rows it found, which is
	// how many it changed unless dry is true. A dry sweep changes nothing in
	// either store, and that half of the contract is absolute.
	//
	// It reports the same number where the pass can count what it would
	// change without changing it. A pass whose every statement is a write
	// has no counting half to run, and such a pass reports nothing on a dry
	// run rather than a number it did not measure: under-reporting is a
	// finding an operator does not see, and mutating in a dry run is a
	// promise broken. The lease pass of spec 009 is the one such pass today.
	Sweep(ctx context.Context, q store.Querier, now time.Time, dry bool) (int, error)
}

// Database is what the reconciler needs of internal/store: a querier for a
// read, and one transaction for a write that has to move rows and the ledger
// together. *store.DB satisfies it.
type Database interface {
	Querier() store.Querier
	Tx(ctx context.Context, fn func(store.Querier) error) error
}

// The windows the reconciler works to. Two are configuration, in Options;
// these two are the design's.
const (
	// OrphanGrace is how long a key with no row is left alone. A put writes
	// the bucket before the database, so a key seen for the first time
	// inside this window may be one statement away from having a row. It is
	// at least the upload session ttl of spec 007, because an incomplete
	// multipart's parts are invisible to a listing.
	OrphanGrace = 24 * time.Hour
	// LogRetention is how long the log keeps a row. The log is a tail and
	// not an archive: an installation that needs durable history tails it
	// and keeps the result somewhere built for keeping things.
	LogRetention = 30 * 24 * time.Hour
	// page is how many keys or rows one round trip carries.
	page = 1000
)

// Options is what a reconciler is built from.
type Options struct {
	// DB is the database. Required.
	DB Database
	// Bucket is the object store. Required.
	Bucket blob.Store
	// Prefix is ARCA_BUCKET_PREFIX, the prefix the sweep of pass 1 lists
	// under and the only part of the bucket this installation owns.
	Prefix string
	// TrashRetention is ARCA_TRASH_RETENTION, how long a trashed object
	// stays restorable.
	TrashRetention time.Duration
	// DryRun reports every finding and changes nothing in either store.
	DryRun bool
	// Metrics is spec 018's seam. Nil records nothing.
	Metrics Metrics
	// Now is the clock, so a test moves time rather than waiting. Nil is
	// time.Now.
	Now func() time.Time
	// Log is where the run's lines go. Nil is the default logger.
	Log *slog.Logger
	// Leases, Sessions, Tombstones and Grants are passes 3, 4, 6 and 7,
	// whose tables specs 009, 007, 009 and 008 create. Nil does not run.
	Leases, Sessions, Tombstones, Grants Pass
}

// Reconciler runs the passes.
type Reconciler struct {
	opts   Options
	log    events.Log
	ledger events.Ledger
	// candidates carries the first-seen time of a key with no row across
	// runs, so the grace window works without asking the store for an
	// object's age. It lives in the process, so a restart re-arms the window
	// rather than shortening it.
	candidates map[string]time.Time
}

// New builds a reconciler. It reaches no store.
func New(o Options) (*Reconciler, error) {
	switch {
	case o.DB == nil:
		return nil, errors.New("reaper: the reconciler needs a database")
	case o.Bucket == nil:
		return nil, errors.New("reaper: the reconciler needs a bucket")
	case o.TrashRetention <= 0:
		return nil, fmt.Errorf("reaper: the trash retention is %s, and a window of no time purges what was deleted a moment ago", o.TrashRetention)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Reconciler{opts: o, log: events.NewLog(), ledger: events.NewLedger(), candidates: map[string]time.Time{}}, nil
}

// Loop runs the sequence every interval until ctx is done. An interval of
// zero or less runs nothing and returns at once, which is what
// ARCA_REAP_INTERVAL of 0 selects: the installation runs arcad reap as a
// process of its own instead.
func (r *Reconciler) Loop(ctx context.Context, interval time.Duration) {
	wait.Every(ctx, interval, func(ctx context.Context) {
		if _, err := r.Run(ctx); err != nil {
			r.opts.Log.ErrorContext(ctx, "the reconciliation did not finish", "err", err)
		}
	})
}

// Run executes the passes once, in the order spec 010 numbers them, and
// answers what they found.
//
// Every pass runs. A pass that fails is collected and the run is reported as
// a failure, because the passes are independent and a bucket that is down
// must not keep the ledger from reconciling.
func (r *Reconciler) Run(ctx context.Context) (Findings, error) {
	started := r.opts.Now()
	var (
		f       Findings
		changed = spaces{}
		errs    []error
	)
	q := r.opts.DB.Querier()
	for _, p := range []struct {
		name string
		run  func(context.Context, store.Querier, *Findings, spaces) error
	}{
		{"orphan bytes", r.orphanBytes},
		{"orphan rows", r.orphanRows},
		{"expired leases", r.sweep(r.opts.Leases, KindLeaseExpired)},
		{"expired uploads", r.sweep(r.opts.Sessions, KindUploadAborted)},
		{"trash", r.purgeTrash},
		{"tombstones", r.sweep(r.opts.Tombstones, KindWorkspacePurged)},
		{"grant hygiene", r.sweep(r.opts.Grants, KindShareExpired)},
		{"stale stars", r.pruneStars},
		{"log retention", r.pruneLog},
		{"ledger reconciliation", r.reconcileLedger},
	} {
		if err := p.run(ctx, q, &f, changed); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
		}
	}
	if err := r.noteRuns(ctx, q, changed); err != nil {
		errs = append(errs, err)
	}

	err := errors.Join(errs...)
	r.record(f, err == nil, r.opts.Now().Sub(started))
	attrs := append([]any{"dry_run", r.opts.DryRun}, f.attributes()...)
	if err != nil {
		r.opts.Log.ErrorContext(ctx, "the reconciliation finished with failures", append(attrs, "err", err)...)
	} else {
		r.opts.Log.InfoContext(ctx, "the reconciliation finished", attrs...)
	}
	return f, err
}

// record hands the run to spec 018's seam.
func (r *Reconciler) record(f Findings, ok bool, took time.Duration) {
	if r.opts.Metrics == nil {
		return
	}
	for _, row := range f.Rows() {
		r.opts.Metrics.Finding(row.Kind, row.Outcome, row.Count)
	}
	r.opts.Metrics.Run(ok, took)
}

// spaces collects the spaces a run changed, so one reap event is appended
// per space and not one per row.
type spaces map[string]map[Kind]int

// touch records that a pass changed n rows of one kind in one space.
func (s spaces) touch(owner string, kind Kind, n int) {
	if n <= 0 {
		return
	}
	if s[owner] == nil {
		s[owner] = map[Kind]int{}
	}
	s[owner][kind] += n
}

// noteRuns appends one reap event per space the run changed. A dry run
// changes nothing, so it appends nothing.
func (r *Reconciler) noteRuns(ctx context.Context, q store.Querier, changed spaces) error {
	if r.opts.DryRun {
		return nil
	}
	for _, owner := range slices.Sorted(maps.Keys(changed)) {
		detail := make(map[string]any, len(changed[owner]))
		for kind, n := range changed[owner] {
			detail[string(kind)] = n
		}
		r.log.Note(ctx, q, events.Event{Owner: owner, Action: events.ActionReap, Detail: detail})
	}
	return nil
}

// sweep answers the pass runner for one of the four seams. A seam the
// reconciler was not given does not run and reports nothing.
func (r *Reconciler) sweep(p Pass, kind Kind) func(context.Context, store.Querier, *Findings, spaces) error {
	return func(ctx context.Context, q store.Querier, f *Findings, _ spaces) error {
		if p == nil {
			return nil
		}
		n, err := p.Sweep(ctx, q, r.opts.Now(), r.opts.DryRun)
		if err != nil {
			return err
		}
		f.Add(kind, Found, n)
		if !r.opts.DryRun {
			f.Add(kind, Repaired, n)
		}
		return nil
	}
}
