// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package apidocs

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// sample is a table of each kind of row: one guarded route with a wildcard,
// one whose wildcard swallows the rest of the path, and one public route the
// build does not answer yet.
var sample = Options{
	Title: "Arca", Version: "1", Description: "Durable storage.",
	Routes: []Route{
		{
			Method: http.MethodPut, Path: "/v1/files/{owner}/{path...}", Action: "file.write",
			Summary: "Write one object.", Status: http.StatusOK,
		},
		{
			Method: http.MethodGet, Path: "/v1/files/{owner}/{path...}", Action: "file.read",
			Summary: "Read one object.", Status: http.StatusOK,
		},
		{
			Method: http.MethodGet, Path: "/v1/shares/links/{token}", Public: true, Pending: true,
			Summary: "A listing of the subtree a link token names.", Status: http.StatusOK,
		},
	},
	Errors: []ErrorCode{
		{Code: "forbidden", Status: 403, Sentence: "You do not have permission to do this."},
		{Code: "not_found", Status: 404, Sentence: "There is no such object."},
		{Code: "rate_limited", Status: 429, Sentence: "Too many requests; wait and retry."},
		{Code: "internal", Status: 500, Sentence: "Something went wrong on this server."},
		{Code: "not_implemented", Status: 501, Sentence: "This server does not serve that."},
		{Code: "unauthenticated", Status: 401, Sentence: "Sign in and send a valid token."},
		{Code: "storage_unavailable", Status: 503, Sentence: "Storage is unavailable right now; retry shortly."},
		{Code: "authorizer_unavailable", Status: 503, Sentence: "The permission service is unavailable; retry shortly."},
	},
}

// TestOneRowIsOneOperation: the route table is the whole of the surface the
// document describes, and two methods on one path are two operations of one
// path item.
func TestOneRowIsOneOperation(t *testing.T) {
	d := Build(sample)
	want := []string{
		"GET /v1/files/{owner}/{path}",
		"GET /v1/shares/links/{token}",
		"PUT /v1/files/{owner}/{path}",
	}
	if got := d.Operations(); !slices.Equal(got, want) {
		t.Errorf("the document describes\n %v\nwant\n %v", got, want)
	}
	if got := d.PathList(); len(got) != 2 {
		t.Errorf("the document names the paths %v; two methods on one path are one path item", got)
	}
}

// TestAPathTemplateIsNotTheRoutersSpelling: the router spells a segment that
// swallows the rest of a path "{path...}", and a path template has no such
// form. The parameter is named once and its description says it carries
// slashes, so a generated client builds a URL that works.
func TestAPathTemplateIsNotTheRoutersSpelling(t *testing.T) {
	d := Build(sample)
	if _, ok := d.Paths["/v1/files/{owner}/{path}"]; !ok {
		t.Fatalf("the document names the paths %v", d.PathList())
	}
	for path := range d.Paths {
		if strings.Contains(path, "...") {
			t.Errorf("the document names the path %q, which is the router's spelling and not a template", path)
		}
	}
	op := d.Paths["/v1/files/{owner}/{path}"]["get"]
	if len(op.Parameters) != 2 {
		t.Fatalf("the operation declares %d parameters, want owner and path", len(op.Parameters))
	}
	if op.Parameters[0].Name != "owner" || op.Parameters[1].Name != "path" {
		t.Errorf("the parameters are %v", op.Parameters)
	}
	for _, p := range op.Parameters {
		if p.In != "path" || !p.Required {
			t.Errorf("%s is declared %+v; a wildcard is a required path parameter", p.Name, p)
		}
	}
	if !strings.Contains(op.Parameters[1].Description, "slashes") {
		t.Errorf("the swallowing wildcard is described as %q", op.Parameters[1].Description)
	}
}

// TestAnOperationIdIsStable: a generated client's method names come from the
// method and the path, so rewording a summary does not move them.
func TestAnOperationIdIsStable(t *testing.T) {
	cases := map[string]string{
		"/v1/files/{owner}/{path...}":            "getV1FilesByOwnerByPath",
		"/v1/shares/links/{token}/meta":          "getV1SharesLinksByTokenMeta",
		"/v1/workspaces/{id}/attach/{aid}/renew": "getV1WorkspacesByIdAttachByAidRenew",
		"/v1/admin/spaces/{owner}/restore":       "getV1AdminSpacesByOwnerRestore",
		"/v1/shares/with-me":                     "getV1SharesWithMe",
	}
	for path, want := range cases {
		if got := operationID(Route{Method: http.MethodGet, Path: path}); got != want {
			t.Errorf("operationID(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestAPendingRouteAnswersOnlyWhatItCan: a route the frame registers and
// does not yet answer describes not_implemented and no success body, because
// a document that promised one would be describing a body nothing returns.
func TestAPendingRouteAnswersOnlyWhatItCan(t *testing.T) {
	op := Build(sample).Paths["/v1/shares/links/{token}"]["get"]
	if _, ok := op.Responses["501"]; !ok {
		t.Errorf("a pending route declares the answers %v", slices.Sorted(keys(op.Responses)))
	}
	if _, ok := op.Responses["200"]; ok {
		t.Error("a pending route declares a success it does not answer")
	}
	if !strings.Contains(op.Description, "not_implemented") {
		t.Errorf("the description is %q", op.Description)
	}
	if len(op.Security) != 0 {
		t.Errorf("a public route declares the security %v", op.Security)
	}
}

// TestAGuardedRouteDeclaresItsActionAndItsRefusals: a reader of the document
// knows what an authorizer is asked before the route acts, and which
// refusals the frame itself can answer.
func TestAGuardedRouteDeclaresItsActionAndItsRefusals(t *testing.T) {
	op := Build(sample).Paths["/v1/files/{owner}/{path}"]["put"]
	if !strings.Contains(op.Description, "file.write") {
		t.Errorf("the description is %q", op.Description)
	}
	for _, status := range []string{"200", "401", "403", "404", "429", "500", "503"} {
		if _, ok := op.Responses[status]; !ok {
			t.Errorf("a guarded route does not declare %s; it declares %v", status, slices.Sorted(keys(op.Responses)))
		}
	}
	if got := op.Responses["403"].Ref; got != "#/components/responses/forbidden" {
		t.Errorf("403 is declared as %+v; one code at a status is a reference to it", op.Responses["403"])
	}
	// Two codes share 503, so the operation names the one it answers rather
	// than referencing a response that would be the other half the time.
	shared := op.Responses["503"]
	if shared.Ref != "" || !strings.Contains(shared.Description, "authorizer_unavailable") {
		t.Errorf("503 is declared as %+v; two codes share it and the description names the one answered", shared)
	}
}

// TestEveryCodeIsAResponse: spec 013's table reaches the document whole, so
// a generator emits one type per code.
func TestEveryCodeIsAResponse(t *testing.T) {
	d := Build(sample)
	if len(d.Components.Responses) != len(sample.Errors) {
		t.Fatalf("the document declares %d responses for %d codes", len(d.Components.Responses), len(sample.Errors))
	}
	for _, e := range sample.Errors {
		got, ok := d.Components.Responses[e.Code]
		if !ok {
			t.Errorf("the document declares no response for %s", e.Code)
			continue
		}
		if !strings.Contains(got.Description, e.Sentence) {
			t.Errorf("%s is described as %q and does not carry its sentence", e.Code, got.Description)
		}
		if got.Content["application/json"].Schema.Ref != "#/components/schemas/Error" {
			t.Errorf("%s does not answer the error envelope", e.Code)
		}
	}
}

// TestTheServerIsTheInstallations: a document built for a running server
// names the base it is reached at, and one built for a repository names
// none, because a file in a repository describes no one installation.
func TestTheServerIsTheInstallations(t *testing.T) {
	if got := Build(sample).Servers; len(got) != 0 {
		t.Errorf("a document built with no server names %v", got)
	}
	with := sample
	with.Server = "https://storage.example"
	got := Build(with).Servers
	if len(got) != 1 || got[0].URL != "https://storage.example" {
		t.Errorf("the document names the servers %v", got)
	}
}

// TestTheDocumentIsTheSameBytesEveryBuild: the committed file is compared
// against a fresh build, so a document whose maps rendered in a different
// order every run would fail that check at random rather than when a route
// changed.
func TestTheDocumentIsTheSameBytesEveryBuild(t *testing.T) {
	first := Build(sample).JSON()
	for range 20 {
		if !bytes.Equal(first, Build(sample).JSON()) {
			t.Fatal("two builds of one table rendered different bytes")
		}
	}
	var back Document
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("the document does not read back: %v", err)
	}
	if back.OpenAPI != Version {
		t.Errorf("the document declares OpenAPI %q, want %q", back.OpenAPI, Version)
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Error("the document does not end in a newline")
	}
}

// TestTheBearerSchemeIsDeclaredOnce: one credential, declared in the
// components and referenced by every guarded route.
func TestTheBearerSchemeIsDeclaredOnce(t *testing.T) {
	d := Build(sample)
	scheme, ok := d.Components.SecuritySchemes[bearer]
	if !ok {
		t.Fatalf("the document declares the schemes %v", slices.Sorted(keys(d.Components.SecuritySchemes)))
	}
	if scheme.Type != "http" || scheme.Scheme != "bearer" {
		t.Errorf("the scheme is %+v", scheme)
	}
}

// TestAValueThatIsNotJSONPanics is the contract of MustJSON: a Document is a
// closed set of values that are all JSON, so a failure is a programming
// error rather than a runtime condition, and the caller is stopped rather
// than handed bytes it never checked.
func TestAValueThatIsNotJSONPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a value that is not JSON rendered")
		}
		if got, _ := r.(string); !strings.Contains(got, "does not marshal") {
			t.Errorf("the panic is %v", r)
		}
	}()
	MustJSON(make(chan int))
}

// keys is the keys of a map, for a message that lists them.
func keys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
