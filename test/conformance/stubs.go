// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"
)

// The control API of the stub authorizer, as the suite drives it over HTTP.
// It is the shared contract's, latere.ai/x/pkg/authz/stub: PUT /rules, PUT
// /fail, POST /hang and POST /resume, served beside the endpoint itself. The
// suite speaks it over the wire and imports nothing, so the package a
// consumer imports pulls in no test helper of this repository.
//
// Everything here is behind Options.AuthorizerControl. A target with no stub
// leaves it empty, and the groups that need it skip with the reason in the
// report.

// stubRule is one row of the stub's table. It is the shared package's own
// type rather than a shape declared here: the authorizer's envelope is
// declared once for the whole family, and a suite that wrote its own copy
// would be a second declaration of it. The shared package is
// latere.ai/x/pkg, which a consumer importing this suite already has, and
// not a helper of this repository's test tree.
type stubRule = stub.Rule

// setRules replaces the stub's table. Called with no rule it clears the
// table, which is how a case puts back what it found.
func (s *session) setRules(t testing.TB, rules ...stubRule) {
	t.Helper()
	if rules == nil {
		rules = []stubRule{}
	}
	// The control API takes {"rules": [...]}, which is the shared package's
	// shape and not a bare array.
	raw, err := json.Marshal(map[string]any{"rules": rules})
	failIf(t, err != nil, "render the stub's rules: %v", err)
	s.control(t, http.MethodPut, "/rules", string(raw))
}

// failAuthorizer puts the stub in one of the outages spec 006's table names,
// or takes it out of one when mode is empty. The modes are the shared
// stub's: a status other than 200, a 200 that is not an answer, and no
// answer at all.
func (s *session) failAuthorizer(t testing.TB, mode string) {
	t.Helper()
	switch {
	case mode == "":
		s.control(t, http.MethodPost, "/resume", "")
		s.control(t, http.MethodPut, "/fail", `{"status":0}`)
	case mode == "timeout":
		s.control(t, http.MethodPost, "/hang", "")
	case mode == "malformed", mode == "no-allow":
		s.control(t, http.MethodPut, "/fail", body(fields{"body": mode}))
	case strings.HasPrefix(mode, "status:"):
		s.control(t, http.MethodPut, "/fail", `{"status":`+strings.TrimPrefix(mode, "status:")+`}`)
	default:
		t.Fatalf("conformance: %q is not an outage of spec 006's table", mode)
	}
}

// grantsMode turns the stub's grants mode on and off and reports whether the
// stub understood the call. It is the one route here outside the shared
// control API: a decider that reads the `grant` of spec 006 is a property of
// the endpoint and not of the contract, so a target running a stub without
// the mode answers false and the case records what it could not verify
// rather than failing a target that conforms.
func (s *session) grantsMode(t testing.TB, on bool) bool {
	t.Helper()
	r := s.do(t, request{
		method:      http.MethodPut,
		path:        strings.TrimRight(s.options.AuthorizerControl, "/") + "/grants",
		body:        strings.NewReader(body(fields{"enabled": on})),
		contentType: "application/json",
	})
	return r.status < 300
}

// quota puts a byte limit on every allow the stub answers, which is the one
// way a limit reaches Arca: the core stores none. An empty limit takes it
// off again.
func (s *session) quota(t testing.TB, bytes int64) {
	t.Helper()
	rule := stubRule{Subject: "*", Action: "*", Resource: "*", Allow: true, TTL: 1}
	if bytes >= 0 {
		rule.Limits = map[string]any{"quota_bytes": bytes}
	}
	s.setRules(t, rule)
}

// control sends one request to the stub's control API and fails the case
// when the stub refuses it, which is a broken harness and never a finding
// about the target.
func (s *session) control(t testing.TB, method, path, payload string) {
	t.Helper()
	r := request{method: method, path: strings.TrimRight(s.options.AuthorizerControl, "/") + path}
	if payload != "" {
		r.body = strings.NewReader(payload)
		r.contentType = "application/json"
	}
	resp := s.do(t, r)
	failIf(t, resp.status >= 300, "the stub authorizer refused %s %s: %d %s", method, path, resp.status, resp.body)
}

// await drives an answer until it is the one the case is waiting for, or
// until the decision cache of spec 006 has had time to turn over. A verdict
// is cached, so a rule the case just installed is not the answer the next
// request gets; the cap is the contract's five seconds for a deny with a
// margin, and nothing here waits on a latency.
func (s *session) await(t testing.TB, drive func() response, done func(response) bool) response {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		r := drive()
		if done(r) || time.Now().After(deadline) {
			return r
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// escape renders a subject for a query parameter, which is how spec 013
// addresses a space in ?owner=.
func escape(value string) string { return url.QueryEscape(value) }

// pathParam renders a subject for the {owner} position of a path. Every
// character a subject holds that a path segment gives meaning to is escaped,
// so https://issuer.example|9ab3 is one segment and not four.
func pathParam(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
