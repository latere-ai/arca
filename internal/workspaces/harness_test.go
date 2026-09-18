// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
)

// The unit tier of spec 009: every handler driven through the surface the
// node mounts, with the stub issuer and the stub authorizer of
// latere.ai/x/pkg behind it and the two stores in a map.
//
// Nothing here calls a handler directly. A request goes through the verifier,
// the rate limit and the router, so what a test sees is what a caller sees,
// and the question each route asks is read off the authorizer the surface
// actually called.

// harness is one mounted surface over the fakes.
type harness struct {
	mux      *http.ServeMux
	store    *memory
	ledger   *recorder
	bucket   *blob.Counting
	issuer   *issuertest.Server
	endpoint *stub.Server
	service  *Service
	subject  string
	clock    func() time.Time
	// t is the test the harness was built with, so a table's drive function
	// reaches one without carrying it twice.
	t *testing.T
}

// newHarness mounts the workspace routes the way the node mounts them.
func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	identity, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: "arca",
		AuthorizerURL: endpoint.URL(), AuthorizerToken: endpoint.Token(),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}

	h := &harness{
		t:     t,
		store: newMemory(), ledger: newRecorder(),
		bucket:   blob.NewCounting(blob.NewMemory()),
		issuer:   iss,
		endpoint: endpoint,
		subject:  iss.URL() + "|9ab3",
	}
	o := Options{
		DB: h.store, Workspaces: h.store, Attachments: attachmentSet{h.store},
		Objects: h.store, Bucket: h.bucket, Prefix: "arca/",
		Authorizer: identity.Authorizer, Ledger: h.ledger,
		Now: func() time.Time { return h.at() },
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
	})
	if err != nil {
		t.Fatalf("the surface would not build: %v", err)
	}
	h.mux = http.NewServeMux()
	surface.Mount(h.mux)
	// Whatever a test drives, no handler may reach the pool while it holds a
	// transaction. The fakes count it rather than each test asserting it, so
	// a handler added later is covered by the tests written for it.
	t.Cleanup(func() {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		if h.store.stray > 0 {
			t.Errorf("%d reads or writes took the pool inside a transaction; pass the querier through",
				h.store.stray)
		}
	})
	return h
}

// at is the clock the lease is measured on: the wall clock unless a test
// moved it, so a test drives an expiry without waiting for one.
func (h *harness) at() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
}

// travel moves the harness clock forward by d for the rest of the test.
func (h *harness) travel(d time.Duration) {
	now := h.at().Add(d)
	h.clock = func() time.Time { return now }
}

// bearer mints a token of the stub issuer for the caller every test uses.
func (h *harness) bearer() string { return h.issuer.Mint(issuertest.Claims{Sub: "9ab3"}) }

// answer is one response, decoded far enough for a test to read it.
type answer struct {
	code int
	body []byte
}

// json decodes the body into v.
func (a answer) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(a.body, v); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, a.body)
	}
}

// code reads the error code of a refusal, and "" from a body that carries
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
	return h.send(t, h.request(t, method, path, body))
}

// request builds one authorized request.
func (h *harness) request(t *testing.T, method, path string, body any) *http.Request {
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
	r.Header.Set("Authorization", "Bearer "+h.bearer())
	return r
}

// send drives a request already built.
func (h *harness) send(t *testing.T, r *http.Request) answer {
	t.Helper()
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return answer{code: w.Code, body: w.Body.Bytes()}
}

// forget drops the questions asked so far, so a test reads the ones its own
// request put.
func (h *harness) forget() { h.endpoint.ClearRequests() }

// asked answers the actions the authorizer was asked since the last reset,
// in order.
func (h *harness) asked() []string {
	requests := h.endpoint.Requests()
	out := make([]string, 0, len(requests))
	for _, r := range requests {
		out = append(out, r.Action)
	}
	return out
}

// page is the list envelope of spec 013 as a test reads it.
type page struct {
	Entries    []Workspace `json:"entries"`
	NextCursor string      `json:"next_cursor"`
}

// create makes one workspace and answers it, failing the test when it could
// not be made.
func (h *harness) create(t *testing.T, slug string) Workspace {
	t.Helper()
	got := h.do(t, http.MethodPost, "/v1/workspaces", map[string]any{"slug": slug})
	if got.code != http.StatusCreated {
		t.Fatalf("POST /v1/workspaces = %d: %s", got.code, got.body)
	}
	var ws Workspace
	got.decode(t, &ws)
	return ws
}

// attach opens one attachment and answers it.
func (h *harness) attach(t *testing.T, ws Workspace, sandbox, mode string) Attachment {
	t.Helper()
	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": sandbox, "mode": mode})
	if got.code != http.StatusCreated {
		t.Fatalf("attach %s = %d: %s", mode, got.code, got.body)
	}
	var a Attachment
	got.decode(t, &a)
	return a
}
