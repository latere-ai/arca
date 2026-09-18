// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command arcad is the Arca server: durable storage for people, agents,
// and sandboxes over an S3 compatible bucket and a Postgres database. This
// file is the entry point and holds wiring only: configuration, the
// listeners, and the run group. The behaviour lives in the packages under
// internal/ and in the exported packages at the module root.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/health"

	"latere.ai/x/arca/internal/admin"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/check"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/metrics"
	"latere.ai/x/arca/internal/reaper"
	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/internal/uploads"
	"latere.ai/x/arca/internal/version"
	"latere.ai/x/arca/internal/workspaces"
)

// Shutdown timing of spec 002: readiness answers 503 at once, the drain
// delay lets a load balancer notice, then the servers close with the
// grace period.
const (
	drainDelay  = 3 * time.Second
	gracePeriod = 60 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run dispatches the subcommand and returns the process exit code, so
// tests drive it without a subprocess: 0 on a clean stop, 1 on a start-up
// or runtime failure, 2 on a usage error. serve is the only subcommand
// today; spec 002 keeps the table.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serve(ctx, rest, getenv, stdout, stderr)
	case "migrate":
		return migrate(rest, getenv, stdout, stderr)
	case "reap":
		return reap(ctx, rest, getenv, stdout, stderr)
	case "check":
		return check.Command(ctx, rest, getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "arcad: unknown subcommand %q; serve, migrate, reap and check are the ones this binary has\n", name)
		return 2
	}
}

// reap runs the reconciler of spec 010 as a process of its own, for an
// installation that wants it off the API replicas. It reads the same
// configuration as the server, because a process reconciling against a
// different one would reconcile a different installation, and it opens no
// listener.
func reap(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("arcad reap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	once := fs.Bool("once", false, "run one sequence of passes and exit, which is the shape a CronJob runs")
	dryRun := fs.Bool("dry-run", false, "report every finding and change nothing in either store")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	// Spec 018's exporter. This process opens no listener, so what it
	// publishes leaves over OTLP: the run's spans and its lines, each
	// carrying the trace id. Its counters are on the registry below and are
	// scraped from a replica that serves, which is what the Current state of
	// that spec records.
	recorder, flush := observe(ctx, cfg, stderr)
	defer func() { _ = flush(context.WithoutCancel(ctx)) }()
	// Zero is the value that turns the in-process loop of serve off. A
	// process whose whole job is that loop cannot take it, and exiting 0
	// having done nothing is how a CronJob looks healthy while nothing is
	// reconciled.
	if !*once && cfg.ReapInterval <= 0 {
		return fail(stderr, errors.New("ARCA_REAP_INTERVAL is 0, which turns the loop off; run arcad reap -once, or set an interval"))
	}

	s3, err := blob.NewS3(ctx, bucketOptions(cfg))
	if err != nil {
		return fail(stderr, err)
	}
	bucket := recorder.Bucket(s3)
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(stderr, err)
	}
	defer db.Close()
	// Pass 3 is not given here. Its sweep is a method of the workspace
	// service of spec 009, which refuses to build without the authorizer
	// every one of its handlers decides through, and this process registers
	// no handler and starts no verifier. A pass that does not run reports
	// nothing, and a table with no row for it reads exactly like a pass that
	// found nothing, so the line below is what says which it is: an
	// installation that moved the reconciler here would otherwise expire no
	// lease and read a healthy log.
	//
	// Passes 4, 6 and 7 are given: the expiry sweep of spec 007, the
	// tombstone purge of spec 009 and the grant hygiene of spec 008 each ask
	// nothing of the authorizer, so they run wherever the two stores are
	// reachable.
	log := events.NewLog()
	tombstones, err := tombstonePass(cfg, db, bucket, log, recorder)
	if err != nil {
		return fail(stderr, err)
	}
	grants, err := grantPass(cfg, store.NewShares())
	if err != nil {
		return fail(stderr, err)
	}
	reconciler, err := reaper.New(reaperOptions(cfg, db, bucket, *dryRun, nil,
		uploadPass(cfg, db, bucket, log), tombstones, grants, recorder))
	if err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, "arcad: this process runs nine of the ten passes; the lease expiry of spec 009 runs on a replica with ARCA_REAP_INTERVAL set")

	if !*once {
		_, _ = fmt.Fprintf(stdout, "arcad: the reconciler runs every %s\n", cfg.ReapInterval)
		reconciler.Loop(ctx, cfg.ReapInterval)
		return 0
	}
	findings, err := reconciler.Run(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	report(stdout, findings, *dryRun)
	return 0
}

// report is what one sequence prints: a summary line and the table of what
// it found, which is the whole output of a CronJob's log.
func report(stdout io.Writer, findings reaper.Findings, dryRun bool) {
	note := ""
	if dryRun {
		note = "; nothing was changed, because this was a dry run"
	}
	_, _ = fmt.Fprintf(stdout, "arcad: the reconciliation finished%s\n", note)
	for _, row := range findings.Rows() {
		_, _ = fmt.Fprintf(stdout, "  %-18s %-9s %d\n", row.Kind, row.Outcome, row.Count)
	}
}

// reaperOptions is what both roles build the reconciler from. serve and reap
// read the same configuration and reconcile the same way; what differs is
// where the loop lives.
func reaperOptions(cfg config.Config, db *store.DB, bucket blob.Store, dryRun bool, leases, sessions, tombstones, grants reaper.Pass, recorder reaper.Metrics) reaper.Options {
	return reaper.Options{
		DB:             db,
		Bucket:         bucket,
		Prefix:         cfg.BucketPrefix,
		TrashRetention: cfg.TrashRetention,
		DryRun:         dryRun,
		Leases:         leases,
		Sessions:       sessions,
		Tombstones:     tombstones,
		Grants:         grants,
		Metrics:        recorder,
	}
}

// uploadPass builds pass 4 of spec 010's table: the expiry sweep of spec
// 007, over the session table that spec owns. It is the same service the
// routes are bound to on a replica, and its own on the reap process, which
// registers no handler and asks nothing: the sweep puts no question, so the
// service it runs on needs no authorizer.
func uploadPass(cfg config.Config, db *store.DB, bucket blob.Store, log events.Log) *uploads.Service {
	return uploads.New(uploads.Options{
		DB: db, Bucket: bucket, Config: cfg,
		Ledger: fileLedger{log: log, usage: events.NewLedger()},
	})
}

// tombstonePass builds pass 6 of spec 010's table: the tombstone purge of
// spec 009, over the workspaces and the subtree that spec and spec 005 own.
// Like the expiry sweep above it asks nothing of the authorizer, so it runs
// wherever the two stores are reachable and not only on a replica that
// started a verifier.
func tombstonePass(cfg config.Config, db *store.DB, bucket blob.Store, log events.Log, recorder *metrics.Set) (reaper.Pass, error) {
	return workspaces.NewTombstones(workspaces.TombstoneOptions{
		DB: db, Workspaces: store.NewWorkspaces(), Objects: store.NewWorkspaceObjects(),
		Bucket: bucket, Prefix: cfg.BucketPrefix, Retention: cfg.TrashRetention,
		Ledger: ledger{log: log, usage: events.NewLedger(), metrics: recorder},
	})
}

// grantPass builds pass 7: the grant hygiene of spec 008, over the table
// that spec owns. The window is the trash retention, because an expired
// grant is kept for an audit as long as a deleted object is kept for a
// restore and spec 009 says there is no second retention setting.
func grantPass(cfg config.Config, grants store.Shares) (reaper.Pass, error) {
	return shares.NewExpiry(shares.ExpiryOptions{Store: grants, Window: cfg.TrashRetention})
}

// The two seams spec 009 declared and spec 010 fills. Both are here rather
// than in either package, because a package that imported the other to bind
// its own seam would be the dependency the seam exists to avoid: the
// workspaces know nothing of the log, the reconciler knows nothing of the
// lease, and the node is what knows both.

// ledger binds the log and the usage counter of spec 010 to the seam of
// spec 009. Both calls run inside the caller's transaction, which is what
// makes a mutation and the row recording it one commit.
type ledger struct {
	log   events.Log
	usage events.Ledger
	// metrics is spec 018's counter of the rows the log took, which is here
	// rather than in internal/events because this is the one place a
	// workspace's mutation becomes a row of that log.
	metrics *metrics.Set
}

// Append writes one row of the log. The action is one word of spec 010's
// closed vocabulary, spelled the same on both sides; the append refuses a
// word the vocabulary does not name, so a mapping that drifted is a refused
// transaction rather than a row no consumer can filter for.
//
// The id the append answers is dropped. A workspace asked for the row to
// exist and reads no cursor of its own.
func (l ledger) Append(ctx context.Context, q store.Querier, e workspaces.Event) error {
	_, err := l.log.Append(ctx, q, events.Event{
		Owner: e.Owner, Path: e.Path, Action: events.Action(e.Action),
		Actor: e.Actor, Detail: e.Detail,
	})
	if err == nil {
		l.metrics.EventAppended(e.Action)
	}
	return err
}

// Release gives the bytes a sync dropped back to the space's counter. The
// total afterwards is the counter's own business: the caller asked for the
// number to move and reads it back through the routes that report usage.
func (l ledger) Release(ctx context.Context, q store.Querier, owner string, bytes int64) error {
	_, err := l.usage.Release(ctx, q, owner, bytes)
	return err
}

// fileLedger binds the usage counter and the log of spec 010 to the seam of
// specs 005 and 007. A charge runs inside the write's own transaction, so
// the commit that records an object and the commit that records its bytes
// are one commit and a refused charge takes the write down with it.
type fileLedger struct {
	log   events.Log
	usage events.Ledger
}

// Charge applies the write's delta and holds it to the limit the
// authorizer's answer carried. A refusal is rendered as the seam's own
// error, because the handler answers 413 quota_exceeded off that type and a
// space with no room left is not a fault of the counter.
func (l fileLedger) Charge(ctx context.Context, q store.Querier, owner string, delta int64, limit files.Limit) (int64, error) {
	total, err := l.usage.Charge(ctx, q, owner, delta, allowance(limit))
	var over *events.OverLimitError
	if errors.As(err, &over) {
		return total, &files.OverLimit{Owner: over.Owner, Used: over.Used, Limit: over.Limit, Delta: over.Delta}
	}
	return total, err
}

// Release gives bytes back, which is never refused.
func (l fileLedger) Release(ctx context.Context, q store.Querier, owner string, bytes int64) (int64, error) {
	return l.usage.Release(ctx, q, owner, bytes)
}

// Append records what happened to an object. It is the log's best-effort
// append: the mutation has already happened when the row is written, so a
// failed insert is a warning and never a refusal to the caller. The action
// is one word of spec 010's closed vocabulary, spelled the same on both
// sides, and a word the vocabulary does not name is dropped with a warning
// rather than written as a row no consumer can filter for.
func (l fileLedger) Append(ctx context.Context, q store.Querier, e files.Event) {
	l.log.Note(ctx, q, events.Event{
		Owner: e.Owner, Path: e.Path, Action: events.Action(e.Action),
		Actor: e.Actor, Detail: e.Detail,
	})
}

// allowance carries the byte limit one authorizer answer named across the
// two spellings of it. Arca stores no limit, so an answer that named none
// leaves the space unlimited.
func allowance(l files.Limit) events.Limit {
	if !l.Set {
		return events.Unlimited()
	}
	return events.LimitBytes(l.Bytes)
}

// shareLedger binds the log of spec 010 to the seam of spec 008. The append
// runs inside the mutation's own transaction, which is what makes a grant
// that was made a grant that was recorded: a change to who may act on a
// space is not a notification that may go missing.
type shareLedger struct{ log events.Log }

// Append writes one row of the log and answers its id, which is the cursor a
// consumer reads the row back at. The action is one word of spec 010's
// closed vocabulary, spelled the same on both sides; the append refuses a
// word the vocabulary does not name, so a mapping that drifted is a refused
// transaction rather than a row no consumer can filter for.
func (l shareLedger) Append(ctx context.Context, q store.Querier, e shares.Event) (int64, error) {
	return l.log.Append(ctx, q, events.Event{
		Owner: e.Owner, Path: e.Path, Action: events.Action(e.Action),
		Actor: e.Actor, Detail: e.Detail,
	})
}

// leasePass binds pass 3 of spec 010's table to the sweep of spec 009. The
// reconciler hands every pass a querier and a dry flag; this sweep opens its
// own transactions, one per row it ends, because a reap is a row, a lease and
// a log entry written together.
type leasePass struct {
	service *workspaces.Service
	// metrics is spec 018's arca_lease_expiries_total. The reconciler counts
	// the same sweep as a finding of kind lease_expired; this is the rate a
	// platform alerts on, and it is recorded here because the sweep's own
	// package knows nothing of a registry.
	metrics *metrics.Set
}

// Sweep ends what outlived its deadline, and runs nothing at all on a dry
// run. The sweep has no counting half: every statement it issues is a write,
// so a dry run that called it would change both stores while reporting that
// it changed neither, which is the one thing a dry run may not do. It
// reports nothing rather than a number it did not measure.
func (p leasePass) Sweep(ctx context.Context, _ store.Querier, now time.Time, dry bool) (int, error) {
	if dry {
		return 0, nil
	}
	ended, err := p.service.ExpireLeases(ctx, now)
	p.metrics.LeaseExpired(ended)
	return ended, err
}

// restorer binds the restore across owners of spec 012 to the two packages
// that own what it undoes. It is here rather than in either of them for the
// reason every seam above is: internal/admin knows no trash and no
// workspace, internal/files and internal/workspaces know no administrator,
// and the node is what knows all three.
type restorer struct {
	objects trashRestorer
	durable tombstoneRestorer
}

// The two arms, as interfaces, so this adapter is driven with the two
// answers that decide it — "not mine" and a failure — without a bucket and a
// database behind each.
type (
	// trashRestorer is spec 005's trash, keyed by the row's id.
	trashRestorer interface {
		RestoreTrashed(ctx context.Context, owner, id string) (store.File, error)
	}
	// tombstoneRestorer is spec 009's soft deleted workspace.
	tombstoneRestorer interface {
		RestoreDeleted(ctx context.Context, owner, id string) (store.Workspace, error)
	}
)

// Restore brings back the trashed object or the soft deleted workspace the
// id names, and answers which it was.
//
// Both ids are database identifiers of the same shape, so an id does not say
// which table it belongs to and spec 012 states no rule that would make it
// say so. The two arms are therefore tried in order, and each answers its
// own "not mine" rather than a fault, so a miss on the first is a question
// put to the second and a miss on both is the one answer the route renders:
// an id that names nothing this space can still bring back.
//
// Both arms are scoped to the space the route named and asked about. An id
// of another owner is not this space's to restore, and each arm answers it
// as an id that names nothing.
func (r restorer) Restore(ctx context.Context, owner, id string) (admin.Restored, error) {
	switch _, err := r.objects.RestoreTrashed(ctx, owner, id); {
	case err == nil:
		return admin.Restored{ID: id, Kind: admin.KindFile}, nil
	case !errors.Is(err, files.ErrNotTrashed):
		return admin.Restored{}, err
	}
	switch _, err := r.durable.RestoreDeleted(ctx, owner, id); {
	case err == nil:
		return admin.Restored{ID: id, Kind: admin.KindWorkspace}, nil
	case !errors.Is(err, workspaces.ErrNotDeleted):
		return admin.Restored{}, err
	}
	return admin.Restored{}, admin.ErrNotRestorable
}

// bucketOptions is the bucket client every role opens, from the one table of
// spec 002.
func bucketOptions(cfg config.Config) blob.Options {
	return blob.Options{
		Bucket:    cfg.Bucket,
		Endpoint:  cfg.BucketEndpoint,
		Region:    cfg.BucketRegion,
		AccessKey: cfg.BucketAccessKey,
		SecretKey: cfg.BucketSecretKey,
		PathStyle: cfg.BucketPathStyle,
	}
}

// migrate applies the pending migrations and exits. It reads the database
// variable and nothing else, so a migration job runs with the database alone
// configured (spec 002's subcommand table).
func migrate(args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("arcad migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	databaseURL, err := config.Database(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	if err := applyMigrations(databaseURL); err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, "arcad: the database holds every migration this binary carries")
	return 0
}

// The two seams of spec 004's schema handling. Both reach a database, so a
// test drives them with a stub and the store tier runs the real ones.
var (
	// pendingMigrations reads what the database has not applied.
	pendingMigrations = store.Pending
	// applyMigrations is what the migrate subcommand runs.
	applyMigrations = store.Migrate
)

// subcommand is spec 002's rule: the first argument that does not start
// with a dash names the subcommand, and the arguments around it are the
// subcommand's own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	return "", args
}

// serve is the node: the two listeners and the probes of spec 002. The
// stores, the API, and the workers of later specs mount here.
func serve(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("arcad serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fail(stderr, err)
	}

	// Spec 018, before anything else is built: the logger every line goes
	// through, the tracer the spans go to, and the one registry the internal
	// listener serves. Nothing leaves the process without
	// ARCA_OTEL_EXPORTER_OTLP_ENDPOINT, and /metrics serves either way.
	recorder, flush := observe(ctx, cfg, stderr)
	defer func() { _ = flush(context.WithoutCancel(ctx)) }()

	// The bucket client opens no connection here: it is built from the
	// configuration, and the readiness check below is what reaches the
	// store. A store that is briefly unreachable at start-up therefore
	// delays readiness rather than crashing the process.
	s3, err := blob.NewS3(ctx, bucketOptions(cfg))
	if err != nil {
		return fail(stderr, err)
	}
	// Spec 018's decorator: every call this replica makes against the bucket
	// is counted by operation and result, timed, and opened as a span named
	// bucket.<op>. internal/blob is left holding the S3 contract alone.
	bucket := recorder.Bucket(s3)

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(stderr, err)
	}
	defer db.Close()
	// A database that answers and is behind this binary is a deploy whose
	// migration job did not run, and serving against a schema the binary
	// does not have is how a write is lost. A database that does not answer
	// is not a verdict: the readiness check below carries it, and makes the
	// same comparison once the database is there.
	if pending, err := pendingMigrations(ctx, db.Querier()); err == nil && len(pending) > 0 {
		return fail(stderr, fmt.Errorf("the database is behind this binary: %s is not applied; run arcad migrate", pending[0]))
	}

	// The grants table of spec 008, read by two callers: the owner policy,
	// through the two seams of spec 006, and the routes of spec 008, through
	// the service below. One query set serves both.
	grants := store.NewShares()

	// Spec 006's two, built once: the verifier over the listed issuers, warm
	// before the first request, and the authorizer the operator configured or
	// the owner policy. An issuer that does not answer and an endpoint with
	// no bearer are start-up failures naming their variable, so a deployment
	// is fixed rather than left answering 401 or 503 to everything.
	identity, err := auth.Start(ctx, auth.Options{
		Issuers: cfg.OIDCIssuers, Audience: cfg.OIDCAudience,
		InsecureIssuers: cfg.OIDCInsecureIssuers,
		AuthorizerURL:   cfg.AuthorizerURL, AuthorizerToken: cfg.AuthorizerToken,
		AdminSubjects: cfg.AdminSubjects,
		Grants:        shares.Grants(db, grants),
		Links:         shares.Links(db, grants),
		// Spec 018's identity row: the duration of a call to the operator's
		// endpoint, and the outcome of every decision this node acted on,
		// whichever of the two answered.
		Observe: recorder.AuthorizerCall, Decided: recorder.Decided,
	})
	if err != nil {
		return fail(stderr, err)
	}
	// The one log of spec 010 in this process, bound to every seam that
	// writes a row and to the tail that reads them back. It holds no state,
	// and one value rather than four is what makes a row appended by a share
	// and a row appended by a workspace the same log.
	log := events.NewLog()

	// The objects of spec 005 and the sessions of spec 007, built once and
	// reached three ways: the routes each package declares, the object route
	// of a public link, and the expiry sweep the reconciler runs. One service
	// rather than three is what makes those three the same read, the same
	// charge and the same log.
	content := files.Options{
		DB: db, Bucket: bucket, Decide: identity.Authorizer, Config: cfg,
		Ledger: fileLedger{log: log, usage: events.NewLedger()},
		// Spec 018's seam for the bytes a put takes in, the bytes a read
		// streams out, and the writes a limit refused.
		Metrics: recorder,
		// The liveness rule of spec 009, which spec 005 applies: a path
		// under workspaces/<slug>/ whose workspace is gone or soft deleted
		// is a missing object to everyone. The query set of spec 004
		// answers it, so neither package reaches the other.
		Workspaces: store.NewWorkspaces(),
	}
	object := files.New(content)
	session := uploads.New(uploads.Options{Options: content, Metrics: recorder})

	// The shares and links of spec 008. The log of spec 010 is bound below,
	// so a grant made and a grant revoked are rows of it, and the read path
	// of spec 005 is bound too, so the object route of a link serves bytes.
	sharing, err := shares.New(shares.Options{
		Authorizer: identity.Authorizer, DB: db, Store: grants,
		Publisher: bucket, BucketPrefix: cfg.BucketPrefix,
		Ledger: shareLedger{log: log},
		Reader: object,
	})
	if err != nil {
		return fail(stderr, err)
	}
	// The workspaces of spec 009. The file plane of spec 005 is still a
	// seam: the queries below are the file-plane half a workspace reads as
	// a subtree, and they answer it until that spec lands its own. The
	// ledger is no longer one: spec 010 is in this build, so an attach, a
	// release, a sync and a reap append a row of its log and a sync gives
	// the bytes it dropped back to its space's counter.
	durable, err := workspaces.New(workspaces.Options{
		DB: db, Workspaces: store.NewWorkspaces(), Attachments: store.NewAttachments(),
		Objects: store.NewWorkspaceObjects(), Bucket: bucket, Prefix: cfg.BucketPrefix,
		Authorizer: identity.Authorizer,
		Ledger:     ledger{log: log, usage: events.NewLedger(), metrics: recorder},
		// Spec 018's seam for the bytes a materialize hands out and a sync
		// declares, which is the workspace plane's half of arca_bytes_*.
		Metrics: recorder,
	})
	if err != nil {
		return fail(stderr, err)
	}
	// The two gauges of spec 018 that no process keeps a number for: a lease
	// and a session are held across replicas, so what each reads at a scrape
	// is the database's count at that instant and not this replica's share
	// of it.
	recorder.LeasesHeld.Bind(sampling(ctx, "arca_leases_held", durable.HeldLeases))
	recorder.SessionsOpen(sampling(ctx, "arca_upload_sessions_open", session.Open))

	// The administration of spec 012, with both of its seams bound. The
	// restore across owners returns rows the trash of spec 005 and the
	// deleted workspaces of spec 009 own, and both are in this build, so the
	// route restores rather than answering not_implemented; the link counter
	// is spec 008's table, and it is here too, so the overview reports the
	// links a space actually holds.
	administration, err := admin.New(admin.Options{
		Querier: db.Querier(), Spaces: store.NewAdmin(), Authorizer: identity.Authorizer,
		Links:    shares.LinkCounts(grants),
		Restorer: restorer{objects: object, durable: durable},
	})
	if err != nil {
		return fail(stderr, err)
	}
	surface, err := api.New(api.Options{
		Verifier: identity.Verifier, Authorizer: identity.Authorizer,
		Links:                            sharing,
		PublicURL:                        cfg.PublicURL,
		RequestsPerMinute:                cfg.RequestsPerMinute,
		UnauthenticatedRequestsPerMinute: cfg.UnauthenticatedRequestsPerMinute,
		// The log of spec 010 and the database it reads through, which is
		// what GET /v1/events tails.
		Events: log, Querier: db.Querier(),
		// The thirty-seven rows five packages own: the twelve of spec 009,
		// the eight of spec 008 that sit behind the verifier, the twelve of
		// spec 005, the three of spec 007 and the two of spec 012. Each is
		// declared by the package that answers it and registered through the
		// one seam of register.go, in the order tools/apidoc unions them, so
		// the committed document and the mux read one list the same way.
		Routes: slices.Concat(
			workspaces.Routes(durable),
			shares.Routes(sharing),
			files.Bind(object),
			uploads.Bind(session),
			admin.Routes(administration),
		),
		// Spec 018's request path: the counters and the one line per request.
		// The logger is slog's default, which the bootstrap above set.
		Metrics: recorder,
	})
	if err != nil {
		return fail(stderr, err)
	}

	// The reconciler of spec 010 runs on every replica, which is what a
	// small installation wants: one workload and nothing else to operate.
	// An installation that moves it off the API replicas sets
	// ARCA_REAP_INTERVAL to 0 here and runs arcad reap as a process of its
	// own. It starts before the listeners, so a reconciler that cannot be
	// built fails the start-up before a port is bound.
	tombstones, err := tombstonePass(cfg, db, bucket, log, recorder)
	if err != nil {
		return fail(stderr, err)
	}
	expiredGrants, err := grantPass(cfg, grants)
	if err != nil {
		return fail(stderr, err)
	}
	if err := startReaper(ctx, cfg, db, bucket, leasePass{service: durable, metrics: recorder},
		session, tombstones, expiredGrants, recorder, stdout); err != nil {
		return fail(stderr, err)
	}

	draining := make(chan struct{})
	probes := health.Handler(health.Options{
		Ready:     health.Checks(readiness(identity, draining, bucket.HeadBucket, databaseReady(db))...),
		Timeout:   2 * time.Second,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
	})

	// The internal listener of spec 002: the four probes, and GET /metrics
	// beside them. The router prefers the more specific pattern, so the
	// probes keep answering under the catch-all while the scrape endpoint
	// answers its own path. It is on this listener and on no other.
	internal := http.NewServeMux()
	internal.Handle("/", probes)
	internal.Handle("GET /metrics", recorder.Handler())

	public := http.NewServeMux()
	for _, p := range []string{"/livez", "/readyz", "/version"} {
		public.Handle("GET "+p, probes)
	}
	public.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, version.String())
	})
	surface.Mount(public)

	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", cfg.PublicAddr)
	if err != nil {
		return fail(stderr, fmt.Errorf("ARCA_PUBLIC_ADDR: %w", err))
	}
	internalLn, err := lc.Listen(ctx, "tcp", cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		return fail(stderr, fmt.Errorf("ARCA_INTERNAL_ADDR: %w", err))
	}
	// The mode is on the line an operator reads at start, so they know
	// whether the endpoint they configured was picked up (spec 006).
	_, _ = fmt.Fprintf(stdout, "arcad: %s listening public=%s internal=%s deciding=%q\n",
		version.Version, publicLn.Addr(), internalLn.Addr(), identity.Mode)

	servers := []*http.Server{
		{Handler: public, ReadHeaderTimeout: 10 * time.Second},
		{Handler: internal, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for i, ln := range []net.Listener{publicLn, internalLn} {
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(servers[i], ln)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fail(stderr, err)
	}
	// The stop signal has fired, so the shutdown runs on a context that
	// keeps the request's values and outlives its cancellation.
	close(draining)
	stopping := context.WithoutCancel(ctx)
	sleepCtx(stopping, drainDelay)
	shutdownCtx, cancel := context.WithTimeout(stopping, gracePeriod)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return 0
}

// startReaper starts the in-process reconciliation loop, or says on the
// start-up line that this replica runs none. Which it is, is a fact an
// operator reads once rather than infers from a missing metric.
func startReaper(ctx context.Context, cfg config.Config, db *store.DB, bucket blob.Store, leases, sessions, tombstones, grants reaper.Pass, recorder reaper.Metrics, stdout io.Writer) error {
	if cfg.ReapInterval <= 0 {
		_, _ = fmt.Fprintln(stdout, "arcad: the reconciler is off on this replica; ARCA_REAP_INTERVAL is 0")
		return nil
	}
	reconciler, err := reaper.New(reaperOptions(cfg, db, bucket, false, leases, sessions, tombstones, grants, recorder))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "arcad: the reconciler runs every %s\n", cfg.ReapInterval)
	go reconciler.Loop(ctx, cfg.ReapInterval)
	return nil
}

// fail writes the one line an operator reads on a start-up or runtime
// failure and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "arcad: %v\n", err)
	return 1
}

// readiness is the checks /readyz runs, in the order it reports them. The
// draining check is spec 002's and is always first, and the bucket and the
// database of specs 003 and 004 follow it. The authorizer check is spec
// 006's and is there only where an endpoint is configured: it sends the
// probe every authorizer of the family denies, so a replica whose endpoint
// is out of reach, or whose endpoint answers an allow without reading the
// request, leaves rotation rather than serving decisions nobody made.
//
// With no endpoint configured the owner policy decides in process, and a
// check of it would be a check of this binary against itself.
func readiness(identity *auth.Identity, draining <-chan struct{}, bucket, database func(context.Context) error) []health.Check {
	checks := []health.Check{
		{Name: "draining", Run: notDraining(draining)},
		{Name: "bucket", Run: bucket},
		{Name: "database", Run: database},
	}
	if identity.Mode == auth.ModeAuthorizer {
		checks = append(checks, health.Check{Name: "authorizer", Run: identity.Authorizer.Check})
	}
	return checks
}

// databaseReady is the readiness check of spec 002 and the other half of
// the schema check above: the database answers, and it holds every migration
// this binary carries.
func databaseReady(db *store.DB) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := db.Ping(ctx); err != nil {
			return err
		}
		return schemaReady(ctx, db.Querier())
	}
}

// schemaReady answers whether the database holds every migration this binary
// carries.
func schemaReady(ctx context.Context, q store.Querier) error {
	pending, err := pendingMigrations(ctx, q)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%s is not applied; run arcad migrate", pending[0])
	}
	return nil
}

// notDraining fails readiness once shutdown has begun, so a load balancer
// stops routing before the servers close.
func notDraining(draining <-chan struct{}) func(context.Context) error {
	return func(context.Context) error {
		select {
		case <-draining:
			return errors.New("shutting down")
		default:
			return nil
		}
	}
}

// sleepCtx waits d or until ctx ends, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
