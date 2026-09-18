// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/metrics"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The unit tier of spec 014: the handlers of spec 005 against the blob fake
// and the metadata store of fakes_test.go, behind the real verifier, the
// real router and the real authorizer client with the family's stub endpoint
// behind it. Nothing here reaches a service.

// inlineBytes is the boundary this tier reads the size rule against. It is
// small so a test writes a few bytes on each side of it rather than sixteen
// mebibytes.
const inlineBytes = 32

// harness is one mounted surface with the stubs behind it.
type harness struct {
	mux      *http.ServeMux
	store    *memory
	objects  *blob.Memory
	bucket   *blob.Counting
	ledger   *counted
	issuer   *issuertest.Server
	endpoint *stub.Server
	asked    *recorder
	// counters is spec 018's one registry, on a set of its own so a case
	// reads the series this surface moved and no other's.
	counters *metrics.Set
	owner    string
	clock    time.Time
	// service is the one the routes are bound to, so a case reaches a
	// method the node calls directly — the object read of a public link,
	// or the restore across owners of spec 012 — without a request.
	service *Service
	// read answers one path from whichever metadata store the harness was
	// built on: the maps of the unit tier, or the real Postgres of the store
	// tier. seed is what both reach it through.
	read func(t *testing.T, path string) (store.File, bool)
}

// configOf is the configuration a case reads the two sizes against.
func configOf(inline, max int64) config.Config {
	return config.Config{
		BucketPrefix: "arca/", InlineBytes: inline, MaxUploadBytes: max,
		TrashRetention: 720 * time.Hour,
	}
}

// newHarness mounts the file surface the way the node mounts it.
func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: "arca",
		AuthorizerURL: endpoint.URL(), AuthorizerToken: endpoint.Token(),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}

	m := newMemory()
	objects := blob.NewMemory()
	h := &harness{
		store: m, objects: objects, bucket: blob.NewCounting(objects),
		ledger: &counted{memory: m}, issuer: iss, endpoint: endpoint,
		asked: &recorder{inner: id.Authorizer}, counters: metrics.Register(nil),
		owner: iss.URL() + "|9ab3", clock: time.Now(),
	}
	o := Options{
		DB: m, Bucket: h.bucket,
		Files: filesOf{m}, Versions: versionsOf{m}, Stars: starsOf{m}, References: m,
		Decide: h.asked, Ledger: h.ledger,
		Config:  configOf(inlineBytes, 1<<20),
		Now:     func() time.Time { return h.clock },
		Metrics: h.counters,
	}
	for _, opt := range opts {
		opt(&o)
	}
	h.service = New(o)
	surface, err := api.New(api.Options{
		Verifier: id.Verifier, Authorizer: id.Authorizer,
		PublicURL: "https://storage.example", Routes: Bind(h.service),
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
	h.read = func(t *testing.T, path string) (store.File, bool) {
		t.Helper()
		row, err := m.Get(t.Context(), nil, h.owner, path)
		return row, err == nil
	}
	return h
}

// bearer mints a token for the space every test writes in.
func (h *harness) bearer() string { return h.issuer.Mint(issuertest.Claims{Sub: "9ab3"}) }

// request builds one request with the caller's bearer and the headers a
// case named, in pairs.
func (h *harness) request(method, target string, body io.Reader, headers ...string) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Authorization", "Bearer "+h.bearer())
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

// send drives one request through the surface.
func (h *harness) send(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

// call builds one request and drives it.
func (h *harness) call(t *testing.T, method, target string, body io.Reader, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return h.send(h.request(method, target, body, headers...))
}

// put writes one object through the surface.
func (h *harness) put(t *testing.T, path, content string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	return h.call(t, http.MethodPut, h.object(path), strings.NewReader(content),
		append([]string{api.HeaderContentType, "text/markdown"}, headers...)...)
}

// object is the URL of one path in the caller's own space, with the subject
// percent-encoded the way spec 013 addresses one.
func (h *harness) object(path string) string {
	return "/v1/files/" + url.PathEscape(h.owner) + "/" + path
}

// seed writes one row and its bytes without going through a handler, for a
// case whose subject is a read rather than the write before it.
func (h *harness) seed(t *testing.T, path, content string) store.File {
	t.Helper()
	w := h.put(t, path, content)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("seeding %q answered %d: %s", path, w.Code, w.Body)
	}
	row, held := h.read(t, path)
	if !held {
		t.Fatalf("seeding %q left no row", path)
	}
	return row
}

// write puts one object in both stores without a handler, for a case whose
// subject is a read of an object larger than a put accepts.
func (h *harness) write(t *testing.T, path, content string) store.File {
	t.Helper()
	id := object.NewID()
	if _, err := h.objects.Put(t.Context(), id.Key("arca/"), strings.NewReader(content),
		int64(len(content)), blob.PutOptions{ContentType: "text/markdown"}); err != nil {
		t.Fatalf("seeding the bytes of %q: %v", path, err)
	}
	if err := h.store.Upsert(t.Context(), nil, store.File{
		Owner: h.owner, Path: path, ObjectID: id, CreatedBy: h.owner,
		ContentType: "text/markdown", SizeBytes: int64(len(content)),
		Checksum: digest(content), ChecksumKind: object.ChecksumSHA256,
	}); err != nil {
		t.Fatalf("seeding the row of %q: %v", path, err)
	}
	row, err := h.store.Get(t.Context(), nil, h.owner, path)
	if err != nil {
		t.Fatalf("seeding %q left no row: %v", path, err)
	}
	return row
}

// code reads the error table row a refusal named.
func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, w, &body)
	if body.Error.Code == "" {
		t.Fatalf("the answer names no error code: %s", w.Body)
	}
	if body.Error.Message != api.Sentence(body.Error.Code) {
		t.Errorf("the sentence of %q is %q and the table says %q",
			body.Error.Code, body.Error.Message, api.Sentence(body.Error.Code))
	}
	return body.Error.Code
}

// decode reads a JSON answer.
func decode(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, w.Body)
	}
}
