// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/apidocs"
	"latere.ai/x/arca/internal/auth"
)

// TestTheSurfaceRefusesToBuildWithoutTheTwoOfSpec006: every route either
// verifies or asks, so a surface missing either would serve what nobody
// decided. It is a start-up failure and not a nil dereference on the first
// request.
func TestTheSurfaceRefusesToBuildWithoutTheTwoOfSpec006(t *testing.T) {
	if _, err := New(Options{}); err == nil || !strings.Contains(err.Error(), "no verifier") {
		t.Errorf("a surface with no verifier built: %v", err)
	}
	h := newHarness(t, routeTable)
	if _, err := New(Options{Verifier: h.api.verifier}); err == nil || !strings.Contains(err.Error(), "no authorizer") {
		t.Errorf("a surface with no authorizer built: %v", err)
	}
}

// TestAPublicLinkRouteTakesNoBearer is the exception of spec 013 end to end:
// the three link routes answer without a token, and answer the frame's
// not_implemented until spec 008 lands their behaviour.
func TestAPublicLinkRouteTakesNoBearer(t *testing.T) {
	h := newHarness(t, routeTable)
	for _, path := range []string{
		"/v1/shares/links/tkn",
		"/v1/shares/links/tkn/meta",
		"/v1/shares/links/tkn/files/reports/q3.pdf",
	} {
		t.Run(path, func(t *testing.T) {
			w := h.do(t, http.MethodGet, path, "")
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("the route answered %d, want %d: %s", w.Code, http.StatusNotImplemented, w.Body)
			}
			body := decode(t, w)
			if body.Error.Code != CodeNotImplemented {
				t.Errorf("the code is %q", body.Error.Code)
			}
			if body.Error.Details["request_id"] == nil {
				t.Error("the refusal carries no request id")
			}
		})
	}
}

// TestEveryOtherPathUnderV1MeetsTheVerifier: the exception is the three
// routes and nothing else. A route of a later phase and a path nobody ever
// registers both answer 401 without a bearer, so whether a route exists is
// not something an unauthenticated caller learns.
func TestEveryOtherPathUnderV1MeetsTheVerifier(t *testing.T) {
	h := newHarness(t, append(slices.Clone(routeTable), probeRoute))
	for _, path := range []string{
		"/v1/files/https%3A%2F%2Fissuer.example%7C9ab3/files/reports/q3.pdf",
		"/v1/shares",
		"/v1/no-such-route",
		"/v1/shares/links",
	} {
		t.Run(path, func(t *testing.T) {
			w := h.do(t, http.MethodGet, path, "")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("the path answered %d without a bearer, want 401: %s", w.Code, w.Body)
			}
			body := decode(t, w)
			if body.Error.Code != CodeUnauthenticated {
				t.Errorf("the code is %q", body.Error.Code)
			}
			if got, _ := body.Error.Details["detail"].(string); !strings.HasPrefix(got, auth.ReasonMissing+":") {
				t.Errorf("the developer detail is %q and does not open with the reason table's row", got)
			}
		})
	}
}

// TestAPathNobodyRegisteredIsNotFoundOnceVerified: past the verifier, a path
// no row of the table names is a 404 in the same envelope as every other
// refusal, and not the router's own bare answer.
func TestAPathNobodyRegisteredIsNotFoundOnceVerified(t *testing.T) {
	h := newHarness(t, routeTable)
	w := h.do(t, http.MethodGet, "/v1/no-such-route", h.bearer())
	if w.Code != http.StatusNotFound {
		t.Fatalf("the path answered %d, want 404: %s", w.Code, w.Body)
	}
	if got := decode(t, w).Error.Code; got != CodeNotFound {
		t.Errorf("the code is %q", got)
	}
}

// TestOpenAPIIsServedWithoutAToken: a route name is not a secret, and the
// existence hiding of invariant 6 protects objects and not the shape of the
// API. The document parses as OpenAPI 3 and names the server this
// installation is reached at.
func TestOpenAPIIsServedWithoutAToken(t *testing.T) {
	h := newHarness(t, routeTable)
	w := h.do(t, http.MethodGet, "/openapi.json", "")
	if w.Code != http.StatusOK {
		t.Fatalf("the document answered %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("the content type is %q", got)
	}
	if w.Header().Get(Header) == "" {
		t.Error("the document carries no request id")
	}
	var d apidocs.Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if !strings.HasPrefix(d.OpenAPI, "3.") {
		t.Errorf("the document declares OpenAPI %q", d.OpenAPI)
	}
	if d.Info.Title != Title || d.Info.Version != DocumentVersion {
		t.Errorf("the document is %q version %q", d.Info.Title, d.Info.Version)
	}
	if len(d.Servers) != 1 || d.Servers[0].URL != "https://storage.example" {
		t.Errorf("the served document names the servers %v; it names ARCA_PUBLIC_URL", d.Servers)
	}
}

// TestTheDocumentDeclaresOneResponsePerCode: spec 013's error table reaches
// the document whole, so a generator emits one type per code.
func TestTheDocumentDeclaresOneResponsePerCode(t *testing.T) {
	d := newHarness(t, routeTable).document(t)
	for _, code := range Codes() {
		if _, ok := d.Components.Responses[code]; !ok {
			t.Errorf("the document declares no response for %s", code)
		}
	}
	if len(d.Components.Responses) != len(Codes()) {
		t.Errorf("the document declares %d responses for %d codes", len(d.Components.Responses), len(Codes()))
	}
	for _, name := range []string{"Error", "Page"} {
		if _, ok := d.Components.Schemas[name]; !ok {
			t.Errorf("the document declares no %s schema", name)
		}
	}
}

// TestAGuardedRouteDeclaresTheBearerAndAPublicOneDoesNot: the document says
// which routes carry a token, so a generated client sends one where it is
// wanted and nowhere else.
func TestAGuardedRouteDeclaresTheBearerAndAPublicOneDoesNot(t *testing.T) {
	d := apidocs.Build(apidocs.Options{
		Title: Title, Version: DocumentVersion,
		Routes: routesOf(append(slices.Clone(routeTable), probeRoute)), Errors: Errors(),
	})
	public := d.Paths["/v1/shares/links/{token}"]["get"]
	if len(public.Security) != 0 {
		t.Errorf("a public link route declares the security %v", public.Security)
	}
	guarded := d.Paths["/v1/files/{owner}/{path}"]["get"]
	if len(guarded.Security) != 1 {
		t.Fatalf("a guarded route declares the security %v", guarded.Security)
	}
	if _, ok := guarded.Security[0]["bearer"]; !ok {
		t.Errorf("a guarded route declares %v, not the bearer scheme", guarded.Security[0])
	}
	if !strings.Contains(guarded.Description, "file.read") {
		t.Errorf("the description does not name the action asked: %q", guarded.Description)
	}
}

// TestTheSubjectRateLimitIsPerSubject: a caller over its rate is refused
// with the code of the table, and another caller is not.
func TestTheSubjectRateLimitIsPerSubject(t *testing.T) {
	rows := append(slices.Clone(routeTable), probeRoute)
	h := newHarness(t, rows, func(o *Options) { o.RequestsPerMinute = 2 })
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	alice := h.issuer.Mint(issuertest.Claims{Sub: "alice"})
	path := fill(probeRoute.path)

	for i := range 2 {
		if w := h.do(t, http.MethodGet, path, alice); w.Code != http.StatusOK {
			t.Fatalf("request %d answered %d: %s", i+1, w.Code, w.Body)
		}
	}
	w := h.do(t, http.MethodGet, path, alice)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the third request answered %d, want 429", w.Code)
	}
	if got := decode(t, w).Error.Code; got != CodeRateLimited {
		t.Errorf("the code is %q", got)
	}
	bob := h.issuer.Mint(issuertest.Claims{Sub: "bob"})
	if w := h.do(t, http.MethodGet, path, bob); w.Code != http.StatusOK {
		t.Errorf("a second subject was refused with %d: %s", w.Code, w.Body)
	}
}

// TestTheAddressRateLimitBoundsWhatHasNoSubject: a caller guessing link
// tokens and a caller replaying bad tokens have no subject to charge, so
// both are charged the address bucket. A caller whose token verifies pays
// the subject bucket alone and never this one.
func TestTheAddressRateLimitBoundsWhatHasNoSubject(t *testing.T) {
	rows := append(slices.Clone(routeTable), probeRoute)
	h := newHarness(t, rows, func(o *Options) { o.UnauthenticatedRequestsPerMinute = 2 })
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	// One guess at a link token and one bad bearer exhaust the bucket.
	if w := h.do(t, http.MethodGet, "/v1/shares/links/guess", ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("a link route answered %d", w.Code)
	}
	if w := h.do(t, http.MethodGet, fill(probeRoute.path), "not-a-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("a bad bearer answered %d", w.Code)
	}
	for _, c := range []struct{ name, path, token string }{
		{"another guess at a link token", "/v1/shares/links/guess2", ""},
		{"another bad bearer", fill(probeRoute.path), "still-not-a-token"},
	} {
		w := h.do(t, http.MethodGet, c.path, c.token)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("%s answered %d, want 429: %s", c.name, w.Code, w.Body)
		}
	}
	// A caller whose token verifies pays the other bucket, which this case
	// left alone.
	if w := h.do(t, http.MethodGet, fill(probeRoute.path), h.bearer()); w.Code != http.StatusOK {
		t.Errorf("a verified caller was charged the address bucket: %d %s", w.Code, w.Body)
	}
}

// TestARateOfZeroLimitsNothing: the row's own value for a limiter that is
// off, which is what an installation behind its own gateway sets.
func TestARateOfZeroLimitsNothing(t *testing.T) {
	rows := append(slices.Clone(routeTable), probeRoute)
	h := newHarness(t, rows, func(o *Options) {
		o.RequestsPerMinute, o.UnauthenticatedRequestsPerMinute = 0, 0
	})
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	for i := range 5 {
		if w := h.do(t, http.MethodGet, fill(probeRoute.path), h.bearer()); w.Code != http.StatusOK {
			t.Fatalf("request %d answered %d with the limiters off", i+1, w.Code)
		}
		if w := h.do(t, http.MethodGet, "/v1/shares/links/tkn", ""); w.Code != http.StatusNotImplemented {
			t.Fatalf("link request %d answered %d with the limiters off", i+1, w.Code)
		}
	}
}

// TestAnAuthorizerThatAnswersNothingIsNeverAnAllow: the seam end to end.
// An endpoint that is out of reach is 503 on the wire and no route acts.
func TestAnAuthorizerThatAnswersNothingIsNeverAnAllow(t *testing.T) {
	rows := append(slices.Clone(routeTable), probeRoute)
	h := newHarness(t, rows)
	h.endpoint.Fail(http.StatusBadGateway)
	w := h.do(t, http.MethodGet, fill(probeRoute.path), h.bearer())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unreachable authorizer answered %d: %s", w.Code, w.Body)
	}
	if got := decode(t, w).Error.Code; got != CodeAuthorizerUnavailable {
		t.Errorf("the code is %q", got)
	}
	if strings.Contains(w.Body.String(), "acted") {
		t.Error("a route acted without a decision")
	}
}

// TestTheQuestionCarriesTheRequestId: the id of spec 013 is the one an
// operator's authorizer sees, so a log line there and one here name one
// request.
func TestTheQuestionCarriesTheRequestId(t *testing.T) {
	rows := append(slices.Clone(routeTable), probeRoute)
	h := newHarness(t, rows)
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	w := h.do(t, http.MethodGet, fill(probeRoute.path), h.bearer())
	asked := h.endpoint.Requests()
	if len(asked) != 1 {
		t.Fatalf("the endpoint was asked %d times", len(asked))
	}
	if got, want := asked[0].Request.ID, w.Header().Get(Header); got != want {
		t.Errorf("the question carried the request id %q; the response carried %q", got, want)
	}
	if asked[0].Request.IP == "" {
		t.Error("the question carries no peer address")
	}
}
