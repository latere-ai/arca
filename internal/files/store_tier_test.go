// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 005: the handlers against the real Postgres and the
// real MinIO of the stack. What it proves is what a fake cannot, which is
// the ordering of the two stores under a fault: a write that fails after the
// bucket leaves no row, a delete that fails after the row leaves the bytes,
// and two conditional writers of one path leave one winner.
//
// It runs when E2E_DATABASE_URL and E2E_S3_ENDPOINT are set and skips
// otherwise, and every run takes a schema and a bucket prefix of its own.
package files

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
)

// skipWithoutTheStack is the remediation a tier prints when the stack is
// not there. It names both variables and the command that starts them.
const skipWithoutTheStack = "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"

// tier is one surface over the real stores, with the family's stub issuer
// and stub authorizer behind it.
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
		bucket: blob.NewCounting(bucket), issuer: iss, endpoint: endpointStub,
		owner: iss.URL() + "|9ab3", clock: time.Now(),
	}
	o := Options{
		DB: db, Bucket: h.bucket, Decide: id.Authorizer,
		Config: config.Config{
			BucketPrefix: prefix, InlineBytes: inlineBytes,
			MaxUploadBytes: 1 << 20, TrashRetention: 720 * time.Hour,
		},
		Now: func() time.Time { return h.clock },
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
	h.read = func(t *testing.T, path string) (store.File, bool) {
		t.Helper()
		return stores.row(t, h.owner, path)
	}
	return h, stores
}

// tierStores are the real stores the tier builds the surface on, and what a
// case reads to see what the handlers left behind.
type tierStores struct {
	db     *store.DB
	prefix string
	bucket *blob.S3
}

// keys answers what the run's prefix holds.
func (s *tierStores) keys(t *testing.T) []string {
	t.Helper()
	page, err := s.bucket.List(t.Context(), s.prefix, "", 1000)
	if err != nil {
		t.Fatalf("list the prefix: %v", err)
	}
	return page.Keys
}

// row reads one path straight from the database.
func (s *tierStores) row(t *testing.T, owner, path string) (store.File, bool) {
	t.Helper()
	f, err := store.NewFiles().Get(t.Context(), s.db.Querier(), owner, path)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.File{}, false
		}
		t.Fatalf("read %q: %v", path, err)
	}
	return f, true
}

// grant writes one active subject grant on a prefix of a space and answers it
// with the id the database assigned.
func (s *tierStores) grant(t *testing.T, owner, prefix string) store.Grant {
	t.Helper()
	g, err := store.NewShares().Create(t.Context(), s.db.Querier(), store.Grant{
		Owner: owner, PathPrefix: prefix, GranteeKind: store.GranteeSubject,
		Grantee: "https://issuer.example|reader", Permission: "read", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("grant %q: %v", prefix, err)
	}
	return g
}

// readGrant reads one grant straight from the database.
func (s *tierStores) readGrant(t *testing.T, id string) store.Grant {
	t.Helper()
	g, err := store.NewShares().Get(t.Context(), s.db.Querier(), id)
	if err != nil {
		t.Fatalf("read the grant %q: %v", id, err)
	}
	return g
}

// database opens a schema of its own, applies the migrations, and drops the
// schema when the test ends.
func database(t *testing.T, url string) *store.DB {
	t.Helper()
	name := fmt.Sprintf("files_%d_%d", time.Now().UnixNano(), os.Getpid())
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
	scoped := url + separator(url) + "search_path=" + name
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

// separator is what a parameter is appended to a connection string with.
func separator(url string) string {
	if strings.Contains(url, "?") {
		return "&"
	}
	return "?"
}

// sweep removes what a run wrote under its prefix.
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

func TestStoreAPutRoundTripsAgainstBothStores(t *testing.T) {
	h, stores := tier(t)
	w := h.put(t, "files/notes/plan.md", "the first content")
	if w.Code != http.StatusCreated {
		t.Fatalf("the put answered %d: %s", w.Code, w.Body)
	}
	var created Object
	decode(t, w, &created)
	if created.Checksum != digest("the first content") {
		t.Fatalf("the put answered %+v", created)
	}
	read := h.call(t, http.MethodGet, h.object("files/notes/plan.md"), nil)
	if read.Code != http.StatusOK || read.Body.String() != "the first content" {
		t.Fatalf("the read answered %d: %q", read.Code, read.Body)
	}
	head := h.call(t, http.MethodHead, h.object("files/notes/plan.md"), nil)
	if head.Code != http.StatusOK || head.Header().Get(api.HeaderETag) != `"`+created.Checksum+`"` {
		t.Fatalf("the head answered %d with the ETag %q", head.Code, head.Header().Get(api.HeaderETag))
	}
	if got := len(stores.keys(t)); got != 1 {
		t.Fatalf("one put left %d objects", got)
	}
	if w := h.call(t, http.MethodDelete, h.object("files/notes/plan.md")+"?permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the delete answered %d: %s", w.Code, w.Body)
	}
	if _, held := stores.row(t, h.owner, "files/notes/plan.md"); held {
		t.Error("a permanent delete left a row")
	}
	if got := len(stores.keys(t)); got != 0 {
		t.Fatalf("a permanent delete left %d objects", got)
	}
}

func TestStoreAWriteRefusedAfterTheBucketLeavesNoRowAndNoBytes(t *testing.T) {
	h, stores := tier(t)
	h.seed(t, "files/plan.md", "first")
	before := len(stores.keys(t))

	// The bytes are written before the transaction opens, so a precondition
	// the transaction refuses is a write that failed after the bucket.
	refused := h.put(t, "files/plan.md", "second", api.HeaderIfMatch, `"`+digest("nothing")+`"`)
	if got := code(t, refused); got != api.CodePreconditionFailed {
		t.Fatalf("a stale If-Match is %q", got)
	}
	row, held := stores.row(t, h.owner, "files/plan.md")
	if !held || row.Checksum != digest("first") {
		t.Fatalf("a refused write changed the row to %+v", row)
	}
	// The key was fresh and nothing else could reference it, so the handler
	// removed it rather than leaving the reaper anything to find.
	if got := len(stores.keys(t)); got != before {
		t.Fatalf("a refused write left %d objects and the space held %d", got, before)
	}

	// A handler that cannot remove its own key leaves the bytes, which is
	// what the reaper's first pass is for.
	h.bucket.FailNth(blob.MethodDelete, h.bucket.Calls(blob.MethodDelete)+1,
		errors.New("the bucket is unreachable"))
	again := h.put(t, "files/plan.md", "third", api.HeaderIfMatch, `"`+digest("nothing")+`"`)
	if got := code(t, again); got != api.CodePreconditionFailed {
		t.Fatalf("a stale If-Match is %q", got)
	}
	if got := len(stores.keys(t)); got != before+1 {
		t.Fatalf("a write whose cleanup failed left %d objects and the space held %d", got, before)
	}
	if _, held := stores.row(t, h.owner, "files/plan.md"); !held {
		t.Error("a refused write removed the row it did not write")
	}
}

func TestStoreADeleteThatFailsAfterTheRowLeavesTheBytesForTheReaper(t *testing.T) {
	h, stores := tier(t)
	h.seed(t, "files/plan.md", "first")
	before := len(stores.keys(t))
	h.bucket.FailNth(blob.MethodDelete, 1, errors.New("the bucket is unreachable"))
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md")+"?permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the delete answered %d: %s", w.Code, w.Body)
	}
	// Rows go before bytes, so a failure between them leaves bytes the
	// reaper finds and never a row whose object is gone.
	if _, held := stores.row(t, h.owner, "files/plan.md"); held {
		t.Error("the delete left a row")
	}
	if got := len(stores.keys(t)); got != before {
		t.Fatalf("the delete left %d objects and the space held %d", got, before)
	}
}

func TestStoreTwoConditionalWritersOfOnePathLeaveOneWinner(t *testing.T) {
	h, stores := tier(t)
	h.seed(t, "files/plan.md", "first")
	stale := `"` + digest("first") + `"`

	var wins, refusals int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := h.put(t, "files/plan.md", fmt.Sprintf("content %d", i), api.HeaderIfMatch, stale)
			mu.Lock()
			defer mu.Unlock()
			switch w.Code {
			case http.StatusOK:
				wins++
			case http.StatusPreconditionFailed:
				refusals++
			default:
				t.Errorf("a conditional write answered %d: %s", w.Code, w.Body)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || refusals != 1 {
		t.Fatalf("two writers of one checksum left %d winners and %d refusals", wins, refusals)
	}
	// The loser's bytes are gone: two puts and one capture leave the first
	// content as a version and the winner's as the row.
	if got := len(stores.keys(t)); got != 2 {
		t.Fatalf("the race left %d objects, and the loser's are not one of them", got)
	}
}

func TestStoreTheVersionsAndTheTrashRoundTripAgainstBothStores(t *testing.T) {
	h, _ := tier(t)
	h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")

	restored := h.call(t, http.MethodPost, h.object("files/plan.md"),
		strings.NewReader(`{"restore_version":1}`), api.HeaderContentType, api.JSONMediaType)
	if restored.Code != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", restored.Code, restored.Body)
	}
	read := h.call(t, http.MethodGet, h.object("files/plan.md"), nil)
	if read.Body.String() != "one" {
		t.Fatalf("the restored object reads %q", read.Body)
	}

	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d", w.Code)
	}
	if got := h.call(t, http.MethodGet, h.object("files/plan.md"), nil); got.Code != http.StatusNotFound {
		t.Fatalf("a trashed path reads %d", got.Code)
	}
	back := h.call(t, http.MethodPost, "/v1/trash/restore",
		strings.NewReader(`{"owner":"me","path":"files/plan.md"}`), api.HeaderContentType, api.JSONMediaType)
	if back.Code != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", back.Code, back.Body)
	}
	read = h.call(t, http.MethodGet, h.object("files/plan.md"), nil)
	if read.Code != http.StatusOK || read.Body.String() != "one" {
		t.Fatalf("the restored object reads %d: %q", read.Code, read.Body)
	}
}

func TestStoreAMoveMakesNoBucketCallAndCarriesWhatKeysOnThePath(t *testing.T) {
	h, stores := tier(t)
	h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")
	if w := h.call(t, http.MethodPut, "/v1/stars",
		strings.NewReader(`{"owner":"me","path":"files/plan.md"}`),
		api.HeaderContentType, api.JSONMediaType); w.Code != http.StatusNoContent {
		t.Fatalf("the star answered %d: %s", w.Code, w.Body)
	}
	// A grant keys on a path the same way the history and the bookmarks do,
	// and the two prefixes answer differently. The one on the exact path
	// means "this object" and follows it; the one on a parent prefix covers
	// a subtree and stays where it is. Left behind, the exact grant would
	// not merely be lost: the old path becomes free, and the next object
	// written there would be covered by a grant its owner gave for something
	// else (criterion 8, spec 008's ladder).
	exact := stores.grant(t, h.owner, "files/plan.md")
	parent := stores.grant(t, h.owner, "files")
	before := h.bucket.Total()
	w := h.call(t, http.MethodPost, h.object("files/plan.md"),
		strings.NewReader(`{"move_to":"files/archive/plan.md"}`), api.HeaderContentType, api.JSONMediaType)
	if w.Code != http.StatusOK {
		t.Fatalf("the move answered %d: %s", w.Code, w.Body)
	}
	if got := h.bucket.Total() - before; got != 0 {
		t.Fatalf("the move made %d bucket calls, and a key derives from an id", got)
	}
	versions, err := store.NewVersions().List(t.Context(), stores.db.Querier(), h.owner, "files/archive/plan.md", 0, 10)
	if err != nil || len(versions) != 1 {
		t.Fatalf("the history after a move is %d rows, %v", len(versions), err)
	}
	stars, err := store.NewStars().List(t.Context(), stores.db.Querier(), h.owner, store.StarCursor{}, 10)
	if err != nil || len(stars) != 1 || stars[0].Path != "files/archive/plan.md" {
		t.Fatalf("the bookmarks after a move are %v, %v", stars, err)
	}
	if held := stores.readGrant(t, exact.ID); held.PathPrefix != "files/archive/plan.md" {
		t.Fatalf("the grant on the exact path is on %q after the move", held.PathPrefix)
	}
	if held := stores.readGrant(t, parent.ID); held.PathPrefix != "files" {
		t.Fatalf("the grant on the parent prefix moved to %q", held.PathPrefix)
	}
	read := h.call(t, http.MethodGet, h.object("files/archive/plan.md"), nil)
	if read.Code != http.StatusOK || read.Body.String() != "two" {
		t.Fatalf("the moved object reads %d: %q", read.Code, read.Body)
	}
}

func TestStoreAListingPagesStablyUnderConcurrentInserts(t *testing.T) {
	h, _ := tier(t)
	for i := range 10 {
		h.seed(t, fmt.Sprintf("files/walk/%03d.md", i), "x")
	}
	seen := map[string]int{}
	cursor := ""
	for {
		target := h.object("files/walk") + "?list=1&limit=3"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		page := h.listing(t, target)
		for _, e := range page.Entries {
			seen[e.Path]++
		}
		// A row inserted during the walk shifts nothing, because the cursor
		// is the sort key of the last row of the page before.
		h.seed(t, fmt.Sprintf("files/walk/%03d.md", 900+len(seen)), "x")
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for path, times := range seen {
		if times != 1 {
			t.Errorf("the walk returned %q %d times", path, times)
		}
	}
	if len(seen) < 10 {
		t.Fatalf("the walk returned %d of the ten rows it started with", len(seen))
	}
}

func TestStoreTheTierSkipsWithoutTheStack(t *testing.T) {
	if os.Getenv("E2E_DATABASE_URL") == "" || os.Getenv("E2E_S3_ENDPOINT") == "" {
		t.Skip(skipWithoutTheStack)
	}
}
