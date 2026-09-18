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
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/health"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/reaper"
	"latere.ai/x/arca/internal/store"
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
	default:
		_, _ = fmt.Fprintf(stderr, "arcad: unknown subcommand %q; serve, migrate and reap are the ones this binary has\n", name)
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
	// Zero is the value that turns the in-process loop of serve off. A
	// process whose whole job is that loop cannot take it, and exiting 0
	// having done nothing is how a CronJob looks healthy while nothing is
	// reconciled.
	if !*once && cfg.ReapInterval <= 0 {
		return fail(stderr, errors.New("ARCA_REAP_INTERVAL is 0, which turns the loop off; run arcad reap -once, or set an interval"))
	}

	bucket, err := blob.NewS3(ctx, bucketOptions(cfg))
	if err != nil {
		return fail(stderr, err)
	}
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fail(stderr, err)
	}
	defer db.Close()
	reconciler, err := reaper.New(reaperOptions(cfg, db, bucket, *dryRun))
	if err != nil {
		return fail(stderr, err)
	}

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
func reaperOptions(cfg config.Config, db *store.DB, bucket blob.Store, dryRun bool) reaper.Options {
	return reaper.Options{
		DB:             db,
		Bucket:         bucket,
		Prefix:         cfg.BucketPrefix,
		TrashRetention: cfg.TrashRetention,
		DryRun:         dryRun,
	}
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

	// The bucket client opens no connection here: it is built from the
	// configuration, and the readiness check below is what reaches the
	// store. A store that is briefly unreachable at start-up therefore
	// delays readiness rather than crashing the process.
	bucket, err := blob.NewS3(ctx, bucketOptions(cfg))
	if err != nil {
		return fail(stderr, err)
	}

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

	// Spec 006's two, built once: the verifier over the listed issuers, warm
	// before the first request, and the authorizer the operator configured or
	// the owner policy. An issuer that does not answer and an endpoint with
	// no bearer are start-up failures naming their variable, so a deployment
	// is fixed rather than left answering 401 or 503 to everything.
	//
	// The grants and the links the owner policy reads arrive with spec 008;
	// until then an installation has issued neither.
	identity, err := auth.Start(ctx, auth.Options{
		Issuers: cfg.OIDCIssuers, Audience: cfg.OIDCAudience,
		InsecureIssuers: cfg.OIDCInsecureIssuers,
		AuthorizerURL:   cfg.AuthorizerURL, AuthorizerToken: cfg.AuthorizerToken,
		AdminSubjects: cfg.AdminSubjects,
	})
	if err != nil {
		return fail(stderr, err)
	}
	// The workspaces of spec 009. The file plane of spec 005 and the ledger
	// of spec 010 are seams: the queries below are the file-plane half a
	// workspace reads as a subtree, and a nil ledger writes nothing until
	// that spec lands its log.
	durable, err := workspaces.New(workspaces.Options{
		DB: db, Workspaces: store.NewWorkspaces(), Attachments: store.NewAttachments(),
		Objects: store.NewWorkspaceObjects(), Bucket: bucket, Prefix: cfg.BucketPrefix,
		Authorizer: identity.Authorizer,
	})
	if err != nil {
		return fail(stderr, err)
	}
	surface, err := api.New(api.Options{
		Verifier: identity.Verifier, Authorizer: identity.Authorizer,
		PublicURL:                        cfg.PublicURL,
		RequestsPerMinute:                cfg.RequestsPerMinute,
		UnauthenticatedRequestsPerMinute: cfg.UnauthenticatedRequestsPerMinute,
		// The log of spec 010 and the database it reads through, which is
		// what GET /v1/events tails.
		Events: events.NewLog(), Querier: db.Querier(),
		// The twelve rows of spec 009, contributed by the package that owns
		// their behaviour and registered through the one seam of register.go.
		Routes: workspaces.Routes(durable),
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
	if err := startReaper(ctx, cfg, db, bucket, stdout); err != nil {
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
		{Handler: probes, ReadHeaderTimeout: 10 * time.Second},
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
func startReaper(ctx context.Context, cfg config.Config, db *store.DB, bucket blob.Store, stdout io.Writer) error {
	if cfg.ReapInterval <= 0 {
		_, _ = fmt.Fprintln(stdout, "arcad: the reconciler is off on this replica; ARCA_REAP_INTERVAL is 0")
		return nil
	}
	reconciler, err := reaper.New(reaperOptions(cfg, db, bucket, false))
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
