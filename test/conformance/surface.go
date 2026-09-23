// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The surface of spec 013 as the suite holds it, and what the target
// answers of it.
//
// The table below is the suite's own declaration of the forty-one routes,
// not a reading of the server's registrations: a build that dropped a route
// has to fail, and a suite that read the build's own list would drop it too.
// What the target serves is read from the document spec 013 publishes, and
// the difference is the pending set.

// route is one row of spec 013's table: the registration in the router's
// spelling, and the spec that owns the behavior behind it.
type route struct {
	method, path string
	// spec is the number of the spec that owns the behavior, which is what
	// the pending group reports a route as waiting on.
	spec string
}

// key is the row as a case names it.
func (r route) key() string { return r.method + " " + r.path }

// template is the row's path as a document names it. The router spells a
// segment that swallows the rest of a path "{path...}", and a path template
// has no such form, so the suffix goes.
func (r route) template() string { return strings.ReplaceAll(r.path, "...", "") }

// surfaceTable is the forty-one routes of spec 013, grouped by the spec that
// owns each. Outside /v1 sits one more document, GET /openapi.json, which
// the errors group asserts and which is not a row here because it is not a
// route of the surface.
var surfaceTable = []route{
	// Files, trash, and stars (spec 005).
	{http.MethodPut, "/v1/files/{owner}/{path...}", "005"},
	{http.MethodGet, "/v1/files/{owner}/{path...}", "005"},
	{http.MethodHead, "/v1/files/{owner}/{path...}", "005"},
	{http.MethodPost, "/v1/files/{owner}/{path...}", "005"},
	{http.MethodDelete, "/v1/files/{owner}/{path...}", "005"},
	{http.MethodGet, "/v1/files/materialize", "005"},
	{http.MethodGet, "/v1/trash", "005"},
	{http.MethodPost, "/v1/trash/restore", "005"},
	{http.MethodDelete, "/v1/trash", "005"},
	{http.MethodPut, "/v1/stars", "005"},
	{http.MethodDelete, "/v1/stars", "005"},
	{http.MethodGet, "/v1/stars", "005"},

	// Uploads (spec 007).
	{http.MethodPost, "/v1/uploads", "007"},
	{http.MethodPost, "/v1/uploads/{id}/complete", "007"},
	{http.MethodDelete, "/v1/uploads/{id}", "007"},

	// Shares and links (spec 008).
	{http.MethodPost, "/v1/shares", "008"},
	{http.MethodGet, "/v1/shares", "008"},
	{http.MethodGet, "/v1/shares/{id}", "008"},
	{http.MethodDelete, "/v1/shares/{id}", "008"},
	{http.MethodGet, "/v1/shares/with-me", "008"},
	{http.MethodPost, "/v1/shares/links", "008"},
	{http.MethodGet, "/v1/shares/links", "008"},
	{http.MethodDelete, "/v1/shares/links/{id}", "008"},
	{http.MethodGet, "/v1/shares/links/{token}/meta", "008"},
	{http.MethodGet, "/v1/shares/links/{token}", "008"},
	{http.MethodGet, "/v1/shares/links/{token}/files/{path...}", "008"},

	// Workspaces (spec 009).
	{http.MethodPost, "/v1/workspaces", "009"},
	{http.MethodGet, "/v1/workspaces", "009"},
	{http.MethodGet, "/v1/workspaces/deleted", "009"},
	{http.MethodGet, "/v1/workspaces/{id}", "009"},
	{http.MethodPatch, "/v1/workspaces/{id}", "009"},
	{http.MethodDelete, "/v1/workspaces/{id}", "009"},
	{http.MethodPost, "/v1/workspaces/{id}/restore", "009"},
	{http.MethodPost, "/v1/workspaces/{id}/attach", "009"},
	{http.MethodPost, "/v1/workspaces/{id}/attach/{aid}/renew", "009"},
	{http.MethodDelete, "/v1/workspaces/{id}/attach/{aid}", "009"},
	{http.MethodGet, "/v1/workspaces/{id}/materialize", "009"},
	{http.MethodPost, "/v1/workspaces/{id}/sync", "009"},

	// Events (spec 010).
	{http.MethodGet, "/v1/events", "010"},

	// Administration (spec 012).
	{http.MethodGet, "/v1/admin/overview", "012"},
	{http.MethodPost, "/v1/admin/spaces/{owner}/restore", "012"},
}

// Routes names every row of spec 013's table as a case names it, so a
// consumer reading the suite's coverage reads one list.
func Routes() []string {
	out := make([]string, 0, len(surfaceTable))
	for _, r := range surfaceTable {
		out = append(out, r.key())
	}
	return out
}

// surface is what the target serves of the table, read once per run.
type surface struct {
	// answered holds the key of every row the target's document names.
	answered map[string]bool
}

// serves reports whether the target answers the row.
func (s surface) serves(key string) bool { return s.answered[key] }

// servesAll reports whether the target answers every row a case drives, and
// names the first it does not.
func (s surface) servesAll(keys []string) (string, bool) {
	for _, key := range keys {
		if !s.answered[key] {
			return key, false
		}
	}
	return "", true
}

// pending is every row of the table the target's document does not name,
// sorted, each with the spec that owns it. It is empty against a complete
// build.
func (s surface) pending() []string {
	var out []string
	for _, r := range surfaceTable {
		if !s.answered[r.key()] {
			out = append(out, fmt.Sprintf("%s (spec %s)", r.key(), r.spec))
		}
	}
	sort.Strings(out)
	return out
}

// readSurface reads the document spec 013 publishes and answers which rows
// of the table the target names. The document takes no token: a route name
// is not a secret, and existence hiding protects objects rather than the
// shape of the API.
//
// A target that serves no document is read as serving every row of the
// table, so its cases run and fail on the routes it does not answer rather
// than skipping whole. An alternative implementation that publishes no
// description is still held to the contract.
func (s *session) readSurface(t testing.TB) surface {
	t.Helper()
	answered := map[string]bool{}
	r := s.do(t, request{method: http.MethodGet, path: "/openapi.json"})
	if r.status != http.StatusOK || r.json == nil {
		t.Logf("the target serves no description at GET /openapi.json (%d); every route of spec 013's table is held to be served", r.status)
		for _, row := range surfaceTable {
			answered[row.key()] = true
		}
		return surface{answered: answered}
	}
	paths := obj(r.json, "paths")
	for _, row := range surfaceTable {
		// The document names each path under the base the target serves at,
		// and the table declares it at the root of the version, so the two
		// are compared through the same rule every request goes through
		// (spec 027).
		item := obj(paths, s.under(row.template()))
		if _, ok := item[strings.ToLower(row.method)]; ok {
			answered[row.key()] = true
		}
	}
	return surface{answered: answered}
}

// case017Pending is the one case that reports the routes spec 013 names and
// the target does not serve. It fails while any is outstanding: a route the
// contract fixes and a build does not answer is a build that does not serve
// the contract, and reporting that as a skip would read a partial build as a
// conforming one.
//
// It is one case rather than one per route on purpose. Seventeen red cases
// say seventeen times what one says once, and the list names the spec each
// route waits on, which is what a reader of the run wants to know.
func case017Pending(t *testing.T, s *session) { s.reportPending(t) }

// reportPending is the case's assertion, taking the part of testing.TB it
// uses, so a test of this suite can watch it fail without failing itself.
func (s *session) reportPending(t testing.TB) {
	t.Helper()
	missing := s.served.pending()
	if len(missing) == 0 {
		return
	}
	waiting := map[string]int{}
	for _, row := range surfaceTable {
		if !s.served.serves(row.key()) {
			waiting[row.spec]++
		}
	}
	specs := make([]string, 0, len(waiting))
	for number, count := range waiting {
		specs = append(specs, fmt.Sprintf("%d of spec %s", count, number))
	}
	sort.Strings(specs)
	t.Fatalf("the target serves %d of the %d routes of spec 013; %s are outstanding:\n\t%s",
		len(surfaceTable)-len(missing), len(surfaceTable),
		strings.Join(specs, ", "), strings.Join(missing, "\n\t"))
}

// pendingRoutes answers the rows a case drives that the target does not
// serve, so a case can report what it is waiting on rather than failing on
// a 404 that means the route is absent and not the object.
func (s *session) pendingRoutes(keys []string) []string {
	var out []string
	for _, key := range keys {
		if !s.served.serves(key) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}
