// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/health"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// publicBase is ARCA_PUBLIC_URL in a test: the address clients would reach
// this installation at, which is not the socket a case happens to bind.
const publicBase = "http://127.0.0.1:8080"

// env is the environment a case runs arcad in: the variables spec 002's
// table marks required, which the binary refuses to start without, plus
// whatever the case sets. A case overrides a required variable by naming it,
// and clears one by naming it empty.
//
// The issuer is a name that resolves to nothing, which is right for every
// case that never reaches the verifier. A case that starts the server uses
// serving instead, which stands one up.
func env(m map[string]string) func(string) string {
	base := map[string]string{
		"ARCA_PUBLIC_URL":   publicBase,
		"ARCA_OIDC_ISSUERS": "https://issuer.example",
	}
	maps.Copy(base, m)
	return func(k string) string { return base[k] }
}

// serving is the environment of a case that starts the server: the same
// table with a stub issuer behind it, because arcad reads every issuer's
// key set at start and refuses to start on one that does not answer.
func serving(t *testing.T, m map[string]string) func(string) string {
	t.Helper()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	return env(merge(map[string]string{"ARCA_OIDC_ISSUERS": iss.URL()}, m))
}

// merge is one map over another, for a case that adds to a base.
func merge(base, extra map[string]string) map[string]string {
	maps.Copy(base, extra)
	return base
}

func TestVersionFlagPrintsTheIdentityAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "arcad dev (") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"frobnicate"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), `unknown subcommand "frobnicate"`) {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"serve", "-no-such-flag"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestSubcommandSplitsAroundTheFirstBareWord(t *testing.T) {
	name, rest := subcommand([]string{"-a", "serve", "-b"})
	if name != "serve" || strings.Join(rest, " ") != "-a -b" {
		t.Fatalf("subcommand() = %q, %q", name, rest)
	}
	if name, rest := subcommand([]string{"-version"}); name != "" || len(rest) != 1 {
		t.Fatalf("subcommand() = %q, %q", name, rest)
	}
}

func TestBadConfigurationExitsOneWithOneLine(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"ARCA_PUBLIC_ADDR": "nope"}), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "arcad: configuration: ") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q", got)
	}
}

func TestOccupiedAddressExitsOne(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	for _, tc := range []struct{ name, public, internal string }{
		{"public", ln.Addr().String(), "127.0.0.1:0"},
		{"internal", "127.0.0.1:0", ln.Addr().String()},
	} {
		var errOut bytes.Buffer
		code := run(t.Context(), nil, serving(t, map[string]string{
			"ARCA_PUBLIC_ADDR":   tc.public,
			"ARCA_INTERNAL_ADDR": tc.internal,
		}), io.Discard, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "address already in use") {
			t.Fatalf("%s: exit %d, stderr %q", tc.name, code, errOut.String())
		}
	}
}

// syncBuffer lets the test read stdout while serve still writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// startServe runs serve on loopback ports and returns the two base URLs
// and a stop function that cancels the context and returns the exit code.
func startServe(t *testing.T, extra ...map[string]string) (publicURL, internalURL, line string, stop func() int) {
	t.Helper()
	settings := map[string]string{
		"ARCA_PUBLIC_ADDR":   "127.0.0.1:0",
		"ARCA_INTERNAL_ADDR": "127.0.0.1:0",
	}
	for _, m := range extra {
		settings = merge(settings, m)
	}
	getenv := serving(t, settings)
	ctx, cancel := context.WithCancel(t.Context())
	var out syncBuffer
	var errOut bytes.Buffer
	codec := make(chan int, 1)
	go func() {
		codec <- run(ctx, nil, getenv, &out, &errOut)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			return "http://" + m[1], "http://" + m[2], out.String(), func() int {
				cancel()
				select {
				case code := <-codec:
					return code
				case <-time.After(gracePeriod + 10*time.Second):
					t.Fatal("serve did not stop")
					return -1
				}
			}
		}
		select {
		case code := <-codec:
			t.Fatalf("serve exited %d before listening; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never reported its listeners; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServeAnswersTheProbesOnBothListenersAndStopsCleanly(t *testing.T) {
	publicURL, internalURL, _, stop := startServe(t)

	for _, base := range []string{publicURL, internalURL} {
		for _, p := range []string{"/livez", "/readyz"} {
			if code, body := get(t, base+p); code != 200 || body != "ok\n" {
				t.Errorf("GET %s%s = %d %q", base, p, code, body)
			}
		}
		if code, body := get(t, base+"/version"); code != 200 || !strings.Contains(body, `"version":"dev"`) {
			t.Errorf("GET %s/version = %d %q", base, code, body)
		}
	}
	if code, body := get(t, publicURL+"/"); code != 200 || !strings.HasPrefix(body, "arcad dev (") {
		t.Errorf("GET / = %d %q", code, body)
	}
	if code, _ := get(t, publicURL+"/metrics"); code != 404 {
		t.Errorf("GET /metrics on the public listener = %d, want 404 until a later spec mounts it", code)
	}

	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestReadinessFailsOnceDrainingBegins(t *testing.T) {
	draining := make(chan struct{})
	check := notDraining(draining)
	if err := check(t.Context()); err != nil {
		t.Fatalf("before draining: %v", err)
	}
	close(draining)
	if err := check(t.Context()); err == nil || err.Error() != "shutting down" {
		t.Fatalf("after draining: %v", err)
	}
}

func TestSleepCtxReturnsEarlyWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Minute)
	if time.Since(start) > time.Second {
		t.Fatal("sleepCtx waited for the timer despite a cancelled context")
	}
}

// TestTheSurfaceIsMountedOnThePublicListener: spec 013's /v1 and its
// document answer beside the probes of spec 002, and the internal listener
// carries neither.
func TestTheSurfaceIsMountedOnThePublicListener(t *testing.T) {
	publicURL, internalURL, _, stop := startServe(t)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()

	// Every route under /v1 but the three link routes runs behind the
	// verifier, so a request with no bearer is refused before it is routed.
	if code, body := get(t, publicURL+"/v1/trash"); code != 401 || !strings.Contains(body, `"unauthenticated"`) {
		t.Errorf("GET /v1/trash = %d %q, want 401 in the error envelope", code, body)
	}
	// The three public link routes take no bearer and answer the frame's
	// not_implemented until spec 008 lands their behaviour.
	if code, body := get(t, publicURL+"/v1/shares/links/tkn"); code != 501 || !strings.Contains(body, `"not_implemented"`) {
		t.Errorf("GET /v1/shares/links/{token} = %d %q, want 501", code, body)
	}
	// The document takes no token: a route name is not a secret.
	code, body := get(t, publicURL+"/openapi.json")
	if code != 200 || !strings.Contains(body, `"openapi"`) {
		t.Errorf("GET /openapi.json = %d %q", code, body)
	}
	// The document names ARCA_PUBLIC_URL, the address clients reach this
	// installation at, and not the socket this replica happens to be bound to.
	if !strings.Contains(body, `"url": "`+publicBase+`"`) {
		t.Errorf("the document does not name ARCA_PUBLIC_URL, %s, as its server", publicBase)
	}
	// The internal listener is the cluster's probes and nothing else.
	if code, _ := get(t, internalURL+"/v1/trash"); code != 404 {
		t.Errorf("GET /v1/trash on the internal listener = %d, want 404", code)
	}
	if code, _ := get(t, internalURL+"/openapi.json"); code != 404 {
		t.Errorf("GET /openapi.json on the internal listener = %d, want 404", code)
	}
}

// TestTheStartLineNamesWhoDecides: an operator reads whether the endpoint
// they configured was picked up, or whether the owner policy is deciding.
func TestTheStartLineNamesWhoDecides(t *testing.T) {
	_, _, started, stop := startServe(t)
	defer stop()
	// The line is what startServe read the listeners from, so reaching here
	// means it was written; the mode is the part this case reads.
	if !strings.Contains(started, string(auth.ModeOwnerPolicy)) {
		t.Errorf("the start line is %q and does not name the owner policy", started)
	}
}

// TestReadinessAsksTheAuthorizerWhenOneIsConfigured is the check named
// authorizer of spec 006: a replica whose endpoint is out of reach leaves
// rotation rather than answering 503 to every request it is sent.
func TestReadinessAsksTheAuthorizerWhenOneIsConfigured(t *testing.T) {
	t.Run("an endpoint that answers", func(t *testing.T) {
		endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		_, internalURL, started, stop := startServe(t, map[string]string{
			"ARCA_AUTHORIZER_URL": endpoint.URL(), "ARCA_AUTHORIZER_TOKEN": endpoint.Token(),
		})
		defer stop()
		if code, body := get(t, internalURL+"/readyz"); code != 200 || body != "ok\n" {
			t.Errorf("GET /readyz = %d %q with a conforming endpoint", code, body)
		}
		if !strings.Contains(started, string(auth.ModeAuthorizer)) {
			t.Errorf("the start line is %q and does not name the endpoint", started)
		}
	})
	t.Run("an endpoint that does not answer", func(t *testing.T) {
		endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		endpoint.Fail(http.StatusBadGateway)
		_, internalURL, _, stop := startServe(t, map[string]string{
			"ARCA_AUTHORIZER_URL": endpoint.URL(), "ARCA_AUTHORIZER_TOKEN": endpoint.Token(),
		})
		defer stop()
		code, body := get(t, internalURL+"/readyz")
		if code != 503 {
			t.Fatalf("GET /readyz = %d %q with an endpoint that does not answer", code, body)
		}
		if !strings.Contains(body, "authorizer") {
			t.Errorf("the body is %q and does not name the check that failed", body)
		}
	})
}

// TestReadinessHasNoAuthorizerCheckWithoutOne: with no endpoint configured
// the owner policy decides in process, and a check of it would be a check of
// this binary against itself.
func TestReadinessHasNoAuthorizerCheckWithoutOne(t *testing.T) {
	policy := &auth.Identity{Mode: auth.ModeOwnerPolicy}
	if got := readiness(policy, make(chan struct{})); len(got) != 1 || got[0].Name != "draining" {
		t.Errorf("the owner policy's checks are %v", names(got))
	}
	asking := &auth.Identity{Mode: auth.ModeAuthorizer, Authorizer: auth.NewAuthorizer(denyAll{})}
	got := readiness(asking, make(chan struct{}))
	if len(got) != 2 || got[0].Name != "draining" || got[1].Name != "authorizer" {
		t.Fatalf("the endpoint's checks are %v", names(got))
	}
	if err := got[1].Run(t.Context()); err != nil {
		t.Errorf("a conforming endpoint failed the check: %v", err)
	}
}

// names is the names of a set of checks, for a message.
func names(checks []health.Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name
	}
	return out
}

// denyAll is a conforming authorizer: it denies the probe, like every
// authorizer of the family.
type denyAll struct{}

func (denyAll) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Reason: "the probe id is reserved"}, nil
}

// TestABadDeploymentOfSpec006ExitsOne: an issuer that does not answer and an
// endpoint with no bearer are start-up failures naming their variable, so a
// deployment is fixed rather than left answering 401 or 503 to everything.
func TestABadDeploymentOfSpec006ExitsOne(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		mustSay string
	}{
		{"an issuer that does not answer", map[string]string{
			"ARCA_OIDC_ISSUERS": "https://issuer.invalid",
		}, "ARCA_OIDC_ISSUERS"},
		{"an endpoint with no bearer", map[string]string{
			"ARCA_AUTHORIZER_URL": "https://authz.example/decide",
		}, "ARCA_AUTHORIZER_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var errOut bytes.Buffer
			code := run(t.Context(), nil, serving(t, c.env), io.Discard, &errOut)
			if code != 1 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(errOut.String(), c.mustSay) {
				t.Errorf("stderr is %q and does not name %q", errOut.String(), c.mustSay)
			}
		})
	}
}
