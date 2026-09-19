// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package authorizer is Arca's stub authorizer of spec 014: the family's
// latere.ai/x/pkg/authz/stub with three additions Arca's tiers need.
//
// The shared stub carries the envelope, the rule table, the record of every
// request, and the outage modes a client must fail closed on: a status other
// than 200, a 200 that is not an answer, and no answer at all. It also
// carries the one rule that binds every authorizer, this one included: the
// probe resource is denied for every subject and every action, so a core's
// check command reads an allow on it as an endpoint that does not read the
// request.
//
// Arca adds the three the shared stub has no place for: a deny chosen per
// request by a header, so one running stub serves a table of cases without
// a rule change between them; a connection dropped before the response
// line, which is the failure the client's one retry exists for; and the
// grants mode, which answers a request the rule table denied by the grant on
// its resource.
//
// The grants mode is the one row a platform's decider has to add to consume
// spec 006's resource grant: a grant admits the ladder's actions of its rung
// and nothing else. The rule table decides first and the mode only ever
// turns a deny into an allow, so it models a decider that has its own rules
// and reads the grant beside them, and never the shared stub's allow-all.
// The probe stays denied, which binds this authorizer as it binds every
// other.
package authorizer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/internal/auth"
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

// Server is one stub authorizer: the shared server, plus the header deny,
// the dropped connection and the grants mode.
type Server struct {
	*stub.Server

	mu     sync.Mutex
	drop   bool
	grants bool

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
	s.mux.HandleFunc("PUT /grants", s.putGrants)
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

// Grants turns the grants mode on and off: with it on, a request the rule
// table denied is allowed when the grant on its resource reaches the rung
// the action needs (spec 006's ladder). It is the -grants flag of spec 014,
// and PUT /grants of the control API.
func (s *Server) Grants(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = on
}

// granting reports whether the grants mode is on.
func (s *Server) granting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants
}

// putGrants is the control route the conformance suite turns the mode on
// through, so one running stub serves the grant cases without a restart.
func (s *Server) putGrants(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.Grants(body.Enabled)
	w.WriteHeader(http.StatusNoContent)
}

// serve is the endpoint: the dropped connection first, then the header deny
// and the grants mode over the shared stub's own verdict.
//
// A request that is not a decision goes straight through. The shared control
// API is served behind this handler, and a PUT of the rules is not an
// envelope to read a resource off.
func (s *Server) serve() http.Handler {
	shared := s.Server.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dropping() {
			dropConnection(w)
			return
		}
		denied := r.Header.Get(DenyHeader)
		if (denied == "" && !s.granting()) || !deciding(r) {
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
		// recorded and its outage modes still apply; what the header and
		// the mode change is the verdict of this one request.
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		shared.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusOK {
			copyResponse(w, recorder)
			return
		}
		switch {
		case request.Action == denied:
			answer(w, false, "denied by the stub's header")
		case s.granting() && !allowed(recorder) && admits(request):
			answer(w, true, "the grant on the resource reaches the rung this action needs")
		default:
			copyResponse(w, recorder)
		}
	})
}

// deciding reports whether a request is a question rather than a call on the
// control API.
func deciding(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/" || r.URL.Path == "")
}

// allowed reads the verdict off the shared stub's own answer.
func allowed(recorder *httptest.ResponseRecorder) bool {
	var answer struct {
		Allow bool `json:"allow"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
	return answer.Allow
}

// admits is the one row the grants mode adds: the grant the question carries
// admits the ladder's actions of its rung, and nothing else. The ladder is
// internal/auth's, read rather than copied, so the stub and the owner policy
// cannot disagree about what a rung reaches.
//
// The probe is refused before the row is read. Every authorizer of the
// family denies the reserved id for every subject and every action, and a
// mode that answered it would be an endpoint that does not read the request.
func admits(req authz.Request) bool {
	if strings.EqualFold(req.Resource.ID, ProbeID) {
		return false
	}
	need, reachable := auth.Granted(req.Action)
	if !reachable {
		return false
	}
	return auth.Admits(auth.Permission(req.Resource.String(auth.GrantField)), need)
}

// verdict is an answer this package writes itself rather than taking from
// the shared stub: the envelope's two fields and nothing else.
type verdict struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

// answer writes one verdict of this package's own.
func answer(w http.ResponseWriter, allow bool, reason string) {
	httpjson.Write(w, http.StatusOK, verdict{Allow: allow, Reason: reason})
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
