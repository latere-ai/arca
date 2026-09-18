// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/shares"
)

// TestStartSelectsTheOwnerPolicyWithNoEndpoint: ARCA_AUTHORIZER_URL unset is
// the owner policy of spec 006, which is a policy with tests and not the
// absence of one. ARCA_ADMIN_SUBJECTS is read here and nowhere else.
func TestStartSelectsTheOwnerPolicyWithNoEndpoint(t *testing.T) {
	iss := issuer(t)
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: audience, AdminSubjects: []string{bob},
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if id.Mode != auth.ModeOwnerPolicy {
		t.Errorf("the mode is %q, want %q", id.Mode, auth.ModeOwnerPolicy)
	}
	if id.Verifier == nil || id.Authorizer == nil {
		t.Fatal("the node started without a verifier or an authorizer")
	}
	// The administrator of the installation acts on a space it does not own,
	// which is what ARCA_ADMIN_SUBJECTS is for.
	ctx := serving(bob)
	if _, err := id.Authorizer.Decide(ctx, authorizer.ActionFileDelete, aFile("01J8R4")); err != nil {
		t.Errorf("an administrator was refused: %v", err)
	}
	if _, err := id.Authorizer.Decide(serving(carol), authorizer.ActionFileDelete, aFile("01J8R4")); err == nil {
		t.Error("a stranger was admitted to a space it does not own")
	}
	// The owner policy denies the probe like every authorizer of the family.
	if err := id.Authorizer.Check(t.Context()); err != nil {
		t.Errorf("the owner policy failed the probe: %v", err)
	}
}

// TestStartSelectsTheEndpointWhenOneIsConfigured: with ARCA_AUTHORIZER_URL
// set every decision is one call, and the owner policy is not consulted.
func TestStartSelectsTheEndpointWhenOneIsConfigured(t *testing.T) {
	iss := issuer(t)
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: audience,
		AuthorizerURL: s.URL(), AuthorizerToken: s.Token(),
		// An administrator of the owner policy, which the endpoint replaces:
		// with an endpoint set the subject decides nothing here.
		AdminSubjects: []string{bob},
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if id.Mode != auth.ModeAuthorizer {
		t.Errorf("the mode is %q, want %q", id.Mode, auth.ModeAuthorizer)
	}
	if _, err := id.Authorizer.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Errorf("the endpoint's allow was not honoured: %v", err)
	}
	if got := len(s.Requests()); got != 1 {
		t.Errorf("the endpoint was asked %d times; every decision is one call", got)
	}
}

// TestStartRefusesABadDeployment: every start-up failure of spec 006 names
// the variable to fix, so a deployment is corrected rather than guessed at.
func TestStartRefusesABadDeployment(t *testing.T) {
	iss := issuer(t)
	cases := []struct {
		name    string
		opts    auth.Options
		mustSay string
	}{
		{"an endpoint with no bearer", auth.Options{
			Issuers: []string{iss.URL()}, Audience: audience, AuthorizerURL: "https://authz.example/decide",
		}, "ARCA_AUTHORIZER_TOKEN"},
		{"no issuer", auth.Options{Audience: audience}, "ARCA_OIDC_ISSUERS"},
		{"an http issuer off loopback", auth.Options{
			Issuers: []string{"http://issuer.example"}, Audience: audience,
		}, "ARCA_OIDC_INSECURE_ISSUERS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := auth.Start(t.Context(), c.opts); err == nil {
				t.Fatal("the node started on a deployment it should refuse")
			} else if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the refusal is %q, which does not name %q", err, c.mustSay)
			}
		})
	}
}

// TestStartWiresTheGrantsAndLinksIntoTheOwnerPolicy: the two tables of spec
// 008 reach the policy the node runs, so the grant step and the link step
// answer once that spec lands them.
func TestStartWiresTheGrantsAndLinksIntoTheOwnerPolicy(t *testing.T) {
	iss := issuer(t)
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: audience,
		Grants: shares.Grants(pool{}, granted(auth.PermissionRead, "files/reports")),
		Links:  shares.Links(pool{}, granted(auth.PermissionRead, "files/reports")),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if _, err := id.Authorizer.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Errorf("a grantee within the ladder was refused: %v", err)
	}
	link := authorizer.Link{ID: "01J8LINK", Owner: alice, Path: "files/reports"}.Resource()
	if _, err := id.Authorizer.Decide(serving(""), authorizer.ActionLinkRead, link); err != nil {
		t.Errorf("a link that resolves was refused: %v", err)
	}
}

// TestTheVerifierStartedByStartIsTheOneTheNodeRuns: one wiring, so a node, a
// test tier and the check command all verify the same way.
func TestTheVerifierStartedByStartIsTheOneTheNodeRuns(t *testing.T) {
	iss := issuer(t)
	id, err := auth.Start(t.Context(), auth.Options{Issuers: []string{iss.URL()}, Audience: audience})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	c, err := id.Verifier.Verify(iss.Mint(issuertest.Claims{Sub: "9ab3"}))
	if err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if want := iss.URL() + "|9ab3"; c.Subject != want {
		t.Errorf("the subject is %q, want %q", c.Subject, want)
	}
	if id.Verifier.Audience() != audience {
		t.Errorf("the audience is %q, want %q", id.Verifier.Audience(), audience)
	}
}
