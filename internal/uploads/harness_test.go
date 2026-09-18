// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/store"
)

// The unit tier of spec 014 for spec 007: the three handlers against the
// blob fake and the metadata store of fakes_test.go, behind the real
// verifier, the real router and the real authorizer client with the family's
// stub endpoint behind it.

// harness is one mounted surface with the stubs behind it.
type harness struct {
	mux      *http.ServeMux
	store    *memory
	objects  *blob.Memory
	bucket   *blob.Counting
	ledger   *counted
	issuer   *issuertest.Server
	endpoint *stub.Server
	owner    string
	clock    time.Time
	// service is the one this surface is mounted on, which is also what the
	// reconciler of spec 010 sweeps through.
	service *Service
	// sessions answers one session by its id, from whichever metadata store
	// the harness was built on.
	sessions func(t *testing.T, id string) store.Session
}

// newHarness mounts the session surface the way the node mounts it.
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
		owner: iss.URL() + "|9ab3", clock: time.Now(),
	}
	o := Options{
		files.Options{
			DB: m, Bucket: h.bucket,
			Files: filesOf{m}, Versions: versionsOf{m}, Stars: starsOf{m}, References: m,
			Decide: id.Authorizer, Ledger: h.ledger,
			Config: config.Config{
				BucketPrefix: "arca/", InlineBytes: 16 << 20, MaxUploadBytes: 5 << 30,
				TrashRetention: 720 * time.Hour,
			},
			Now: func() time.Time { return h.clock },
		},
		sessionsOf{m},
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
	h.sessions = func(t *testing.T, id string) store.Session {
		t.Helper()
		held, err := m.GetSession(t.Context(), nil, id)
		if err != nil {
			t.Fatalf("the session is not there: %v", err)
		}
		return held
	}
	return h
}

// bearer mints a token for the space every test writes in.
func (h *harness) bearer() string { return h.issuer.Mint(issuertest.Claims{Sub: "9ab3"}) }

// call drives one request with the caller's bearer.
func (h *harness) call(t *testing.T, method, target string, body io.Reader, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Authorization", "Bearer "+h.bearer())
	if body != nil {
		r.Header.Set(api.HeaderContentType, api.JSONMediaType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

// open creates one session of the given declared size.
func (h *harness) open(t *testing.T, path string, size int64) Session {
	t.Helper()
	body := `{"owner":"me","path":"` + path + `","size":` + itoa(size) + `,"content_type":"video/mp4"}`
	w := h.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("opening a session answered %d: %s", w.Code, w.Body)
	}
	var out Session
	decode(t, w, &out)
	return out
}

// upload sends one part's bytes straight to the bucket, which is what a
// client does against the presigned URL the session answered.
func (h *harness) upload(t *testing.T, session Session, part int32, content string) string {
	t.Helper()
	held := h.sessions(t, session.ID)
	etag, err := h.objects.UploadPart(held.ObjectID.Key("arca/"), held.UploadID, part, []byte(content))
	if err != nil {
		t.Fatalf("uploading part %d: %v", part, err)
	}
	return etag
}

// finish completes a session with the parts a case uploaded.
func (h *harness) finish(t *testing.T, session Session, etags []string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var parts []string
	for i, etag := range etags {
		parts = append(parts, `{"n":`+itoa(int64(i+1))+`,"etag":"`+etag+`"}`)
	}
	body := `{"parts":[` + strings.Join(parts, ",") + `]}`
	return h.call(t, http.MethodPost, "/v1/uploads/"+session.ID+"/complete", strings.NewReader(body), headers...)
}

// itoa renders a number for a JSON body of a test.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

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
