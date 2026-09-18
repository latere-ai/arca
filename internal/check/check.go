// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package check is `arcad check` of spec 012: the question an operator asks
// before anything else, which is whether this installation is correctly
// configured.
//
// It reads the whole configuration table of spec 002, tests every
// requirement of an installation once, prints one line per requirement, and
// exits 1 if any line failed. It opens no listener, runs no migration, and
// writes nothing that it does not delete.
//
// Nothing here is a substitute for the node's own start-up. A value that is
// missing or malformed fails config.Load before any of this runs; these are
// the answers only the outside world can give, and each costs one round trip
// to one dependency and holds no connection open, which is what lets a
// release smoke, an operator after an edit, and a container HEALTHCHECK all
// call it.
//
// The lines are printed in the order of spec 012's table and never in the
// order they finished, and no line names a value that changes between runs,
// so two runs against a healthy installation print identical output.
package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/store"
)

// Budget bounds one requirement. A requirement that hangs is a failure of
// that requirement and never of the whole command, so the operator reads the
// other four rather than a command that never returns.
const Budget = 5 * time.Second

// The requirements, in the order the report prints them. The count never
// changes: a requirement that does not apply to this installation says so on
// its own line rather than leaving the table a line shorter.
const (
	NameBucket     = "bucket"
	NameDatabase   = "database"
	NameIssuer     = "issuer"
	NameAuthorizer = "authorizer"
	NamePublicURL  = "public-url"
)

// Requirement is one line of the report: what was tested, whether it passed,
// and one sentence naming what was reached and what happened.
type Requirement struct {
	// Name is the requirement, one word of the table above.
	Name string
	// OK is whether it passed.
	OK bool
	// Detail is the sentence the line ends with. It names what was reached
	// and what happened, and never a value that differs between two runs of
	// a healthy installation.
	Detail string
}

// Line renders the requirement as the report writes it, in columns so a
// human reads down the table and a script cuts the first two fields.
func (r Requirement) Line() string {
	verdict := "fail"
	if r.OK {
		verdict = "ok"
	}
	return fmt.Sprintf("%-6s%-12s%s", verdict, r.Name, r.Detail)
}

// passed and failed are the two a check answers.
func passed(name, format string, a ...any) Requirement {
	return Requirement{Name: name, OK: true, Detail: fmt.Sprintf(format, a...)}
}

func failed(name, format string, a ...any) Requirement {
	return Requirement{Name: name, Detail: fmt.Sprintf(format, a...)}
}

// Database is what the database requirement reaches. *store.DB satisfies it,
// and a test passes its own, so the failure modes of a database are driven
// without one.
type Database interface {
	// Ping answers whether the database is reachable.
	Ping(ctx context.Context) error
	// Querier answers the pool, for the two reads the line makes.
	Querier() store.Querier
	// Close returns every connection.
	Close()
}

// Options is what one run reads. Every seam has a default that reaches the
// real dependency, so the command hands over the configuration alone and a
// test hands over the fault it is driving.
type Options struct {
	// Config is the whole table of spec 002, as the node reads it.
	Config config.Config
	// HTTP sends the discovery reads, the authorizer's call and the read of
	// the public URL. A client bounded by [Budget] when nil.
	HTTP *http.Client
	// Bucket is the store the bucket line reaches. Built from the
	// configuration when nil.
	Bucket blob.Store
	// Open opens the database the database line reaches. store.Open when
	// nil.
	Open func(ctx context.Context, databaseURL string) (Database, error)
	// Pending answers the migrations the database has not applied.
	// store.Pending when nil.
	Pending func(ctx context.Context, q store.Querier) ([]string, error)
	// bucketErr is the failure building the client from the configuration.
	// A client that cannot be built is the bucket line's finding and not the
	// command's, so the other four requirements are still answered.
	bucketErr error
}

// Run tests every requirement and answers one line each, in the order of
// spec 012's table.
//
// The checks run concurrently, because they reach four different
// dependencies and a serial run pays the slowest of them four times over.
// Each writes into its own slot of the answer, so what is printed is the
// table's order and not the order they finished.
func Run(ctx context.Context, o Options) []Requirement {
	o = o.resolved(ctx)
	checks := []func(context.Context, Options) Requirement{
		checkBucket, checkDatabase, checkIssuers, checkAuthorizer, checkPublicURL,
	}
	out := make([]Requirement, len(checks))
	var running sync.WaitGroup
	for i, check := range checks {
		running.Go(func() {
			bounded, cancel := context.WithTimeout(ctx, Budget)
			defer cancel()
			out[i] = check(bounded, o)
		})
	}
	running.Wait()
	return out
}

// resolved fills the seams a caller left open with the ones that reach the
// real dependencies, so every check below reads one shape.
func (o Options) resolved(ctx context.Context) Options {
	if o.HTTP == nil {
		// The same instrumented transport the node's own reads go through,
		// so a check run inside a traced environment is one trace rather
		// than four calls nobody can attribute.
		o.HTTP = &http.Client{Timeout: Budget, Transport: otel.Transport(nil)}
	}
	if o.Bucket == nil {
		// A client that cannot even be built is not an outage, so the bucket
		// line reports it rather than the whole command failing.
		o.Bucket, o.bucketErr = blob.NewS3(ctx, BucketOptions(o.Config))
	}
	if o.Open == nil {
		o.Open = func(ctx context.Context, databaseURL string) (Database, error) {
			return store.Open(ctx, databaseURL)
		}
	}
	if o.Pending == nil {
		o.Pending = store.Pending
	}
	return o
}

// BucketOptions is the bucket client the check opens, from the one table of
// spec 002. It is the twin of bucketOptions in cmd/arcad, which serve and
// reap open theirs from; a test there holds the two equal, because a check
// that reached a different bucket than the server would pass an installation
// the server cannot serve.
func BucketOptions(cfg config.Config) blob.Options {
	return blob.Options{
		Bucket:    cfg.Bucket,
		Endpoint:  cfg.BucketEndpoint,
		Region:    cfg.BucketRegion,
		AccessKey: cfg.BucketAccessKey,
		SecretKey: cfg.BucketSecretKey,
		PathStyle: cfg.BucketPathStyle,
	}
}

// Report prints the table and the summary and answers the process exit code:
// 0 when every requirement passed and 1 when any failed.
//
// The table goes to stdout and the summary to stderr, so a script reads the
// table and a human reads both.
func Report(found []Requirement, stdout, stderr io.Writer) int {
	failures := 0
	for _, r := range found {
		_, _ = fmt.Fprintln(stdout, r.Line())
		if !r.OK {
			failures++
		}
	}
	if failures == 0 {
		_, _ = fmt.Fprintf(stderr, "arcad: %d checks passed\n", len(found))
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "arcad: %d of %d checks failed\n", failures, len(found))
	return 1
}
