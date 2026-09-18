// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
)

// The rows of this package held to spec 013's table, and the question each
// one asks held to spec 012's.

var (
	// routeRow matches one row of a route table of spec 013: the method, the
	// path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`(/v1/admin[^`]*)`" + ` \| ([^|]+) \|`)
	// tickedAction matches an action a row names.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// specRoutes reads the administration rows of specs/013-api.md.
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
		t.Fatal("spec 013 names no administrative route")
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
		if !slices.Contains(actions, r.Action) {
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

func TestEveryAdministrativeRowOfSpec013IsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range Table() {
		registered[r.Method+" "+r.Path] = true
	}
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

// TestAdminRouteActions is criterion 1 of spec 012: both routes ask
// space.admin before they act, with the resource owner set on the restore
// and absent on the overview, which names no one space.
func TestAdminRouteActions(t *testing.T) {
	owner := "https://other.example|c1d0"
	for _, tc := range []struct {
		name, method, path string
		body               any
		wantOwner          string
	}{
		{name: "the overview", method: http.MethodGet, path: "/v1/admin/overview"},
		{
			name: "the restore", method: http.MethodPost, path: restorePath(owner),
			body: map[string]any{"id": "01J8R4A"}, wantOwner: owner,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withRestorer(&fakeRestorer{}))
			h.forget()
			if got := h.do(t, tc.method, tc.path, tc.body); got.code != http.StatusOK {
				t.Fatalf("%s %s answered %d: %s", tc.method, tc.path, got.code, got.body)
			}
			asked := h.asked()
			if len(asked) != 1 {
				t.Fatalf("the route asked %d questions, want one", len(asked))
			}
			q := asked[0]
			if q.Action != authorizer.ActionSpaceAdmin {
				t.Errorf("the route asked %q, want %q", q.Action, authorizer.ActionSpaceAdmin)
			}
			if q.Resource.Kind != authorizer.KindSpace {
				t.Errorf("the resource is of kind %q, want %q", q.Resource.Kind, authorizer.KindSpace)
			}
			if got := q.Resource.String("owner"); got != tc.wantOwner {
				t.Errorf("the resource names the owner %q, want %q", got, tc.wantOwner)
			}
		})
	}
}

// TestADeniedCallerIsForbiddenAndNotHidden is criterion 2: a deny is 403
// with the code of a refused action, the same as any other refusal, and not
// the 404 the service Arca replaces answered to hide the surface.
func TestADeniedCallerIsForbiddenAndNotHidden(t *testing.T) {
	owner := "https://other.example|c1d0"
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{name: "the overview", method: http.MethodGet, path: "/v1/admin/overview"},
		{
			name: "the restore", method: http.MethodPost, path: restorePath(owner),
			body: map[string]any{"id": "01J8R4A"},
		},
		{
			name: "the restore on the caller's own space", method: http.MethodPost,
			path: "/v1/admin/spaces/me/restore", body: map[string]any{"id": "01J8R4A"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seam := &fakeRestorer{}
			h := newHarness(t, withRestorer(seam))
			h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")

			got := h.do(t, tc.method, tc.path, tc.body)
			if got.code != http.StatusForbidden {
				t.Fatalf("a denied caller answered %d: %s", got.code, got.body)
			}
			if code := got.errorCode(t); code != "forbidden" {
				t.Errorf("a denied caller answered %q", code)
			}
			if h.spaces.calls != 0 || seam.calls != 0 {
				t.Error("a denied route acted")
			}
		})
	}
}

// TestAnInstallationWithNoAdministratorRefusesEveryone is criterion 3: with
// no authorizer configured and no listed subject, both routes answer 403 to
// every caller, the space's own owner included. That is the correct default
// for a self-hosted installation, which needs no administrator to work.
func TestAnInstallationWithNoAdministratorRefusesEveryone(t *testing.T) {
	h := newHarness(t, ownerPolicy(), withRestorer(&fakeRestorer{}))
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{name: "the overview", method: http.MethodGet, path: "/v1/admin/overview"},
		{
			name: "the restore on the caller's own space", method: http.MethodPost,
			path: "/v1/admin/spaces/me/restore", body: map[string]any{"id": "01J8R4A"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.do(t, tc.method, tc.path, tc.body)
			if got.code != http.StatusForbidden {
				t.Fatalf("%s answered %d: %s", tc.name, got.code, got.body)
			}
		})
	}
}

// TestAListedSubjectIsTheAdministratorOfAnInstallationWithNoEndpoint is the
// other half: ARCA_ADMIN_SUBJECTS is where an administrator comes from when
// no endpoint decides.
func TestAListedSubjectIsTheAdministratorOfAnInstallationWithNoEndpoint(t *testing.T) {
	h := newHarness(t)
	h.service.authorizer = ownerPolicyFor(h.subject)
	if got := h.do(t, http.MethodGet, "/v1/admin/overview", nil); got.code != http.StatusOK {
		t.Fatalf("a listed subject answered %d: %s", got.code, got.body)
	}
}

// TestAdminReadsNoClaims is criterion 4: a caller the authorizer allows may
// act whatever its claims say about what kind of principal it is. The
// service Arca replaces refused a mutation from a non-human principal by
// reading a claim for meaning, which invariant 5 of spec 001 forbids; the
// second half of this criterion is the identity gate's claims rule, which
// fails the build on the name anywhere outside internal/auth.
func TestAdminReadsNoClaims(t *testing.T) {
	seam := &fakeRestorer{}
	h := newHarness(t, withRestorer(seam))
	machine := h.issuer.Mint(issuertest.Claims{Sub: "sbx_1", PrincipalType: "agent"})
	got := h.as(t, machine, http.MethodPost, restorePath("https://other.example|c1d0"),
		map[string]any{"id": "01J8R4A"})
	if got.code != http.StatusOK {
		t.Fatalf("a non-human caller the authorizer allowed answered %d: %s", got.code, got.body)
	}
	if seam.calls != 1 {
		t.Error("the restore did not reach the seam")
	}
}

// TestTheServiceRefusesToBuildWithoutASeamItWouldReachThrough: each of these
// is wiring the node settles at start, and a surface missing one would
// answer 500 to a route that looks registered.
func TestTheServiceRefusesToBuildWithoutASeamItWouldReachThrough(t *testing.T) {
	h := newHarness(t)
	full := Options{
		Querier: fakeQuerier{}, Spaces: h.spaces, Authorizer: h.service.authorizer,
	}
	for _, tc := range []struct {
		name string
		drop func(*Options)
	}{
		{name: "no database", drop: func(o *Options) { o.Querier = nil }},
		{name: "no overview query", drop: func(o *Options) { o.Spaces = nil }},
		{name: "no authorizer", drop: func(o *Options) { o.Authorizer = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := full
			tc.drop(&o)
			if _, err := New(o); err == nil {
				t.Fatal("the service built with a seam missing")
			}
		})
	}
	// The two seams a build may legitimately lack are not among them.
	if _, err := New(full); err != nil {
		t.Fatalf("a build with neither the restore nor the link count would not build: %v", err)
	}
}
