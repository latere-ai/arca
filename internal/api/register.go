// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"net/http"
	"slices"

	"latere.ai/x/arca/authorizer"
)

// Route is one row of spec 013's table registered by the package that owns
// its behaviour. The frame owns the wire and the three public link routes;
// every other row arrives from the spec that answers it, because a handler
// needs the stores and the seams its own package holds and this package
// holds none of them.
//
// A row is declared without its handler and bound with one, so the generator
// of the committed document reads the same declaration the node mounts: see
// Routes, which takes the declared rows, and Options.Routes, which takes the
// bound ones.
type Route struct {
	// Method and Path are the registration, in the router's own spelling:
	// "PUT", "/v1/files/{owner}/{path...}".
	Method, Path string
	// Action is the one action of spec 006's vocabulary the handler asks
	// before it acts. It is never empty here: the three routes that ask
	// nothing are the frame's own, and a fourth would be a hole in
	// invariant 5 of spec 001.
	Action string
	// Summary is the one line a reader of the document sees.
	Summary string
	// Status is the status a success answers.
	Status int
	// Handle answers the route. A declared row carries none, and New
	// refuses to build a surface that mounts one.
	Handle http.HandlerFunc
}

// Bind answers the row with its handler, so a package declares its rows once
// and hands the node the same rows with the handlers attached.
func (r Route) Bind(h http.HandlerFunc) Route {
	r.Handle = h
	return r
}

// methods are the methods a row may register. A method outside this list is
// a typo rather than a decision, and the router would answer 405 to every
// request for a route nobody could reach.
var methods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete,
}

// rowsOf converts registered rows into the table's own shape, refusing every
// row the surface would not be able to hold to spec 013.
//
// The refusals are start-up failures and not runtime ones. A row with an
// action outside the vocabulary is a question no authorizer can answer, and
// the shared client refuses it before the wire, so it would be an outage
// rather than a deny; a row registered twice is two handlers for one path,
// and the router would panic at mount; a row with no action would be a route
// under /v1 that acts without asking.
func rowsOf(base []route, extra []Route) ([]route, error) {
	out := slices.Clone(base)
	seen := map[string]bool{}
	for _, r := range out {
		seen[r.method+" "+r.path] = true
	}
	for _, r := range extra {
		key := r.Method + " " + r.Path
		switch {
		case !slices.Contains(methods, r.Method):
			return nil, fmt.Errorf("api: %s registers the method %q", key, r.Method)
		case !isVersioned(r.Path):
			return nil, fmt.Errorf("api: %s is not under /v1, which is the one surface this server has", key)
		case r.Action == "":
			return nil, fmt.Errorf("api: %s asks nothing, and every route under /v1 but the three link routes asks before it acts", key)
		case !authorizer.Known(r.Action):
			return nil, fmt.Errorf("api: %s asks %q, which spec 006's vocabulary does not name", key, r.Action)
		case r.Handle == nil:
			return nil, fmt.Errorf("api: %s is registered without a handler", key)
		case seen[key]:
			return nil, fmt.Errorf("api: %s is registered twice", key)
		}
		seen[key] = true
		answer := r.Handle
		out = append(out, route{
			method: r.Method, path: r.Path, action: r.Action,
			summary: r.Summary, status: r.Status,
			handler: func(_ *API, w http.ResponseWriter, req *http.Request) { answer(w, req) },
		})
	}
	return out, nil
}

// isVersioned reports whether a path is under the one surface this server
// serves.
func isVersioned(path string) bool { return len(path) > 4 && path[:4] == "/v1/" }
