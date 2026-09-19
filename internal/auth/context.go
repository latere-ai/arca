// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"

	"latere.ai/x/pkg/authz"
)

// The caller and what is known about the request ride on the context, so a
// handler names an action and a resource and nothing else, and every
// question carries the same request block the log and the trace carry.

type callerKey struct{}

type requestKey struct{}

type marksKey struct{}

// WithCaller carries the verified caller. [Verifier.Middleware] sets it on
// every request under /v1 that is not one of the three public link routes;
// those carry the anonymous caller, which is the zero value.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom reads the verified caller. The zero Caller is the anonymous
// one, which is what a public link route sees and what a question about it
// carries.
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// WithRequest carries the request block the authorizer sees: the request id
// of spec 013, the peer address, and the user agent. The id is the one the
// error envelope, the event and the trace carry, so an operator's authorizer
// and Arca's own log name one request.
func WithRequest(ctx context.Context, info authz.Caller) context.Context {
	return context.WithValue(ctx, requestKey{}, info)
}

// RequestFrom reads the request block, zero where none was set.
func RequestFrom(ctx context.Context) authz.Caller {
	info, _ := ctx.Value(requestKey{}).(authz.Caller)
	return info
}

// marks is what the decision path learned about one request and the log
// reads back. Today it holds one fact, spec 012's: an allow this request
// received on a space its caller neither owns nor holds a covering grant on.
//
// It is a holder on the context rather than a context value of its own
// because the two ends are far apart and only one of them may know. What
// makes an event administrative is a property of the answer, not of the
// handler that acted on it, and a handler that carried the property along
// would be a route that has to be told to record itself. A value cannot be
// added to a context from inside a call, so the request carries the holder
// and the decision writes into it.
//
// Nothing clears a mark. One administrative allow on a request is what the
// request was, and a later question about the caller's own space does not
// undo it.
type marks struct{ administrative atomic.Bool }

// WithMarks installs the record for one request. The API's first middleware
// sets it on every route under /v1, before the verifier, so a question asked
// anywhere on the request writes into the same record. A context with none
// is a call outside a request, where nothing marks and nothing reads: the
// reaper is the one that matters, and its events belong to no caller.
func WithMarks(ctx context.Context) context.Context {
	return context.WithValue(ctx, marksKey{}, &marks{})
}

// markAdministrative records one allow that neither ownership nor a grant
// explains. It is unexported on purpose: the only caller is the decision
// path below, and a handler that could set it is the thing this mechanism
// exists to avoid.
func markAdministrative(ctx context.Context) {
	if m, ok := ctx.Value(marksKey{}).(*marks); ok {
		m.administrative.Store(true)
	}
}

// Administrative reports whether this request received such an allow, which
// is what an event's detail carries as admin (spec 012). False outside a
// request and false for every caller acting in its own space.
func Administrative(ctx context.Context) bool {
	m, ok := ctx.Value(marksKey{}).(*marks)
	return ok && m.administrative.Load()
}

// RequestInfo reads what the authorizer learns about the request itself off
// an http.Request: the id already on the context, the peer address with its
// port removed, and the user agent.
func RequestInfo(r *http.Request, id string) authz.Caller {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return authz.Caller{ID: id, IP: ip, UserAgent: r.UserAgent()}
}

// Refuser writes the refusal a middleware decided. The envelope is spec
// 013's and lives with the API, so the verifier names the reason and the API
// writes the status line and the body; that is also what keeps this package
// free of the error table.
type Refuser func(w http.ResponseWriter, r *http.Request, err error)

// Middleware refuses every request whose bearer the verifier does not
// accept, with the row of the reason table in the developer detail and the
// one fixed sentence of unauthenticated in the message. An admitted request
// carries its [Caller].
//
// It is one middleware over /v1. The probes, GET /, GET /openapi.json and
// the three public link routes of spec 013 are outside it, and nothing else
// is: that list is the whole exception, and a route-table test in the API
// package names it and no more.
func (v *Verifier) Middleware(refused Refuser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := v.Authenticate(r)
			if err != nil {
				refused(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), c)))
		})
	}
}
