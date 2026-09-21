// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"net/http"
	"strings"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/apidocs"
)

// The seam a spec's own package contributes its rows of spec 013's table
// through.
//
// The route table of routes.go holds the rows the frame itself answers, and
// there are two kinds. Three are the public link routes, which belong
// outside the verifier and could not be registered anywhere else. The fourth
// is the event tail of spec 010: its handler is internal/events', and the
// frame binds it, because that package is under this one and a row it
// contributed would be an import cycle.
//
// Every other row's behaviour belongs to a package above this one, which
// holds its own handlers, so it declares its rows and the node hands them
// here. The mux and the OpenAPI document are still built from one list read
// twice, which is the property the frame exists to keep.
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
	// Summary is a verb-first navigation label of at most four words.
	Summary string
	// Description preserves behavior, qualifications, and alternatives.
	Description string
	// Status is the status a success answers.
	Status int
	// Handler answers the route, behind the verifier and the rate limit.
	Handler http.Handler
}

// reserved are the words of spec 013's fifth grammar rule: a literal that
// sits beside a wildcard in the same position, wins there, and is therefore
// not a value that position can carry. materialize is not an owner, links
// and with-me are not share ids, and deleted is not a workspace id.
//
// A fifth shadowing literal is refused by [merge] until it is written into
// that rule. Go's router gives a literal precedence over a wildcard without
// complaining, so nothing else in this program would say that a new route
// had just taken a word out of a caller's namespace.
var reserved = map[string]bool{
	"materialize": true,
	"links":       true,
	"with-me":     true,
	"deleted":     true,
}

// shadowing answers the literal segment at the first position where one path
// carries a literal and the other a wildcard, and the position it sits at,
// which is where the router decides between the two.
//
// Two literals that differ put the paths in different subtrees, so nothing
// below that position can shadow anything and the walk stops. Two wildcards
// are one position spelled with two names, so the walk goes on. A path that
// runs out is a prefix of the other and shadows nothing.
func shadowing(a, b string) (word string, at int, ok bool) {
	x, y := segmentsOf(a), segmentsOf(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		switch {
		case x[i] == y[i]:
		case wildcard(x[i]) && wildcard(y[i]):
		case wildcard(x[i]):
			return y[i], i, true
		case wildcard(y[i]):
			return x[i], i, true
		default:
			return "", 0, false
		}
	}
	return "", 0, false
}

// segmentsOf splits a registration into its path segments.
func segmentsOf(path string) []string { return strings.Split(strings.Trim(path, "/"), "/") }

// wildcard reports whether a segment is one of the router's, "{id}" or
// "{path...}", rather than a literal word.
func wildcard(segment string) bool {
	return strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")
}

// merge folds the contributed rows into the frame's table and refuses a set
// that could not be a surface: a row that asks nothing, one that asks a
// string no authorizer can answer, one with no handler, one registering a
// method and path another row already holds, and one whose literal shadows
// another row's wildcard without a word in spec 013's fifth grammar rule.
// Each of those is a programming error that would otherwise become a route
// nobody decided, so the node fails to start rather than serving it.
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
			summary: r.Summary, description: r.Description, status: r.Status,
			handler: func(_ *API, w http.ResponseWriter, req *http.Request) {
				handler.ServeHTTP(w, req)
			},
		})
	}
	// The fifth grammar rule of spec 013, over the merged list: the pair it
	// governs may be one frame row and one contributed row, or two
	// contributed rows, so neither half of the surface can be checked alone.
	for i, a := range rows {
		for _, b := range rows[i+1:] {
			if a.method != b.method {
				continue
			}
			word, at, shadows := shadowing(a.path, b.path)
			if !shadows || reserved[word] {
				continue
			}
			literal, wild := a, b
			if wildcard(segmentsOf(a.path)[at]) {
				literal, wild = b, a
			}
			return nil, fmt.Errorf(
				"api: %s %s puts the literal %q where %s %s has a wildcard, and %q is not one of spec 013's reserved words",
				literal.method, literal.path, word, wild.method, wild.path, word)
		}
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
			Summary: r.Summary, Description: r.Description, Status: r.Status,
		})
	}
	return out
}
