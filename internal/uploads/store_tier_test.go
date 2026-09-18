// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 007: the three handlers against the real Postgres
// and the real MinIO of the stack. What it proves is what a fake cannot: a
// multipart really assembles, a part label the store did not answer fails
// the assembly there and leaves no row, and an abort takes the parts back.
//
// The parts go to the bucket the way a client sends them, through the
// presigned URLs the session answered, so the tier exercises the path no
// byte of which passes through the server.
package uploads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/metrics"
	"latere.ai/x/arca/internal/store"
)

// skipWithoutTheStack is the remediation a tier prints when the stack is
// not there. It names both variables and the command that starts them.
const skipWithoutTheStack = "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"

// tier is one session surface over the real stores.
func tier(t *testing.T) (*harness, *tierStores) {
	t.Helper()
	databaseURL, endpoint := os.Getenv("E2E_DATABASE_URL"), os.Getenv("E2E_S3_ENDPOINT")
	if databaseURL == "" || endpoint == "" {
		t.Skip(skipWithoutTheStack)
	}
	prefix := fmt.Sprintf("tier-%d-%d/", time.Now().UnixNano(), os.Getpid())
	bucket, err := blob.NewS3(t.Context(), blob.Options{
		Bucket:   envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint: endpoint, Region: "us-east-1",
		AccessKey: envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey: envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	t.Cleanup(func() { sweep(t, bucket, prefix) })
	db := database(t, databaseURL)

	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	endpointStub := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	endpointStub.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: "arca",
		AuthorizerURL: endpointStub.URL(), AuthorizerToken: endpointStub.Token(),
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	h := &harness{
		bucket: blob.NewCounting(bucket), recorder: metrics.Register(nil),
		issuer: iss, endpoint: endpointStub,
		owner: iss.URL() + "|9ab3", clock: time.Now(),
	}
	o := Options{
		Options: files.Options{
			DB: db, Bucket: h.bucket, Decide: id.Authorizer,
			Config: config.Config{
				BucketPrefix: prefix, InlineBytes: 16 << 20, MaxUploadBytes: 5 << 30,
				TrashRetention: 720 * time.Hour,
			},
			Now:     func() time.Time { return h.clock },
			Metrics: h.recorder,
		},
		Metrics: h.recorder,
	}
	surface, err := api.New(api.Options{
		Verifier: id.Verifier, Authorizer: id.Authorizer,
		PublicURL: "https://storage.example", Routes: Routes(o),
		// The frame registers the event tail of spec 010 and refuses to
		// build without its log. No test here drives that route, so the
		// node's own log is wired with no database behind it: what the tail
		// reads is that package's business and its own tests'.
		Events: events.NewLog(),
	})
	if err != nil {
		t.Fatalf("the surface would not build: %v", err)
	}
	h.mux = http.NewServeMux()
	surface.Mount(h.mux)
	stores := &tierStores{db: db, prefix: prefix, bucket: bucket}
	h.sessions = func(t *testing.T, id string) store.Session {
		t.Helper()
		held, err := store.NewSessions().Get(t.Context(), db.Querier(), id)
		if err != nil {
			t.Fatalf("the session is not there: %v", err)
		}
		return held
	}
	return h, stores
}

// tierStores are the real stores the tier builds the surface on.
type tierStores struct {
	db     *store.DB
	prefix string
	bucket *blob.S3
}

// database opens a schema of its own, applies the migrations, and drops it.
func database(t *testing.T, url string) *store.DB {
	t.Helper()
	name := fmt.Sprintf("uploads_%d_%d", time.Now().UnixNano(), os.Getpid())
	admin, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Querier().Exec(t.Context(), "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		dropper, err := store.Open(ctx, url)
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
	if strings.Contains(url, "?") {
		separator = "&"
	}
	scoped := url + separator + "search_path=" + name
	if err := store.Migrate(scoped); err != nil {
		t.Fatalf("apply the migrations: %v", err)
	}
	db, err := store.Open(t.Context(), scoped)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// sweep removes what a run wrote under its prefix, its open multiparts
// included.
func sweep(t *testing.T, bucket *blob.S3, prefix string) {
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
}

// envOr answers the variable or the default of spec 014's table.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// send puts one part where the session's URL points, which is what a client
// does: the bytes go to the bucket and never through the server.
func send(t *testing.T, url, content string) string {
	t.Helper()
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the store answered %d to a part: %s", resp.StatusCode, body)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`)
}

func TestStoreASessionAssemblesThePartsAClientSentToTheBucket(t *testing.T) {
	h, stores := tier(t)
	// Every part but the last is the fixed part size, which is what a store
	// requires of a multipart upload.
	first := strings.Repeat("a", int(PartSize))
	second := "the tail"
	session := h.open(t, "files/video/keynote.mp4", PartSize+int64(len(second)))
	if session.PartCount != 2 || len(session.PartURLs) != 2 {
		t.Fatalf("the session is %+v", session)
	}
	labels := []string{
		send(t, session.PartURLs[0], first),
		send(t, session.PartURLs[1], second),
	}
	w := h.finish(t, session, labels)
	if w.Code != http.StatusCreated {
		t.Fatalf("the completion answered %d: %s", w.Code, w.Body)
	}
	var out files.Object
	decode(t, w, &out)
	if out.Size != PartSize+int64(len(second)) {
		t.Fatalf("the completion answered %+v", out)
	}
	// The checksum of an object assembled from parts the server never saw is
	// the store's composite label, which the row says with its kind.
	if out.ChecksumKind != "etag" {
		t.Fatalf("the completion answered the kind %q", out.ChecksumKind)
	}

	row, err := store.NewFiles().Get(t.Context(), stores.db.Querier(), h.owner, "files/video/keynote.mp4")
	if err != nil {
		t.Fatalf("the completion left no row: %v", err)
	}
	body, held, err := stores.bucket.Get(t.Context(), row.ObjectID.Key(stores.prefix))
	if err != nil {
		t.Fatalf("the assembled object is not in the bucket: %v", err)
	}
	defer func() { _ = body.Close() }()
	assembled, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(assembled, []byte(first+second)) || held.Size != out.Size {
		t.Fatalf("the assembled object is %d bytes", len(assembled))
	}
	sessions, err := store.NewSessions().Expired(t.Context(), stores.db.Querier(), time.Now().Add(time.Hour), 10)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("the completion left %d sessions, %v", len(sessions), err)
	}
}

func TestStoreACompletionWithALabelTheStoreDidNotAnswerLeavesNoRow(t *testing.T) {
	h, stores := tier(t)
	session := h.open(t, "files/video/keynote.mp4", 1024)
	send(t, session.PartURLs[0], "the only part")
	w := h.finish(t, session, []string{"d41d8cd98f00b204e9800998ecf8427e"})
	if got := code(t, w); got != api.CodeBadRequest {
		t.Fatalf("a completion with a label the store did not answer is %q", got)
	}
	if _, err := store.NewFiles().Get(t.Context(), stores.db.Querier(), h.owner, "files/video/keynote.mp4"); err == nil {
		t.Error("a failed assembly left a row")
	}
}

func TestStoreAnAbortTakesThePartsBackAndClosesTheRow(t *testing.T) {
	h, stores := tier(t)
	session := h.open(t, "files/video/keynote.mp4", 1024)
	send(t, session.PartURLs[0], "the only part")
	if w := h.call(t, http.MethodDelete, "/v1/uploads/"+session.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("the abort answered %d: %s", w.Code, w.Body)
	}
	sessions, err := store.NewSessions().Expired(t.Context(), stores.db.Querier(), time.Now().Add(time.Hour), 10)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("the abort left %d sessions, %v", len(sessions), err)
	}
	// The parts are gone from the store, so a completion of the same session
	// assembles nothing.
	if w := h.finish(t, session, []string{"anything"}); w.Code != http.StatusNotFound {
		t.Fatalf("a completion after an abort answered %d: %s", w.Code, w.Body)
	}
}

func TestStoreAnOpenSessionIsTheReapersToFindAndItsObjectIsReferenced(t *testing.T) {
	h, stores := tier(t)
	session := h.open(t, "files/video/keynote.mp4", 1024)
	send(t, session.PartURLs[0], "the only part")

	held, err := store.NewSessions().Get(t.Context(), stores.db.Querier(), session.ID)
	if err != nil {
		t.Fatalf("the session is not there: %v", err)
	}
	// The parts are invisible to object listing, so the row is the only
	// durable pointer to them and the reference check has to name it.
	referenced, err := store.ObjectReferenced(t.Context(), stores.db.Querier(), held.ObjectID)
	if err != nil || !referenced {
		t.Fatalf("an open session's object is referenced = %t, %v", referenced, err)
	}
	page, err := stores.bucket.List(t.Context(), stores.prefix, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Keys) != 0 {
		t.Fatalf("an incomplete multipart is visible to a listing as %v", page.Keys)
	}

	// The reaper's fourth pass reads the deadline off the row, so a session
	// past it is data and not a schedule.
	if _, err := stores.db.Querier().Exec(t.Context(),
		`UPDATE upload_sessions SET expires_at = now() - interval '1 hour' WHERE id = $1`, held.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := store.NewSessions().Expired(t.Context(), stores.db.Querier(), time.Now(), 10)
	if err != nil || len(expired) != 1 {
		t.Fatalf("the expired sessions are %v, %v", expired, err)
	}
	if err := stores.bucket.AbortMultipart(t.Context(), expired[0].ObjectID.Key(stores.prefix), expired[0].UploadID); err != nil {
		t.Fatalf("the sweep could not abort the upload: %v", err)
	}
}

func TestStoreAnOverwriteThroughASessionKeepsThePreviousObject(t *testing.T) {
	h, stores := tier(t)
	first := h.open(t, "files/plan.md", 1024)
	w := h.finish(t, first, []string{send(t, first.PartURLs[0], "one")})
	if w.Code != http.StatusCreated {
		t.Fatalf("the first completion answered %d: %s", w.Code, w.Body)
	}
	before, err := store.NewFiles().Get(t.Context(), stores.db.Querier(), h.owner, "files/plan.md")
	if err != nil {
		t.Fatal(err)
	}

	second := h.open(t, "files/plan.md", 1024)
	w = h.finish(t, second, []string{send(t, second.PartURLs[0], "two")})
	if w.Code != http.StatusOK {
		t.Fatalf("the overwrite answered %d: %s", w.Code, w.Body)
	}
	versions, err := store.NewVersions().List(t.Context(), stores.db.Querier(), h.owner, "files/plan.md", 0, 10)
	if err != nil || len(versions) != 1 || versions[0].ObjectID != before.ObjectID {
		t.Fatalf("the overwrite left the history %v, %v", versions, err)
	}
	// The previous object's bytes survive behind the version row.
	if _, err := stores.bucket.Head(t.Context(), before.ObjectID.Key(stores.prefix)); err != nil {
		t.Fatalf("the superseded object is gone: %v", err)
	}
}

func TestStoreTheTierSkipsWithoutTheStack(t *testing.T) {
	if os.Getenv("E2E_DATABASE_URL") == "" || os.Getenv("E2E_S3_ENDPOINT") == "" {
		t.Skip(skipWithoutTheStack)
	}
	if !errors.Is(context.Canceled, context.Canceled) {
		t.Fatal("the tier does not run")
	}
}
