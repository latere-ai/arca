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
	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/internal/version"
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
	default:
		_, _ = fmt.Fprintf(stderr, "arcad: unknown subcommand %q; serve and migrate are the ones this binary has\n", name)
		return 2
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
	bucket, err := blob.NewS3(ctx, blob.Options{
		Bucket:    cfg.Bucket,
		Endpoint:  cfg.BucketEndpoint,
		Region:    cfg.BucketRegion,
		AccessKey: cfg.BucketAccessKey,
		SecretKey: cfg.BucketSecretKey,
		PathStyle: cfg.BucketPathStyle,
	})
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
	})
	if err != nil {
		return fail(stderr, err)
	}
	// The shares and links of spec 008. The log of spec 010 and the read path
	// of spec 005 are bound with those specs; until then a mutation records
	// nowhere and the object route of a link answers not_implemented.
	sharing, err := shares.New(shares.Options{
		Authorizer: identity.Authorizer, DB: db, Store: grants,
		Publisher: bucket, BucketPrefix: cfg.BucketPrefix,
	})
	if err != nil {
		return fail(stderr, err)
	}
	surface, err := api.New(api.Options{
		Verifier: identity.Verifier, Authorizer: identity.Authorizer,
		Shares:                           sharing,
		PublicURL:                        cfg.PublicURL,
		RequestsPerMinute:                cfg.RequestsPerMinute,
		UnauthenticatedRequestsPerMinute: cfg.UnauthenticatedRequestsPerMinute,
	})
	if err != nil {
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
