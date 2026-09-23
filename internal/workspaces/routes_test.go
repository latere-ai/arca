// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The rows of this package held to spec 013's table, read out of the spec.
// The frame's own test holds the frame's rows the same way; this is the half
// of criterion 1 that belongs to the package owning the behavior.

var (
	// routeRow matches one row of a route table of spec 013: the method,
	// the path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`(/v1/workspaces[^`]*)`" + ` \| ([^|]+) \|`)
	// tickedAction matches an action a row names.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// specRoutes reads the workspace rows of specs/013-api.md: each row's method
// and path, mapped to every action its third column names. A row whose
// action the request chooses names more than one, which is why the map holds
// a list rather than a string.
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
		t.Fatal("spec 013 names no workspace route")
	}
	return out
}

func TestEveryRowOfThisPackageIsOneOfSpec013sTable(t *testing.T) {
	spec := specRoutes(t)
	for _, r := range Table() {
		key := r.Method + " " + r.Path
		actions, ok := spec[key]
		if !ok {
			t.Errorf("%s is registered and spec 013's table does not name it", key)
			continue
		}
		// A row whose third column names actions is held to them. The two
		// rows that follow the action their attach asked name none, and the
		// handler's own test holds each to the action it puts.
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

func TestEveryWorkspaceRowOfSpec013IsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range Table() {
		registered[r.Method+" "+r.Path] = true
	}
	// Every workspace row of spec 013 is registered, which is that spec's
	// first criterion for this prefix.
	for key := range specRoutes(t) {
		if !registered[key] {
			t.Errorf("spec 013 names %s and nothing registers it", key)
		}
	}
}

func TestTheRowsAndTheHandlersAreOneDeclarationReadTwice(t *testing.T) {
	h := newHarness(t)
	bound := Routes(h.service)
	declared := Table()
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

func TestTheReservedWordWinsOverTheWildcardBesideIt(t *testing.T) {
	h := newHarness(t)
	h.create(t, "build")
	// deleted is a literal in the position an id otherwise fills, and Go's
	// router prefers the literal, so the listing answers and no workspace is
	// looked up under that name.
	got := h.do(t, http.MethodGet, "/v1/workspaces/deleted", nil)
	if got.code != http.StatusOK {
		t.Fatalf("GET /v1/workspaces/deleted = %d: %s", got.code, got.body)
	}
	var tombstones page
	got.decode(t, &tombstones)
	if len(tombstones.Entries) != 0 {
		t.Fatalf("the deleted listing holds %+v", tombstones.Entries)
	}
	if asked := h.asked(); !slices.Contains(asked, "workspace.list") {
		t.Errorf("the literal route asked %v", asked)
	}
}
