// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// errOutage is a store that answers nothing at all, which no handler may
// read as a verdict.
var errOutage = errors.New("the store is unreachable")

// TestEveryHandlerReadsAnOutageAsAnOutage: a database that answers nothing
// is not a missing object, a conflict, or a refusal. It is 503, and every
// handler of this package says so.
func TestEveryHandlerReadsAnOutageAsAnOutage(t *testing.T) {
	for i, name := range routeNames {
		t.Run(name, func(t *testing.T) {
			h := seeded(t)
			r := h.routes()[i]
			h.store.failAll = errOutage
			w := h.drive(t, r.method, r.target, r.body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s against a store that answers nothing is %d: %s", name, w.Code, w.Body)
			}
			if r.method == http.MethodHead {
				return
			}
			if got := code(t, w); got != api.CodeStorageUnavailable {
				t.Fatalf("%s against a store that answers nothing is %q", name, got)
			}
			// Nothing above 499 names a store, a query, a key or an internal
			// type, even in the developer detail.
			for _, leak := range []string{"unreachable", "memory:", "SELECT", "arca/"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("the answer names %q:\n%s", leak, w.Body)
				}
			}
		})
	}
}

// TestEveryWriteThatCannotBeRecordedIsRefused: the rows a handler reads
// answer, and every statement that would change one does not. A mutation
// then reaches its own store failure rather than the lookup's, which is the
// branch that decides whether a half-written change can reach a caller as a
// success.
func TestEveryWriteThatCannotBeRecordedIsRefused(t *testing.T) {
	for i, name := range routeNames {
		if !slices.Contains([]string{
			"put", "move", "restore version", "delete", "restore", "purge", "star",
		}, name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			h := seeded(t)
			r := h.routes()[i]
			h.store.failWrite = errOutage
			w := h.drive(t, r.method, r.target, r.body)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s whose write failed is %d: %s", name, w.Code, w.Body)
			}
		})
	}
	// The three write arms each refuse the same way, so the precondition a
	// request carried decides which one fails and not what the failure is.
	for _, precondition := range [][]string{
		{},
		{api.HeaderIfNoneMatch, "*"},
		{api.HeaderIfMatch, `"` + digest("again") + `"`},
	} {
		h := seeded(t)
		h.store.failWrite = errOutage
		path := "files/plan.md"
		if len(precondition) > 0 && precondition[1] == "*" {
			path = "files/fresh.md"
		}
		if got := code(t, h.put(t, path, "x", precondition...)); got != api.CodeStorageUnavailable {
			t.Errorf("a write with %v whose statement failed is %q", precondition, got)
		}
	}
	// A prune of one version and a delete of a whole history reach their own
	// statements too.
	h := seeded(t)
	h.store.failWrite = errOutage
	if got := code(t, h.call(t, http.MethodDelete, h.object("files/plan.md")+"?version=1", nil)); got != api.CodeStorageUnavailable {
		t.Errorf("a prune whose statement failed is %q", got)
	}
	if got := code(t, h.call(t, http.MethodDelete, h.object("files/plan.md")+"?permanent=1", nil)); got != api.CodeStorageUnavailable {
		t.Errorf("a permanent delete whose statement failed is %q", got)
	}
	unstar := seeded(t)
	unstar.store.failWrite = errOutage
	if got := code(t, unstar.call(t, http.MethodDelete, "/v1/stars?owner=me&path=files/plan.md", nil)); got != api.CodeStorageUnavailable {
		t.Errorf("an unstar whose statement failed is %q", got)
	}
}

// TestTheRefusalNamesWhichHeaderRefusedTheWrite: a caller that has to retry
// needs to know which precondition failed and what the path holds.
func TestTheRefusalNamesWhichHeaderRefusedTheWrite(t *testing.T) {
	for _, c := range []struct {
		pre  api.Precondition
		want string
	}{
		{api.Precondition{CreateOnly: true}, api.HeaderIfNoneMatch},
		{api.Precondition{IfMatch: []string{"abc"}}, api.HeaderIfMatch},
		{api.Precondition{IfNoneMatch: []string{"abc"}}, api.HeaderIfNoneMatch},
	} {
		err := refusePrecondition(c.pre, "")
		if got := codeOf(t, err); got != api.CodePreconditionFailed {
			t.Errorf("a refused precondition is %q", got)
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "nothing") {
			t.Errorf("the refusal reads %q and names neither %s nor what the path holds", err, c.want)
		}
	}
}

// TestABucketThatWillNotSignIsAnOutageToo: a redirect a caller cannot be
// given is a failure of the store and not of the request.
func TestABucketThatWillNotSignIsAnOutageToo(t *testing.T) {
	h := newHarness(t)
	h.write(t, "files/large.md", strings.Repeat("x", inlineBytes+1))
	h.bucket.FailNth(blob.MethodPresignGet, 1, errOutage)
	if got := code(t, h.call(t, http.MethodGet, h.object("files/large.md"), nil)); got != api.CodeStorageUnavailable {
		t.Fatalf("a bucket that would not sign is %q", got)
	}
	manifest := newHarness(t)
	manifest.write(t, "files/large.md", strings.Repeat("x", inlineBytes+1))
	manifest.bucket.FailNth(blob.MethodPresignGet, 1, errOutage)
	if got := code(t, manifest.call(t, http.MethodGet, "/v1/files/materialize", nil)); got != api.CodeStorageUnavailable {
		t.Fatalf("a manifest whose bucket would not sign is %q", got)
	}

	reading := newHarness(t)
	reading.seed(t, "files/small.md", "small")
	reading.bucket.FailNth(blob.MethodGet, 1, errOutage)
	if got := code(t, reading.call(t, http.MethodGet, reading.object("files/small.md"), nil)); got != api.CodeStorageUnavailable {
		t.Fatalf("a bucket that would not read is %q", got)
	}
}

// TestBytesNothingCanRemoveAreLeftForTheReaper: the rows are already gone
// when the bytes are dropped, so a reference check that will not answer and
// a bucket that will not delete both leave an orphan and neither fails the
// request the caller already had answered.
func TestBytesNothingCanRemoveAreLeftForTheReaper(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.References = refused{} })
	row := h.write(t, "workspaces/build/main.go", "package main")
	if w := h.call(t, http.MethodDelete, h.object("workspaces/build/main.go"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the delete answered %d: %s", w.Code, w.Body)
	}
	if _, held := h.objects.Bytes(row.ObjectID.Key("arca/")); !held {
		t.Error("the bytes went while the reference check was unreadable")
	}

	stubborn := newHarness(t)
	row = stubborn.write(t, "workspaces/build/main.go", "package main")
	stubborn.bucket.FailNth(blob.MethodDelete, 1, errOutage)
	if w := stubborn.call(t, http.MethodDelete, stubborn.object("workspaces/build/main.go"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the delete answered %d: %s", w.Code, w.Body)
	}
	if _, err := stubborn.store.Get(t.Context(), nil, stubborn.owner, "workspaces/build/main.go"); err == nil {
		t.Error("a bucket that would not delete kept the row")
	}
	if _, held := stubborn.objects.Bytes(row.ObjectID.Key("arca/")); !held {
		t.Error("the bucket that refused the delete lost the bytes anyway")
	}
}

// refused is a reference check that answers nothing, which is not a licence
// to delete.
type refused struct{}

func (refused) Referenced(context.Context, store.Querier, object.ID) (bool, error) {
	return false, errOutage
}

// TestTheHelpersEveryHandlerShares holds the small rules one route or
// another reaches but no route reaches twice.
func TestTheHelpersEveryHandlerShares(t *testing.T) {
	if got := contentType(""); got != blob.DefaultContentType {
		t.Errorf("a write that declared no media type records %q", got)
	}
	if got := lastSegment("plan.md"); got != "plan.md" {
		t.Errorf("the download name of a path with no slash is %q", got)
	}
	if got := subtree("files/notes/"); got != "files/notes/" {
		t.Errorf("a prefix that already ends in a slash became %q", got)
	}
	if got := declared(-1); got != nil {
		t.Errorf("a body with no length carries the size %v", *got)
	}
	// A prefix a listing may name, and one it may not.
	if _, err := ValidatePrefix("workspaces/build"); err != nil {
		t.Errorf("a workspace root is not a subtree a listing may name: %v", err)
	}
	for _, prefix := range []string{"workspaces/../build", "workspaces/build/", "workspaces/"} {
		if _, err := ValidatePrefix(prefix); err == nil {
			t.Errorf("%q was accepted as a prefix", prefix)
		}
	}
	// The defaults of a build whose spec 010 has not landed record nothing
	// and refuse nothing.
	noLedger{}.Append(t.Context(), nil, Event{Action: EventPut})
}

// TestTheAliasMeNeedsACallerAndAResponseRendersTheSubjectInFull.
func TestTheAliasMeNeedsACallerAndAResponseRendersTheSubjectInFull(t *testing.T) {
	s := New(Options{DB: newMemory(), Config: configOf(8, 16)})
	if _, err := s.owner(t.Context(), meAlias); err == nil {
		t.Fatal("the alias me resolved with no caller")
	} else if got := codeOf(t, err); got != api.CodeUnauthenticated {
		t.Fatalf("the alias me with no caller is %q", got)
	}
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	// A star names the space in full, so a client that stores what it read
	// can send it back.
	var page struct {
		Entries []Starred `json:"entries"`
	}
	if w := h.call(t, http.MethodPut, "/v1/stars",
		strings.NewReader(`{"owner":"me","path":"files/plan.md"}`),
		api.HeaderContentType, api.MediaJSON); w.Code != http.StatusNoContent {
		t.Fatalf("the star answered %d", w.Code)
	}
	decode(t, h.call(t, http.MethodGet, "/v1/stars", nil), &page)
	if len(page.Entries) != 1 || page.Entries[0].Owner != h.owner {
		t.Fatalf("the star names the space %q", page.Entries[0].Owner)
	}
	// The subject read back addresses the same space.
	again := h.call(t, http.MethodGet, "/v1/trash?owner="+page.Entries[0].Owner, nil)
	if again.Code != http.StatusOK {
		t.Fatalf("the subject read back answered %d: %s", again.Code, again.Body)
	}
}
