// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// aRow is a registration of the shape a package that owns its behaviour
// hands the node.
func aRow() Route {
	return Route{
		Method:  http.MethodGet,
		Path:    "/v1/probe/{id}",
		Action:  authorizer.ActionFileRead,
		Summary: "A route of this test.",
		Status:  http.StatusOK,
		Handle:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	}
}

func TestARegisteredRouteIsMountedDescribedAndBehindTheVerifier(t *testing.T) {
	row := aRow()
	h := newHarness(t, nil, func(o *Options) { o.Routes = []Route{row} })
	h.mux = http.NewServeMux()
	h.api.Mount(h.mux)
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	if got := h.do(t, row.Method, "/v1/probe/one", h.bearer()); got.Code != http.StatusOK {
		t.Fatalf("the registered route answered %d: %s", got.Code, got.Body)
	}
	// The verifier is in front of it, which is what makes it one of the
	// thirty-eight rows rather than a fourth exception.
	if got := h.do(t, row.Method, "/v1/probe/one", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("the registered route answered %d without a bearer", got.Code)
	}
	if operations := h.document(t).Operations(); !slices.Contains(operations, "GET /v1/probe/{id}") {
		t.Fatalf("the document describes %v and the router registered the route", operations)
	}
}

func TestTheSurfaceRefusesARegistrationItCouldNotHoldToSpec013(t *testing.T) {
	for name, broken := range map[string]func(Route) Route{
		"a method the router does not serve": func(r Route) Route { r.Method = "FETCH"; return r },
		"a path outside /v1":                 func(r Route) Route { r.Path = "/internal/probe"; return r },
		"no action at all":                   func(r Route) Route { r.Action = ""; return r },
		"an action outside the vocabulary":   func(r Route) Route { r.Action = "file.publish"; return r },
		"no handler":                         func(r Route) Route { r.Handle = nil; return r },
	} {
		t.Run(name, func(t *testing.T) {
			if err := build(t, []Route{broken(aRow())}); err == nil {
				t.Fatalf("the surface built with %s", name)
			}
		})
	}
	t.Run("one path registered twice", func(t *testing.T) {
		if err := build(t, []Route{aRow(), aRow()}); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a path the frame already registers", func(t *testing.T) {
		clash := aRow()
		clash.Path = "/v1/shares/links/{token}"
		if err := build(t, []Route{clash}); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestTheDocumentDescribesWhatThePackagesDeclare: tools/apidoc reads the
// declared rows and the node mounts the same rows bound to their handlers,
// so the committed document and the served one are one surface.
func TestTheDocumentDescribesWhatThePackagesDeclare(t *testing.T) {
	declared := Route{
		Method: http.MethodPut, Path: "/v1/probe/{id}", Action: authorizer.ActionFileWrite,
		Summary: "A declaration of this test.", Status: http.StatusCreated,
	}
	described := Described([]Route{declared})
	if len(described) != len(routeTable)+1 {
		t.Fatalf("the document describes %d rows and the table holds %d plus one declaration",
			len(described), len(routeTable))
	}
	last := described[len(described)-1]
	if last.Method != declared.Method || last.Path != declared.Path || last.Action != declared.Action {
		t.Fatalf("the declared row is described as %+v", last)
	}
	if len(Described()) != len(routeTable) {
		t.Fatalf("the frame's own table describes %d rows", len(Described()))
	}
}

func TestADeclarationThatCouldNotBeMountedIsNotDescribedEither(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("a declaration with no action was described")
		}
	}()
	Described([]Route{{Method: http.MethodGet, Path: "/v1/probe/{id}"}})
}

// build answers the error of building a surface with the given rows, with
// the two of spec 006 in place so the rows are the only thing under test.
func build(t *testing.T, rows []Route) error {
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
	_, err = New(Options{Verifier: id.Verifier, Authorizer: id.Authorizer, Routes: rows})
	return err
}
