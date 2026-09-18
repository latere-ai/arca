// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
)

// digest is the checksum a put of this content answers.
func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestAPutCreatesThenOverwritesAndAnswersTheChecksumOfTheBytes(t *testing.T) {
	h := newHarness(t)
	w := h.put(t, "files/notes/plan.md", "first")
	if w.Code != http.StatusCreated {
		t.Fatalf("the create answered %d: %s", w.Code, w.Body)
	}
	var created Object
	decode(t, w, &created)
	if created.Checksum != digest("first") || created.ChecksumKind != "sha256" {
		t.Fatalf("the create answered %+v", created)
	}
	if created.Size != 5 || created.ContentType != "text/markdown" {
		t.Fatalf("the create answered %+v", created)
	}
	if got := w.Header().Get(api.HeaderETag); got != `"`+digest("first")+`"` {
		t.Fatalf("the ETag is %q", got)
	}

	again := h.put(t, "files/notes/plan.md", "second")
	if again.Code != http.StatusOK {
		t.Fatalf("the overwrite answered %d: %s", again.Code, again.Body)
	}
	// The bytes of the two writes are two objects, so the superseded ones
	// are still in the bucket behind a version row.
	if got := len(h.objects.Keys()); got != 2 {
		t.Fatalf("two writes left %d objects", got)
	}
	if got := h.store.actions(); len(got) != 2 || got[0] != EventPut || got[1] != EventPut {
		t.Fatalf("the log holds %v", got)
	}
	if held := h.store.usage[h.owner]; held != 11 {
		t.Fatalf("the space holds %d bytes of the eleven two writes put in it", held)
	}
}

func TestAPutIsRefusedWithoutALengthAndAboveTheTwoSizes(t *testing.T) {
	h := newHarness(t)
	// A body that streams off the socket with no declared length cannot be
	// admitted: the store needs the length up front and this server buffers
	// no body to find it.
	chunked := h.request(http.MethodPut, h.object("files/a.md"), strings.NewReader("body"))
	chunked.ContentLength = -1
	if got := code(t, h.send(chunked)); got != api.CodeLengthRequired {
		t.Fatalf("a body with no declared length is %q", got)
	}
	if h.bucket.Calls(blob.MethodPut) != 0 {
		t.Error("a body with no declared length reached the bucket")
	}

	over := h.put(t, "files/a.md", strings.Repeat("x", inlineBytes+1))
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body above the inline size answered %d", over.Code)
	}
	if got := code(t, over); got != api.CodeObjectTooLarge {
		t.Fatalf("a body above the inline size is %q", got)
	}
	if !strings.Contains(over.Body.String(), "/v1/uploads") {
		t.Errorf("the refusal does not name the session API:\n%s", over.Body)
	}

	small := newHarness(t, func(o *Options) { o.Config = configOf(4, 8) })
	huge := small.put(t, "files/a.md", strings.Repeat("x", 9))
	if got := code(t, huge); got != api.CodeObjectTooLarge {
		t.Fatalf("a body above the largest object is %q", got)
	}
	if strings.Contains(huge.Body.String(), "/v1/uploads") {
		t.Errorf("a body no route accepts was pointed at the session API:\n%s", huge.Body)
	}
	if small.bucket.Calls(blob.MethodPut) != 0 {
		t.Error("a refused write reached the bucket")
	}
}

func TestAWriteWithNeitherPreconditionSucceedsOnAnyPathUnderFiles(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"files/plan.md", "files/memory/state.json", "files/agents/notes.md", "files/public/logo.png",
	} {
		if w := h.put(t, path, "x"); w.Code != http.StatusCreated {
			t.Errorf("a write of %q answered %d: %s", path, w.Code, w.Body)
		}
	}
}

func TestIfMatchIsACompareAndSwapAndIfNoneMatchIsCreateOnly(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")

	stale := h.put(t, "files/plan.md", "second", api.HeaderIfMatch, `"`+digest("nothing")+`"`)
	if got := code(t, stale); got != api.CodePreconditionFailed {
		t.Fatalf("a stale If-Match is %q", got)
	}
	fresh := h.put(t, "files/plan.md", "second", api.HeaderIfMatch, `"`+digest("first")+`"`)
	if fresh.Code != http.StatusOK {
		t.Fatalf("a fresh If-Match answered %d: %s", fresh.Code, fresh.Body)
	}
	occupied := h.put(t, "files/plan.md", "third", api.HeaderIfNoneMatch, "*")
	if got := code(t, occupied); got != api.CodePreconditionFailed {
		t.Fatalf("a create-only write onto a live path is %q", got)
	}
	empty := h.put(t, "files/other.md", "third", api.HeaderIfNoneMatch, "*")
	if empty.Code != http.StatusCreated {
		t.Fatalf("a create-only write onto an empty path answered %d: %s", empty.Code, empty.Body)
	}
	weak := h.put(t, "files/plan.md", "fourth", api.HeaderIfMatch, `W/"`+digest("first")+`"`)
	if got := code(t, weak); got != api.CodeInvalidField {
		t.Fatalf("a weak validator is %q", got)
	}
}

func TestACreateOnlyWriteRevivesATrashedPath(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
	revived := h.put(t, "files/plan.md", "again", api.HeaderIfNoneMatch, "*")
	if revived.Code != http.StatusCreated {
		t.Fatalf("a create-only write onto a trashed path answered %d: %s", revived.Code, revived.Body)
	}
	// The trashed content became a version, so a revive keeps the history
	// the path had.
	if got := len(h.store.versions); got != 1 {
		t.Fatalf("the revived path holds %d versions", got)
	}
}

func TestAWriteRefusedByTheLedgerLeavesNoRowAndNoBytes(t *testing.T) {
	h := newHarness(t)
	h.endpoint.Allow(stub.Rule{
		Subject: "*", Action: "*", Resource: "*", Allow: true,
		Limits: map[string]any{"quota_bytes": 4},
	})
	w := h.put(t, "files/plan.md", "five!")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a write past the answer's limit answered %d: %s", w.Code, w.Body)
	}
	if got := code(t, w); got != api.CodeQuotaExceeded {
		t.Fatalf("a write past the answer's limit is %q", got)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md"); err == nil {
		t.Error("a refused write left a row")
	}
	if got := len(h.objects.Keys()); got != 0 {
		t.Errorf("a refused write left %d objects for the reaper", got)
	}
}

func TestALedgerThatCannotBeWrittenTakesTheWriteDownWithIt(t *testing.T) {
	h := newHarness(t)
	h.ledger.refuse = errors.New("the ledger is unreachable")
	w := h.put(t, "files/plan.md", "first")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a write whose charge failed answered %d: %s", w.Code, w.Body)
	}
	if got := code(t, w); got != api.CodeStorageUnavailable {
		t.Fatalf("a write whose charge failed is %q", got)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md"); err == nil {
		t.Error("a write whose charge failed left a row")
	}
	if got := len(h.objects.Keys()); got != 0 {
		t.Errorf("a write whose charge failed left %d objects", got)
	}
}

func TestAWriteThatFailsAfterTheBucketLeavesNoRow(t *testing.T) {
	h := newHarness(t)
	h.store.failTx = errors.New("the database is unreachable")
	w := h.put(t, "files/plan.md", "first")
	if w.Code < 500 {
		t.Fatalf("a write whose commit failed answered %d: %s", w.Code, w.Body)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md"); err == nil {
		t.Error("a write whose commit failed left a row")
	}
	// The key was fresh and nothing else could reference it, so the handler
	// removed it rather than leaving the reaper anything to find.
	if got := len(h.objects.Keys()); got != 0 {
		t.Errorf("a write whose commit failed left %d objects", got)
	}
}

func TestABucketThatRefusesTheWriteIsAnOutageAndNotARefusal(t *testing.T) {
	h := newHarness(t)
	h.bucket.FailNth(blob.MethodPut, 1, errors.New("the bucket is unreachable"))
	w := h.put(t, "files/plan.md", "first")
	if got := code(t, w); got != api.CodeStorageUnavailable {
		t.Fatalf("a bucket that refused the write is %q", got)
	}
}

// codeOf reads the row of the error table an error names, for a case that
// calls a helper rather than a route.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var refusal *api.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the error names no row of the table: %v", err)
	}
	return refusal.Code
}
