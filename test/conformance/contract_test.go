// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The conformance tier of spec 017: the suite of this package driven against
// an installation, and the proof that it catches a server that answers spec
// 013 wrong.
//
// The tag is the tiers tag of spec 014, which is what every tier of this
// repository carries and what `make test-conformance` and the release
// pipeline pass. Spec 017 wrote e2e; the tree settled on one tag for every
// tier before that spec was built, and a second tag would be a second way to
// say the same thing.
//
// Nothing here is in the package a consumer imports: the files that carry
// Run are untagged and reach no helper of this repository's test tree, so
// `go get latere.ai/x/arca` and an import of test/conformance pull in the
// suite and none of this.
package conformance_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/test/conformance"
	"latere.ai/x/arca/test/stubs/authorizer"
	"latere.ai/x/arca/test/stubs/issuer"
)

// The flags the tier is pointed at a target with. Each falls back to the
// variable the release pipeline and the Makefile set, so a run is one
// command with either.
var (
	flagURL        = flag.String("url", os.Getenv("ARCA_TEST_URL"), "the base URL of the installation, without /v1")
	flagIssuer     = flag.String("issuer", os.Getenv("ARCA_TEST_ISSUER_URL"), "a stub issuer to mint every subject at; empty uses -token and -token-bob")
	flagToken      = flag.String("token", os.Getenv("ARCA_TEST_TOKEN"), "a bearer for the first principal, for a target with no stub issuer")
	flagTokenBob   = flag.String("token-bob", os.Getenv("ARCA_TEST_TOKEN_BOB"), "a bearer for the second principal")
	flagAdmin      = flag.String("admin", os.Getenv("ARCA_TEST_ADMIN"), "the subject the target treats as an administrator; empty skips the administration group")
	flagAuthorizer = flag.String("authorizer", os.Getenv("ARCA_TEST_AUTHORIZER_URL"), "the stub authorizer's control URL; empty skips the deny, outage and byte limit cases")
	flagAnonymous  = flag.Bool("anonymous", os.Getenv("ARCA_TEST_ANONYMOUS") != "", "the target serves public links to an unauthenticated caller")
	// The bucket is the one host the suite reaches that the target names
	// rather than serves, and a presigned URL is signed over that name. A
	// run from outside the target's network sets this to where the same
	// store is reached from here; a run beside the target leaves it empty.
	flagS3Endpoint = flag.String("s3-endpoint", os.Getenv("ARCA_TEST_S3_ENDPOINT"), "the address the bucket is reached at from here, when the target signs presigned URLs for another; empty dials them as given")
)

// skipWithoutATarget is the remediation the tier prints when nothing was
// pointed at. It names the flag and the command that starts a target.
const skipWithoutATarget = "set -url or ARCA_TEST_URL (make test-conformance brings one up)"

// defaultAdmin is the subject an installation this tier starts itself is
// told to treat as an administrator, when -admin names no other. The
// administration group of spec 017 drives the two routes of spec 012 as this
// principal, and a run that named none skipped the group.
const defaultAdmin = "admin"

// admin answers the subject the run's administrator is, which is one value
// read in two places: the token the suite mints, and the installation this
// tier starts.
func admin() string {
	if *flagAdmin != "" {
		return *flagAdmin
	}
	return defaultAdmin
}

// TestContract is the suite against the target. It fails on any case that
// failed, reports what skipped and why, and reports the routes of spec 013
// the target does not serve, which the pending group has already failed on.
//
// It does not fail on a skip. A target that runs no stub authorizer skips
// the groups that need one, with the reason in the report, and that is a
// property of the target rather than of the server under it; the release
// pipeline's job is what names the skip list a release is allowed.
func TestContract(t *testing.T) {
	report := conformance.Run(t, options(t))

	if len(report.Pending) > 0 {
		t.Logf("the target does not serve %d of the routes of spec 013:\n\t%s",
			len(report.Pending), strings.Join(report.Pending, "\n\t"))
	}
	for _, name := range report.Skipped {
		t.Logf("skipped %s: %s", name, report.Reasons[name])
	}
	for _, what := range report.Unverified {
		t.Logf("unverified: %s", what)
	}
	t.Logf("%d passed, %d failed, %d skipped, %d objects created and deleted",
		len(report.Passed), len(report.Failed), len(report.Skipped), len(report.Created))
	if len(report.Failed) > 0 {
		t.Fatalf("failed: %v", report.Failed)
	}
}

// options builds what the flags describe. A stub issuer mints every subject
// the suite asks for; two fixed tokens serve a released installation, where
// the run is what a consumer's own CI does.
//
// With no -url the tier brings an installation up on the stack, which is
// what `make test-conformance` runs: one command from a clean clone to a
// suite against a real server with the two stubs behind it. With no stack
// either there is nothing to drive, and the tier skips with the remediation.
func options(t *testing.T) conformance.Options {
	t.Helper()
	if *flagURL == "" {
		s, reason := readStack()
		if reason != "" {
			t.Skip(skipWithoutATarget + "; " + reason)
		}
		own := start(t, s)
		return conformance.Options{
			URL:               own.publicURL,
			Admin:             admin(),
			AuthorizerControl: own.authorizerURL,
			Token: func(ctx context.Context, subject string) (string, error) {
				return mint(ctx, own.issuerURL, subject)
			},
		}
	}
	opts := conformance.Options{
		URL:               *flagURL,
		Admin:             *flagAdmin,
		AuthorizerControl: *flagAuthorizer,
		Anonymous:         *flagAnonymous,
		BucketDial:        *flagS3Endpoint,
	}
	switch {
	case *flagIssuer != "":
		opts.Token = func(ctx context.Context, subject string) (string, error) {
			return mint(ctx, *flagIssuer, subject)
		}
	case *flagToken != "":
		fixed := map[string]string{conformance.Alice: *flagToken, conformance.Bob: *flagTokenBob}
		if *flagAdmin != "" {
			fixed[*flagAdmin] = os.Getenv("ARCA_TEST_ADMIN_TOKEN")
		}
		opts.Token = func(_ context.Context, subject string) (string, error) {
			token, held := fixed[subject]
			if !held || token == "" {
				return "", fmt.Errorf("no token for %q; the run was given -token and -token-bob", subject)
			}
			return token, nil
		}
	default:
		t.Fatal("set -issuer or -token: the suite needs a way to mint a bearer for a subject")
	}
	return opts
}

// mint asks the stub issuer of spec 014 for a token of the subject. The body
// is what that stub's mint endpoint takes.
func mint(ctx context.Context, issuer, subject string) (string, error) {
	payload, err := json.Marshal(map[string]string{"sub": subject})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(issuer, "/")+"/mint", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mint at %s: %d %s", issuer, resp.StatusCode, raw)
	}
	var answer struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", err
	}
	if answer.Token == "" {
		return "", fmt.Errorf("mint at %s answered no token: %s", issuer, raw)
	}
	return answer.Token, nil
}

// TestSuiteCatchesADrift is criterion 5 of spec 017. It starts arcad with
// each value of ARCA_TEST_DRIFT against the stack and asserts that the cases
// the drift's answer belongs to fail and that no other case does.
//
// The suite runs in a process of its own rather than through Run here,
// because a case that fails fails the test it was given, and a test that
// wants a failure cannot also be the test that took it. What this asserts is
// therefore what any reader asserts about a suite: it was run, and the run
// reported these cases as failed.
//
// The expectation is a set of case names rather than a single one, because a
// drift is applied where an answer reaches the wire and more than one case
// reads that answer: the codes drift changes one row of spec 013's table,
// and every case that provokes that row sees it. What the criterion asks is
// that the suite catches the drift and reports nothing else as broken, which
// is what an equal set of failures states.
//
// A case whose routes the target does not serve cannot see the drift its
// answer belongs to, so the expectation is narrowed to the cases that ran.
// On a build without the file routes of spec 005 the etag drift expects
// nothing, and the test says so by name rather than passing quietly.
func TestSuiteCatchesADrift(t *testing.T) {
	// It always starts its own servers: a drift is a property of a build and
	// not of a request, so there is no way to ask the installation -url
	// names to answer one.
	stack, reason := readStack()
	if reason != "" {
		t.Skip(reason)
	}

	// The build's pending routes, read once, so a case that waits on one is
	// left out of the expectation rather than expected to see a drift it
	// could not reach.
	pending := pendingRoutes(t, stack)

	for _, tc := range []struct {
		drift  string
		caught map[string][]string
	}{
		{"paths", map[string][]string{
			"008/Ladder":     {"POST /v1/shares"},
			"013/Planes":     {"POST /v1/shares"},
			"013/ErrorTable": {"POST /v1/shares"},
		}},
		{"codes", map[string][]string{
			"009/SlugTaken":  {"POST /v1/workspaces"},
			"013/ErrorTable": {"POST /v1/workspaces"},
		}},
		// The etag drift reaches the validator a route answers, so the
		// cases that read one catch it. The version history is not one of
		// them: it names a checksum in the body of a listing and no
		// validator, so a build whose ETag is not the checksum answers that
		// listing the same way a conforming build does.
		{"etag", map[string][]string{
			"005/PutGetHeadDelete": {"PUT /v1/files/{owner}/{path...}"},
			"005/MoveKeepsTheETag": {"POST /v1/files/{owner}/{path...}"},
			"005/Conditional":      {"PUT /v1/files/{owner}/{path...}"},
		}},
	} {
		t.Run(tc.drift, func(t *testing.T) {
			var want []string
			for name, routes := range tc.caught {
				if reachable(routes, pending) {
					want = append(want, name)
				}
			}
			slices.Sort(want)

			got := failedCases(t, stack, "ARCA_TEST_DRIFT="+tc.drift)
			if len(want) == 0 {
				t.Logf("the %s drift is not observable on this build: every case that reads its answer waits on a route the target does not serve (%d outstanding)",
					tc.drift, len(pending))
				if len(got) > 0 {
					t.Fatalf("the %s drift failed %v, and no case that reads its answer ran", tc.drift, got)
				}
				return
			}
			if !slices.Equal(got, want) {
				t.Fatalf("the %s drift failed %v, want exactly %v", tc.drift, got, want)
			}
		})
	}

	// And the same stack with no drift: every case that runs passes, so the
	// failures above are the drift and not the suite.
	if got := failedCases(t, stack); len(got) > 0 {
		t.Fatalf("a build with no drift failed %v", got)
	}
}

// reachable reports whether every route a case drives is served, so a case
// waiting on a route the pending group reports is left out of what a drift
// is expected to break.
func reachable(routes, pending []string) bool {
	for _, route := range routes {
		if slices.Contains(pending, route) {
			return false
		}
	}
	return true
}

// pendingRoutes answers the routes of spec 013 this build does not serve,
// read from the document it publishes.
func pendingRoutes(t *testing.T, s stack) []string {
	t.Helper()
	target := start(t, s)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.publicURL+"/openapi.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read the served document: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var document struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		t.Fatalf("read the served document: %v", err)
	}
	var out []string
	for _, key := range conformance.Routes() {
		method, path, _ := strings.Cut(key, " ")
		if _, served := document.Paths[strings.ReplaceAll(path, "...", "")][strings.ToLower(method)]; !served {
			out = append(out, key)
		}
	}
	return out
}

// failedCases starts an installation with the extra environment, runs the
// suite against it in a process of its own, and answers the cases that run
// reported as failed, sorted. The pending group is left out: it fails on
// every build that does not serve the whole of spec 013, which is what it is
// for and not what a drift did.
func failedCases(t *testing.T, s stack, extra ...string) []string {
	t.Helper()
	target := start(t, s, extra...)
	run := exec.CommandContext(t.Context(), "go", "test", "-tags=tiers", "-count=1", "-v",
		"-timeout", "10m", "-run", "^TestContract$", ".",
		"-args", "-url", target.publicURL, "-issuer", target.issuerURL,
		"-authorizer", target.authorizerURL)
	// The target is named on the command line, so the child reads no
	// variable of this process that would point it somewhere else.
	run.Env = append(os.Environ(), "ARCA_TEST_URL=", "ARCA_TEST_ISSUER_URL=", "ARCA_TEST_AUTHORIZER_URL=")
	out, err := run.CombinedOutput()
	if err != nil && len(out) == 0 {
		t.Fatalf("run the suite: %v", err)
	}

	// The lines the suite's own report writes: one per failed case, two
	// levels under TestContract, which is <NNN>/<Name>.
	failure := regexp.MustCompile(`--- FAIL: TestContract/(\d{3})/([A-Za-z0-9]+)\s`)
	seen := map[string]bool{}
	for _, m := range failure.FindAllStringSubmatch(string(out), -1) {
		name := m[1] + "/" + m[2]
		if name == "017/Pending" {
			continue
		}
		seen[name] = true
	}
	got := make([]string, 0, len(seen))
	for name := range seen {
		got = append(got, name)
	}
	slices.Sort(got)
	return got
}

// TestConcurrentRuns is criterion 4's second half: two runs against one
// installation touch none of each other's objects. Each draws its own value
// at start and names everything it creates after it, so the two write
// disjoint sets and each deletes exactly what it made.
//
// The authorizer and usage groups are skipped. They are not what the
// criterion is about, and they cannot be concurrent against one target
// anyway: both reach through the stub's control API to one rule table, so
// two runs would be changing each other's verdicts rather than each other's
// objects. Object isolation is the property, and it is the property the two
// skipped groups do not touch.
func TestConcurrentRuns(t *testing.T) {
	stack, reason := readStack()
	if reason != "" {
		t.Skip(reason)
	}
	target := start(t, stack)
	opts := func() conformance.Options {
		return conformance.Options{
			URL: target.publicURL,
			Token: func(ctx context.Context, subject string) (string, error) {
				return mint(ctx, target.issuerURL, subject)
			},
			Skip: []string{conformance.GroupAuthorizer, conformance.GroupUsage, "017/Pending"},
		}
	}

	// The two runs are parallel subtests of one wrapper, so the wrapper
	// returns only once both have finished and their reports are there to
	// compare.
	reports := make([]conformance.Report, 2)
	t.Run("two runs at once", func(t *testing.T) {
		for i := range reports {
			t.Run(fmt.Sprintf("run-%d", i), func(t *testing.T) {
				t.Parallel()
				reports[i] = conformance.Run(t, opts())
			})
		}
	})

	for i, report := range reports {
		if len(report.Failed) > 0 {
			t.Errorf("run %d failed %v; two runs against one installation are each a run of their own", i, report.Failed)
		}
		if len(report.Created) == 0 {
			t.Errorf("run %d created nothing, so there is nothing to have kept apart", i)
		}
	}

	// Neither run made anything the other made, which is what deleting by id
	// rather than by prefix is for.
	first := map[string]bool{}
	for _, made := range reports[0].Created {
		first[made] = true
	}
	for _, made := range reports[1].Created {
		if first[made] {
			t.Errorf("both runs created %q", made)
		}
	}
}

// TestTheSuiteReachesNoHelperOfThisTree is criterion 8's structural half:
// the package a consumer imports pulls in no package of this repository's
// test tree and nothing under internal/. The CI job proves the command runs
// from a clean checkout; this proves the import graph, which is the half a
// job cannot see until it has already pulled the module.
func TestTheSuiteReachesNoHelperOfThisTree(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps",
		"latere.ai/x/arca/test/conformance").Output()
	if err != nil {
		t.Skipf("the module's dependencies cannot be listed here: %v", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		switch {
		case strings.HasPrefix(line, "latere.ai/x/arca/internal/"):
			t.Errorf("the suite reaches %s, and it is black box", line)
		case strings.HasPrefix(line, "latere.ai/x/arca/test/") && line != "latere.ai/x/arca/test/conformance":
			t.Errorf("the suite reaches %s, and a consumer importing it would pull in this repository's test tree", line)
		}
	}
}

// The stack the drift test starts arcad against. It is the two stores of
// compose.yaml, read through the variables spec 014's tiers read, and the
// two stubs, started in process here because this test needs a server of its
// own per drift and the harness of test/e2e is that package's.

type stack struct {
	databaseURL, endpoint, bucket, key, secret string
}

func readStack() (stack, string) {
	s := stack{
		databaseURL: os.Getenv("E2E_DATABASE_URL"),
		endpoint:    os.Getenv("E2E_S3_ENDPOINT"),
		bucket:      envOr("E2E_S3_BUCKET", "arca-test"),
		key:         envOr("E2E_S3_KEY", "minioadmin"),
		secret:      envOr("E2E_S3_SECRET", "minioadmin"),
	}
	if s.databaseURL == "" || s.endpoint == "" {
		return stack{}, "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"
	}
	return s, ""
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// installation is one arcad the drift test started, with the stub endpoints
// it was pointed at.
type installation struct {
	publicURL, issuerURL, authorizerURL string
}

// start runs arcad against the stack with the two stubs of spec 014 and the
// extra environment the caller named, under a bucket prefix of its own, and
// stops it with the test. Every name the suite creates is that run's own, so
// two servers against one schema touch nothing of each other's.
func start(t *testing.T, s stack, extra ...string) *installation {
	t.Helper()
	iss := issuer.New(t)
	authz := authorizer.New(t)
	authz.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})

	prefix := fmt.Sprintf("conformance-%d-%d/", time.Now().UnixNano(), os.Getpid())
	env := append(os.Environ(),
		"ARCA_PUBLIC_ADDR=127.0.0.1:0",
		"ARCA_INTERNAL_ADDR=127.0.0.1:0",
		"ARCA_PUBLIC_URL=http://127.0.0.1",
		"ARCA_DB_URL="+s.databaseURL,
		"ARCA_BUCKET="+s.bucket,
		"ARCA_BUCKET_ENDPOINT="+s.endpoint,
		"ARCA_BUCKET_REGION=us-east-1",
		"ARCA_BUCKET_PATH_STYLE=true",
		"ARCA_BUCKET_PREFIX="+prefix,
		"ARCA_BUCKET_ACCESS_KEY="+s.key,
		"ARCA_BUCKET_SECRET_KEY="+s.secret,
		"ARCA_OIDC_ISSUERS="+iss.URL(),
		"ARCA_OIDC_INSECURE_ISSUERS=true",
		"ARCA_AUTHORIZER_URL="+authz.URL(),
		"ARCA_AUTHORIZER_TOKEN="+authz.Token(),
		// The administrator of the run, named in the installation's own
		// spelling of a subject. The stub authorizer answers every action
		// for every subject, so it is what admits the administration group
		// here; the variable names the same principal to the owner policy,
		// so an installation started from this environment without an
		// authorizer treats the same subject as the administrator.
		"ARCA_ADMIN_SUBJECTS="+iss.URL()+"|"+admin(),
		"ARCA_REAP_INTERVAL=0",
	)
	env = append(env, extra...)

	binary := arcad(t)
	migrate := exec.CommandContext(t.Context(), binary, "migrate")
	migrate.Env = env
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("arcad migrate: %v\n%s", err, out)
	}
	return &installation{
		publicURL:     serve(t, binary, env),
		issuerURL:     iss.URL(),
		authorizerURL: authz.URL(),
	}
}

// arcad answers the binary under test: the one the Makefile built when
// ARCA_BINARY names it, and one built for the run otherwise.
func arcad(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("ARCA_BINARY"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	path := filepath.Join(t.TempDir(), "arcad")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", path, "latere.ai/x/arca/cmd/arcad")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build arcad: %v\n%s", err, out)
	}
	return path
}

// listening reads the line arcad prints once both listeners are bound.
var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// serve starts arcad in the background, waits for readiness the way an
// operator does, and answers the public URL.
func serve(t *testing.T, binary string, env []string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = env
	var out lines
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start arcad: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Error("arcad did not stop")
		}
	})

	var public, internal string
	deadline := time.Now().Add(60 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil && public == "" {
			public, internal = "http://"+m[1], "http://"+m[2]
		}
		if internal != "" && ready(t, internal) {
			return public
		}
		select {
		case err := <-done:
			t.Fatalf("arcad exited before it was ready (%v):\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("arcad never became ready:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ready reads the probe an operator waits on.
func ready(t *testing.T, internal string) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, internal+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// lines collects a process's output while it runs.
type lines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
