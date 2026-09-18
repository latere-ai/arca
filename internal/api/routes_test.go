// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/apidocs"
	"latere.ai/x/arca/internal/auth"
)

// exceptions are the three routes of spec 013 that sit outside the verifier.
// They are written out here rather than read off the table, because the
// point of the test below is to hold the table to this list: a fourth route
// registered outside the verifier is a hole in invariant 5 of spec 001, and
// it has to be added here, in a test, before it can exist.
var exceptions = []string{
	"GET /v1/shares/links/{token}",
	"GET /v1/shares/links/{token}/meta",
	"GET /v1/shares/links/{token}/files/{path...}",
}

// TestTheThreeVerifierExceptionsAndNoMore is criterion 2 of spec 006: no
// handler under /v1 other than the three public link routes runs before the
// verifier.
func TestTheThreeVerifierExceptionsAndNoMore(t *testing.T) {
	var public []string
	for _, r := range routeTable {
		if r.public {
			public = append(public, r.method+" "+r.path)
		}
	}
	slices.Sort(public)
	want := slices.Sorted(slices.Values(exceptions))
	if !slices.Equal(public, want) {
		t.Errorf("the routes outside the verifier are\n %v\nspec 013 allows\n %v", public, want)
	}
	for _, r := range routeTable {
		if r.public && r.action != "" {
			t.Errorf("%s %s is outside the verifier and asks %s; the three exceptions ask nothing, "+
				"and the grant the token resolves to is the whole of the authorization", r.method, r.path, r.action)
		}
		if !r.public && r.action == "" {
			t.Errorf("%s %s is behind the verifier and asks nothing", r.method, r.path)
		}
	}
}

// TestEveryActionIsOneOfTheVocabulary: a row asking a string outside spec
// 006's table is a question no authorizer can answer, and the shared client
// refuses it before the wire, so it would be an outage rather than a deny.
func TestEveryActionIsOneOfTheVocabulary(t *testing.T) {
	for _, r := range routeTable {
		if r.action != "" && !authorizer.Known(r.action) {
			t.Errorf("%s %s asks %q, which spec 006's vocabulary does not name", r.method, r.path, r.action)
		}
	}
}

// TestEveryRouteAsksExactlyOneAction is criterion 3 of spec 006 and
// criterion 2 of spec 013, driven against a recording authorizer: a route
// behind the verifier asks the action of its row, once, before it acts, and
// a route outside it asks nothing at all.
//
// It reads the merged registry and not the frame's own table: a contributed
// row runs behind the same verifier and asks the same way, so the rule is
// measured for every route the surface registers rather than for the frame's
// half of it.
func TestEveryRouteAsksExactlyOneAction(t *testing.T) {
	for _, r := range registry(t) {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			h := newHarness(t)
			h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			h.do(t, r.method, fill(r.path), h.bearer())
			asked := h.endpoint.Requests()
			if r.action == "" {
				if len(asked) != 0 {
					t.Fatalf("a route outside the verifier asked %d question(s): %v", len(asked), asked)
				}
				return
			}
			if len(asked) != 1 {
				t.Fatalf("the route asked %d questions, want exactly one", len(asked))
			}
			if asked[0].Action != r.action {
				t.Errorf("the route asked %q; its row says %q", asked[0].Action, r.action)
			}
		})
	}
}

// TestARouteThatIsDeniedDoesNotAct: the question comes before the act, so a
// deny is the whole of the answer and the handler's work never runs.
func TestARouteThatIsDeniedDoesNotAct(t *testing.T) {
	h := newHarness(t)
	h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")
	w := h.do(t, http.MethodGet, fill(probePath), h.bearer())
	if w.Code != http.StatusForbidden {
		t.Fatalf("a denied route answered %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "acted") {
		t.Error("the handler acted after a deny")
	}
}

// TestEveryRegisteredRouteIsInSpec013sTable holds the registrations to the
// spec's route table, read out of the spec: a route registered that the
// table does not name is a surface nobody wrote down, and an action that
// differs from its row is a question the document and the server disagree
// about.
//
// It reads the merged registry, so a contributed row is held to the spec the
// same way a frame row is. How far this build is from the whole surface is
// not a question this package can answer — the packages that contribute rows
// import it and it imports none of them — so the count of the build is
// tools/apidoc's, where the one union of every declaration lives.
//
// The converse, that every row of spec 013's table is registered, is
// criterion 1 of that spec and closes when the last handler lands;
// registered is a subset of the table until then.
func TestEveryRegisteredRouteIsInSpec013sTable(t *testing.T) {
	spec := routesOfSpec013(t)
	if len(spec) < 41 {
		t.Fatalf("spec 013's tables name %d routes, and the surface is forty-one plus the document", len(spec))
	}
	for _, r := range registry(t) {
		key := r.method + " " + r.path
		action, ok := spec[key]
		if !ok {
			t.Errorf("%s is registered and spec 013's table does not name it", key)
			continue
		}
		if action != r.action {
			t.Errorf("%s asks %q; spec 013's row says %q", key, r.action, action)
		}
	}
}

// TestTheDocumentDescribesEveryRegisteredRouteAndNoOther: the route table is
// the one declaration, so the document and the router are one reading of it.
func TestTheDocumentDescribesEveryRegisteredRouteAndNoOther(t *testing.T) {
	h := newHarness(t)
	var want []string
	for _, r := range h.api.rows {
		want = append(want, r.method+" "+strings.ReplaceAll(r.path, "...}", "}"))
	}
	slices.Sort(want)
	got := h.document(t).Operations()
	if !slices.Equal(got, want) {
		t.Errorf("the document describes\n %v\nthe router registers\n %v", got, want)
	}
}

// probePath is the row these tests contribute: one row of spec 013's file
// table, which is a row of the kind the frame's own table never holds.
const probePath = "/v1/files/{owner}/{path...}"

// probe is that row's handler. It is contributed through Options.Routes, the
// way a spec's own package contributes its rows, and it decides through the
// authorizer seam the surface hands every handler. It exists so the tests
// below read a route of each kind — a frame row and a contributed one — from
// one merged registry, while spec 005's own handlers are still to land.
//
// The authorizer is bound after New, because the seam is the surface's and a
// contributed row is declared before there is a surface to read it from.
type probe struct{ authorizer *auth.Authorizer }

// route is the declaration the surface merges.
func (p *probe) route() Route {
	return Route{
		Method: http.MethodGet, Path: probePath,
		Action: authorizer.ActionFileRead, Status: http.StatusOK,
		Summary: "Read one object.",
		Handler: http.HandlerFunc(p.answer),
	}
}

// answer asks its one action and acts only on an allow.
func (p *probe) answer(w http.ResponseWriter, r *http.Request) {
	res := authorizer.File{
		ID: "01J8R4", Owner: r.PathValue("owner"), Path: r.PathValue("path"), Plane: "files",
	}.Resource()
	if _, err := p.authorizer.Decide(r.Context(), authorizer.ActionFileRead, res); err != nil {
		WriteError(w, r, FromAuth(err))
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]string{"state": "acted"})
}

// registry is the list a surface of these tests registers: the frame's own
// table and the row they contribute, merged the way New merges them. The mux
// and the document are both built from it, so a test that reads it reads the
// surface and not one half of it.
func registry(t *testing.T) []route {
	t.Helper()
	rows, err := merge(routeTable, []Route{(&probe{}).route()})
	if err != nil {
		t.Fatalf("the surface would not merge: %v", err)
	}
	return rows
}

// fill replaces the wildcards of a registration with values a request can
// carry, so a table-driven test drives every row with one rule.
func fill(path string) string {
	out := strings.ReplaceAll(path, "{token}", "tkn")
	out = strings.ReplaceAll(out, "{owner}", "https%3A%2F%2Fissuer.example%7C9ab3")
	out = strings.ReplaceAll(out, "{path...}", "files/reports/q3.pdf")
	return out
}

// harness is one mounted surface with the stub issuer and the stub
// authorizer of latere.ai/x/pkg behind it, which is the whole of what a test
// of the frame needs: no store, no bucket, and no stub of Arca's own.
type harness struct {
	mux      *http.ServeMux
	api      *API
	issuer   *issuertest.Server
	endpoint *stub.Server
}

// newHarness mounts a surface wired the way the node wires one: the frame's
// own table plus the rows the options contribute, which by default is the
// probe above. What it mounts is the surface's own merged list, so no test
// drives a route the surface did not register.
func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: "arca",
		AuthorizerURL: endpoint.URL(), AuthorizerToken: endpoint.Token(),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	p := &probe{}
	o := Options{
		Verifier: id.Verifier, Authorizer: id.Authorizer,
		PublicURL: "https://storage.example",
		// The node wires the log of spec 010 and the database it reads
		// through; a test of the frame wires a log that answers an empty
		// page and no database, because what the tail reads is that
		// package's own business.
		Events: &fakeLog{},
		Routes: []Route{p.route()},
	}
	for _, opt := range opts {
		opt(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatalf("the surface would not build: %v", err)
	}
	p.authorizer = a.Authorizer()
	mux := http.NewServeMux()
	a.mount(mux, a.rows)
	return &harness{mux: mux, api: a, issuer: iss, endpoint: endpoint}
}

// bearer mints a token of the stub issuer for the caller every test uses.
func (h *harness) bearer() string { return h.issuer.Mint(issuertest.Claims{Sub: "9ab3"}) }

// document is the description this surface serves, read back from the bytes
// GET /openapi.json answers rather than from a second build of it.
func (h *harness) document(t *testing.T) *apidocs.Document {
	t.Helper()
	var d apidocs.Document
	if err := json.Unmarshal(h.api.Document(), &d); err != nil {
		t.Fatalf("the document this build serves is not JSON: %v", err)
	}
	return &d
}

// do drives one request, with the bearer when one is given.
func (h *harness) do(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

var (
	// routeRow matches one row of a route table of spec 013: the method,
	// the path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`([^`]+)`" + ` \| ([^|]+) \|`)
	// tickedAction matches the action a row names, where it names one.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// routesOfSpec013 reads every route table of specs/013-api.md: each row's
// method and path, mapped to the action its third column names, with "" for
// a row whose action is none. A row whose path carries a query selects a
// representation of a route already named, so the query is dropped and the
// first row of a path wins, which is the row that names the route's action
// for the method.
func routesOfSpec013(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "013-api.md"))
	if err != nil {
		t.Fatalf("spec 013: %v", err)
	}
	out := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		m := routeRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		path, _, _ := strings.Cut(m[2], "?")
		key := m[1] + " " + path
		if _, seen := out[key]; seen {
			continue
		}
		action := ""
		if a := tickedAction.FindStringSubmatch(m[3]); a != nil {
			action = a[1]
		}
		out[key] = action
	}
	if len(out) == 0 {
		t.Fatal("spec 013 has no route table")
	}
	return out
}
