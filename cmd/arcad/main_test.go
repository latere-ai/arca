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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/arca/internal/store"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// stores is the environment of a server that starts: the variables every
// spec so far marks required, with the bucket pointing at an endpoint that
// answers the readiness probe. The e2e tier of spec 014 runs the same server
// against a real store.
func stores(t *testing.T, overrides map[string]string) func(string) string {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(endpoint.Close)
	m := map[string]string{
		"ARCA_DATABASE_URL":      "postgres://arca:arca@127.0.0.1:1/arca?sslmode=disable",
		"ARCA_BUCKET":            "arca",
		"ARCA_BUCKET_REGION":     "us-east-1",
		"ARCA_BUCKET_ENDPOINT":   endpoint.URL,
		"ARCA_BUCKET_PATH_STYLE": "true",
		"ARCA_BUCKET_ACCESS_KEY": "key",
		"ARCA_BUCKET_SECRET_KEY": "secret",
	}
	maps.Copy(m, overrides)
	return env(m)
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
func startServe(t *testing.T, overrides map[string]string) (publicURL, internalURL string, stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var out syncBuffer
	var errOut bytes.Buffer
	codec := make(chan int, 1)
	go func() {
		addresses := map[string]string{"ARCA_PUBLIC_ADDR": "127.0.0.1:0", "ARCA_INTERNAL_ADDR": "127.0.0.1:0"}
		maps.Copy(addresses, overrides)
		codec <- run(ctx, nil, stores(t, addresses), &out, &errOut)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			return "http://" + m[1], "http://" + m[2], func() int {
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
	publicURL, internalURL, stop := startServe(t, nil)

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

	publicURL, _, stop := startServe(t, map[string]string{"ARCA_BUCKET_ENDPOINT": "http://" + address})
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
