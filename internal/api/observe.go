// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"latere.ai/x/pkg/otel"
)

// The request path's half of spec 018: one counter, one histogram, one gauge,
// and one log line per request.
//
// The middleware is the outermost of the frame, outside the verifier and
// outside both rate limits, because a 401 and a 429 are requests this replica
// served and an error rate computed without them is the wrong number. What
// the outermost wrapper cannot know at that point is the route and the error
// code: the route is settled by the mux below it, and the code is settled by
// whichever refusal writes the envelope. Both are written into an
// [observation] whose pointer rides on the context, so the parts of the frame
// that know a fact write it where this wrapper reads it once the answer is
// out.
//
// The route is the mux pattern of the row that matched, never the path: a
// path carries what a person called their file, and a series per path is a
// series per file.

// Metrics is where the request path's counters go. Spec 018 owns the
// registry and the names; this is the seam it binds, so this package
// registers nothing and a replica exporting nothing still serves.
type Metrics interface {
	// RequestStarted reports that one request has begun.
	RequestStarted()
	// RequestFinished reports one answered request: the mux pattern it
	// matched, the class of its status, the error code it carried or ok, and
	// how long it took.
	RequestFinished(route, statusClass, code string, took time.Duration)
	// TokenRejected reports one bearer the verifier refused, by the row of
	// spec 006's reason table.
	TokenRejected(reason string)
}

// Unmatched is the route label of a request no row of the table registers.
// It is one bounded value rather than the path, which is the whole reason a
// route label exists.
const Unmatched = "unmatched"

// OK is the code label of a response that refused nothing.
const OK = "ok"

type observationKey struct{}

// observation is what the frame learns about one request while serving it.
// The pointer is what makes it work: the mux hands each layer a request with
// a context derived from this one, and a derived context carries the same
// pointer, so a write from inside is a read from outside.
type observation struct {
	route   string
	code    string
	subject string
}

// observed puts a fresh observation on the context.
func observed(ctx context.Context) (context.Context, *observation) {
	o := &observation{route: Unmatched, code: OK}
	return context.WithValue(ctx, observationKey{}, o), o
}

// observationFrom reads the observation of the request being served, and nil
// outside one.
func observationFrom(ctx context.Context) *observation {
	o, _ := ctx.Value(observationKey{}).(*observation)
	return o
}

// Naming records the route one handler answers, for the metric and the log
// line of spec 018. It is what a package contributing rows through
// [Options.Routes] gets for free: the frame wraps every registration, so a
// row cannot be registered without its pattern reaching the series.
func naming(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := observationFrom(r.Context()); o != nil {
			o.route = pattern
		}
		next.ServeHTTP(w, r)
	})
}

// knowing records the subject the verifier settled, which is what the line
// of spec 018 names the caller by. It is the rendered subject the authorizer
// is already told and never a claim, a token or an address.
func knowing(ctx context.Context, subject string) {
	if o := observationFrom(ctx); o != nil {
		o.subject = subject
	}
}

// noting records the error code a refusal answered. Every refusal of this
// surface goes out through [WriteError], so this is the one call site.
func noting(ctx context.Context, code string) {
	if o := observationFrom(ctx); o != nil {
		o.code = code
	}
}

// observe is the outermost middleware: it counts the request, times it,
// holds the in-flight gauge, and writes the one line spec 018 asks for.
func (a *API) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, o := observed(r.Context())
		r = r.WithContext(ctx)
		recorder := &recording{ResponseWriter: w, status: http.StatusOK}
		started := a.now()

		a.metrics.RequestStarted()
		next.ServeHTTP(recorder, r)
		took := a.now().Sub(started)
		a.metrics.RequestFinished(o.route, class(recorder.status), o.code, took)

		a.log(r, o, recorder.status, took)
	})
}

// log writes the one line per request of spec 018, at INFO, with the request
// id and the trace id on it. The two are different ids and both are here, so
// a log line and a trace find each other in either direction.
//
// No attribute carries a token, a credential, or a presigned URL, and none
// carries the path: the route is the pattern and the subject is the rendered
// subject the authorizer was already told.
func (a *API) log(r *http.Request, o *observation, status int, took time.Duration) {
	ctx := r.Context()
	traceID, _ := otel.TraceIDs(ctx)
	a.logger().InfoContext(ctx, "request",
		"route", o.route,
		"method", r.Method,
		"status", status,
		"code", o.code,
		"duration_ms", took.Milliseconds(),
		"subject", o.subject,
		"request_id", RequestID(ctx),
		"trace_id", traceID,
	)
}

// logger is where the request lines go: the one the node bootstrapped, or
// slog's default, which is what that bootstrap set.
func (a *API) logger() *slog.Logger {
	if a.logs != nil {
		return a.logs
	}
	return slog.Default()
}

// class is the status_class label: the hundreds digit, which is the only
// part of a status an error rate is computed over. A status outside the four
// classes spec 018 names is recorded as none of them, and the recording
// surface drops it.
func class(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return ""
	}
}

// recording is the response writer the middleware reads the status off.
//
// It keeps the interfaces the surface needs rather than hiding them: Unwrap
// is what http.ResponseController follows, so a handler that flushes a stream
// or sets a write deadline reaches the real writer through it.
type recording struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *recording) WriteHeader(status int) {
	if !r.written {
		r.status, r.written = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recording) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Unwrap answers the writer beneath, which is what http.ResponseController
// follows to reach a flusher or a deadline.
func (r *recording) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// nothing records nothing. It is the seam's value when the node gave none,
// so the request path costs no branch and a test of the frame needs no
// registry.
type nothing struct{}

func (nothing) RequestStarted()                                       {}
func (nothing) RequestFinished(string, string, string, time.Duration) {}
func (nothing) TokenRejected(string)                                  {}
