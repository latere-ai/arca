// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// The two subjects every case below decides between.
const (
	alice = "https://issuer.example|alice"
	bob   = "https://issuer.example|bob"
)

// asking builds the authorizer arcad runs against an endpoint, with the
// clock a cache test drives.
func asking(t *testing.T, s *stub.Server, now func() time.Time) *auth.Authorizer {
	t.Helper()
	c, err := auth.NewClient(auth.ClientOptions{
		URL: s.URL(), Token: s.Token(), HTTP: &http.Client{Timeout: 5 * time.Second}, Now: now,
	})
	if err != nil {
		t.Fatalf("the client would not build: %v", err)
	}
	return auth.NewAuthorizer(c)
}

// endpoint starts the family's stub authorizer, told Arca's vocabulary so it
// refuses an action outside the table the way a conforming endpoint does.
func endpoint(t *testing.T) *stub.Server {
	t.Helper()
	return stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
}

// serving is the context a handler decides on: the caller the verifier put
// there and the request block the API put there.
func serving(subject string) context.Context {
	c := auth.Caller{Claims: map[string]any{}}
	if subject != "" {
		iss, sub, _ := strings.Cut(subject, "|")
		c = auth.Caller{Subject: subject, Issuer: iss, Sub: sub, Claims: map[string]any{"sub": sub}}
	}
	ctx := auth.WithCaller(context.Background(), c)
	return auth.WithRequest(ctx, authz.Caller{ID: "req_01J8R4", IP: "203.0.113.4", UserAgent: "curl/8.7"})
}

// aFile is the resource of a question about one object.
func aFile(id string) authz.Resource {
	return authorizer.File{
		ID: id, Owner: alice, Path: "files/reports/q3.pdf", Plane: "files", Size: authorizer.Bytes(48213),
	}.Resource()
}

// TestAnAllowIsCachedPerSubjectActionAndResource is criterion 6 of spec 006:
// the cache is keyed by the three, so a second question about the same
// object costs no round trip and a question about another subject, another
// action or another object does.
func TestAnAllowIsCachedPerSubjectActionAndResource(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true, TTL: 60})
	a := asking(t, s, nil)

	if _, err := a.Decide(serving(alice), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the first question was refused: %v", err)
	}
	if _, err := a.Decide(serving(alice), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the repeated question was refused: %v", err)
	}
	if got := len(s.Requests()); got != 1 {
		t.Errorf("the same question was asked %d times; an allow is cached", got)
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		act  string
		res  authz.Resource
	}{
		{"another subject", serving(bob), authorizer.ActionFileRead, aFile("01J8R4")},
		{"another action", serving(alice), authorizer.ActionFileWrite, aFile("01J8R4")},
		{"another object", serving(alice), authorizer.ActionFileRead, aFile("01J8R5")},
	} {
		before := len(s.Requests())
		if _, err := a.Decide(c.ctx, c.act, c.res); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(s.Requests()) != before+1 {
			t.Errorf("%s did not reach the endpoint; the cache key is subject, action and resource id", c.name)
		}
	}
}

// TestAnExpiredAnswerIsAskedAgain: an allow is held for the answer's ttl and
// no longer, so an authorizer that changes its mind is obeyed within a
// minute by default.
func TestAnExpiredAnswerIsAskedAgain(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true, TTL: 60})
	clock := time.Now()
	a := asking(t, s, func() time.Time { return clock })

	if _, err := a.Decide(serving(alice), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the first question was refused: %v", err)
	}
	clock = clock.Add(61 * time.Second)
	if _, err := a.Decide(serving(alice), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the second question was refused: %v", err)
	}
	if got := len(s.Requests()); got != 2 {
		t.Errorf("the endpoint was asked %d times across the ttl boundary, want 2", got)
	}
}

// TestUnavailableIsNeverAnAllow is criterion 5 of spec 006: anything but a
// 200 carrying an allow is 503 authorizer_unavailable, and no mode of
// failure produces a decision. The forms are the contract's own: a status
// that is not 200, a body that is not JSON, a body with no allow, and no
// answer at all.
func TestUnavailableIsNeverAnAllow(t *testing.T) {
	cases := []struct {
		name string
		fail func(*stub.Server)
	}{
		{"a 500", func(s *stub.Server) { s.Fail(http.StatusInternalServerError) }},
		{"a 403 from a gateway", func(s *stub.Server) { s.Fail(http.StatusForbidden) }},
		{"a body that is not JSON", func(s *stub.Server) { s.FailBody(stub.BodyMalformed) }},
		{"a 200 with no allow", func(s *stub.Server) { s.FailBody(stub.BodyNoAllow) }},
		{"no answer at all", func(s *stub.Server) { s.Hang() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			c.fail(s)
			a := asking(t, s, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			d, err := a.Decide(ctx, authorizer.ActionFileRead, aFile("01J8R4"))
			if err == nil {
				t.Fatalf("an unavailable authorizer produced the decision %+v", d)
			}
			if got := auth.CodeOf(err); got != auth.CodeAuthorizerUnavailable {
				t.Errorf("the refusal is %q, want %q (detail: %s)", got, auth.CodeAuthorizerUnavailable, auth.DetailOf(err))
			}
			s.Resume()
		})
	}
}

// TestADenyIsForbiddenAndALookupIsNotFound: a refusal of the caller's own
// action says so, and a refusal of something the request merely named is
// indistinguishable from a missing object, which is invariant 6 of spec 001.
// The endpoint's reason is the developer detail and reaches no user
// sentence.
func TestADenyIsForbiddenAndALookupIsNotFound(t *testing.T) {
	s := endpoint(t)
	s.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "bob is not a member of the space's organization")
	a := asking(t, s, nil)

	_, err := a.Decide(serving(bob), authorizer.ActionFileRead, aFile("01J8R4"))
	if got := auth.CodeOf(err); got != auth.CodeForbidden {
		t.Errorf("a deny on the request's own action is %q, want %q", got, auth.CodeForbidden)
	}
	if !strings.Contains(auth.DetailOf(err), "not a member") {
		t.Errorf("the endpoint's reason is not in the developer detail: %q", auth.DetailOf(err))
	}
	_, err = a.Lookup(serving(bob), authorizer.ActionFileRead, aFile("01J8R5"))
	if got := auth.CodeOf(err); got != auth.CodeNotFound {
		t.Errorf("a deny at lookup is %q, want %q", got, auth.CodeNotFound)
	}
}

// TestADenyWithNoReasonStillSaysSomething: a log line always carries a
// reason, even when the endpoint named none.
func TestADenyWithNoReasonStillSaysSomething(t *testing.T) {
	s := endpoint(t)
	s.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "")
	_, err := asking(t, s, nil).Decide(serving(bob), authorizer.ActionFileRead, aFile("01J8R4"))
	if !strings.Contains(auth.DetailOf(err), "named no reason") {
		t.Errorf("the detail is %q", auth.DetailOf(err))
	}
}

// TestAnActionOutsideTheVocabularyCostsNoRoundTrip: the vocabulary rides on
// the client, so a handler that asks a question nobody decided on is a bug
// caught in this process and never an outage at the endpoint.
func TestAnActionOutsideTheVocabularyCostsNoRoundTrip(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	_, err := asking(t, s, nil).Decide(serving(alice), "quota.write", aFile("01J8R4"))
	if _, ok := errors.AsType[*authz.UnknownAction](err); !ok {
		t.Fatalf("an action outside the vocabulary answered %v", err)
	}
	if auth.CodeOf(err) == auth.CodeAuthorizerUnavailable {
		t.Error("an action outside the vocabulary reads as an outage")
	}
	if got := len(s.Requests()); got != 0 {
		t.Errorf("the endpoint was asked %d times about an action outside the table", got)
	}
}

// TestTheQuestionCarriesTheCallerAndTheRequest: the envelope is the shared
// one and carries the subject, its two halves, the claims verbatim, the
// action, the resource as one flat object, and the request block an
// operator's authorizer reads to correlate with Arca's own log.
func TestTheQuestionCarriesTheCallerAndTheRequest(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	if _, err := asking(t, s, nil).Decide(serving(alice), authorizer.ActionFileWrite, aFile("01J8R4")); err != nil {
		t.Fatalf("the question was refused: %v", err)
	}
	asked := s.Requests()
	if len(asked) != 1 {
		t.Fatalf("the endpoint was asked %d times, want 1", len(asked))
	}
	req := asked[0]
	if req.Subject != alice || req.Issuer != "https://issuer.example" || req.Sub != "alice" {
		t.Errorf("the subject is %q, %q, %q", req.Subject, req.Issuer, req.Sub)
	}
	if req.Claims["sub"] != "alice" {
		t.Errorf("the claims did not arrive verbatim: %v", req.Claims)
	}
	if req.Action != authorizer.ActionFileWrite {
		t.Errorf("the action is %q", req.Action)
	}
	if req.Resource.Kind != authorizer.KindFile || req.Resource.ID != "01J8R4" {
		t.Errorf("the resource is %+v", req.Resource)
	}
	if got := req.Resource.String("owner"); got != alice {
		t.Errorf("the resource names the owner %q", got)
	}
	if req.Request.ID != "req_01J8R4" || req.Request.IP != "203.0.113.4" || req.Request.UserAgent != "curl/8.7" {
		t.Errorf("the request block is %+v", req.Request)
	}
	if req.Workload != nil {
		t.Errorf("the question carries a workload member, %v; Arca mints no token", req.Workload)
	}
}

// TestAnAnonymousQuestionCarriesEmptyClaims: the three public link routes
// carry no bearer, and the question they ask names no subject and no claims
// rather than a nil map an endpoint would have to guard against.
func TestAnAnonymousQuestionCarriesEmptyClaims(t *testing.T) {
	req := auth.Envelope(auth.Caller{}, authz.Caller{ID: "req_1"}, authorizer.ActionLinkRead,
		authorizer.Link{ID: "01J8R9", Owner: alice, Path: "files/reports"}.Resource())
	if req.Subject != "" {
		t.Errorf("an anonymous question names the subject %q", req.Subject)
	}
	if req.Claims == nil {
		t.Error("an anonymous question carries nil claims")
	}
}

// TestTheProbeIsDeniedAndAnEndpointThatAllowsItIsReported: the check of spec
// 006 and spec 012. An endpoint that allows the reserved id is one that does
// not read the request, and arcad refuses it rather than trusting it.
func TestTheProbeIsDeniedAndAnEndpointThatAllowsItIsReported(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	if err := asking(t, s, nil).Check(t.Context()); err != nil {
		t.Errorf("a conforming endpoint failed the probe: %v", err)
	}

	// An endpoint that answers an allow to everything, the probe included.
	yes := allowEverything{}
	err := auth.NewAuthorizer(yes).Check(t.Context())
	if !errors.Is(err, authz.ErrProbeAllowed) {
		t.Errorf("an endpoint that allows the probe was accepted: %v", err)
	}
}

// TestAnUnavailableEndpointFailsTheCheck: the readiness check named
// authorizer reports an endpoint that cannot be reached, so a replica that
// cannot decide leaves rotation instead of answering 503 to every request.
func TestAnUnavailableEndpointFailsTheCheck(t *testing.T) {
	s := endpoint(t)
	s.Fail(http.StatusBadGateway)
	if err := asking(t, s, nil).Check(t.Context()); err == nil {
		t.Error("an unreachable endpoint passed the check")
	}
}

// allowEverything is the endpoint spec 006 refuses: one that answers an
// allow without reading the request, the probe id included.
type allowEverything struct{}

func (allowEverything) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Allow: true}, nil
}
