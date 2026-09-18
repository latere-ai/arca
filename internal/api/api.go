// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package api is the /v1 surface of spec 013: the router, the one error
// envelope, the list envelope, the conditional request headers, the request
// id, the two rate limits, and the OpenAPI document generated from the route
// table.
//
// The route table of routes.go is the one declaration of the surface. The
// mux is built from it, the document is built from it, and the tests read
// it, so a route cannot be registered without an action, described without
// being registered, or registered outside the verifier without a test naming
// it as one of the three exceptions spec 013 allows.
//
// Every request under /v1 runs behind the verifier of spec 006, with three
// exceptions: GET /v1/shares/links/{token}, its /meta and its /files/...,
// where the token in the URL is the whole of the authorization. They sit
// outside the verifier, resolve the token first, and ask link.read with an
// anonymous subject. Nothing else is outside it.
//
// The behaviour of each route is the owning spec's. This package holds the
// wire and the frame, and a row whose spec has not landed answers
// not_implemented from its right place in the router.
package api

import (
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/arca/internal/apidocs"
	"latere.ai/x/arca/internal/auth"
)

// The identity of the OpenAPI document, shared by the one GET /openapi.json
// answers and the one committed at api/openapi.yaml, so the two differ in
// nothing but the server they name.
const (
	Title = "Arca"
	// DocumentVersion is the version of the contract the document
	// describes, not of the binary that serves it. A binary's version moves
	// on every release and the surface does not, so a committed document
	// that carried one would change with every tag and say nothing.
	DocumentVersion = "1"
	Description     = "Durable storage for people, agents, and sandboxes: files with versions, " +
		"trash and stars, uploads in parts, shares with a permission ladder and public links, " +
		"workspaces with a writer lease, and an event log."
)

// Options is what the node hands the surface. Verifier and Authorizer are
// the two of spec 006, built once by auth.Start; PublicURL is the base the
// OpenAPI document names; the two rates are spec 002's variables.
type Options struct {
	Verifier                         *auth.Verifier
	Authorizer                       *auth.Authorizer
	PublicURL                        string
	RequestsPerMinute                int
	UnauthenticatedRequestsPerMinute int
	// Routes are the rows of spec 013's table registered by the packages
	// that own their behaviour, each bound to its handler. The frame's own
	// three are in the route table of routes.go and are not repeated here.
	Routes []Route
	// Now is the clock request ids are minted on. time.Now when nil.
	Now func() time.Time
}

// API is the /v1 surface. It is built once at start and serves every
// replica's requests; it keeps two token buckets and nothing else.
type API struct {
	verifier   *auth.Verifier
	authorizer *auth.Authorizer
	publicURL  string
	perSubject *ratelimit.Buckets
	perAddress *ratelimit.Buckets
	clock      func() time.Time
	document   []byte
	// rows is the surface this build serves: the frame's table and the rows
	// the node registered. The mux and the document are both built from it,
	// so a route cannot be served without being described.
	rows []route
}

// New builds the surface. It refuses to build without the two of spec 006,
// because every route either verifies or asks and a surface missing either
// would serve what nobody decided.
func New(o Options) (*API, error) {
	if o.Verifier == nil {
		return nil, errors.New("api: no verifier, and every route under /v1 but the three link routes runs behind one")
	}
	if o.Authorizer == nil {
		return nil, errors.New("api: no authorizer, and every route asks before it acts")
	}
	rows, err := rowsOf(routeTable, o.Routes)
	if err != nil {
		return nil, err
	}
	a := &API{
		verifier: o.Verifier, authorizer: o.Authorizer,
		publicURL: o.PublicURL, clock: o.Now,
		perSubject: buckets(o.RequestsPerMinute),
		perAddress: buckets(o.UnauthenticatedRequestsPerMinute),
		rows:       rows,
	}
	a.document = a.build(rows)
	return a, nil
}

// Authorizer is the seam handlers decide through, for the packages of later
// phases that hold their own handlers.
func (a *API) Authorizer() *auth.Authorizer { return a.authorizer }

// Mount registers the surface on the public listener's mux beside the probes
// and GET / of spec 002.
//
// The three public routes are registered as their own patterns and the rest
// of /v1 behind the verifier as one subtree. The router prefers the more
// specific pattern, so a link route reaches its handler without a bearer
// while every other path under /v1, registered or not, meets the verifier
// first. A path nobody registered is therefore a 401 before it is a 404,
// which is the right order: whether a route exists is not something an
// unauthenticated caller learns.
func (a *API) Mount(mux *http.ServeMux) {
	a.mount(mux, a.rows)
}

func (a *API) mount(mux *http.ServeMux, rows []route) {
	mux.Handle("GET /openapi.json", a.requestID(http.HandlerFunc(a.openapi)))
	guarded := http.NewServeMux()
	guarded.Handle("/", http.HandlerFunc(a.notFound))
	for _, r := range rows {
		pattern := r.method + " " + r.path
		answer := r.handler
		handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { answer(a, w, req) })
		if r.public {
			mux.Handle(pattern, a.requestID(a.limitAddress(handler)))
			continue
		}
		guarded.Handle(pattern, handler)
	}
	mux.Handle("/v1/", a.requestID(
		a.verifier.Middleware(a.refuseVerification)(
			a.limitSubject(guarded))))
}

// notFound answers a path under /v1 that no row of the route table
// registers, in the envelope every other refusal uses.
func (a *API) notFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, Refuse(CodeNotFound, "%s %s is not a route of this API", r.Method, r.URL.Path))
}

// build renders the OpenAPI document once at start, from the route table and
// the error table. It is built rather than embedded, so the document served
// is the surface registered and the two cannot drift.
func (a *API) build(rows []route) []byte {
	return apidocs.Build(apidocs.Options{
		Title: Title, Version: DocumentVersion, Description: Description,
		Server: a.publicURL,
		Routes: routesOf(rows),
		Errors: Errors(),
	}).JSON()
}

// routesOf projects rows onto what the document reads.
func routesOf(rows []route) []apidocs.Route {
	out := make([]apidocs.Route, len(rows))
	for i, r := range rows {
		out[i] = apidocs.Route{
			Method: r.method, Path: r.path, Action: r.action, Public: r.public,
			Summary: r.summary, Status: r.status, Pending: r.pending,
		}
	}
	return out
}
