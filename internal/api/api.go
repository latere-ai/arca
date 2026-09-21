// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package api is the /v1 surface of spec 013: the router, the one error
// envelope, the list envelope, the conditional request headers, the request
// id, the two rate limits, and the OpenAPI document generated from the route
// table.
//
// The route table of routes.go and the rows the owning packages contribute
// through Options.Routes merge into one list at start, and that list is the
// one declaration of the surface. The mux is built from it, the document is
// built from it, and the tests read it, so a route cannot be registered
// without an action, described without being registered, or registered
// outside the verifier without a test naming it as one of the three
// exceptions spec 013 allows. See register.go for the seam.
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
	"cmp"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/arca/internal/apidocs"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
)

// DefaultBasePath is the base the surface is registered under where nothing
// else is configured (spec 027): the root of the version, which is what a
// self-hosted installation serves and what the committed document names.
const DefaultBasePath = "/v1"

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
	Verifier   *auth.Verifier
	Authorizer *auth.Authorizer
	PublicURL  string
	// BasePath is ARCA_BASE_PATH: the base every row of the route table is
	// registered under and the base the served document writes into its
	// paths (spec 027). Empty is DefaultBasePath, which is the table as it
	// is declared. The value is validated where it is read, in
	// internal/config, so the surface takes it as given.
	BasePath                         string
	RequestsPerMinute                int
	UnauthenticatedRequestsPerMinute int
	// Events is the log GET /v1/events tails and Querier the database it
	// reads through, both spec 010's. The log is required, because the route
	// is registered and a surface that registers a route it cannot answer
	// would serve what nobody wrote; the querier is the log's own business,
	// and a log that reaches no database takes none.
	Events  events.Log
	Querier store.Querier
	// Links answers the three public link routes of spec 008. They are the
	// frame's own rows, because a row contributed through Routes is behind
	// the verifier and these three are the exception to it, so the service
	// that holds their behaviour is handed over rather than registered. A
	// build that binds none answers not_implemented from them.
	Links Links
	// Routes are the rows of spec 013's table the packages that own their
	// behaviour contribute. See register.go: a contributed row is behind
	// the verifier, asks one action of spec 006's vocabulary, and joins the
	// one list the mux and the document are both built from.
	Routes []Route
	// Metrics is spec 018's seam on the request path. Nil records nothing,
	// which is what a test of the frame and a replica exporting nothing both
	// want.
	Metrics Metrics
	// Logger is where the one line per request goes. Nil is slog's default,
	// which is the logger cmd/arcad bootstrapped.
	Logger *slog.Logger
	// Now is the clock request ids are minted on. time.Now when nil.
	Now func() time.Time
}

// API is the /v1 surface. It is built once at start and serves every
// replica's requests; it keeps two token buckets and nothing else.
type API struct {
	verifier   *auth.Verifier
	authorizer *auth.Authorizer
	publicURL  string
	basePath   string
	perSubject *ratelimit.Buckets
	perAddress *ratelimit.Buckets
	links      Links
	clock      func() time.Time
	rows       []route
	document   []byte
	// eventTail is the handler of spec 010, built once over the log and the
	// database of this build.
	eventTail http.Handler
	// metrics and logs are spec 018's two: the counters of the request path
	// and the line each request ends on. Neither is ever nil.
	metrics Metrics
	logs    *slog.Logger
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
	if o.Events == nil {
		return nil, errors.New("api: no event log, and GET /v1/events is a route of this surface")
	}
	rows, err := merge(routeTable, o.Routes)
	if err != nil {
		return nil, err
	}
	a := &API{
		verifier: o.Verifier, authorizer: o.Authorizer,
		publicURL: o.PublicURL, basePath: cmp.Or(o.BasePath, DefaultBasePath),
		links: o.Links, clock: o.Now,
		perSubject: buckets(o.RequestsPerMinute),
		perAddress: buckets(o.UnauthenticatedRequestsPerMinute),
		rows:       rows,
		metrics:    o.Metrics, logs: o.Logger,
	}
	if a.metrics == nil {
		a.metrics = nothing{}
	}
	a.eventTail = events.Handler(o.Events, o.Querier, eventGuard{api: a}, refuseEvent)
	a.document = a.build(rows)
	return a, nil
}

// Authorizer is the seam handlers decide through, for the packages of later
// phases that hold their own handlers.
func (a *API) Authorizer() *auth.Authorizer { return a.authorizer }

// Mount registers the surface on the public listener's mux beside the probes
// and GET / of spec 002.
//
// Every row is registered under the base path: the table declares /v1 and
// the mount swaps that leading segment for ARCA_BASE_PATH, so an
// installation behind an origin partitioned by capability answers at the
// prefix it was given and a self-hosted one answers at the root of the
// version (spec 027).
//
// The three public routes are registered as their own patterns and the rest
// of the base path behind the verifier as one subtree. The router prefers
// the more specific pattern, so a link route reaches its handler without a
// bearer while every other path under the base, registered or not, meets the
// verifier first. A path nobody registered is therefore a 401 before it is a
// 404, which is the right order: whether a route exists is not something an
// unauthenticated caller learns. A path outside the base path matches no
// pattern of this mux at all and takes the router's own bare 404, which is
// the inverse of the same rule: there is no surface there to protect.
func (a *API) Mount(mux *http.ServeMux) {
	a.mount(mux, a.rows)
}

func (a *API) mount(mux *http.ServeMux, rows []route) {
	const document = "GET /openapi.json"
	mux.Handle(document, a.requestID(a.observe(naming(document, http.HandlerFunc(a.openapi)))))
	guarded := http.NewServeMux()
	guarded.Handle("/", http.HandlerFunc(a.notFound))
	for _, r := range rows {
		pattern := r.method + " " + a.under(r.path)
		answer := r.handler
		handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { answer(a, w, req) })
		if r.public {
			mux.Handle(pattern, a.requestID(a.observe(naming(pattern, a.limitAddress(handler)))))
			continue
		}
		guarded.Handle(pattern, naming(pattern, handler))
	}
	// The observation of spec 018 sits directly inside the request id and
	// outside everything else: outside the verifier and outside the rate
	// limit, because a 401 and a 429 are requests this replica served and an
	// error rate computed without them is the wrong number, and inside the
	// request id because the line it writes carries that id.
	//
	// What it cannot see from there reaches it from inside: the route
	// through the wrapper each registration carries, the code through the
	// one place a refusal is written, and the subject through the limit that
	// runs the moment the verifier settles one.
	mux.Handle(a.basePath+"/", a.requestID(a.observe(
		a.verifier.Middleware(a.refuseVerification)(
			a.limitSubject(guarded)))))
}

// under is the one place a declared path becomes a registered one: the
// table of spec 013 declares /v1/files/..., and an installation serves it
// under its base path. The mux and the document both read it, so what a
// client generates against and what the router answers at cannot drift.
func (a *API) under(path string) string {
	return a.basePath + strings.TrimPrefix(path, DefaultBasePath)
}

// BasePath is the base this surface is registered under, for the node's
// start-up line and for a test that asks where a route answers.
func (a *API) BasePath() string { return a.basePath }

// notFound answers a path under the base path that no row of the route table
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
		Routes: a.described(rows),
		Errors: Errors(),
	}).JSON()
}

// described is what the served document reads: the rows projected once, each
// path under the base path this build mounts at. The server stays what
// ARCA_PUBLIC_URL names, the origin root, so the address of a route is that
// server and the path the document writes, and the paths are the patterns
// the mux registered because both read [API.under].
func (a *API) described(rows []route) []apidocs.Route {
	out := routesOf(rows)
	for i := range out {
		out[i].Path = a.under(out[i].Path)
	}
	return out
}

// routesOf projects rows onto what a document reads, at the paths the table
// declares. The committed document of tools/apidoc is generated from these,
// which is the self-hoster's shape and the shape a consumer reads.
func routesOf(rows []route) []apidocs.Route {
	out := make([]apidocs.Route, len(rows))
	for i, r := range rows {
		out[i] = apidocs.Route{
			Method: r.method, Path: r.path, Action: r.action, Public: r.public,
			Summary: r.summary, Description: r.description, Status: r.status, Pending: r.pending,
		}
	}
	return out
}
