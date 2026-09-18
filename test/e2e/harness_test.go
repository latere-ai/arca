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
	"bytes"
	"context"
	"encoding/json"
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

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
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
	// The stores this run was given, for a tier that seeds a row or an
	// object the routes of a later spec would otherwise write. Nothing here
	// is read by the server: it reaches the same schema and the same prefix
	// through its own ARCA_* values.
	stack       stack
	databaseURL string
	prefix      string
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
	i.stack = s
	i.databaseURL = schema(t, s)
	i.prefix = bucketPrefix(t, s)
	databaseURL, prefix := i.databaseURL, i.prefix
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
	return i.commandWith(t, nil, args...)
}

// commandWith runs one subcommand with the environment this installation
// starts its server with and the overrides given, each a NAME=value that wins
// over the installation's own. It is how a tier points one variable somewhere
// else and reads what the binary says about it.
func (i *installation) commandWith(t *testing.T, overrides []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), i.binary, args...)
	cmd.Env = append(append([]string{}, i.env...), overrides...)
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

// The helpers every tier of this package drives the installation through:
// one request, one request held to its status, the stub's one rule, the two
// stores a tier seeds through, and a presigned URL fetched the way a sandbox
// fetches one. They are here rather than beside the tier that first needed
// them, so the tier of the next spec drives the same installation the same
// way.

// api drives one request against the running server with the caller's
// bearer and answers the status and the body.
func (i *installation) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, i.publicURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+i.issuer.Mint(issuer.Claims{Sub: "dev"}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, read
}

// expect drives one request and fails the test when the status is not the
// one wanted, decoding the body into out when it is given.
func (i *installation) expect(t *testing.T, want int, method, path string, body, out any) []byte {
	t.Helper()
	code, read := i.api(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, code, want, read)
	}
	if out != nil {
		if err := json.Unmarshal(read, out); err != nil {
			t.Fatalf("%s %s answered %s: %v", method, path, read, err)
		}
	}
	return read
}

// errorCode reads the code of a refusal in the envelope of spec 013.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				RequestID string   `json:"request_id"`
				Fields    []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("the refusal is not the envelope: %s", body)
	}
	if envelope.Error.Code != "" && envelope.Error.Details.RequestID == "" {
		t.Errorf("the refusal carries no request id: %s", body)
	}
	return envelope.Error.Code
}

// subject is the space every request of these tiers acts in.
func (i *installation) subject() string { return i.issuer.URL() + "|dev" }

// allowEverything puts one rule in the stub's table, so the tiers below
// exercise the routes rather than the ladder. The ladder itself is the unit
// tier's and the conformance suite's.
func (i *installation) allowEverything() {
	i.authorizer.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
}

// bucket opens the run's own bucket, for seeding and for reading back what a
// sync removed.
func (i *installation) bucket(t *testing.T) blob.Store {
	t.Helper()
	b, err := blob.NewS3(t.Context(), blob.Options{
		Bucket: i.stack.bucket, Endpoint: i.stack.endpoint, Region: "us-east-1",
		AccessKey: i.stack.key, SecretKey: i.stack.secret, PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	return b
}

// database opens the run's own schema.
func (i *installation) database(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), i.databaseURL)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seeded is one object a tier wrote straight into the two stores.
type seeded struct {
	path     string
	id       object.ID
	checksum string
	size     int64
	bytes    []byte
}

// put writes one object in one piece and the row that names it, which is
// what a put of spec 005 leaves behind.
func (i *installation) put(t *testing.T, db *store.DB, path, content string) seeded {
	t.Helper()
	id := object.NewID()
	written, err := i.bucket(t).Put(t.Context(), id.Key(i.prefix), strings.NewReader(content), int64(len(content)), blob.PutOptions{})
	if err != nil {
		t.Fatalf("seed %q: %v", path, err)
	}
	return i.row(t, db, path, id, written.SHA256, int64(len(content)), object.ChecksumSHA256, []byte(content))
}

// putInParts writes one object as a multipart upload and the row that names
// it. Such an object carries the store's composite label rather than a
// digest of its bytes, and a key the path does not predict, which is the
// case materialize has to presign from the row.
func (i *installation) putInParts(t *testing.T, db *store.DB, path, content string) seeded {
	t.Helper()
	b := i.bucket(t)
	id := object.NewID()
	key := id.Key(i.prefix)
	upload, err := b.CreateMultipart(t.Context(), key, blob.PutOptions{})
	if err != nil {
		t.Fatalf("open the multipart upload: %v", err)
	}
	url, err := b.PresignPart(t.Context(), key, upload, 1)
	if err != nil {
		t.Fatalf("sign the part: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send the part: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("the part answered %d: %s", resp.StatusCode, body)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	assembled, err := b.CompleteMultipart(t.Context(), key, upload, []blob.Part{{Number: 1, ETag: etag}})
	if err != nil {
		t.Fatalf("assemble the parts: %v", err)
	}
	if !strings.Contains(assembled.ETag, "-") {
		t.Fatalf("the assembled object's label is %q, which is not a composite", assembled.ETag)
	}
	return i.row(t, db, path, id, assembled.ETag, int64(len(content)), object.ChecksumETag, []byte(content))
}

// row writes the metadata half of a seeded object.
func (i *installation) row(t *testing.T, db *store.DB, path string, id object.ID, checksum string, size int64, kind object.ChecksumKind, content []byte) seeded {
	t.Helper()
	created, err := store.NewFiles().Insert(t.Context(), db.Querier(), store.File{
		Owner: i.subject(), Path: path, ObjectID: id, CreatedBy: i.subject(),
		SizeBytes: size, Checksum: checksum, ChecksumKind: kind,
	})
	if err != nil || !created {
		t.Fatalf("seed the row for %q: %v, %v", path, created, err)
	}
	return seeded{path: path, id: id, checksum: checksum, size: size, bytes: content}
}

// fetch reads a presigned URL the way a sandbox does: straight from the
// bucket, with no bearer of Arca's.
func fetch(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}
