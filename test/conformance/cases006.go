// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The rows of spec 006 the wire shows: every route behind the verifier
// refuses a caller it cannot identify, a request for another principal's
// space answers as a missing one and never as a refused one, and the
// authorizer's verdict is the whole of the decision, with an endpoint that
// does not answer refusing rather than allowing.

func cases006() []testCase {
	events := []string{"GET /v1/events"}
	// Reading what the target calls a principal takes a resource whose
	// answer renders the owner in full, which is a workspace: the cases that
	// name a second space drive those rows too.
	named := []string{"GET /v1/events", "POST /v1/workspaces", "DELETE /v1/workspaces/{id}"}
	return []testCase{
		{name: "NoBearer", group: GroupIdentity, routes: events, run: case006NoBearer},
		{name: "UnlistedIssuer", group: GroupIdentity, routes: events, run: case006UnlistedIssuer},
		{name: "AnotherSpace", group: GroupIdentity, routes: named, run: case006AnotherSpace},
		{name: "PublicRoutesTakeNoBearer", group: GroupIdentity, run: case006PublicRoutesTakeNoBearer},
		{name: "Deny", group: GroupAuthorizer, routes: named, run: case006Deny},
		{name: "Outage", group: GroupAuthorizer, routes: named, run: case006Outage},
		{name: "PersonalKey", group: GroupAuthorizer, routes: named, run: case006PersonalKey},
	}
}

// case006NoBearer: a request with no bearer is refused with unauthenticated,
// whatever the route, and the refusal is the envelope of spec 013 with the
// fixed sentence. Invariant 5 of spec 001 seen from outside: every /v1
// request carries a token, and there are three exceptions, which the case
// below names.
func case006NoBearer(t *testing.T, s *session) {
	r := s.do(t, request{method: http.MethodGet, path: "/v1/events"})
	expectError(t, r, CodeUnauthenticated)
}

// case006UnlistedIssuer: a bearer the target cannot verify is refused with
// unauthenticated and never with forbidden, and a path under /v1 that no
// route registers is refused the same way, so whether a route exists is not
// something an unauthenticated caller learns.
func case006UnlistedIssuer(t *testing.T, s *session) {
	for _, bearer := range []string{"not-a-token", strangerToken()} {
		r := s.do(t, request{method: http.MethodGet, path: "/v1/events",
			header: map[string]string{"Authorization": "Bearer " + bearer}})
		expectError(t, r, CodeUnauthenticated)
	}
	r := s.do(t, request{method: http.MethodGet, path: "/v1/there-is-no-such-route",
		header: map[string]string{"Authorization": "Bearer " + strangerToken()}})
	expectError(t, r, CodeUnauthenticated)

	// The two halves of spec 006's row a suite with a mint function and no
	// key cannot make: a token past its expiry and one addressed to another
	// audience. Both are a signature this suite cannot produce, and both are
	// proved in process by the family's audience suite beside the verifier.
	s.unverifiable(t, "an expired token and a token for another audience are refused",
		"Options.Token mints a valid bearer and the suite holds no signing key; latere.ai/x/pkg/authkit/conformance proves both in process")
}

// case006AnotherSpace: one principal asking about another's space is
// answered as a missing object and never as a refused one, which is
// invariant 6 of spec 001. The two answers are compared byte for byte
// against a space that does not exist at all, because the invariant is that
// they are indistinguishable and not merely that both are a 404.
func case006AnotherSpace(t *testing.T, s *session) {
	other := s.do(t, request{method: http.MethodGet, subject: Alice,
		path: "/v1/events?owner=" + subjectParam(s.subject(t, Bob))})
	absent := s.do(t, request{method: http.MethodGet, subject: Alice,
		path: "/v1/events?owner=" + subjectParam(s.absentSubject())})

	// A target whose authorizer allows every caller every space has nothing
	// to refuse here, and the invariant is about what a refusal looks like.
	if other.status == http.StatusOK {
		s.unverifiable(t, "a refused space answers as a missing one",
			"the target allows this caller another principal's space, so no refusal is produced")
		return
	}
	expectError(t, other, CodeNotFound)
	expectError(t, absent, CodeNotFound)
	failIf(t, other.code() != absent.code() || other.status != absent.status,
		"a refused space answers %d %s and a missing one %d %s; invariant 6 makes them one answer",
		other.status, other.code(), absent.status, absent.code())
	failIf(t, other.message() != absent.message(),
		"a refused space and a missing one answer different sentences: %q and %q", other.message(), absent.message())
}

// case006PublicRoutesTakeNoBearer: the three link redemption routes are the
// whole of the exception to invariant 5, so each answers a caller with no
// bearer, and a token that does not resolve is a missing object before any
// question is asked. A fourth route outside the verifier would show here as
// a route answering something other than unauthenticated without one.
func case006PublicRoutesTakeNoBearer(t *testing.T, s *session) {
	token := s.name("no-such-link-token")
	for _, path := range []string{
		"/v1/shares/links/" + token + "/meta",
		"/v1/shares/links/" + token,
		"/v1/shares/links/" + token + "/files/files/a.txt",
	} {
		if !s.served.serves("GET " + publicRoute(path)) {
			continue
		}
		r := s.do(t, request{method: http.MethodGet, path: path})
		failIf(t, r.status == http.StatusUnauthorized,
			"%s refused a caller with no bearer; it is one of spec 013's three exceptions to the verifier", path)
		expectError(t, r, CodeNotFound)
	}
}

// case006Deny: a deny the authorizer answers refuses the request, and the
// refusal is the contract's and not the endpoint's sentence. The rule is put
// in and taken out through the stub's control API, so one running stub
// serves the case without a restart.
func case006Deny(t *testing.T, s *session) {
	s.setRules(t, stubRule{Subject: "*", Action: "event.read", Resource: "*", Allow: false, Reason: "denied by the conformance suite"})
	defer s.setRules(t)
	// The verdict is cached for five seconds on a deny, so the case reads
	// the answer the rule produced rather than one cached before it.
	r := s.await(t, func() response {
		return s.call(t, Alice, http.MethodGet, "/v1/events?owner="+subjectParam(s.subject(t, Alice)), "")
	}, func(r response) bool { return r.status != http.StatusOK })
	failIf(t, r.status == http.StatusOK, "a denied action was allowed")
	code := r.code()
	failIf(t, code != CodeForbidden && code != CodeNotFound,
		"a deny answered %d %s; spec 013 makes it forbidden on the caller's own action and not_found on a reference denied at lookup",
		r.status, code)
	expectError(t, r, code)
}

// case006Outage: an authorizer that answers anything but a 200 with a
// verdict refuses every request with authorizer_unavailable and never
// allows. Failing closed is the property; the three shapes are the ones spec
// 006's table names.
func case006Outage(t *testing.T, s *session) {
	for _, mode := range []string{"malformed", "no-allow", "status:500"} {
		t.Run(mode, func(t *testing.T) {
			s.failAuthorizer(t, mode)
			defer s.failAuthorizer(t, "")
			r := s.await(t, func() response {
				return s.call(t, Alice, http.MethodGet, "/v1/events?owner="+subjectParam(s.subject(t, Alice)), "")
			}, func(r response) bool { return r.status != http.StatusOK })
			failIf(t, r.status == http.StatusOK, "an authorizer answering %s allowed a request; the client fails closed", mode)
			expectError(t, r, CodeAuthorizerUnavailable)
		})
	}
}

// case006PersonalKey: a token narrowed by grants reaches the actions its
// grants name and no other, and an action outside them is a deny with the
// reason grant, which is indistinguishable on the wire from any other deny.
// The narrowing is the shared library's, so what this case proves is that
// the core applies it rather than how it is computed.
func case006PersonalKey(t *testing.T, s *session) {
	// A narrowed key is a token the suite cannot mint, the same bound as the
	// expired token above. What the wire does show is the half the core owns:
	// a deny carrying the reason grant is refused with the same body as a
	// deny carrying any other reason, which the stub produces by answering
	// that reason.
	s.setRules(t, stubRule{Subject: "*", Action: "event.read", Resource: "*", Allow: false, Reason: "grant"})
	defer s.setRules(t)
	byGrant := s.await(t, func() response {
		return s.call(t, Alice, http.MethodGet, "/v1/events?owner="+subjectParam(s.subject(t, Alice)), "")
	}, func(r response) bool { return r.status != http.StatusOK })

	s.setRules(t, stubRule{Subject: "*", Action: "event.read", Resource: "*", Allow: false, Reason: "the caller is not a member"})
	byPolicy := s.await(t, func() response {
		return s.call(t, Alice, http.MethodGet, "/v1/events?owner="+subjectParam(s.subject(t, Alice)), "")
	}, func(r response) bool { return r.status != http.StatusOK })

	failIf(t, byGrant.status != byPolicy.status || byGrant.code() != byPolicy.code() || byGrant.message() != byPolicy.message(),
		"a deny with reason grant answers %d %s and one with another reason %d %s; a refusal names no reason on the wire",
		byGrant.status, byGrant.code(), byPolicy.status, byPolicy.code())
	failIf(t, strings.Contains(string(byGrant.body), "grant") && byGrant.status < 500,
		"the refusal carries the authorizer's reason into a body a caller reads: %s", byGrant.body)
}

// strangerToken is a bearer shaped like a JWT from an issuer no target
// lists. It is built here rather than minted, because a suite that could
// mint one would be holding a signing key a conforming target trusts.
func strangerToken() string {
	header := base64url(`{"alg":"RS256","typ":"JWT","kid":"conformance"}`)
	claims, err := json.Marshal(strangerClaims{
		Issuer: "https://issuer.invalid", Subject: "stranger", Audience: "arca",
		IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		panic("conformance: the stranger's claims cannot be rendered: " + err.Error())
	}
	return header + "." + base64url(string(claims)) + "." + base64url("not a signature")
}

// strangerClaims is the payload of the token above: the registered claims a
// verifier reads before it reaches a signature it cannot check.
type strangerClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

func base64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// subjectParam percent-encodes a subject for the {owner} position and the
// ?owner= parameter, which spec 013 addresses a space by.
func subjectParam(subject string) string { return escape(subject) }

// publicRoute reads the row of spec 013's table a redemption path belongs
// to, so the case drives only what the target serves.
func publicRoute(path string) string {
	switch {
	case strings.HasSuffix(path, "/meta"):
		return "/v1/shares/links/{token}/meta"
	case strings.Contains(path, "/files/"):
		return "/v1/shares/links/{token}/files/{path...}"
	}
	return "/v1/shares/links/{token}"
}

// subject answers the subject a principal's own token identifies, read off
// the target rather than assumed: spec 013 renders a subject in full in
// every response, so a client that stores what it read sends it back.
func (s *session) subject(t testing.TB, principal string) string {
	t.Helper()
	s.mu.Lock()
	known, held := s.subjects[principal]
	s.mu.Unlock()
	if held {
		return known
	}
	// A workspace names its owner, and me resolves to the caller, so one
	// create and one delete answer what the target calls this principal.
	r := s.call(t, principal, http.MethodPost, "/v1/workspaces",
		body(fields{"owner": "me", "slug": s.name("who-" + principal)}))
	failIf(t, r.status != http.StatusCreated, "read the subject of %q: POST /v1/workspaces answered %d: %s", principal, r.status, r.body)
	id, owner := str(r.json, "id"), str(r.json, "owner")
	failIf(t, owner == "", "the workspace answers no owner, and spec 013 renders a subject in full: %s", r.body)
	_ = s.call(t, principal, http.MethodDelete, "/v1/workspaces/"+id, "")
	s.mu.Lock()
	s.subjects[principal] = owner
	s.mu.Unlock()
	return owner
}

// absentSubject is a subject no principal has: a space that does not exist,
// which invariant 6 compares a refused one against.
func (s *session) absentSubject() string {
	return fmt.Sprintf("https://issuer.invalid|%s", s.name("absent"))
}
