// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/authz"
)

// ask sends one question and answers the status and the decoded body.
func ask(t *testing.T, s *Server, request authz.Request, headers map[string]string) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the stub did not answer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	answer := map[string]any{}
	_ = json.Unmarshal(raw, &answer)
	return resp.StatusCode, answer
}

// question is one request over a space, the shape a handler of spec 006
// asks with.
func question(action, resource string) authz.Request {
	return authz.Request{
		Subject:  "https://issuer.example|dev",
		Issuer:   "https://issuer.example",
		Sub:      "dev",
		Action:   action,
		Resource: authz.NewResource("Space", resource, map[string]any{"owner": "https://issuer.example|dev"}),
	}
}

func TestAuthorizerStub(t *testing.T) {
	t.Run("it allows what no rule denies and records every request", func(t *testing.T) {
		s := New(t)
		status, answer := ask(t, s, question("file.read", "space-1"), nil)
		if status != http.StatusOK || answer["allow"] != true {
			t.Fatalf("the answer is %d %v", status, answer)
		}
		requests := s.Requests()
		if len(requests) != 1 || requests[0].Action != "file.read" {
			t.Fatalf("the stub recorded %+v", requests)
		}
		if got := requests[0].Resource.String("owner"); got != "https://issuer.example|dev" {
			t.Errorf("the resource fields did not survive: %q", got)
		}
		s.ClearRequests()
		if len(s.Requests()) != 0 {
			t.Error("the record was not cleared")
		}
	})

	t.Run("it denies the probe for every subject and every action", func(t *testing.T) {
		s := New(t)
		for _, action := range []string{"file.read", "space.admin"} {
			status, answer := ask(t, s, question(action, ProbeID), nil)
			if status != http.StatusOK || answer["allow"] != false {
				t.Fatalf("%s on the probe = %d %v", action, status, answer)
			}
		}
	})

	t.Run("a rule denies one action, and the limits ride every allow", func(t *testing.T) {
		s := New(t)
		// The table is read from the last row back, so the catch-all goes
		// first and the row that denies one action after it.
		s.SetRules(
			Rule{Action: "*", Allow: true, TTL: 30, Limits: map[string]any{"quota_bytes": 1024}},
			Rule{Action: "file.write", Allow: false, Reason: "grant"},
		)
		status, answer := ask(t, s, question("file.write", "space-1"), nil)
		if status != http.StatusOK || answer["allow"] != false || answer["reason"] != "grant" {
			t.Fatalf("the denied action = %d %v", status, answer)
		}
		_, allowed := ask(t, s, question("file.read", "space-1"), nil)
		if allowed["allow"] != true || allowed["ttl"] != float64(30) {
			t.Fatalf("the allowed action = %v", allowed)
		}
		limits, ok := allowed["limits"].(map[string]any)
		if !ok || limits["quota_bytes"] != float64(1024) {
			t.Fatalf("the limits are %v", allowed["limits"])
		}
	})

	t.Run("a header denies one request and leaves the table alone", func(t *testing.T) {
		s := New(t)
		status, answer := ask(t, s, question("file.write", "space-1"), map[string]string{DenyHeader: "file.write"})
		if status != http.StatusOK || answer["allow"] != false {
			t.Fatalf("the denied request = %d %v", status, answer)
		}
		if answer["reason"] != "denied by the stub's header" {
			t.Errorf("the reason is %v", answer["reason"])
		}
		if _, other := ask(t, s, question("file.read", "space-1"), map[string]string{DenyHeader: "file.write"}); other["allow"] != true {
			t.Fatalf("a header naming another action changed this one: %v", other)
		}
		if len(s.Requests()) != 2 {
			t.Fatalf("the stub recorded %d requests, and both reached it", len(s.Requests()))
		}
	})

	t.Run("a header deny does not paper over a refusal", func(t *testing.T) {
		s := New(t)
		s.Fail(http.StatusInternalServerError)
		if status, _ := ask(t, s, question("file.write", "space-1"), map[string]string{DenyHeader: "file.write"}); status != http.StatusInternalServerError {
			t.Fatalf("the refusal answered %d", status)
		}
	})

	t.Run("every outage mode the contract names", func(t *testing.T) {
		s := New(t)
		s.Fail(http.StatusBadGateway)
		if status, _ := ask(t, s, question("file.read", "space-1"), nil); status != http.StatusBadGateway {
			t.Errorf("the status mode answered %d", status)
		}
		s.Fail(0)
		s.FailBody(BodyMalformed)
		if _, answer := ask(t, s, question("file.read", "space-1"), nil); len(answer) != 0 {
			t.Errorf("the malformed mode answered %v", answer)
		}
		s.FailBody(BodyNoAllow)
		if _, answer := ask(t, s, question("file.read", "space-1"), nil); answer["allow"] != nil {
			t.Errorf("the no-allow mode carried a verdict: %v", answer)
		}
		s.FailBody("")
		s.DropConnections(true)
		body, err := json.Marshal(question("file.read", "space-1"))
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+s.Token())
		resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the connection closed before a response line
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the dropped connection answered")
		}
		s.DropConnections(false)
		if status, _ := ask(t, s, question("file.read", "space-1"), nil); status != http.StatusOK {
			t.Errorf("the stub did not recover: %d", status)
		}
	})

	t.Run("a request without the bearer is refused", func(t *testing.T) {
		s := New(t, WithToken("another-token"))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a request with no bearer answered %d", resp.StatusCode)
		}
	})

	t.Run("a body that is no question is refused", func(t *testing.T) {
		s := New(t)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), bytes.NewReader([]byte("{not json")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+s.Token())
		req.Header.Set(DenyHeader, "file.write")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("a body that is no question answered %d", resp.StatusCode)
		}
	})

	t.Run("a handler without a listener serves the binary", func(t *testing.T) {
		s := NewHandler(WithAllow("dev"))
		defer s.Close()
		if s.Handler() == nil {
			t.Fatal("Handler() is nil")
		}
		if s.Token() != DefaultToken {
			t.Errorf("Token() = %q", s.Token())
		}
		if WithVocabulary == nil {
			t.Error("the shared options are not re-exported")
		}
	})
}

// TestADroppedConnectionFallsBackWhenTheWriterCannotHijack keeps the mode
// usable behind a writer that holds no connection, where the nearest thing
// to no answer is an outage the client reads the same way.
func TestADroppedConnectionFallsBackWhenTheWriterCannotHijack(t *testing.T) {
	recorder := httptest.NewRecorder()
	dropConnection(recorder)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("the fallback answered %d", recorder.Code)
	}
}

// fileQuestion is one question over an object of somebody else's space, with
// the rung spec 006's resource carries, which is what the grants mode reads.
func fileQuestion(action, grant string) authz.Request {
	fields := map[string]any{
		"owner": "https://issuer.example|owner", "path": "files/reports/q3.pdf", "plane": "files",
	}
	if grant != "" {
		fields["grant"] = grant
	}
	return authz.Request{
		Subject: "https://issuer.example|dev", Issuer: "https://issuer.example", Sub: "dev",
		Action: action, Resource: authz.NewResource("File", "01J8R4", fields),
	}
}

// denyEverything is the table a decider with rules of its own stands in for:
// nothing is allowed, so what the grants mode adds is the whole of what a
// grantee gets.
func denyEverything(s *Server) {
	s.SetRules(Rule{Action: "*", Allow: false, Reason: "no rule of the stub allows it"})
}

// TestTheGrantsModeAdmitsTheLaddersActionsOfTheRung is the row a platform's
// decider adds to consume the grant of spec 006: a rung admits the ladder's
// actions of that rung and nothing else. The mode is what lets the tiers and
// the conformance suite prove a grantee reads a shared object under an
// external authorizer.
func TestTheGrantsModeAdmitsTheLaddersActionsOfTheRung(t *testing.T) {
	for _, c := range []struct {
		name   string
		action string
		grant  string
		allow  bool
	}{
		{"a read grant reads", "file.read", "read", true},
		{"a read grant lists", "file.list", "read", true},
		{"a read grant does not write", "file.write", "read", false},
		{"a write grant writes", "file.write", "write", true},
		{"a write grant reads, because the ladder is cumulative", "file.read", "write", true},
		{"a manage grant manages", "share.create", "manage", true},
		{"no grant on the resource stays denied", "file.read", "", false},
		{"an action no grant reaches stays denied", "link.read", "manage", false},
		{"the administrator's action stays denied", "space.admin", "manage", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New(t)
			denyEverything(s)
			s.Grants(true)
			status, answer := ask(t, s, fileQuestion(c.action, c.grant), nil)
			if status != http.StatusOK {
				t.Fatalf("the question answered %d", status)
			}
			if answer["allow"] != c.allow {
				t.Errorf("%s with the grant %q answered %v, want allow=%v", c.action, c.grant, answer, c.allow)
			}
		})
	}
}

// TestTheGrantsModeOnlyEverAllows: the rule table decides first and the mode
// is one row below it. It turns a deny into an allow and never the other way,
// so a stub allowing everything answers the same with the mode on, and the
// header deny still wins over both.
func TestTheGrantsModeOnlyEverAllows(t *testing.T) {
	s := New(t)
	s.Grants(true)
	if _, answer := ask(t, s, fileQuestion("file.write", ""), nil); answer["allow"] != true {
		t.Errorf("the mode refused what the table allows: %v", answer)
	}
	if _, answer := ask(t, s, fileQuestion("file.read", "read"), map[string]string{DenyHeader: "file.read"}); answer["allow"] != false {
		t.Errorf("the mode overruled the header deny: %v", answer)
	}
}

// TestTheGrantsModeKeepsTheProbeDenied: the reserved id is denied for every
// subject and every action by every authorizer of the family, and a mode that
// answered it would be an endpoint that does not read the request.
func TestTheGrantsModeKeepsTheProbeDenied(t *testing.T) {
	s := New(t)
	denyEverything(s)
	s.Grants(true)
	probe := fileQuestion("file.read", "manage")
	probe.Resource = authz.NewResource("Space", ProbeID, map[string]any{"grant": "manage"})
	if _, answer := ask(t, s, probe, nil); answer["allow"] != false {
		t.Errorf("the probe was allowed by the grants mode: %v", answer)
	}
}

// TestTheGrantsModeIsDrivenThroughTheControlAPI: the conformance suite turns
// the mode on over HTTP, so one running stub serves the grant cases without a
// restart, and the shared control API still answers with the mode on.
func TestTheGrantsModeIsDrivenThroughTheControlAPI(t *testing.T) {
	s := New(t)
	denyEverything(s)

	control := func(t *testing.T, path, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, s.URL()+path, bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if status := control(t, "/grants", `{"enabled":true}`); status != http.StatusNoContent {
		t.Fatalf("turning the mode on answered %d", status)
	}
	if _, answer := ask(t, s, fileQuestion("file.read", "read"), nil); answer["allow"] != true {
		t.Errorf("the mode was not on after the control call: %v", answer)
	}
	// The rules still reach the shared stub with the mode on, which a case
	// that puts its table back depends on.
	if status := control(t, "/rules", `{"rules":[]}`); status != http.StatusNoContent {
		t.Fatalf("a rule change with the mode on answered %d", status)
	}
	if status := control(t, "/grants", `{"enabled":false}`); status != http.StatusNoContent {
		t.Fatalf("turning the mode off answered %d", status)
	}
	denyEverything(s)
	if _, answer := ask(t, s, fileQuestion("file.read", "read"), nil); answer["allow"] != false {
		t.Errorf("the mode was still on after it was turned off: %v", answer)
	}
	if status := control(t, "/grants", "{not json"); status != http.StatusBadRequest {
		t.Errorf("a body that is no setting answered %d", status)
	}
}

// TestTheGrantsModeDoesNotPaperOverARefusal: an outage is an outage with the
// mode on, because the mode reads a verdict and a refusal is none.
func TestTheGrantsModeDoesNotPaperOverARefusal(t *testing.T) {
	s := New(t)
	s.Grants(true)
	s.Fail(http.StatusBadGateway)
	if status, _ := ask(t, s, fileQuestion("file.read", "read"), nil); status != http.StatusBadGateway {
		t.Fatalf("the refusal answered %d", status)
	}
}
