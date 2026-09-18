// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
)

// contributed is one well formed row of the kind a spec's own package hands
// the surface.
func contributed() Route {
	return Route{
		Method: http.MethodGet, Path: "/v1/workspaces/{id}",
		Action: authorizer.ActionWorkspaceRead, Status: http.StatusOK,
		Summary: "Read one workspace.",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"state":"acted"}`))
		}),
	}
}

func TestAContributedRouteIsRegisteredBehindTheVerifierAndDescribed(t *testing.T) {
	row := contributed()
	h := newHarness(t, func(o *Options) { o.Routes = []Route{row} })
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	// No bearer: the contributed row meets the verifier like every other
	// route under /v1, because a contributed row cannot be public.
	if got := h.do(t, row.Method, "/v1/workspaces/2b7e", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated request reached the handler: %d %s", got.Code, got.Body)
	}
	got := h.do(t, row.Method, "/v1/workspaces/2b7e", h.bearer())
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "acted") {
		t.Fatalf("the contributed route = %d %s", got.Code, got.Body)
	}
	// The document is built from the same list the mux is, so a route
	// cannot be registered without being described.
	if ops := h.document(t).Operations(); !slices.Contains(ops, "GET /v1/workspaces/{id}") {
		t.Errorf("the document describes %v", ops)
	}
}

func TestASetOfRowsThatCouldNotBeASurfaceIsRefusedAtStart(t *testing.T) {
	for _, c := range []struct {
		name  string
		spoil func(*Route)
		want  string
	}{
		{"no handler", func(r *Route) { r.Handler = nil }, "has no handler"},
		{"no action", func(r *Route) { r.Action = "" }, "asks nothing"},
		{"an action no authorizer answers", func(r *Route) { r.Action = "workspace.frobnicate" }, "does not name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := contributed()
			c.spoil(&row)
			if _, err := merge(routeTable, []Route{row}); err == nil {
				t.Fatal("the surface was built")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the failure reads %q", err)
			}
		})
	}
	// One method and path is registered once. Two rows claiming it is a
	// surface whose second row the router would never reach.
	twice := []Route{contributed(), contributed()}
	if _, err := merge(routeTable, twice); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("two rows on one path = %v", err)
	}
	// A row on a path the frame already holds is the same refusal.
	clash := contributed()
	clash.Method, clash.Path = http.MethodGet, "/v1/shares/links/{token}"
	if _, err := merge(routeTable, []Route{clash}); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("a row on the frame's own path = %v", err)
	}
}

func TestTheDocumentReadsTheDeclarationAndNotTheHandler(t *testing.T) {
	rows := []Route{contributed()}
	described := Described(rows)
	if len(described) != 1 {
		t.Fatalf("Described answered %d rows", len(described))
	}
	if described[0].Method != rows[0].Method || described[0].Path != rows[0].Path ||
		described[0].Action != rows[0].Action || described[0].Summary != rows[0].Summary ||
		described[0].Status != rows[0].Status {
		t.Errorf("the description is %+v", described[0])
	}
	// A contributed row is never public: the three exceptions of spec 013
	// are the frame's own, and the projection cannot open a fourth.
	if described[0].Public {
		t.Error("a contributed row was described as outside the verifier")
	}
	if Described(nil) == nil {
		t.Error("an empty set described as nil rather than as no rows")
	}
}

func TestASurfaceBuiltWithNoContributedRowsIsTheFrame(t *testing.T) {
	rows, err := merge(routeTable, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(routeTable) {
		t.Fatalf("the frame alone holds %d rows, want %d", len(rows), len(routeTable))
	}
}
