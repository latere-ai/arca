// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package authorizer is Arca's stub authorizer of spec 014: the family's
// latere.ai/x/pkg/authz/stub with two additions Arca's tiers need.
//
// The shared stub carries the envelope, the rule table, the record of every
// request, and the outage modes a client must fail closed on: a status other
// than 200, a 200 that is not an answer, and no answer at all. It also
// carries the one rule that binds every authorizer, this one included: the
// probe resource is denied for every subject and every action, so a core's
// check command reads an allow on it as an endpoint that does not read the
// request.
//
// Arca adds the two the shared stub has no place for: a deny chosen per
// request by a header, so one running stub serves a table of cases without
// a rule change between them, and a connection dropped before the response
// line, which is the failure the client's one retry exists for.
package authorizer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
)

// ProbeID is the reserved resource id every authorizer denies.
const ProbeID = authz.ProbeID

// DenyHeader names the action one request is denied, whatever the rule
// table says.
const DenyHeader = "X-Stub-Deny"

// DefaultToken is the bearer the stub expects unless WithToken sets
// another; make run and the tiers use it.
const DefaultToken = stub.DefaultToken

// The shared stub's types and options, under the names Arca's tiers use.
type (
	Rule   = stub.Rule
	Option = stub.Option
	Body   = stub.Body
)

// The two bodies that are a 200 and no answer.
const (
	BodyMalformed = stub.BodyMalformed
	BodyNoAllow   = stub.BodyNoAllow
)

// The shared stub's options, re-exported.
var (
	WithToken      = stub.WithToken
	WithAllow      = stub.WithAllow
	WithVocabulary = stub.WithVocabulary
)

// Server is one stub authorizer: the shared server, plus the header deny
// and the dropped connection.
type Server struct {
	*stub.Server

	mu   sync.Mutex
	drop bool

	mux *http.ServeMux
	srv *httptest.Server
}

// New starts a stub for the test and stops it with the test.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := NewHandler(opts...)
	s.srv = httptest.NewServer(s.mux)
	t.Cleanup(s.Close)
	return s
}

// NewHandler builds a stub without a listener, for the arca-stubs binary.
func NewHandler(opts ...Option) *Server {
	s := &Server{Server: stub.NewHandler(opts...), mux: http.NewServeMux()}
	s.mux.Handle("/", s.serve())
	return s
}

// Handler serves the endpoint, the shared control API, and Arca's two
// additions.
func (s *Server) Handler() http.Handler { return s.mux }

// URL is the endpoint, the value of ARCA_AUTHORIZER_URL.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the listener and releases every hung request.
func (s *Server) Close() {
	s.Server.Close()
	if s.srv != nil {
		s.srv.Close()
	}
}

// DropConnections makes every request close before a response line, which
// is the transport failure a retry is written for. It is the -fail-mode
// conn-drop of spec 014, and it is not the shared stub's Fail: a status is
// an answer, and this is none.
func (s *Server) DropConnections(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = drop
}

// dropping reports whether the connection is dropped.
func (s *Server) dropping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drop
}

// serve is the endpoint: the dropped connection first, then the header
// deny, then the shared stub.
func (s *Server) serve() http.Handler {
	shared := s.Server.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dropping() {
			dropConnection(w)
			return
		}
		denied := r.Header.Get(DenyHeader)
		if denied == "" {
			shared.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var request authz.Request
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The request reaches the shared stub either way, so it is
		// recorded and its outage modes still apply; what the header
		// changes is the verdict of this one request.
		r.Body = io.NopCloser(bytes.NewReader(body))
		if request.Action != denied {
			shared.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		shared.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusOK {
			copyResponse(w, recorder)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": false, "reason": "denied by the stub's header"})
	})
}

// copyResponse writes a recorded response through, for a request the shared
// stub refused before any verdict.
func copyResponse(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for name, values := range recorder.Header() {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

// dropConnection closes the connection without a response line. A handler
// that cannot hijack answers a 503 instead, which is an outage a client
// reads the same way.
func dropConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_ = conn.Close()
}
