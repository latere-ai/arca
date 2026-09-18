// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/apidocs"
)

// route is one row of spec 013's route table as arcad registers it: the
// registration, the authorizer action it asks, and the handler that answers
// it. The table is the one declaration of the surface. The mux is built from
// it, the OpenAPI document is built from it, and the tests read it, so a
// route cannot exist in one of the three and not the others.
//
// A later phase adds its rows here beside the handlers it lands. A row with
// pending set is registered at its right place, behind the right verifier
// exception, and answers not_implemented until the spec that owns its
// behaviour arrives.
type route struct {
	method string
	path   string
	// action is the one action of spec 006's vocabulary this route asks
	// before it acts. It is empty only on the three public link routes,
	// where the grant the token in the URL resolves to is the whole of the
	// authorization.
	action string
	// public registers the route outside the verifier. It is the exception
	// to invariant 5 of spec 001, and the route table test names every row
	// that carries it.
	public  bool
	summary string
	// status is the status a success answers.
	status int
	// pending marks a row whose behaviour has not landed.
	pending bool
	// handler answers the route.
	handler func(*API, http.ResponseWriter, *http.Request)
}

// Shares is the surface of spec 008 as internal/shares implements it: one
// method per row of the route table below, each the handler of one route.
//
// It is an interface because the behaviour of a route belongs to the spec
// that owns it and the declaration of the surface belongs here. A package of
// a later phase implements the interface of its own rows, the node hands the
// implementation over at start, and neither package imports the other's
// handlers.
type Shares interface {
	// CreateGrant answers POST /v1/shares.
	CreateGrant(w http.ResponseWriter, r *http.Request)
	// ListGrants answers GET /v1/shares.
	ListGrants(w http.ResponseWriter, r *http.Request)
	// GrantsWithMe answers GET /v1/shares/with-me.
	GrantsWithMe(w http.ResponseWriter, r *http.Request)
	// ReadGrant answers GET /v1/shares/{id}.
	ReadGrant(w http.ResponseWriter, r *http.Request)
	// RevokeGrant answers DELETE /v1/shares/{id}.
	RevokeGrant(w http.ResponseWriter, r *http.Request)
}

// share dispatches one row of spec 008's surface to the implementation the
// node bound, and answers not_implemented in a build that bound none. A row
// is registered at its right place either way, which is what keeps the
// surface one declaration.
func (a *API) share(w http.ResponseWriter, r *http.Request, h func(Shares, http.ResponseWriter, *http.Request)) {
	if a.shares == nil {
		WriteError(w, r, Refuse(CodeNotImplemented, "this build binds no shares and links service"))
		return
	}
	h(a.shares, w, r)
}

// routeTable is the surface. Today it is the frame of spec 013 and the
// grants of spec 008: the five routes a grant is created, read, listed and
// revoked through, and the three public link routes, registered outside the
// verifier because that is where they belong and because registering them
// later would be registering them somewhere else. Every other row of spec
// 013's table arrives with the spec that owns its behaviour, on the phases
// of spec 019.
var routeTable = []route{
	{
		method: http.MethodPost, path: "/v1/shares",
		action: authorizer.ActionShareCreate, status: http.StatusCreated,
		summary: "Grant a subject a permission on a subtree of a space.",
		handler: func(a *API, w http.ResponseWriter, r *http.Request) { a.share(w, r, Shares.CreateGrant) },
	},
	{
		method: http.MethodGet, path: "/v1/shares",
		action: authorizer.ActionShareList, status: http.StatusOK,
		summary: "The grants on a space.",
		handler: func(a *API, w http.ResponseWriter, r *http.Request) { a.share(w, r, Shares.ListGrants) },
	},
	{
		method: http.MethodGet, path: "/v1/shares/with-me",
		action: authorizer.ActionShareList, status: http.StatusOK,
		summary: "The grants whose grantee is the caller.",
		handler: func(a *API, w http.ResponseWriter, r *http.Request) { a.share(w, r, Shares.GrantsWithMe) },
	},
	{
		method: http.MethodGet, path: "/v1/shares/{id}",
		action: authorizer.ActionShareRead, status: http.StatusOK,
		summary: "One grant.",
		handler: func(a *API, w http.ResponseWriter, r *http.Request) { a.share(w, r, Shares.ReadGrant) },
	},
	{
		method: http.MethodDelete, path: "/v1/shares/{id}",
		action: authorizer.ActionShareRevoke, status: http.StatusNoContent,
		summary: "Revoke a grant.",
		handler: func(a *API, w http.ResponseWriter, r *http.Request) { a.share(w, r, Shares.RevokeGrant) },
	},
	{
		method: http.MethodGet, path: "/v1/shares/links/{token}/meta",
		public: true, pending: true, status: http.StatusOK,
		summary: "What a link token names, before anything is fetched.",
		handler: (*API).link,
	},
	{
		method: http.MethodGet, path: "/v1/shares/links/{token}",
		public: true, pending: true, status: http.StatusOK,
		summary: "A listing of the subtree a link token names.",
		handler: (*API).link,
	},
	{
		method: http.MethodGet, path: "/v1/shares/links/{token}/files/{path...}",
		public: true, pending: true, status: http.StatusOK,
		summary: "One object under the subtree a link token names.",
		handler: (*API).link,
	},
}

// Routes is the route table as the OpenAPI document reads it: the
// descriptive half of every row, in the table's order. The handlers are the
// API's own and no document names them.
func Routes() []apidocs.Route { return routesOf(routeTable) }

// Errors is the error table as the OpenAPI document reads it, sorted by
// code so a regenerated document does not reorder.
func Errors() []apidocs.ErrorCode {
	codes := Codes()
	out := make([]apidocs.ErrorCode, len(codes))
	for i, code := range codes {
		out[i] = apidocs.ErrorCode{Code: code, Status: Status(code), Sentence: Sentence(code)}
	}
	return out
}

// link answers the three public link routes until spec 008 lands their
// behaviour. It is registered at the right place with the right exception,
// so the shape of the surface is settled before the handler is.
func (a *API) link(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, Refuse(CodeNotImplemented,
		"the public link routes arrive with the shares and links of spec 008"))
}
