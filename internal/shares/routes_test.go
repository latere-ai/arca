// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
)

// TestEveryRowAsksTheActionItDeclares is criterion 2 of spec 008 read off
// the declaration rather than per handler: the rows this package contributes
// drive the router and the document, so a row whose action is not the one
// its handler asks is a surface that describes one question and asks
// another.
//
// It is a test of the table and not of a route, which is why it is here and
// not beside each case: the frame held these rows before they were declared
// here, and the property the frame's own table test had over them is the one
// this keeps.
func TestEveryRowAsksTheActionItDeclares(t *testing.T) {
	h := newHarness(t)
	alice := h.subject("alice")
	grant := h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: h.subject("carol"), Permission: "read", CreatedBy: alice,
	})
	link := h.table.put(store.Grant{
		Owner: alice, PathPrefix: "files/reports", GranteeKind: store.GranteeLink,
		Permission: "read", Token: "t0ken-of-the-table", CreatedBy: alice,
	})

	// One request per row, written out so a row added without one fails
	// below rather than going untested.
	requests := map[string]struct {
		path string
		body any
	}{
		"POST /v1/shares": {"/v1/shares", map[string]any{
			"owner": "me", "path_prefix": "files/reports",
			"grantee": h.subject("carol"), "permission": "read",
		}},
		"GET /v1/shares":               {"/v1/shares?owner=me", nil},
		"GET /v1/shares/with-me":       {"/v1/shares/with-me", nil},
		"GET /v1/shares/{id}":          {"/v1/shares/" + grant.ID, nil},
		"DELETE /v1/shares/{id}":       {"/v1/shares/" + grant.ID, nil},
		"POST /v1/shares/links":        {"/v1/shares/links", map[string]any{"owner": "me", "path_prefix": "files/reports"}},
		"GET /v1/shares/links":         {"/v1/shares/links?owner=me", nil},
		"DELETE /v1/shares/links/{id}": {"/v1/shares/links/" + link.ID, nil},
	}

	table := shares.Table()
	if len(table) != len(requests) {
		t.Fatalf("the table declares %d rows and this test drives %d", len(table), len(requests))
	}
	for _, r := range table {
		key := r.Method + " " + r.Path
		req, ok := requests[key]
		if !ok {
			t.Errorf("%s is declared and this test drives no request for it", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			h.endpoint.ClearRequests()
			w := h.do(t, r.Method, req.path, "alice", req.body)
			if w.Code >= http.StatusInternalServerError {
				t.Fatalf("the row answered %d: %s", w.Code, w.Body)
			}
			asked := h.asked()
			if len(asked) != 1 {
				t.Fatalf("the row asked %d questions, want exactly one (%d: %s)", len(asked), w.Code, w.Body)
			}
			if asked[0].Action != r.Action {
				t.Errorf("the row asks %q and its declaration says %q", asked[0].Action, r.Action)
			}
		})
	}
}

// The rows of this package held to spec 013's table, read out of the spec.
// The frame's own test holds the frame's rows the same way; this is the half
// of criterion 1 that belongs to the package owning the behaviour.

var (
	// routeRow matches one share row of a route table of spec 013: the
	// method, the path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`(/v1/shares[^`]*)`" + ` \| ([^|]+) \|`)
	// tickedAction matches an action a row names.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// frameRows are the share rows spec 013 names that this package does not
// declare: the three that redeem a token, which sit outside the verifier and
// are therefore the frame's own. They are written out rather than skipped by
// a rule, so a fourth cannot quietly join them.
var frameRows = []string{
	"GET /v1/shares/links/{token}",
	"GET /v1/shares/links/{token}/meta",
	"GET /v1/shares/links/{token}/files/{path...}",
}

// specRoutes reads the share rows of specs/013-api.md: each row's method and
// path, mapped to every action its third column names.
func specRoutes(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "013-api.md"))
	if err != nil {
		t.Fatalf("spec 013: %v", err)
	}
	out := map[string][]string{}
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
		var actions []string
		for _, a := range tickedAction.FindAllStringSubmatch(m[3], -1) {
			actions = append(actions, a[1])
		}
		out[key] = actions
	}
	if len(out) == 0 {
		t.Fatal("spec 013 names no share route")
	}
	return out
}

func TestEveryRowOfThisPackageIsOneOfSpec013sTable(t *testing.T) {
	spec := specRoutes(t)
	for _, r := range shares.Table() {
		key := r.Method + " " + r.Path
		actions, ok := spec[key]
		if !ok {
			t.Errorf("%s is registered and spec 013's table does not name it", key)
			continue
		}
		if len(actions) > 0 && !slices.Contains(actions, r.Action) {
			t.Errorf("%s asks %q; spec 013's row names %v", key, r.Action, actions)
		}
		if r.Status == 0 {
			t.Errorf("%s answers no status on a success", key)
		}
		if r.Summary == "" {
			t.Errorf("%s carries no summary for the document", key)
		}
	}
}

func TestEveryShareRowOfSpec013IsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range shares.Table() {
		registered[r.Method+" "+r.Path] = true
	}
	for _, key := range frameRows {
		registered[key] = true
	}
	for key := range specRoutes(t) {
		if !registered[key] {
			t.Errorf("spec 013 names %s and nothing registers it", key)
		}
	}
}

func TestTheRowsAndTheHandlersAreOneDeclarationReadTwice(t *testing.T) {
	h := newHarness(t)
	bound := shares.Routes(h.service)
	declared := shares.Table()
	if len(bound) != len(declared) {
		t.Fatalf("the node registers %d rows and the document describes %d", len(bound), len(declared))
	}
	for i := range bound {
		if bound[i].Method != declared[i].Method || bound[i].Path != declared[i].Path ||
			bound[i].Action != declared[i].Action || bound[i].Status != declared[i].Status ||
			bound[i].Summary != declared[i].Summary {
			t.Errorf("row %d differs: %+v against %+v", i, bound[i], declared[i])
		}
		if bound[i].Handler == nil {
			t.Errorf("%s %s is registered with no handler", bound[i].Method, bound[i].Path)
		}
		if declared[i].Handler != nil {
			t.Errorf("%s %s is described with a handler", declared[i].Method, declared[i].Path)
		}
	}
}
