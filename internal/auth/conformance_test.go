// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit"
	authkitconformance "latere.ai/x/pkg/authkit/conformance"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// The two variables that point this tier at a running endpoint. Unset, every
// run below is against the stub authorizer of the shared contract, which is
// what a clean clone gets. Set, they are the endpoint a release run checks.
const (
	envAuthorizerURL   = "ARCA_TEST_AUTHORIZER_URL"
	envAuthorizerToken = "ARCA_TEST_AUTHORIZER_TOKEN"
)

// TestServiceConformance is rule R2 of the family's identity shape as a
// test, and criterion 2 of spec 006: arcad verifies that the audience is its
// own, reads one identity, and calls the issuer for nothing but its
// discovery document and its key set. The authenticator under test is the
// one arcad runs in production, built the way the node builds it.
func TestServiceConformance(t *testing.T) {
	authkitconformance.Run(t, authkitconformance.Service{
		Audience: audience,
		New: func(tb testing.TB, issuerURL, _ string) authkit.Authenticator {
			tb.Helper()
			v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
				Issuers: []string{issuerURL}, Audience: audience,
			})
			if err != nil {
				tb.Fatalf("the verifier arcad runs would not build: %v", err)
			}
			return v.Authenticator()
		},
	})
}

// TestAuthorizerConformance is the other half: the endpoint arcad asks
// answers the shared contract, for every row of Arca's declared table. The
// suite runs against the stub of latere.ai/x/pkg/authz, and against whatever
// ARCA_TEST_AUTHORIZER_URL names when a release run sets it.
func TestAuthorizerConformance(t *testing.T) {
	url, token, named := endpointFromEnv()
	if !named {
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		url, token = s.URL(), s.Token()
	}
	conformance.Run(t, url, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob))
}

// TestOwnerPolicyConformance is criterion 11 of spec 006: the policy arcad
// runs with no authorizer configured answers the same contract as an
// operator's endpoint, served through the scaffold of
// latere.ai/x/pkg/authz/server. Arca names no page action, so one Decider
// answers all twenty-three rows, the three lists included.
func TestOwnerPolicyConformance(t *testing.T) {
	const token = "conformance-bearer"
	endpoint := httptest.NewServer(server.New(server.Options{
		Bearer:     token,
		Vocabulary: authorizer.Vocabulary(),
		Decider:    policy(auth.PermissionNone, ""),
	}))
	defer endpoint.Close()
	conformance.Run(t, endpoint.URL, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob),
		conformance.WithHTTPClient(&http.Client{}))
}

// TestOwnerPolicyNarrowsByTheGrants is the grants row of the family's
// identity shape against the path arcad actually runs: the owner policy, in
// process, with nothing in front of it. A personal key granted one action on
// one object is refused every other action on that object and that action on
// every other object, and the request its own grant names is never refused
// as a grant.
//
// It is a second run of the same suite because
// latere.ai/x/pkg/authz/server applies the intersection to whatever its
// decider returned, so the run above passes whether or not the policy
// narrows anything. arcad reaches the policy through no scaffold, and this
// is the run that reads the policy's own answer.
func TestOwnerPolicyNarrowsByTheGrants(t *testing.T) {
	const token = "conformance-bearer"
	endpoint := httptest.NewServer(bare(t, policy(auth.PermissionNone, ""), token))
	defer endpoint.Close()
	conformance.Run(t, endpoint.URL, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob),
		conformance.WithHTTPClient(&http.Client{}))
}

// TestTheClientSpeaksToTheEndpointItIsPointedAt: the client arcad runs
// reaches the endpoint this tier is pointed at and reads its answers as
// decisions. Against the stub that is a local check; against a deployed
// endpoint it is the release run's.
func TestTheClientSpeaksToTheEndpointItIsPointedAt(t *testing.T) {
	url, token, named := endpointFromEnv()
	if !named {
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		url, token = s.URL(), s.Token()
	}
	c, err := auth.NewClient(auth.ClientOptions{URL: url, Token: token, HTTP: &http.Client{}})
	if err != nil {
		t.Fatalf("the client would not build: %v", err)
	}
	if err := auth.NewAuthorizer(c).Check(t.Context()); err != nil {
		t.Fatalf("the endpoint did not deny the probe: %v", err)
	}
}

// endpointFromEnv reads the endpoint a release run points this tier at.
func endpointFromEnv() (url, token string, named bool) {
	url = strings.TrimSpace(os.Getenv(envAuthorizerURL))
	token = strings.TrimSpace(os.Getenv(envAuthorizerToken))
	return url, token, url != ""
}

// bare serves one decider over the contract's wire: the bearer, the 400 an
// action outside the vocabulary answers, and the decision as the decider
// gave it. It is the scaffold of latere.ai/x/pkg/authz/server with the one
// thing left out that the run above cannot see around, and it exists for
// that run alone.
func bare(t *testing.T, d *auth.OwnerPolicy, token string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req authz.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The owner policy answers an unknown action with a verdict, and the
		// contract answers a malformed request: the client refuses one
		// before the wire, so the check is the endpoint's here.
		if !authorizer.Known(req.Action) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		decision, err := d.Decide(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer{
			Allow: decision.Allow, Reason: decision.Reason,
			TTL: int(decision.TTL.Seconds()), Limits: decision.Limits, Filter: decision.Filter,
		})
	})
}

// answer is the wire form of a decision, which the shared package renders
// from its own unexported type.
type answer struct {
	Allow  bool            `json:"allow"`
	Reason string          `json:"reason,omitempty"`
	TTL    int             `json:"ttl,omitempty"`
	Limits json.RawMessage `json:"limits,omitempty"`
	Filter *authz.Filter   `json:"filter,omitempty"`
}
