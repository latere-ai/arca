// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/health"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/reaper"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/internal/workspaces"
)

// publicBase is ARCA_PUBLIC_URL in a test: the address clients would reach
// this installation at, which is not the socket a case happens to bind.
const publicBase = "http://127.0.0.1:8080"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// stores is the environment of a server that starts: the variables every
// spec so far marks required, with the bucket pointing at an endpoint that
// answers the readiness probe and a stub issuer behind ARCA_OIDC_ISSUERS,
// because arcad reads every issuer's key set at start and refuses to start
// on one that does not answer (spec 006). The e2e tier of spec 014 runs the
// same server against a real store and the stub of spec 014.
func stores(t *testing.T, overrides map[string]string) func(string) string {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(endpoint.Close)
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	m := map[string]string{
		"ARCA_DATABASE_URL":      "postgres://arca:arca@127.0.0.1:1/arca?sslmode=disable",
		"ARCA_BUCKET":            "arca",
		"ARCA_BUCKET_REGION":     "us-east-1",
		"ARCA_BUCKET_ENDPOINT":   endpoint.URL,
		"ARCA_BUCKET_PATH_STYLE": "true",
		"ARCA_BUCKET_ACCESS_KEY": "key",
		"ARCA_BUCKET_SECRET_KEY": "secret",
		"ARCA_PUBLIC_URL":        publicBase,
		"ARCA_OIDC_ISSUERS":      iss.URL(),
	}
	maps.Copy(m, overrides)
	return env(m)
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
		code := run(t.Context(), nil, stores(t, map[string]string{
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
	getenv := stores(t, settings)
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
		if code, body := get(t, base+"/livez"); code != 200 || body != "ok\n" {
			t.Errorf("GET %s/livez = %d %q", base, code, body)
		}
		// Readiness reaches the two stores, and the unit tier has neither
		// beside it, so what it proves here is that the checks are
		// registered and that a failure names the one that failed. The e2e
		// tier of spec 014 proves the 200 against a real bucket and a real
		// database.
		if code, body := get(t, base+"/readyz"); code != 503 || !strings.HasPrefix(body, "not ready: database: ") {
			t.Errorf("GET %s/readyz = %d %q", base, code, body)
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

// TestReadinessNamesTheStoreItCannotReach is what an operator reads when a
// bucket is misconfigured: the probe says which check failed, in the
// developer's register, and the process keeps serving so the answer is
// readable at all.
func TestReadinessNamesTheStoreItCannotReach(t *testing.T) {
	unreachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := unreachable.Addr().String()
	_ = unreachable.Close()

	publicURL, _, _, stop := startServe(t, map[string]string{"ARCA_BUCKET_ENDPOINT": "http://" + address})
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()
	if code, body := get(t, publicURL+"/livez"); code != 200 {
		t.Errorf("GET /livez = %d %q; liveness touches no dependency", code, body)
	}
	code, body := get(t, publicURL+"/readyz")
	if code != 503 || !strings.Contains(body, "bucket: ") {
		t.Fatalf("GET /readyz = %d %q", code, body)
	}
}

// TestTheServerRefusesToStartAgainstADatabaseBehindIt is criterion 2 of spec
// 004: a deploy whose migration job did not run fails at once rather than
// serving against a schema it does not have.
func TestTheServerRefusesToStartAgainstADatabaseBehindIt(t *testing.T) {
	behind := pendingMigrations
	t.Cleanup(func() { pendingMigrations = behind })
	pendingMigrations = func(context.Context, store.Querier) ([]string, error) {
		return []string{"0002_uploads.up.sql", "0003_shares.up.sql"}, nil
	}
	var errOut bytes.Buffer
	code := run(t.Context(), nil, stores(t, map[string]string{
		"ARCA_PUBLIC_ADDR":   "127.0.0.1:0",
		"ARCA_INTERNAL_ADDR": "127.0.0.1:0",
	}), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.Contains(got, "0002_uploads.up.sql is not applied") || !strings.Contains(got, "arcad migrate") {
		t.Fatalf("stderr = %q", got)
	}
}

// TestReadinessCarriesTheSchemaCheckWhenTheDatabaseArrivesLate is the other
// half: a database that was unreachable at start-up is compared against the
// embedded set by the readiness check instead.
func TestReadinessCarriesTheSchemaCheckWhenTheDatabaseArrivesLate(t *testing.T) {
	behind := pendingMigrations
	t.Cleanup(func() { pendingMigrations = behind })
	unreachable := true
	pendingMigrations = func(context.Context, store.Querier) ([]string, error) {
		if unreachable {
			return nil, errors.New("the database is away")
		}
		return []string{"0002_uploads.up.sql"}, nil
	}
	db, err := store.Open(t.Context(), "postgres://arca@127.0.0.1:1/arca?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := databaseReady(db)(t.Context()); err == nil || !strings.Contains(err.Error(), "ping") {
		t.Fatalf("a database that does not answer = %v", err)
	}
	// Once the database answers, the ping passes and the comparison is what
	// is left, so the schema half is exercised on its own.
	if err := schemaReady(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "the database is away") {
		t.Fatalf("a database that does not answer the version = %v", err)
	}
	unreachable = false
	err = schemaReady(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "0002_uploads.up.sql is not applied") {
		t.Fatalf("a database behind the binary = %v", err)
	}
	pendingMigrations = func(context.Context, store.Querier) ([]string, error) { return nil, nil }
	if err := schemaReady(t.Context(), nil); err != nil {
		t.Fatalf("a database that holds every migration = %v", err)
	}
}

func TestMigrateAppliesWhatIsPendingAndSaysSo(t *testing.T) {
	applied := applyMigrations
	t.Cleanup(func() { applyMigrations = applied })

	var url string
	applyMigrations = func(databaseURL string) error { url = databaseURL; return nil }
	var out bytes.Buffer
	code := run(t.Context(), []string{"migrate"}, env(map[string]string{
		"ARCA_DATABASE_URL": "postgres://arca@db/arca",
	}), &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if url != "postgres://arca@db/arca" {
		t.Errorf("the migrator was given %q", url)
	}
	if !strings.Contains(out.String(), "every migration this binary carries") {
		t.Errorf("stdout = %q", out.String())
	}

	applyMigrations = func(string) error { return errors.New("the database is away") }
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env(map[string]string{
		"ARCA_DATABASE_URL": "postgres://arca@db/arca",
	}), io.Discard, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(errOut.String(), "arcad: ") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestMigrateReadsTheDatabaseVariableAndNothingElse(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env(nil), io.Discard, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.Contains(got, "ARCA_DATABASE_URL is unset") || strings.Contains(got, "ARCA_BUCKET") {
		t.Fatalf("stderr = %q; migrate reads the database variable and nothing else", got)
	}
	if code := run(t.Context(), []string{"migrate", "-no-such-flag"}, env(nil), io.Discard, io.Discard); code != 2 {
		t.Fatalf("a bad flag exited %d", code)
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
		// Readiness also reaches the two stores, and the unit tier has
		// neither, so the probe is 503 on the database whatever the
		// endpoint answers. What this case reads is that the authorizer is
		// not among the checks that failed. The e2e tier of spec 014 proves
		// the 200 against a real bucket, a real database, and the stub.
		code, body := get(t, internalURL+"/readyz")
		if code != 503 || strings.Contains(body, "authorizer") {
			t.Errorf("GET /readyz = %d %q; a conforming endpoint is not a failed check", code, body)
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
	// The two store checks are the merged set's, and neither is run here.
	reachable := func(context.Context) error { return nil }
	stores := []string{"draining", "bucket", "database"}

	policy := &auth.Identity{Mode: auth.ModeOwnerPolicy}
	if got := readiness(policy, make(chan struct{}), reachable, reachable); !reflect.DeepEqual(names(got), stores) {
		t.Errorf("the owner policy's checks are %v, want %v", names(got), stores)
	}
	asking := &auth.Identity{Mode: auth.ModeAuthorizer, Authorizer: auth.NewAuthorizer(denyAll{})}
	got := readiness(asking, make(chan struct{}), reachable, reachable)
	if !reflect.DeepEqual(names(got), append(stores, "authorizer")) {
		t.Fatalf("the endpoint's checks are %v", names(got))
	}
	if err := got[3].Run(t.Context()); err != nil {
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
			code := run(t.Context(), nil, stores(t, c.env), io.Discard, &errOut)
			if code != 1 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(errOut.String(), c.mustSay) {
				t.Errorf("stderr is %q and does not name %q", errOut.String(), c.mustSay)
			}
		})
	}
}

// Criterion 20 of spec 010: ARCA_REAP_INTERVAL of 0 leaves serve with no
// reconciliation loop. Which it is, is a fact an operator reads on the
// start-up line rather than infers from a metric that never moves.
func TestServeSaysWhetherThisReplicaReconciles(t *testing.T) {
	for _, c := range []struct{ interval, line string }{
		{"", "the reconciler runs every 5m0s"},
		{"90s", "the reconciler runs every 1m30s"},
		{"0", "the reconciler is off on this replica"},
	} {
		t.Run(c.line, func(t *testing.T) {
			out, stop := serveReading(t, map[string]string{"ARCA_REAP_INTERVAL": c.interval})
			defer func() {
				if code := stop(); code != 0 {
					t.Fatalf("exit %d", code)
				}
			}()
			if !strings.Contains(out.String(), c.line) {
				t.Fatalf("the start-up said %q", out.String())
			}
		})
	}
}

// serveReading runs serve on loopback ports and answers its stdout and a
// stop function, for a case that reads what start-up printed.
func serveReading(t *testing.T, overrides map[string]string) (*syncBuffer, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out, errOut := &syncBuffer{}, &bytes.Buffer{}
	codec := make(chan int, 1)
	go func() {
		addresses := map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:0", "ARCA_INTERNAL_ADDR": "127.0.0.1:0"}
		maps.Copy(addresses, overrides)
		codec <- run(ctx, nil, stores(t, addresses), out, errOut)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !listening.MatchString(out.String()) {
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
	return out, func() int {
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

func TestReapIsASubcommandWithTwoFlags(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"reap", "-no-such-flag"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("a bad flag exited %d", code)
	}
	// Zero is the value that turns the in-process loop off, and a process
	// whose whole job is that loop cannot take it: exiting 0 having done
	// nothing is how a CronJob looks healthy while nothing is reconciled.
	errOut.Reset()
	code := run(t.Context(), []string{"reap"}, stores(t, map[string]string{"ARCA_REAP_INTERVAL": "0"}), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "ARCA_REAP_INTERVAL is 0") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	// A sequence that could not reach a store is a failure and not a quiet
	// success. A sequence that reaches both is the e2e tier's.
	errOut.Reset()
	code = run(t.Context(), []string{"reap", "-once"}, stores(t, nil), io.Discard, &errOut)
	if code != 1 || !strings.HasPrefix(errOut.String(), "arcad: ") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	// And a configuration it cannot read is exit 1 with one line.
	errOut.Reset()
	if code := run(t.Context(), []string{"reap", "-once"}, env(nil), io.Discard, &errOut); code != 1 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestReapAsALoopRunsUntilItIsStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, errOut bytes.Buffer
	code := run(ctx, []string{"reap"}, stores(t, map[string]string{"ARCA_REAP_INTERVAL": "1h"}), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "the reconciler runs every 1h0m0s") {
		t.Fatalf("stdout = %q", out.String())
	}
	// A pass that does not run reports nothing, and a findings table with no
	// row for it reads like a pass that found nothing. This process runs no
	// lease expiry, and it says so where an operator reads the rest.
	if !strings.Contains(out.String(), "nine of the ten passes") {
		t.Errorf("stdout is %q and does not say which pass this process leaves to a replica", out.String())
	}
}

func TestTheReconcilerIsNotStartedOnAConfigurationItCannotRun(t *testing.T) {
	var out bytes.Buffer
	if err := startReaper(t.Context(), config.Config{ReapInterval: time.Minute}, nil, nil, nil, &out); err == nil {
		t.Fatal("a reconciler with no stores was started anyway")
	}
}

func TestOneSequenceReportsWhatItFound(t *testing.T) {
	var f reaper.Findings
	f.Add(reaper.KindUsageCorrected, reaper.Repaired, 2)

	var live, dry bytes.Buffer
	report(&live, f, false)
	report(&dry, f, true)
	if !strings.Contains(live.String(), "usage_corrected") || !strings.Contains(live.String(), "repaired") {
		t.Fatalf("a live run printed %q", live.String())
	}
	if strings.Contains(live.String(), "nothing was changed") {
		t.Fatalf("a live run printed %q", live.String())
	}
	if !strings.Contains(dry.String(), "nothing was changed") {
		t.Fatalf("a dry run printed %q", dry.String())
	}
}

// The two seams the node binds: the log and the usage counter of spec 010
// under the workspaces of spec 009, and the lease sweep of spec 009 under
// the reconciler of spec 010. What each package does behind its seam is its
// own tests'; what is proved here is the binding.

// recordingLog is the log of spec 010 with the appends kept.
type recordingLog struct {
	appended []events.Event
	err      error
}

func (l *recordingLog) Append(_ context.Context, _ store.Querier, e events.Event) (int64, error) {
	if l.err != nil {
		return 0, l.err
	}
	l.appended = append(l.appended, e)
	return int64(len(l.appended)), nil
}

func (l *recordingLog) Note(ctx context.Context, q store.Querier, e events.Event) {
	_, _ = l.Append(ctx, q, e)
}

func (*recordingLog) Tail(context.Context, store.Querier, events.Query) (events.Page, error) {
	return events.Page{}, nil
}

func (*recordingLog) Prune(context.Context, store.Querier, time.Time) (int64, error) { return 0, nil }

func (*recordingLog) Older(context.Context, store.Querier, time.Time) (int64, error) { return 0, nil }

// recordingLedger is the usage counter of spec 010 with the releases kept.
type recordingLedger struct {
	released map[string]int64
	err      error
}

func (l *recordingLedger) Release(_ context.Context, _ store.Querier, owner string, bytes int64) (int64, error) {
	if l.err != nil {
		return 0, l.err
	}
	if l.released == nil {
		l.released = map[string]int64{}
	}
	l.released[owner] += bytes
	return l.released[owner], nil
}

func (*recordingLedger) Charge(context.Context, store.Querier, string, int64, events.Limit) (int64, error) {
	return 0, nil
}

func (*recordingLedger) Read(context.Context, store.Querier, string) (int64, error) { return 0, nil }

func (*recordingLedger) Recompute(context.Context, store.Querier, string) (int64, error) {
	return 0, nil
}

func (*recordingLedger) Correct(context.Context, store.Querier, string, int64, int64) (bool, error) {
	return false, nil
}

func (*recordingLedger) Spaces(context.Context, store.Querier, string, int) ([]events.Space, error) {
	return nil, nil
}

// TestEveryActionAWorkspaceAppendsIsOneOfSpec010sVocabulary: the column
// carries no constraint and the append refuses a word the table does not
// name, so a word spelled one way in spec 009's package and another in spec
// 010's would be a refused transaction on a live installation. The five are
// held to the table here, where the two spellings meet.
func TestEveryActionAWorkspaceAppendsIsOneOfSpec010sVocabulary(t *testing.T) {
	for _, action := range []string{
		workspaces.ActionAttach, workspaces.ActionRelease, workspaces.ActionSync,
		workspaces.ActionReap, workspaces.ActionRestore,
	} {
		if !events.Action(action).Valid() {
			t.Errorf("a workspace appends %q, which spec 010's vocabulary does not name", action)
		}
	}
}

// TestTheWorkspaceLedgerWritesThroughTheLogAndTheCounter is the binding
// itself: an event reaches the log as a row of spec 010's shape, and the
// bytes a sync dropped reach the space's counter.
func TestTheWorkspaceLedgerWritesThroughTheLogAndTheCounter(t *testing.T) {
	log, usage := &recordingLog{}, &recordingLedger{}
	bound := ledger{log: log, usage: usage}

	event := workspaces.Event{
		Owner: "https://issuer.example|9ab3", Path: "workspaces/build/",
		Action: workspaces.ActionSync, Actor: "https://issuer.example|9ab3",
		Detail: map[string]any{"synced_files": 2},
	}
	if err := bound.Append(t.Context(), nil, event); err != nil {
		t.Fatalf("the append = %v", err)
	}
	if len(log.appended) != 1 {
		t.Fatalf("the log holds %d rows", len(log.appended))
	}
	got := log.appended[0]
	if got.Owner != event.Owner || got.Path != event.Path || got.Actor != event.Actor ||
		got.Action != events.ActionSync || !reflect.DeepEqual(got.Detail, event.Detail) {
		t.Errorf("the row is %+v", got)
	}

	if err := bound.Release(t.Context(), nil, event.Owner, 4096); err != nil {
		t.Fatalf("the release = %v", err)
	}
	if usage.released[event.Owner] != 4096 {
		t.Errorf("the counter gave back %d bytes", usage.released[event.Owner])
	}

	// A failure on either half is the caller's: both run inside the
	// transaction of the mutation they record, and a number that cannot be
	// written is a number that stops being current.
	failure := errors.New("the store said no")
	broken := ledger{log: &recordingLog{err: failure}, usage: &recordingLedger{err: failure}}
	if err := broken.Append(t.Context(), nil, event); !errors.Is(err, failure) {
		t.Errorf("a failed append = %v", err)
	}
	if err := broken.Release(t.Context(), nil, event.Owner, 1); !errors.Is(err, failure) {
		t.Errorf("a failed release = %v", err)
	}
}

// failingStore is the transaction seam of spec 009 and the querier under it,
// answering every statement with one failure. It is what proves a call
// reached the store, rather than what the store said.
type failingStore struct{ err error }

func (s failingStore) Querier() store.Querier { return s }

func (s failingStore) Tx(_ context.Context, fn func(store.Querier) error) error { return fn(s) }

func (s failingStore) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, s.err
}

func (s failingStore) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, s.err }

func (s failingStore) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingRow{fault: s.err}
}

// failingRow is the one row such a querier answers.
type failingRow struct{ fault error }

func (r failingRow) Scan(...any) error { return r.fault }

// TestTheLeaseSweepRunsOnALiveRunAndNeverOnADryOne is the other binding, and
// the one property a dry run rests on: pass 3 writes on every statement it
// issues, so a dry run reaches no store at all. The nil service is the
// proof: a dry sweep that called through would panic on it.
func TestTheLeaseSweepRunsOnALiveRunAndNeverOnADryOne(t *testing.T) {
	n, err := leasePass{}.Sweep(t.Context(), nil, time.Now(), true)
	if n != 0 || err != nil {
		t.Fatalf("a dry sweep = %d, %v", n, err)
	}

	failure := errors.New("the store said no")
	service, err := workspaces.New(workspaces.Options{
		DB: failingStore{err: failure}, Workspaces: store.NewWorkspaces(),
		Attachments: store.NewAttachments(), Objects: store.NewWorkspaceObjects(),
		Bucket:     blob.NewMemory(),
		Authorizer: auth.NewAuthorizer(&auth.OwnerPolicy{}),
	})
	if err != nil {
		t.Fatalf("the service would not build: %v", err)
	}
	if _, err := (leasePass{service: service}).Sweep(t.Context(), nil, time.Now(), false); !errors.Is(err, failure) {
		t.Errorf("a live sweep = %v", err)
	}
}
