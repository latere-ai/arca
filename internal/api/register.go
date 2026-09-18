// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"net/http"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/apidocs"
)

// The seam a spec's own package contributes its rows of spec 013's table
// through.
//
// The route table of routes.go is the frame's: the three public link routes,
// which belong outside the verifier and could not be registered anywhere
// else. Every other row's behaviour belongs to the package that owns it, and
// that package holds its own handlers, so it declares its rows and the node
// hands them here. The mux and the OpenAPI document are still built from one
// list read twice, which is the property the frame exists to keep.
//
// A contributed row cannot be public. The three exceptions of spec 013 are
// the whole exception to invariant 5 of spec 001, and a fourth would have to
// be written into the frame's own table and into the test that names them,
// which is the point: a hole in the verifier is not something a later phase
// opens by passing a field.

// Route is one row of spec 013's table contributed by the package that owns
// its behaviour.
type Route struct {
	// Method and Path are the registration, with the wildcards in the
	// router's own spelling: "/v1/workspaces/{id}/attach/{aid}/renew".
	Method, Path string
	// Action is the one action of spec 006's vocabulary the route asks
	// before it acts. A row that asks none is refused: every route behind
	// the verifier asks.
	//
	// Where the action a route asks is chosen per request, this is the row's
	// action for the document and the test that reads it, and the handler
	// asks the one the request selected. Spec 013 names two such rows, the
	// attach and the two that follow the action the attach asked.
	Action string
	// Summary is the one line a reader of the document sees.
	Summary string
	// Status is the status a success answers.
	Status int
	// Handler answers the route, behind the verifier and the rate limit.
	Handler http.Handler
}

// merge folds the contributed rows into the frame's table and refuses a set
// that could not be a surface: a row that asks nothing, one that asks a
// string no authorizer can answer, one with no handler, and one registering
// a method and path another row already holds. Each of those is a
// programming error that would otherwise become a route nobody decided, so
// the node fails to start rather than serving it.
func merge(frame []route, added []Route) ([]route, error) {
	rows := make([]route, 0, len(frame)+len(added))
	seen := make(map[string]bool, len(frame)+len(added))
	for _, r := range frame {
		rows = append(rows, r)
		seen[r.method+" "+r.path] = true
	}
	for _, r := range added {
		key := r.Method + " " + r.Path
		switch {
		case seen[key]:
			return nil, fmt.Errorf("api: %s is registered twice", key)
		case r.Handler == nil:
			return nil, fmt.Errorf("api: %s has no handler", key)
		case r.Action == "":
			return nil, fmt.Errorf("api: %s asks nothing, and every route behind the verifier asks", key)
		case !authorizer.Known(r.Action):
			return nil, fmt.Errorf("api: %s asks %q, which spec 006's vocabulary does not name", key, r.Action)
		}
		seen[key] = true
		handler := r.Handler
		rows = append(rows, route{
			method: r.Method, path: r.Path, action: r.Action,
			summary: r.Summary, status: r.Status,
			handler: func(_ *API, w http.ResponseWriter, req *http.Request) {
				handler.ServeHTTP(w, req)
			},
		})
	}
	return rows, nil
}

// Described projects contributed rows onto what the OpenAPI document reads,
// so the generator of the committed file renders the whole surface from the
// same declarations the node registers. The handlers are not read, which is
// why a package answers the generator with its table and the node with its
// routes.
func Described(rows []Route) []apidocs.Route {
	out := make([]apidocs.Route, 0, len(rows))
	for _, r := range rows {
		out = append(out, apidocs.Route{
			Method: r.Method, Path: r.Path, Action: r.Action,
			Summary: r.Summary, Status: r.Status,
		})
	}
	return out
}
