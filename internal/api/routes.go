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
// A row with pending set is registered at its right place, behind the right
// verifier exception, and answers not_implemented until the spec that owns
// its behaviour arrives. A later phase does not add rows here: it declares
// them as [Route] values and the node contributes them (register.go).
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
	public      bool
	summary     string
	description string
	// status is the status a success answers.
	status int
	// pending marks a row whose behaviour has not landed.
	pending bool
	// handler answers the route.
	handler func(*API, http.ResponseWriter, *http.Request)
}

// Links is the half of spec 008's surface the frame registers: the three
// routes that redeem a token, which sit outside the verifier because the
// token in the URL is the whole of their authorization.
//
// The other eight rows of that spec are contributed through Options.Routes
// like every other package's, because they are behind the verifier and ask
// an action. These three cannot be: a row contributed there is refused when
// it asks nothing, and a hole in the verifier is the frame's own business
// rather than something a later phase opens by passing a field. So the frame
// declares the rows and takes the service that answers them.
type Links interface {
	// LinkMeta answers GET /v1/shares/links/{token}/meta.
	LinkMeta(w http.ResponseWriter, r *http.Request)
	// LinkList answers GET /v1/shares/links/{token}.
	LinkList(w http.ResponseWriter, r *http.Request)
	// LinkFile answers GET /v1/shares/links/{token}/files/{path...}.
	LinkFile(w http.ResponseWriter, r *http.Request)
}

// link dispatches one of the three public rows to the service the node
// bound, and answers not_implemented in a build that bound none. A row is
// registered whether or not a build answers it, so the surface has one shape
// everywhere and a build that cannot answer a row says so.
func (a *API) link(w http.ResponseWriter, r *http.Request, h func(Links, http.ResponseWriter, *http.Request)) {
	if a.links == nil {
		WriteError(w, r, Refuse(CodeNotImplemented, "this build binds no shares and links service"))
		return
	}
	h(a.links, w, r)
}

// routeTable is the frame's own half of the surface: the three public link
// routes of spec 008, registered outside the verifier because that is where
// they belong and because registering them later would be registering them
// somewhere else, and the event tail of spec 010, whose handler this package
// binds because internal/events is under it. Every other row of spec 013's
// table arrives through the seam of register.go, declared by the package
// that owns its behaviour, on the phases of spec 019.
var routeTable = []route{
	{
		method: http.MethodGet, path: "/v1/events",
		action: authorizer.ActionEventRead, status: http.StatusOK,
		summary:     "List events",
		description: "One page of a space's log, oldest first.",
		handler:     (*API).events,
	},
	{
		method: http.MethodGet, path: "/v1/shares/links/{token}/meta",
		public: true, status: http.StatusOK,
		summary:     "Get link metadata",
		description: "What a link token names, before anything is fetched.",
		handler:     func(a *API, w http.ResponseWriter, r *http.Request) { a.link(w, r, Links.LinkMeta) },
	},
	{
		method: http.MethodGet, path: "/v1/shares/links/{token}",
		public: true, status: http.StatusOK,
		summary:     "List linked files",
		description: "A listing of the subtree a link token names.",
		handler:     func(a *API, w http.ResponseWriter, r *http.Request) { a.link(w, r, Links.LinkList) },
	},
	{
		method: http.MethodGet, path: "/v1/shares/links/{token}/files/{path...}",
		public: true, status: http.StatusOK,
		summary:     "Read linked file",
		description: "One object under the subtree a link token names.",
		handler:     func(a *API, w http.ResponseWriter, r *http.Request) { a.link(w, r, Links.LinkFile) },
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
