// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/arca/internal/auth"
)

// audience is ARCA_OIDC_AUDIENCE's default, the aud every token arcad
// accepts must carry.
const audience = "arca"

// issuer starts the family's stub issuer, which serves a discovery document,
// a key set, and a mint control the table below sets one field wrong on. It
// is latere.ai/x/pkg's, so Arca stands up no issuer of its own for a test.
func issuer(t *testing.T) *issuertest.Server {
	t.Helper()
	return issuertest.New(t, issuertest.WithDefaultAudience(audience))
}

// verifier builds the verifier arcad runs, over the given issuers.
func verifier(t *testing.T, issuers ...string) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: issuers, Audience: audience})
	if err != nil {
		t.Fatalf("the verifier would not build: %v", err)
	}
	return v
}

// TestTheReasonTable is criterion 1 of spec 006: every way a bearer is
// refused is a 401 carrying the row of the shared reason table, so an
// operator reading a log sees the same word every service of the family
// writes. The user sentence is the API's and never varies; the row is the
// developer detail.
func TestTheReasonTable(t *testing.T) {
	iss, other := issuer(t), issuer(t)
	v := verifier(t, iss.URL())
	valid := iss.Mint(issuertest.Claims{Sub: "9ab3"})

	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"no bearer at all", "", auth.ReasonMissing},
		{"not a JWS", "this-is-not-a-token", "malformed"},
		{"an issuer ARCA_OIDC_ISSUERS does not list", other.Mint(issuertest.Claims{Sub: "9ab3"}), "issuer"},
		{"an audience that is not this server", iss.Mint(issuertest.Claims{Sub: "9ab3", Aud: issuertest.StringList{"cella"}}), "audience"},
		{"no audience at all", iss.Mint(issuertest.Claims{Sub: "9ab3", Omit: []string{"aud"}}), "audience"},
		{"expired", iss.Mint(issuertest.Claims{Sub: "9ab3", Exp: time.Now().Add(-time.Hour).Unix()}), "expired"},
		{"not yet valid", iss.Mint(issuertest.Claims{Sub: "9ab3", Nbf: time.Now().Add(time.Hour).Unix()}), "nbf"},
		{"no iat", iss.Mint(issuertest.Claims{Sub: "9ab3", Omit: []string{"iat"}}), "iat"},
		{"an iat older than a day", iss.Mint(issuertest.Claims{
			Sub: "9ab3", Iat: time.Now().Add(-25 * time.Hour).Unix(),
			Exp: time.Now().Add(time.Hour).Unix(),
		}), "iat"},
		{"a kid the key set does not hold", iss.Mint(issuertest.Claims{Sub: "9ab3", Kid: "no-such-key"}), "unknown_key"},
		{"a signature that does not check out", tamper(valid), "signature"},
		{"a token above the size bound", iss.Mint(issuertest.Claims{
			Sub: "9ab3", Extra: map[string]any{"padding": strings.Repeat("x", 9<<10)},
		}), "size"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/trash", nil)
			if c.token != "" {
				r.Header.Set("Authorization", "Bearer "+c.token)
			}
			caller, err := v.Authenticate(r)
			if err == nil {
				t.Fatalf("the bearer was admitted as %q", caller.Subject)
			}
			if got := auth.CodeOf(err); got != auth.CodeUnauthenticated {
				t.Errorf("the refusal is %q, want %q", got, auth.CodeUnauthenticated)
			}
			if got := auth.ReasonOf(err); got != c.reason {
				t.Errorf("the reason is %q, want %q (detail: %s)", got, c.reason, auth.DetailOf(err))
			}
			if auth.DetailOf(err) == "" {
				t.Error("the refusal carries no developer detail")
			}
		})
	}
}

// TestASubjectIsRenderedOnce: a verified token becomes the subject every
// owner field, every event and every authorizer request carries, and the
// claims reach the caller verbatim because the authorizer reads them and
// Arca reads none of them.
func TestASubjectIsRenderedOnce(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss.URL())
	token := iss.Mint(issuertest.Claims{Sub: "9ab3", Email: "a@example.com"})

	c, err := v.Verify(token)
	if err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if want := iss.URL() + "|9ab3"; c.Subject != want {
		t.Errorf("the subject is %q, want %q", c.Subject, want)
	}
	if c.Issuer != iss.URL() || c.Sub != "9ab3" {
		t.Errorf("the halves are %q and %q", c.Issuer, c.Sub)
	}
	if c.Anonymous() {
		t.Error("a verified caller reads as anonymous")
	}
	if got := c.Claims["email"]; got != "a@example.com" {
		t.Errorf("the claims did not arrive verbatim: email is %v", got)
	}
	if got := c.Claims["iss"]; got != iss.URL() {
		t.Errorf("the claims did not arrive verbatim: iss is %v", got)
	}
}

// TestATrailingSlashIsOneIssuer: an issuer is the same issuer with or
// without its trailing slash, so a subject is one string either way and a
// space is not addressed twice.
func TestATrailingSlashIsOneIssuer(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss.URL()+"/")
	c, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "9ab3"}))
	if err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if want := iss.URL() + "|9ab3"; c.Subject != want {
		t.Errorf("the subject is %q, want %q", c.Subject, want)
	}
	if got := v.Issuers(); len(got) != 1 || got[0] != iss.URL() {
		t.Errorf("the verifier lists the issuers %v", got)
	}
}

// TestATokenWithNoSubjectIsRefused: a space is addressed by a subject, so a
// token that names none names no space. The shared validator refuses it as
// malformed, and no caller reaches a handler with a subject of its own.
func TestATokenWithNoSubjectIsRefused(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss.URL())
	c, err := v.Verify(iss.Mint(issuertest.Claims{Omit: []string{"sub"}}))
	if auth.CodeOf(err) != auth.CodeUnauthenticated {
		t.Fatalf("a token with no sub was admitted: %v", err)
	}
	if auth.ReasonOf(err) != "malformed" {
		t.Errorf("the reason is %q, want %q", auth.ReasonOf(err), "malformed")
	}
	if c.Subject != "" || !c.Anonymous() {
		t.Errorf("a refused token yielded the subject %q", c.Subject)
	}
}

// TestTheVerifierRefusesToStartOnABadDeployment: spec 006's start-up rule.
// An issuer that does not answer, an empty list and a repeated issuer are a
// deployment to fix, and every refusal names the variable rather than
// becoming a stream of 401s in a log.
func TestTheVerifierRefusesToStartOnABadDeployment(t *testing.T) {
	iss := issuer(t)
	cases := []struct {
		name    string
		opts    auth.VerifierOptions
		mustSay string
	}{
		{"no issuer", auth.VerifierOptions{Audience: audience}, "ARCA_OIDC_ISSUERS names no issuer"},
		{"an issuer listed twice", auth.VerifierOptions{
			Issuers: []string{iss.URL(), iss.URL() + "/"}, Audience: audience,
		}, "twice"},
		{"an issuer that is not a URL", auth.VerifierOptions{
			Issuers: []string{"::not a url"}, Audience: audience,
		}, "is not a URL"},
		{"an issuer on a scheme that is not http", auth.VerifierOptions{
			Issuers: []string{"ftp://issuer.example"}, Audience: audience,
		}, "a key set is read over https"},
		{"an http issuer off loopback", auth.VerifierOptions{
			Issuers: []string{"http://issuer.example"}, Audience: audience,
		}, "ARCA_OIDC_INSECURE_ISSUERS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := auth.NewVerifier(t.Context(), c.opts); err == nil {
				t.Fatal("the verifier started on a deployment it should refuse")
			} else if !strings.Contains(err.Error(), c.mustSay) {
				t.Errorf("the refusal is %q, which does not say %q", err, c.mustSay)
			}
		})
	}
}

// TestAnHTTPIssuerOffLoopbackNeedsTheVariable: ARCA_OIDC_INSECURE_ISSUERS is
// the one way an http issuer that is not this machine is admitted, which the
// test tiers of spec 014 set and a deployment does not. Loopback needs no
// variable, which is what lets the stub issuer of this test run at all.
func TestAnHTTPIssuerOffLoopbackNeedsTheVariable(t *testing.T) {
	iss := issuer(t)
	if !strings.HasPrefix(iss.URL(), "http://127.0.0.1") {
		t.Fatalf("the stub issuer is at %s, and the loopback rule is what admits it", iss.URL())
	}
	if _, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{iss.URL()}, Audience: audience,
	}); err != nil {
		t.Fatalf("an http issuer on loopback was refused: %v", err)
	}
	// Off loopback the same scheme needs the variable, and with it the
	// start-up gets as far as reading the issuer: the verifier is built, and
	// what fails is the readiness check against an issuer that is not there
	// rather than the scheme.
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{"http://issuer.example"}, Audience: audience, Insecure: true,
		HTTP: &http.Client{Timeout: 2 * time.Second},
		Log:  slog.New(slog.DiscardHandler), WarmRetry: time.Hour, WarmRetryMax: time.Hour,
	})
	if err != nil {
		t.Fatalf("the scheme was still refused with the variable set: %v", err)
	}
	err = v.Check(t.Context())
	if err == nil {
		t.Fatal("an issuer that does not exist passed the issuers check")
	}
	if strings.Contains(err.Error(), "ARCA_OIDC_INSECURE_ISSUERS") {
		t.Errorf("the scheme was still refused with the variable set: %v", err)
	}
}

// TestTheAudienceDefaults: an empty ARCA_OIDC_AUDIENCE is the default of
// spec 002's table, not an audience of nobody.
func TestTheAudienceDefaults(t *testing.T) {
	iss := issuer(t)
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{iss.URL()}})
	if err != nil {
		t.Fatalf("the verifier would not build: %v", err)
	}
	if v.Audience() != auth.DefaultAudience {
		t.Errorf("the audience is %q, want %q", v.Audience(), auth.DefaultAudience)
	}
	if _, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "9ab3"})); err != nil {
		t.Errorf("a token for the default audience was refused: %v", err)
	}
}

// TestNothingCallsTheIssuerWhileARequestIsServed: the verifier warms at
// start, so the first request a replica serves pays for no discovery and no
// key set. The stub records every request it serves, which is how a test in
// this process proves the count is zero.
func TestNothingCallsTheIssuerWhileARequestIsServed(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss.URL())
	if len(iss.Requests()) == 0 {
		t.Fatal("the verifier started without reading the issuer, so Warm did nothing")
	}
	iss.ResetRequests()
	for range 3 {
		if _, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "9ab3"})); err != nil {
			t.Fatalf("a good token was refused: %v", err)
		}
	}
	if got := iss.Requests(); len(got) != 0 {
		t.Errorf("serving three requests called the issuer %d time(s): %v", len(got), got)
	}
}

// TestMiddlewareAdmitsAndRefuses: the one middleware over /v1. An admitted
// request carries its caller on the context, where the Decide seam reads it;
// a refused one never reaches the handler, and the API writes the envelope
// from the reason the verifier named.
func TestMiddlewareAdmitsAndRefuses(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss.URL())
	var reached bool
	var subject string
	handler := v.Middleware(func(w http.ResponseWriter, _ *http.Request, err error) {
		w.Header().Set("X-Reason", auth.ReasonOf(err))
		w.WriteHeader(http.StatusUnauthorized)
	})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached, subject = true, auth.CallerFrom(r.Context()).Subject
	}))

	r := httptest.NewRequest(http.MethodGet, "/v1/trash", nil)
	r.Header.Set("Authorization", "Bearer "+iss.Mint(issuertest.Claims{Sub: "9ab3"}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !reached {
		t.Fatalf("a good token did not reach the handler: %d", w.Code)
	}
	if want := iss.URL() + "|9ab3"; subject != want {
		t.Errorf("the handler read the subject %q, want %q", subject, want)
	}

	reached = false
	w = httptest.NewRecorder()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/trash", nil))
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/trash", nil))
	if reached {
		t.Error("a request with no bearer reached the handler")
	}
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Reason") != auth.ReasonMissing {
		t.Errorf("the refusal is %d with the reason %q", w.Code, w.Header().Get("X-Reason"))
	}
}

// TestAWarmFailureNamesTheVariable: an issuer that answers its discovery
// document and then goes away leaves a verifier that is built and not ready.
// The failure is on the readiness check named issuers, where it names
// ARCA_OIDC_ISSUERS, and not on the start-up: a replica that exited here
// would crash-loop through an outage its own configuration is right about.
func TestAWarmFailureNamesTheVariable(t *testing.T) {
	iss := issuer(t)
	url := iss.URL()
	iss.Close()
	v, err := auth.NewVerifier(context.Background(), auth.VerifierOptions{
		Issuers: []string{url}, Audience: audience, HTTP: &http.Client{Timeout: 2 * time.Second},
		Log: slog.New(slog.DiscardHandler), WarmRetry: time.Hour, WarmRetryMax: time.Hour,
	})
	if err != nil {
		t.Fatalf("an issuer that is not there stopped the verifier being built: %v", err)
	}
	err = v.Check(context.Background())
	if err == nil {
		t.Fatal("the issuers check passes against an issuer that is not there")
	}
	if !strings.Contains(err.Error(), "ARCA_OIDC_ISSUERS") {
		t.Errorf("the check reports %q and does not name the variable", err)
	}
}

// tamper flips one character of a token's signature, which is the one way to
// produce a well-formed token no key could have signed.
func tamper(token string) string {
	i := strings.LastIndex(token, ".")
	sig := token[i+1:]
	swapped := "A"
	if strings.HasPrefix(sig, "A") {
		swapped = "B"
	}
	return token[:i+1] + swapped + sig[1:]
}
