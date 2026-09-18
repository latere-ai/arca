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

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
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
	default:
		_, _ = fmt.Fprintf(stderr, "arcad: unknown subcommand %q; serve is the default and the only one\n", name)
		return 2
	}
}

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

	draining := make(chan struct{})
	probes := health.Handler(health.Options{
		Ready: health.Checks(
			health.Check{Name: "draining", Run: notDraining(draining)},
			health.Check{Name: "bucket", Run: bucket.HeadBucket},
		),
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
	_, _ = fmt.Fprintf(stdout, "arcad: %s listening public=%s internal=%s\n",
		version.Version, publicLn.Addr(), internalLn.Addr())

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
