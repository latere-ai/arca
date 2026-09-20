// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, AdminSubjects: []string{bob},
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
		Issuers: []string{iss.URL()}, Audiences: []string{audience},
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
			Issuers: []string{iss.URL()}, Audiences: []string{audience}, AuthorizerURL: "https://authz.example/decide",
		}, "ARCA_AUTHORIZER_TOKEN"},
		{"no issuer", auth.Options{Audiences: []string{audience}}, "ARCA_OIDC_ISSUERS"},
		{"an http issuer off loopback", auth.Options{
			Issuers: []string{"http://issuer.example"}, Audiences: []string{audience},
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
		Issuers: []string{iss.URL()}, Audiences: []string{audience},
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
	id, err := auth.Start(t.Context(), auth.Options{Issuers: []string{iss.URL()}, Audiences: []string{audience}})
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

// gatedIssuer is the family's stub issuer behind a gate a test opens and
// closes. The URL never moves, so a test takes the issuer away and puts it
// back the way a cluster does while the issuer's pod is not up yet, and a
// closed gate answers 503 rather than refusing the connection, which is the
// same verdict a warm reads from an issuer that is not there.
type gatedIssuer struct {
	*issuertest.Server
	open atomic.Bool
}

// gated starts one, closed.
func gated(t *testing.T) *gatedIssuer {
	t.Helper()
	front := httptest.NewUnstartedServer(nil)
	g := &gatedIssuer{Server: issuertest.NewHandler(
		issuertest.WithIssuer("http://"+front.Listener.Addr().String()),
		issuertest.WithDefaultAudience(audience),
	)}
	stub := g.Handler()
	front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.open.Load() {
			http.Error(w, "the issuer is not there", http.StatusServiceUnavailable)
			return
		}
		stub.ServeHTTP(w, r)
	})
	front.Start()
	t.Cleanup(front.Close)
	return g
}

// quiet is a logger a test does not read.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// awaitWarm waits for the issuers check to pass.
func awaitWarm(t *testing.T, id *auth.Identity) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := id.Verifier.Check(t.Context()); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("the issuer came back and the check never passed: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartWarmsBestEffort is spec 006's warm-up row and spec 002's issuers
// check together: the warm is an optimisation and not a gate, so the three
// states it can be in are a replica that serves, a replica that serves and
// is ready, and a replica that serves and is not.
//
// A failed warm that exited would make every installation's start
// order-dependent on its issuer and would crash-loop a replica through a
// transient issuer outage, which is what the kind stack of spec 016 hit.
func TestStartWarmsBestEffort(t *testing.T) {
	t.Run("the issuer answers at start", func(t *testing.T) {
		iss := gated(t)
		iss.open.Store(true)
		id, err := auth.Start(t.Context(), auth.Options{
			Issuers: []string{iss.URL()}, Audiences: []string{audience}, Log: quiet(),
		})
		if err != nil {
			t.Fatalf("the node would not start: %v", err)
		}
		if err := id.Verifier.Check(t.Context()); err != nil {
			t.Errorf("the issuers check fails against an issuer that answered: %v", err)
		}
		if len(iss.Requests()) == 0 {
			t.Error("the node started without reading the issuer, so the warm did nothing")
		}
	})

	t.Run("the issuer answers later", func(t *testing.T) {
		iss := gated(t)
		id, err := auth.Start(t.Context(), auth.Options{
			Issuers: []string{iss.URL()}, Audiences: []string{audience}, Log: quiet(),
			WarmRetry: 5 * time.Millisecond, WarmRetryMax: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("an issuer that is not up yet stopped the node: %v", err)
		}
		if err := id.Verifier.Check(t.Context()); err == nil {
			t.Fatal("the issuers check passes while no issuer has answered")
		}
		iss.open.Store(true)
		awaitWarm(t, id)
	})

	t.Run("the issuer never answers", func(t *testing.T) {
		var log strings.Builder
		iss := gated(t)
		// The retry is left at its default here, which is the schedule a
		// deployment runs on: the first is a second away, so nothing has
		// retried by the time the check below reads the verdict.
		id, err := auth.Start(t.Context(), auth.Options{
			Issuers: []string{iss.URL()}, Audiences: []string{audience},
			Log: slog.New(slog.NewTextHandler(&log, nil)),
		})
		if err != nil {
			t.Fatalf("an issuer that is not there stopped the node: %v", err)
		}
		// One line in the developer register, naming the variable to fix.
		if got := log.String(); !strings.Contains(got, "ARCA_OIDC_ISSUERS") {
			t.Errorf("the start-up log is %q and does not name the variable", got)
		}
		time.Sleep(50 * time.Millisecond)
		err = id.Verifier.Check(t.Context())
		if err == nil {
			t.Fatal("the issuers check passes against an issuer that is not there")
		}
		if !strings.Contains(err.Error(), "ARCA_OIDC_ISSUERS") {
			t.Errorf("the check reports %q, which does not name the variable", err)
		}
	})

	// The warm is what saves the first request a discovery, not what makes a
	// token verifiable: the shared validator fetches on the first token of an
	// issuer it has not read. The retry here is an hour, so nothing in the
	// background can have warmed it.
	t.Run("the first request pays the discovery the warm did not", func(t *testing.T) {
		iss := gated(t)
		id, err := auth.Start(t.Context(), auth.Options{
			Issuers: []string{iss.URL()}, Audiences: []string{audience}, Log: quiet(),
			WarmRetry: time.Hour, WarmRetryMax: time.Hour,
		})
		if err != nil {
			t.Fatalf("an issuer that is not up yet stopped the node: %v", err)
		}
		iss.open.Store(true)
		if _, err := id.Verifier.Verify(iss.Mint(issuertest.Claims{Sub: "9ab3"})); err != nil {
			t.Fatalf("the first request did not pay for the discovery the warm missed: %v", err)
		}
	})
}
