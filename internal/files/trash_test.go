// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/api"
)

func TestADeleteIsSoftAndKeepsTheBytes(t *testing.T) {
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

// TestADeleteHoldsToItsPreconditionsAsAWriteDoes: a delete conditioned on a
// checksum the object no longer carries, because another session wrote it
// since, is refused with the 412 a write answers and leaves the object
// where it was, trashed or not; one conditioned on what is there goes to
// the trash with its bytes.
func TestADeleteHoldsToItsPreconditionsAsAWriteDoes(t *testing.T) {
	read := `"` + digest("read") + `"`
	current := `"` + digest("written since") + `"`
	for _, c := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"a stale If-Match", []string{api.HeaderIfMatch, read}, api.CodePreconditionFailed},
		{"a list of stale checksums", []string{api.HeaderIfMatch, read + `, "abc"`}, api.CodePreconditionFailed},
		{"If-None-Match: *", []string{api.HeaderIfNoneMatch, "*"}, api.CodePreconditionFailed},
		{"an If-None-Match naming what is there", []string{api.HeaderIfNoneMatch, current}, api.CodePreconditionFailed},
		{"a weak validator", []string{api.HeaderIfMatch, "W/" + current}, api.CodeInvalidField},
	} {
		for _, query := range []string{"", "?permanent=1"} {
			t.Run(c.name+query, func(t *testing.T) {
				h := newHarness(t)
				h.seed(t, "files/memory/note.md", "read")
				row := h.seed(t, "files/memory/note.md", "written since")
				w := h.call(t, http.MethodDelete, h.object("files/memory/note.md")+query, nil, c.headers...)
				if got := code(t, w); got != c.want {
					t.Fatalf("the delete is %q, want %q", got, c.want)
				}
				if c.want == api.CodePreconditionFailed {
					if w.Code != http.StatusPreconditionFailed {
						t.Errorf("a refused delete answered %d", w.Code)
					}
					// The detail names the header and what the path holds,
					// as a refused write's does.
					if !strings.Contains(w.Body.String(), c.headers[0]) || !strings.Contains(w.Body.String(), digest("written since")) {
						t.Errorf("the refusal names neither the header nor the checksum:\n%s", w.Body)
					}
				}
				held, live := h.read(t, "files/memory/note.md")
				if !live || held.DeletedAt != nil || held.Checksum != row.Checksum {
					t.Fatalf("a refused delete left the row %+v", held)
				}
				if got := len(h.store.versions); got != 1 {
					t.Errorf("a refused delete left %d versions", got)
				}
				if _, ok := h.objects.Bytes(row.ObjectID.Key("arca/")); !ok {
					t.Error("a refused delete removed the bytes")
				}
			})
		}
	}

	for _, headers := range [][]string{
		{api.HeaderIfMatch, current},
		{api.HeaderIfMatch, read + ", " + current},
		{api.HeaderIfMatch, "*"},
		{api.HeaderIfNoneMatch, read},
	} {
		h := newHarness(t)
		h.seed(t, "files/memory/note.md", "read")
		row := h.seed(t, "files/memory/note.md", "written since")
		if w := h.call(t, http.MethodDelete, h.object("files/memory/note.md"), nil, headers...); w.Code != http.StatusNoContent {
			t.Fatalf("a delete with %v answered %d: %s", headers, w.Code, w.Body)
		}
		// The delete is the soft one it is without a precondition: the row
		// is in the trash and the bytes stay for a restore.
		if got := h.trashed(); !slices.Equal(got, []string{"files/memory/note.md"}) {
			t.Errorf("a delete with %v left %v in the trash", headers, got)
		}
		if _, ok := h.objects.Bytes(row.ObjectID.Key("arca/")); !ok {
			t.Errorf("a delete with %v removed the bytes", headers)
		}
	}
}

// TestIfMatchStarDeletesOnlyWhatExists: If-Match: * asks for anything that
// exists, so it deletes a live object and finds nothing at a path that
// holds none or only a trashed one, as a delete with no precondition does.
func TestIfMatchStarDeletesOnlyWhatExists(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	star := []string{api.HeaderIfMatch, "*"}
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil, star...); w.Code != http.StatusNoContent {
		t.Fatalf("If-Match: * on a live object answered %d: %s", w.Code, w.Body)
	}
	for _, path := range []string{"files/plan.md", "files/never.md"} {
		for _, query := range []string{"", "?permanent=1"} {
			if got := code(t, h.call(t, http.MethodDelete, h.object(path)+query, nil, star...)); got != api.CodeNotFound {
				t.Errorf("If-Match: * on %s%s is %q", path, query, got)
			}
		}
	}
	if got := h.trashed(); !slices.Equal(got, []string{"files/plan.md"}) {
		t.Errorf("the trash holds %v", got)
	}
}

// TestAPruneHoldsToTheVersionsChecksum: a version's ETag is its own
// checksum, so a prune conditioned on another is refused and keeps the
// version, and one conditioned on it removes it.
func TestAPruneHoldsToTheVersionsChecksum(t *testing.T) {
	h := newHarness(t)
	first := h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")
	target := h.object("files/plan.md") + "?version=1"
	for _, headers := range [][]string{
		{api.HeaderIfMatch, `"` + digest("two") + `"`},
		{api.HeaderIfNoneMatch, `"` + digest("one") + `"`},
		{api.HeaderIfNoneMatch, "*"},
	} {
		w := h.call(t, http.MethodDelete, target, nil, headers...)
		if got := code(t, w); got != api.CodePreconditionFailed {
			t.Fatalf("a prune with %v is %q", headers, got)
		}
		if !strings.Contains(w.Body.String(), "version 1") {
			t.Errorf("the refusal does not name the version:\n%s", w.Body)
		}
		if got := len(h.store.versions); got != 1 {
			t.Fatalf("a refused prune left %d versions", got)
		}
		if _, ok := h.objects.Bytes(first.ObjectID.Key("arca/")); !ok {
			t.Fatal("a refused prune removed the version's bytes")
		}
	}
	if w := h.call(t, http.MethodDelete, target, nil, api.HeaderIfMatch, `"`+digest("one")+`"`); w.Code != http.StatusNoContent {
		t.Fatalf("a prune conditioned on the version answered %d: %s", w.Code, w.Body)
	}
	if got := len(h.store.versions); got != 0 {
		t.Errorf("the prune left %d versions", got)
	}
}

// TestAFolderDeleteRefusesAPrecondition: a folder carries no checksum, so a
// precondition on deleting one is refused rather than ignored, and nothing
// under the folder moves.
func TestAFolderDeleteRefusesAPrecondition(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/notes/a.md", "a")
	for _, headers := range [][]string{
		{api.HeaderIfMatch, `"` + digest("a") + `"`},
		{api.HeaderIfMatch, "*"},
		{api.HeaderIfNoneMatch, "*"},
	} {
		for _, query := range []string{"?recursive=1", "?recursive=1&permanent=1"} {
			w := h.call(t, http.MethodDelete, h.object("files/notes")+query, nil, headers...)
			if got := code(t, w); got != api.CodeInvalidField {
				t.Fatalf("a folder delete %s with %v is %q", query, headers, got)
			}
			if !strings.Contains(w.Body.String(), headers[0]) {
				t.Errorf("the refusal does not name %s:\n%s", headers[0], w.Body)
			}
		}
	}
	if got := h.live(); !slices.Equal(got, []string{"files/notes/a.md"}) {
		t.Errorf("a refused folder delete left %v live", got)
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

// The id-addressed restore of spec 012, which the node binds as the file arm
// of the administrative restore across owners. It asks nothing: the caller
// asked space.admin before it got here, and asking file.restore as well would
// ask an administrator for a permission on a space it does not own.
func TestRestoringATrashedObjectByItsIDBringsThePathBack(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/reports/q3.pdf", "figures")
	if w := h.call(t, http.MethodDelete, h.object("files/reports/q3.pdf"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
	trashed, ok := h.read(t, "files/reports/q3.pdf")
	if !ok || trashed.DeletedAt == nil {
		t.Fatalf("the row is %+v", trashed)
	}

	h.asked.asked = nil
	back, err := h.service.RestoreTrashed(t.Context(), h.owner, trashed.ID)
	if err != nil {
		t.Fatalf("RestoreTrashed: %v", err)
	}
	if back.Path != "files/reports/q3.pdf" || back.DeletedAt != nil {
		t.Fatalf("the restored row is %+v", back)
	}
	if live, ok := h.read(t, "files/reports/q3.pdf"); !ok || live.DeletedAt != nil {
		t.Fatalf("the path is %+v after the restore", live)
	}
	if asked := h.asked.actions(); len(asked) != 0 {
		t.Errorf("the restore across owners asked %v; the caller was already allowed", asked)
	}
	if !slices.Contains(h.store.actions(), EventRestore) {
		t.Errorf("the restore appended %v and no restore row", h.store.actions())
	}
}

// An id of another space, one that names nothing, one past the retention
// window, and a string that is not an id at all are one answer, which the
// node's adapter renders as the one refusal of the administrative restore.
func TestRestoringATrashedObjectRefusesWhatTheSpaceCannotBringBack(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	if w := h.call(t, http.MethodDelete, h.object("files/plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
	trashed, _ := h.read(t, "files/plan.md")

	for _, tc := range []struct {
		name  string
		owner string
		id    string
		aged  time.Duration
	}{
		{name: "another space's id", owner: "https://issuer.example|c1d0", id: trashed.ID},
		{name: "an id that names nothing", owner: h.owner, id: "no-such-id"},
		{name: "past the retention window", owner: h.owner, id: trashed.ID, aged: 721 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.clock = time.Now().Add(tc.aged)
			defer func() { h.clock = time.Now() }()
			if _, err := h.service.RestoreTrashed(t.Context(), tc.owner, tc.id); !errors.Is(err, ErrNotTrashed) {
				t.Fatalf("RestoreTrashed answered %v", err)
			}
		})
	}
}

// A store that will not answer is a fault and not a missing object: the
// adapter above must not read it as an id that names nothing.
func TestRestoringATrashedObjectCarriesAStoreFailure(t *testing.T) {
	h := newHarness(t)
	h.store.failAll = errOutage
	_, err := h.service.RestoreTrashed(t.Context(), h.owner, "id")
	if err == nil || errors.Is(err, ErrNotTrashed) {
		t.Fatalf("a failed restore answered %v", err)
	}
}
