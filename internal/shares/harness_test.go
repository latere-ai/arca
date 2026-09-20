// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"bytes"
	"encoding/json"
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
	"latere.ai/x/arca/internal/shares"
)

// harness is the surface of spec 013 with the service of spec 008 behind it,
// the stub issuer and the stub authorizer of latere.ai/x/pkg in front, and
// the two stores faked. Every request below goes through the real router,
// the real verifier and the real decision seam, so what a test drives is
// what an operator's client drives.
type harness struct {
	mux      *http.ServeMux
	issuer   *issuertest.Server
	endpoint *stub.Server
	table    *table
	db       *database
	ledger   *ledger
	reader   *reader
	bucket   *bucket
	service  *shares.Service
}

// newHarness builds one, with every question allowed unless a case says
// otherwise.
func newHarness(t *testing.T, opts ...func(*shares.Options)) *harness {
	t.Helper()
	h := &harness{
		issuer:   issuertest.New(t, issuertest.WithDefaultAudience("arca")),
		endpoint: stub.New(t, stub.WithVocabulary(authorizer.Vocabulary())),
		table:    newTable(),
		ledger:   &ledger{},
		reader:   &reader{},
		bucket:   newBucket(),
	}
	h.db = &database{table: h.table}
	h.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{h.issuer.URL()}, Audiences: []string{"arca"},
		AuthorizerURL: h.endpoint.URL(), AuthorizerToken: h.endpoint.Token(),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	o := shares.Options{
		Authorizer: id.Authorizer, DB: h.db, Store: h.table, Ledger: h.ledger,
		Reader: h.reader, Publisher: h.bucket, BucketPrefix: "arca/",
	}
	for _, opt := range opts {
		opt(&o)
	}
	if h.service, err = shares.New(o); err != nil {
		t.Fatalf("the service would not build: %v", err)
	}
	surface, err := api.New(api.Options{
		Verifier: id.Verifier, Authorizer: id.Authorizer,
		Routes: shares.Routes(h.service), Links: h.service,
		PublicURL: "https://storage.example",
		// The node hands one base path to both, so a harness that pointed
		// the service at one prefix and the mux at another would prove
		// nothing about an installation (spec 027).
		BasePath: o.BasePath,
		// The frame registers the event tail of spec 010 and refuses to
		// build without its log. No test here drives that route, so the
		// node's own log is wired with no database behind it: what the tail
		// reads is that package's business and its own tests'.
		Events: events.NewLog(),
	})
	if err != nil {
		t.Fatalf("the surface would not build: %v", err)
	}
	h.mux = http.NewServeMux()
	surface.Mount(h.mux)
	return h
}

// subject renders the subject of one of the issuer's people, the way spec
// 006 renders every principal.
func (h *harness) subject(sub string) string { return h.issuer.URL() + "|" + sub }

// bearer mints a token for one of them.
func (h *harness) bearer(sub string) string {
	return h.issuer.Mint(issuertest.Claims{Sub: sub})
}

// do drives one request as a subject, with a JSON body when one is given and
// no bearer at all when sub is empty.
func (h *harness) do(t *testing.T, method, path, sub string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	var r *http.Request
	if reader != nil {
		r = httptest.NewRequest(method, path, reader)
		r.Header.Set("Content-Type", api.JSONMediaType)
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if sub != "" {
		r.Header.Set("Authorization", "Bearer "+h.bearer(sub))
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

// raw drives one request whose body is bytes of the caller's own making, for
// the cases about the body rule itself.
func (h *harness) raw(t *testing.T, method, path, sub, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if sub != "" {
		r.Header.Set("Authorization", "Bearer "+h.bearer(sub))
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

// asked is every question the endpoint was put, in order.
func (h *harness) asked() []authz.Request { return h.endpoint.Requests() }

// grant reads one grant out of a response body.
func grantOf(t *testing.T, w *httptest.ResponseRecorder) shares.Grant {
	t.Helper()
	var g shares.Grant
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatalf("the body is not a grant: %v\n%s", err, w.Body)
	}
	return g
}

// page reads one list page out of a response body.
func pageOf[T any](t *testing.T, w *httptest.ResponseRecorder) api.Page[T] {
	t.Helper()
	var p api.Page[T]
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("the body is not a page: %v\n%s", err, w.Body)
	}
	return p
}

// envelope is the error body of spec 013, read back.
type envelope struct {
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

// detailOf reads the developer detail of a refusal, which is the field an
// error log and a trace carry.
func detailOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("the refusal is not an envelope: %v\n%s", err, w.Body)
	}
	return e.Error.Details.Detail
}

// refusalOf reads the code a refusal carries, and fails the test when the
// status is not the one the table gives that code.
func refusalOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("the refusal is not an envelope: %v\n%s", err, w.Body)
	}
	if e.Error.Details.RequestID == "" {
		t.Error("the refusal carries no request id")
	}
	if got := api.Status(e.Error.Code); got != w.Code {
		t.Errorf("the code %q was answered with %d; its row is %d", e.Error.Code, w.Code, got)
	}
	if e.Error.Message != api.Sentence(e.Error.Code) {
		t.Errorf("the message of %q is %q", e.Error.Code, e.Error.Message)
	}
	return e.Error.Code
}
