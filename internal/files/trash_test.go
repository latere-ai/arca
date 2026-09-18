// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/api"
)

func TestADeleteUnderFilesIsSoftAndOneUnderWorkspacesIsHard(t *testing.T) {
	h := newHarness(t)
	row := h.seed(t, "files/plan.md", "first")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
	trashed, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md")
	if err != nil || trashed.DeletedAt == nil {
		t.Fatalf("the row is %+v, %v", trashed, err)
	}
	// Trashed bytes count towards the space for the whole window, so a
	// reader of the figure is not surprised a month later.
	if held := h.store.usage[h.owner]; held != 5 {
		t.Errorf("the space holds %d bytes after a trash", held)
	}
	if _, ok := h.objects.Bytes(row.ObjectID.Key("arca/")); !ok {
		t.Error("a trash removed the bytes")
	}

	under := h.write(t, "workspaces/build/main.go", "package main")
	if w := h.call(t, http.MethodDelete, h.object("workspaces/build/main.go"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("a delete under the workspaces plane answered %d: %s", w.Code, w.Body)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "workspaces/build/main.go"); err == nil {
		t.Error("a delete under the workspaces plane left a row")
	}
	if _, ok := h.objects.Bytes(under.ObjectID.Key("arca/")); ok {
		t.Error("a delete under the workspaces plane left the bytes")
	}
}

func TestAPermanentDeleteTakesTheHistoryAndTheBytesWithIt(t *testing.T) {
	h := newHarness(t)
	first := h.seed(t, "files/plan.md", "one")
	second := h.seed(t, "files/plan.md", "two")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md")+"?permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("a permanent delete answered %d: %s", w.Code, w.Body)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md"); err == nil {
		t.Error("a permanent delete left a row")
	}
	if got := len(h.store.versions); got != 0 {
		t.Errorf("a permanent delete left %d versions", got)
	}
	for _, id := range []string{first.ObjectID.Key("arca/"), second.ObjectID.Key("arca/")} {
		if _, ok := h.objects.Bytes(id); ok {
			t.Errorf("a permanent delete left the bytes at %s", id)
		}
	}
	if held := h.store.usage[h.owner]; held != 0 {
		t.Errorf("the space holds %d bytes after a permanent delete", held)
	}
}

func TestPruningOneVersionLeavesTheLiveObjectAlone(t *testing.T) {
	h := newHarness(t)
	first := h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md")+"?version=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the prune answered %d: %s", w.Code, w.Body)
	}
	if got := len(h.store.versions); got != 0 {
		t.Errorf("the prune left %d versions", got)
	}
	if _, ok := h.objects.Bytes(first.ObjectID.Key("arca/")); ok {
		t.Error("the prune left the bytes of the version it removed")
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md"); err != nil {
		t.Error("the prune removed the live row")
	}
	if got := code(t, h.call(t, http.MethodDelete, h.object("files/plan.md")+"?version=9", nil)); got != api.CodeNotFound {
		t.Errorf("a prune of a version that is not there is %q", got)
	}
}

func TestTheTrashListsWhatIsRestorableAndRestoreReturnsTheBytes(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"a", "b"} {
		h.seed(t, "files/"+name+".md", name)
		if w := h.call(t, http.MethodDelete, h.object("files/"+name+".md"), nil); w.Code != http.StatusNoContent {
			t.Fatalf("the trash of %q answered %d", name, w.Code)
		}
	}
	var page struct {
		Entries    []Trashed `json:"entries"`
		NextCursor string    `json:"next_cursor"`
	}
	decode(t, h.call(t, http.MethodGet, "/v1/trash", nil), &page)
	if len(page.Entries) != 2 || page.Entries[0].Path != "files/b.md" {
		t.Fatalf("the trash is %+v", page.Entries)
	}
	if page.Entries[0].PurgesAt == "" || page.Entries[0].DeletedAt == "" {
		t.Errorf("the entry names no deadline: %+v", page.Entries[0])
	}

	// One page at a time, resuming on the cursor the listing wrote.
	var first struct {
		Entries    []Trashed `json:"entries"`
		NextCursor string    `json:"next_cursor"`
	}
	decode(t, h.call(t, http.MethodGet, "/v1/trash?limit=1", nil), &first)
	if len(first.Entries) != 1 || first.NextCursor == "" {
		t.Fatalf("the first page is %+v", first)
	}
	var next struct {
		Entries    []Trashed `json:"entries"`
		NextCursor string    `json:"next_cursor"`
	}
	decode(t, h.call(t, http.MethodGet, "/v1/trash?limit=1&cursor="+first.NextCursor, nil), &next)
	if len(next.Entries) != 1 || next.Entries[0].Path == first.Entries[0].Path {
		t.Fatalf("the second page is %+v", next)
	}
	if next.NextCursor != "" {
		t.Errorf("the last page named the cursor %q", next.NextCursor)
	}
	if got := code(t, h.call(t, http.MethodGet, "/v1/trash?cursor=nonsense", nil)); got != api.CodeInvalidField {
		t.Errorf("a cursor this listing did not write is %q", got)
	}

	restored := h.call(t, http.MethodPost, "/v1/trash/restore",
		strings.NewReader(`{"owner":"me","path":"files/a.md"}`), api.HeaderContentType, api.JSONMediaType)
	if restored.Code != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", restored.Code, restored.Body)
	}
	read := h.call(t, http.MethodGet, h.object("files/a.md"), nil)
	if read.Code != http.StatusOK || read.Body.String() != "a" {
		t.Fatalf("the restored object reads %d: %q", read.Code, read.Body)
	}
}

func TestARestoreOntoAReoccupiedPathIsAConflictAndOnePastTheWindowIsGone(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d", w.Code)
	}
	h.seed(t, "files/plan.md", "second")
	conflict := h.call(t, http.MethodPost, "/v1/trash/restore",
		strings.NewReader(`{"owner":"me","path":"files/plan.md"}`), api.HeaderContentType, api.JSONMediaType)
	if got := code(t, conflict); got != api.CodePathTaken {
		t.Fatalf("a restore onto a reoccupied path is %q", got)
	}

	h.seed(t, "files/other.md", "third")
	if w := h.call(t, http.MethodDelete, h.object("files/other.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d", w.Code)
	}
	// The window closes without the clock of the rows moving, which is what
	// makes the retention a predicate rather than a convention.
	h.clock = h.clock.Add(721 * time.Hour)
	var page struct {
		Entries []Trashed `json:"entries"`
	}
	decode(t, h.call(t, http.MethodGet, "/v1/trash", nil), &page)
	if len(page.Entries) != 0 {
		t.Fatalf("the trash past the window is %+v", page.Entries)
	}
	expired := h.call(t, http.MethodPost, "/v1/trash/restore",
		strings.NewReader(`{"owner":"me","path":"files/other.md"}`), api.HeaderContentType, api.JSONMediaType)
	if got := code(t, expired); got != api.CodeNotFound {
		t.Fatalf("a restore past the window is %q", got)
	}
	if got := code(t, h.call(t, http.MethodPost, "/v1/trash/restore",
		strings.NewReader(`{"owner":"me","path":"files/never.md"}`), api.HeaderContentType, api.JSONMediaType)); got != api.CodeNotFound {
		t.Error("a restore of a path that was never trashed was accepted")
	}
}

func TestEmptyingTheTrashRemovesTheRowsTheHistoryAndTheBytes(t *testing.T) {
	h := newHarness(t)
	first := h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")
	h.seed(t, "files/other.md", "three")
	for _, path := range []string{"files/plan.md", "files/other.md"} {
		if w := h.call(t, http.MethodDelete, h.object(path), nil); w.Code != http.StatusNoContent {
			t.Fatalf("the trash of %q answered %d", path, w.Code)
		}
	}
	w := h.call(t, http.MethodDelete, "/v1/trash", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("emptying the trash answered %d: %s", w.Code, w.Body)
	}
	var out purged
	decode(t, w, &out)
	if out.Purged != 2 {
		t.Fatalf("emptying the trash removed %d entries", out.Purged)
	}
	if got := len(h.store.versions); got != 0 {
		t.Errorf("emptying the trash left %d versions", got)
	}
	if _, ok := h.objects.Bytes(first.ObjectID.Key("arca/")); ok {
		t.Error("emptying the trash left the bytes of a version")
	}
	if held := h.store.usage[h.owner]; held != 0 {
		t.Errorf("the space holds %d bytes after its trash was emptied", held)
	}
	if got := code(t, h.call(t, http.MethodDelete, "/v1/trash?path=files/never.md", nil)); got != api.CodeNotFound {
		t.Errorf("purging a path that is not in the trash is %q", got)
	}
	if got := code(t, h.call(t, http.MethodDelete, "/v1/trash?path=nowhere/x", nil)); got != api.CodeUnknownPlane {
		t.Errorf("purging a path that is not one is %q", got)
	}
}

func TestPurgingOnePathLeavesTheRest(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"a", "b"} {
		h.seed(t, "files/"+name+".md", name)
		if w := h.call(t, http.MethodDelete, h.object("files/"+name+".md"), nil); w.Code != http.StatusNoContent {
			t.Fatalf("the trash of %q answered %d", name, w.Code)
		}
	}
	w := h.call(t, http.MethodDelete, "/v1/trash?path=files/a.md", nil)
	var out purged
	decode(t, w, &out)
	if out.Purged != 1 {
		t.Fatalf("purging one path removed %d entries", out.Purged)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/b.md"); err != nil {
		t.Error("purging one path took the other")
	}
}
