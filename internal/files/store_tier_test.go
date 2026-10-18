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
		Issuers: []string{iss.URL()}, Audiences: []string{"arca"},
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
	stars, err := store.NewStars().List(t.Context(), stores.db.Querier(), h.owner, nil, store.StarCursor{}, 10, nil)
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

// TestStoreAFolderCopyIsReadableAtBothEnds is criterion 17 of spec 028
// against the real stores: the bucket copies, every copy is an object of its
// own with the source's bytes, and the server reads and writes no body.
func TestStoreAFolderCopyIsReadableAtBothEnds(t *testing.T) {
	h, stores := tier(t)
	contents := map[string]string{"a.md": "the first content", "deep/b.md": "the second, nested"}
	for rest, content := range contents {
		if w := h.put(t, "files/notes/"+rest, content); w.Code != http.StatusCreated {
			t.Fatalf("seeding %s answered %d: %s", rest, w.Code, w.Body)
		}
	}
	puts, gets := h.bucket.Calls(blob.MethodPut), h.bucket.Calls(blob.MethodGet)

	w := h.post(t, "files/notes", `{"copy_to":"files/copy","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the copy answered %d: %s", w.Code, w.Body)
	}
	if h.bucket.Calls(blob.MethodPut) != puts || h.bucket.Calls(blob.MethodGet) != gets {
		t.Error("the copy read or wrote an object body through the server")
	}
	if got := len(stores.keys(t)); got != 4 {
		t.Fatalf("the bucket holds %d keys, want the two objects and their two copies", got)
	}
	for rest, content := range contents {
		source, _ := stores.row(t, h.owner, "files/notes/"+rest)
		copied, held := stores.row(t, h.owner, "files/copy/"+rest)
		if !held || copied.ObjectID == source.ObjectID || copied.Checksum != source.Checksum {
			t.Fatalf("the copy of %s is %+v, from %+v", rest, copied, source)
		}
		for _, path := range []string{"files/notes/" + rest, "files/copy/" + rest} {
			read := h.call(t, http.MethodGet, h.object(path), nil)
			if read.Code != http.StatusOK || read.Body.String() != content {
				t.Errorf("%s reads %d %q, want %q", path, read.Code, read.Body, content)
			}
		}
	}
}

// TestStoreAPermanentFolderDeleteLeavesBothStoresEmpty is criterion 14 of
// spec 028 against the real stores: the rows under the folder, live and
// trashed, their versions and their bytes go, and what is beside the folder
// stays.
func TestStoreAPermanentFolderDeleteLeavesBothStoresEmpty(t *testing.T) {
	h, stores := tier(t)
	for path, content := range map[string]string{
		"files/notes/a.md":    "one",
		"files/notes/b.md":    "two",
		"files/notes-x/c.md":  "beside",
		"files/notes/deep/d":  "deep",
		"files/notes/old.txt": "old",
	} {
		if w := h.put(t, path, content); w.Code != http.StatusCreated {
			t.Fatalf("seeding %s answered %d: %s", path, w.Code, w.Body)
		}
	}
	if w := h.put(t, "files/notes/a.md", "one, again"); w.Code != http.StatusOK {
		t.Fatalf("the overwrite answered %d: %s", w.Code, w.Body)
	}
	if w := h.call(t, http.MethodDelete, h.object("files/notes/old.txt"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
	if got := len(stores.keys(t)); got != 6 {
		t.Fatalf("the bucket holds %d keys before the delete", got)
	}

	if w := h.call(t, http.MethodDelete, h.object("files/notes")+"?recursive=1&permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
	}
	if got := len(stores.keys(t)); got != 1 {
		t.Errorf("the bucket holds %d keys after the delete, want the one beside the folder", got)
	}
	for _, path := range []string{"files/notes/a.md", "files/notes/b.md", "files/notes/deep/d", "files/notes/old.txt"} {
		if _, held := stores.row(t, h.owner, path); held {
			t.Errorf("the folder delete left the row of %s", path)
		}
	}
	if _, held := stores.row(t, h.owner, "files/notes-x/c.md"); !held {
		t.Error("the folder delete took an object beside the folder")
	}
	var versions int
	if err := stores.db.Querier().QueryRow(t.Context(),
		`SELECT count(*) FROM file_versions WHERE owner = $1`, h.owner).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Errorf("the folder delete left %d versions", versions)
	}
}

// TestStoreAFolderMoveMakesNoBucketCallAndCarriesWhatKeysOnThePath is spec
// 030 against the real stores: every live object under the folder moves in
// one call with its id, its object and its key, no bucket call is made, the
// history, the bookmarks and the grants under the folder follow, a trashed
// object and a grant above the folder stay, and a second move onto the same
// destination is refused whole.
func TestStoreAFolderMoveMakesNoBucketCallAndCarriesWhatKeysOnThePath(t *testing.T) {
	h, stores := tier(t)
	h.seed(t, "files/notes/a.md", "one")
	h.seed(t, "files/notes/a.md", "two")
	h.seed(t, "files/notes/deep/b.md", "nested")
	h.seed(t, "files/notes/old.md", "in the trash")
	if w := h.call(t, http.MethodDelete, h.object("files/notes/old.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
	if w := h.call(t, http.MethodPut, "/v1/stars",
		strings.NewReader(`{"owner":"me","path":"files/notes/deep/b.md"}`),
		api.HeaderContentType, api.JSONMediaType); w.Code != http.StatusNoContent {
		t.Fatalf("the star answered %d: %s", w.Code, w.Body)
	}
	folder := stores.grant(t, h.owner, "files/notes")
	exact := stores.grant(t, h.owner, "files/notes/a.md")
	trashed := stores.grant(t, h.owner, "files/notes/old.md")
	parent := stores.grant(t, h.owner, "files")
	before := map[string]store.File{}
	for _, rest := range []string{"a.md", "deep/b.md"} {
		before[rest], _ = stores.row(t, h.owner, "files/notes/"+rest)
	}
	keys, calls := len(stores.keys(t)), h.bucket.Total()

	w := h.post(t, "files/notes", `{"move_to":"files/archive/notes","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the folder move answered %d: %s", w.Code, w.Body)
	}
	if got := h.bucket.Total() - calls; got != 0 {
		t.Fatalf("the folder move made %d bucket calls, and a key derives from an id", got)
	}
	if got := len(stores.keys(t)); got != keys {
		t.Errorf("the bucket holds %d keys after the move, %d before", got, keys)
	}
	for rest, row := range before {
		moved, held := stores.row(t, h.owner, "files/archive/notes/"+rest)
		if !held || moved.ID != row.ID || moved.ObjectID != row.ObjectID || moved.Checksum != row.Checksum {
			t.Errorf("%s moved as %+v from %+v", rest, moved, row)
		}
		if _, held := stores.row(t, h.owner, "files/notes/"+rest); held {
			t.Errorf("files/notes/%s still holds a row", rest)
		}
	}
	if row, held := stores.row(t, h.owner, "files/notes/old.md"); !held || row.DeletedAt == nil {
		t.Errorf("the trashed object is %+v, %t; it stays where it was", row, held)
	}
	versions, err := store.NewVersions().List(t.Context(), stores.db.Querier(), h.owner, "files/archive/notes/a.md", 0, 10)
	if err != nil || len(versions) != 1 {
		t.Errorf("the history after the folder move is %d rows, %v", len(versions), err)
	}
	stars, err := store.NewStars().List(t.Context(), stores.db.Querier(), h.owner, nil, store.StarCursor{}, 10, nil)
	if err != nil || len(stars) != 1 || stars[0].Path != "files/archive/notes/deep/b.md" {
		t.Errorf("the bookmarks after the folder move are %v, %v", stars, err)
	}
	for _, c := range []struct {
		grant store.Grant
		want  string
	}{
		{folder, "files/archive/notes"},
		{exact, "files/archive/notes/a.md"},
		{trashed, "files/notes/old.md"},
		{parent, "files"},
	} {
		if held := stores.readGrant(t, c.grant.ID); held.PathPrefix != c.want {
			t.Errorf("the grant on %q is on %q after the folder move, want %q", c.grant.PathPrefix, held.PathPrefix, c.want)
		}
	}
	read := h.call(t, http.MethodGet, h.object("files/archive/notes/a.md"), nil)
	if read.Code != http.StatusOK || read.Body.String() != "two" {
		t.Errorf("the moved object reads %d: %q", read.Code, read.Body)
	}

	// Moved back over a destination one row holds, the folder stays whole
	// where it is.
	h.seed(t, "files/notes/deep/b.md", "written where the folder was")
	if got := code(t, h.post(t, "files/archive/notes", `{"move_to":"files/notes","recursive":true}`)); got != api.CodePathTaken {
		t.Fatalf("a folder move onto a taken path is %q", got)
	}
	for _, rest := range []string{"a.md", "deep/b.md"} {
		if _, held := stores.row(t, h.owner, "files/archive/notes/"+rest); !held {
			t.Errorf("a refused folder move took files/archive/notes/%s", rest)
		}
	}
}
