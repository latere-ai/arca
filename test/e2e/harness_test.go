// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// Package e2e is the e2e tier of spec 014: arcad as a process, against the
// two stores of the stack and the two stubs of spec 014, which is every
// dependency an installation has. Nothing here reaches inside the server:
// what a test sees is what an operator sees.
//
// It runs when E2E_DATABASE_URL and E2E_S3_ENDPOINT are set and skips
// otherwise, so a plain go test on a clean clone stays green with no
// services. Every run takes a schema and a bucket prefix of its own and
// removes both when it ends.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/test/stubs/authorizer"
	"latere.ai/x/arca/test/stubs/issuer"
)

// skipWithoutTheStack is the remediation a tier prints when the stack is
// not there. It names both variables and the command that starts them.
const skipWithoutTheStack = "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"

// stack is what the tier reads the services of compose.yaml through. These
// are test variables: no server reads them, and the harness builds the
// ARCA_* values it starts the server with from them.
type stack struct {
	databaseURL string
	endpoint    string
	bucket      string
	key, secret string
}

// readStack answers the stack's variables, or the reason to skip.
func readStack() (stack, string) {
	s := stack{
		databaseURL: os.Getenv("E2E_DATABASE_URL"),
		endpoint:    os.Getenv("E2E_S3_ENDPOINT"),
		bucket:      envOr("E2E_S3_BUCKET", "arca-test"),
		key:         envOr("E2E_S3_KEY", "minioadmin"),
		secret:      envOr("E2E_S3_SECRET", "minioadmin"),
	}
	if s.databaseURL == "" || s.endpoint == "" {
		return stack{}, skipWithoutTheStack
	}
	return s, ""
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// installation is one arcad, running as a process against the stack and
// the stubs.
type installation struct {
	publicURL   string
	internalURL string
	issuer      *issuer.Server
	authorizer  *authorizer.Server
	binary      string
	env         []string
}

// start builds the installation of spec 014's make run, in the same order,
// and answers it once readiness passes.
func start(t *testing.T) *installation {
	t.Helper()
	s, reason := readStack()
	if reason != "" {
		t.Skip(reason)
	}

	i := &installation{
		binary:     binary(t),
		issuer:     issuer.New(t),
		authorizer: authorizer.New(t),
	}
	databaseURL := schema(t, s)
	prefix := bucketPrefix(t, s)
	i.env = append(os.Environ(),
		"ARCA_PUBLIC_ADDR=127.0.0.1:0",
		"ARCA_INTERNAL_ADDR=127.0.0.1:0",
		"ARCA_PUBLIC_URL=http://127.0.0.1",
		"ARCA_DATABASE_URL="+databaseURL,
		"ARCA_BUCKET="+s.bucket,
		"ARCA_BUCKET_ENDPOINT="+s.endpoint,
		"ARCA_BUCKET_REGION=us-east-1",
		"ARCA_BUCKET_PATH_STYLE=true",
		"ARCA_BUCKET_PREFIX="+prefix,
		"ARCA_BUCKET_ACCESS_KEY="+s.key,
		"ARCA_BUCKET_SECRET_KEY="+s.secret,
		// The identity rows spec 006 begins reading. The stub issuer
		// serves plain HTTP, which spec 006 admits only on loopback or
		// on this list.
		"ARCA_OIDC_ISSUERS="+i.issuer.URL(),
		"ARCA_OIDC_INSECURE_ISSUERS=true",
		"ARCA_AUTHORIZER_URL="+i.authorizer.URL(),
		"ARCA_AUTHORIZER_TOKEN="+i.authorizer.Token(),
		"ARCA_ADMIN_SUBJECTS="+i.issuer.URL()+"|dev",
	)

	if out, err := i.command(t, "migrate"); err != nil {
		t.Fatalf("arcad migrate: %v\n%s", err, out)
	}
	i.serve(t)
	return i
}

// binary answers the arcad under test: the one the Makefile built when
// ARCA_BINARY names it, and one built for the run otherwise.
func binary(t *testing.T) string {
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

// schema gives the run a schema of its own and drops it afterwards, so two
// packages never share a migration state.
func schema(t *testing.T, s stack) string {
	t.Helper()
	name := fmt.Sprintf("e2e_%d_%d", time.Now().UnixNano(), os.Getpid())
	db, err := store.Open(t.Context(), s.databaseURL)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	if _, err := db.Querier().Exec(t.Context(), "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		dropper, err := store.Open(ctx, s.databaseURL)
		if err != nil {
			t.Errorf("drop the schema: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Querier().Exec(ctx, "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Errorf("drop the schema: %v", err)
		}
	})
	separator := "?"
	if strings.Contains(s.databaseURL, "?") {
		separator = "&"
	}
	return s.databaseURL + separator + "search_path=" + name
}

// bucketPrefix gives the run a prefix of its own and sweeps it afterwards,
// so the bucket holds nothing of it when the run ends.
func bucketPrefix(t *testing.T, s stack) string {
	t.Helper()
	prefix := fmt.Sprintf("e2e-%d-%d/", time.Now().UnixNano(), os.Getpid())
	bucket, err := blob.NewS3(t.Context(), blob.Options{
		Bucket: s.bucket, Endpoint: s.endpoint, Region: "us-east-1",
		AccessKey: s.key, SecretKey: s.secret, PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		page, err := bucket.List(ctx, prefix, "", 1000)
		if err != nil {
			t.Errorf("sweep the prefix: %v", err)
			return
		}
		if len(page.Keys) == 0 {
			return
		}
		if err := bucket.DeleteMany(ctx, page.Keys); err != nil {
			t.Errorf("sweep the prefix: %v", err)
		}
	})
	return prefix
}

// command runs one subcommand of arcad to completion and answers its
// output.
func (i *installation) command(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), i.binary, args...)
	cmd.Env = i.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// listening reads the line serve prints once both listeners are bound.
var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// serve starts arcad in the background and waits for readiness, which is
// what an operator waits for.
func (i *installation) serve(t *testing.T) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), i.binary)
	cmd.Env = i.env
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

	deadline := time.Now().Add(30 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil && i.publicURL == "" {
			i.publicURL, i.internalURL = "http://"+m[1], "http://"+m[2]
		}
		if i.internalURL != "" {
			if code, _ := get(t, i.internalURL+"/readyz"); code == http.StatusOK {
				return
			}
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

// get reads one probe and answers the status and the body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
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
