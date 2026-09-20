// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/events"
)

// The unit tier of spec 012: both routes driven through the surface the node
// mounts, with the stub issuer and the stub authorizer of latere.ai/x/pkg
// behind them.
//
// Nothing here calls a handler directly. A request goes through the verifier,
// the rate limit and the router, so what a test sees is what a caller sees,
// and the question each route asks is read off the authorizer the surface
// actually called.

// harness is one mounted surface over the fakes.
type harness struct {
	mux      *http.ServeMux
	spaces   *fakeSpaces
	links    *fakeLinks
	restorer *fakeRestorer
	issuer   *issuertest.Server
	endpoint *stub.Server
	service  *Service
	subject  string
}

// withRestorer binds the restore seam, which the node leaves unbound until
// the trash of spec 005 and the workspaces of spec 009 answer it.
func withRestorer(f *fakeRestorer) func(*Options) {
	return func(o *Options) { o.Restorer = f }
}

// ownerPolicy decides with the built-in policy of spec 006 instead of an
// endpoint, which is what an installation with ARCA_AUTHORIZER_URL unset
// runs. The subjects given are ARCA_ADMIN_SUBJECTS.
func ownerPolicy(admins ...string) func(*Options) {
	return func(o *Options) { o.Authorizer = ownerPolicyFor(admins...) }
}

// ownerPolicyFor is the same policy as a value, for a case that learns the
// caller's subject only once the harness has minted its issuer.
func ownerPolicyFor(admins ...string) *auth.Authorizer {
	return auth.NewAuthorizer(&auth.OwnerPolicy{Admins: admins})
}

// newHarness mounts the administrative routes the way the node mounts them.
func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	identity, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audiences: []string{"arca"},
		AuthorizerURL: endpoint.URL(), AuthorizerToken: endpoint.Token(),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}

	h := &harness{
		spaces: &fakeSpaces{}, links: &fakeLinks{}, restorer: &fakeRestorer{},
		issuer: iss, endpoint: endpoint, subject: iss.URL() + "|9ab3",
	}
	o := Options{
		Querier: fakeQuerier{}, Spaces: h.spaces, Links: h.links,
		Authorizer: identity.Authorizer,
	}
	for _, opt := range opts {
		opt(&o)
	}
	service, err := New(o)
	if err != nil {
		t.Fatalf("the service would not build: %v", err)
	}
	h.service = service

	surface, err := api.New(api.Options{
		Verifier: identity.Verifier, Authorizer: identity.Authorizer,
		PublicURL: "https://storage.example", Routes: Routes(service),
		// The frame registers the event tail of spec 010 and refuses to
		// build without its log. No test here drives that route, so the
		// log is wired with no database behind it.
		Events: events.NewLog(),
	})
	if err != nil {
		t.Fatalf("the surface would not build: %v", err)
	}
	h.mux = http.NewServeMux()
	surface.Mount(h.mux)
	return h
}

// bearer mints a token of the stub issuer for the caller every test uses.
func (h *harness) bearer() string { return h.issuer.Mint(issuertest.Claims{Sub: "9ab3"}) }

// answer is one response, decoded far enough for a test to read it.
type answer struct {
	code int
	body []byte
}

// decode reads the body into v.
func (a answer) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(a.body, v); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, a.body)
	}
}

// errorCode reads the code of a refusal, and "" from a body that carries
// none, so a test names the row of spec 013's table it expects.
func (a answer) errorCode(t *testing.T) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				RequestID string   `json:"request_id"`
				Detail    string   `json:"detail"`
				Fields    []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &envelope)
	if envelope.Error.Code != "" && envelope.Error.Details.RequestID == "" {
		t.Errorf("the refusal carries no request id: %s", a.body)
	}
	return envelope.Error.Code
}

// detail reads the developer sentence of a refusal.
func (a answer) detail(t *testing.T) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &envelope)
	return envelope.Error.Details.Detail
}

// fields reads the field paths a refusal named.
func (a answer) fields(t *testing.T) []string {
	t.Helper()
	var envelope struct {
		Error struct {
			Details struct {
				Fields []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &envelope)
	return envelope.Error.Details.Fields
}

// do drives one request with the caller's bearer and a JSON body.
func (h *harness) do(t *testing.T, method, path string, body any) answer {
	t.Helper()
	return h.as(t, h.bearer(), method, path, body)
}

// as drives one request with the bearer a case minted, so a test drives a
// caller other than the one every other test uses.
func (h *harness) as(t *testing.T, bearer, method, path string, body any) answer {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return answer{code: w.Code, body: w.Body.Bytes()}
}

// forget drops the questions asked so far, so a test reads the ones its own
// request put.
func (h *harness) forget() { h.endpoint.ClearRequests() }

// asked answers the questions the authorizer was asked since the last reset.
func (h *harness) asked() []authz.Request { return h.endpoint.Requests() }

// page is the list envelope of spec 013 as a test reads it.
type page struct {
	Entries    []Space `json:"entries"`
	NextCursor string  `json:"next_cursor"`
}
