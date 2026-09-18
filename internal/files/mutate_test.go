// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/api"
)

// post drives the one verb route on an object.
func (h *harness) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return h.call(t, http.MethodPost, h.object(path), strings.NewReader(body),
		api.HeaderContentType, api.JSONMediaType)
}

func TestAMoveTouchesNoBytesAndCarriesTheHistoryAndTheBookmarks(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	h.seed(t, "files/plan.md", "second")
	if w := h.call(t, http.MethodPut, "/v1/stars", strings.NewReader(
		`{"owner":"me","path":"files/plan.md"}`), api.HeaderContentType, api.JSONMediaType); w.Code != http.StatusNoContent {
		t.Fatalf("the star answered %d: %s", w.Code, w.Body)
	}

	before := h.bucket.Total()
	w := h.post(t, "files/plan.md", `{"move_to":"files/archive/plan.md"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the move answered %d: %s", w.Code, w.Body)
	}
	if got := h.bucket.Total() - before; got != 0 {
		t.Fatalf("the move made %d bucket calls, and a key derives from an id", got)
	}
	var moved Object
	decode(t, w, &moved)
	if moved.Path != "files/archive/plan.md" || moved.Checksum != digest("second") {
		t.Fatalf("the move answered %+v", moved)
	}
	if _, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md"); err == nil {
		t.Error("the source path still holds a row")
	}
	for _, v := range h.store.versions {
		if v.Path != "files/archive/plan.md" {
			t.Errorf("a version stayed at %q", v.Path)
		}
	}
	for _, s := range h.store.stars {
		if s.Path != "files/archive/plan.md" {
			t.Errorf("a bookmark stayed at %q", s.Path)
		}
	}
	if got := h.store.actions(); got[len(got)-1] != EventMove {
		t.Errorf("the log holds %v", got)
	}
}

func TestAMoveOntoAnOccupiedPathIsAConflictAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	h.seed(t, "files/other.md", "second")
	w := h.post(t, "files/plan.md", `{"move_to":"files/other.md"}`)
	if got := code(t, w); got != api.CodePathTaken {
		t.Fatalf("a move onto an occupied path is %q", got)
	}
	row, err := h.store.Get(t.Context(), nil, h.owner, "files/plan.md")
	if err != nil || row.Checksum != digest("first") {
		t.Fatalf("the source is %+v, %v", row, err)
	}
}

func TestAMoveIsLimitedToTheFilesPlaneAndToOnePath(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	h.write(t, "workspaces/build/main.go", "package main")

	for body, want := range map[string]string{
		`{"move_to":"workspaces/build/plan.md"}`:       api.CodeInvalidPath,
		`{"move_to":"files/plan.md"}`:                  api.CodeInvalidField,
		`{"move_to":"nowhere/plan.md"}`:                api.CodeUnknownPlane,
		`{}`:                                           api.CodeMissingField,
		`{"move_to":"files/a.md","restore_version":1}`: api.CodeExclusiveFields,
		`{"restore_version":-1}`:                       api.CodeInvalidField,
		`{"zone":"agents"}`:                            api.CodeUnknownField,
	} {
		if got := code(t, h.post(t, "files/plan.md", body)); got != want {
			t.Errorf("%s is %q, want %q", body, got, want)
		}
	}
	// A path under workspaces/ belongs to the sync protocol, which reads a
	// move as a delete and a create.
	if got := code(t, h.post(t, "workspaces/build/main.go", `{"move_to":"workspaces/build/other.go"}`)); got != api.CodeInvalidPath {
		t.Errorf("a move under the workspaces plane is %q", got)
	}
}

func TestTwoOverwritesLeaveTwoVersionsAndARestoreSwapsTheIdentities(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")
	h.seed(t, "files/plan.md", "three")

	list := h.call(t, http.MethodGet, h.object("files/plan.md")+"?versions=1", nil)
	var page struct {
		Entries    []Version `json:"entries"`
		NextCursor string    `json:"next_cursor"`
	}
	decode(t, list, &page)
	if len(page.Entries) != 2 || page.Entries[0].VersionNo != 1 || page.Entries[1].VersionNo != 2 {
		t.Fatalf("the history is %+v", page.Entries)
	}
	if page.Entries[0].Checksum != digest("one") || page.Entries[1].Checksum != digest("two") {
		t.Fatalf("the history is %+v", page.Entries)
	}

	// One version reads by the same size rule as a current read.
	one := h.call(t, http.MethodGet, h.object("files/plan.md")+"?version=1", nil)
	if one.Code != http.StatusOK || one.Body.String() != "one" {
		t.Fatalf("a version read answered %d: %q", one.Code, one.Body)
	}

	restored := h.post(t, "files/plan.md", `{"restore_version":1}`)
	if restored.Code != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", restored.Code, restored.Body)
	}
	var back Object
	decode(t, restored, &back)
	if back.Checksum != digest("one") {
		t.Fatalf("the restore answered %+v", back)
	}
	// The restored version left the list, because its object now backs the
	// live file and a retention pass would otherwise delete bytes in use.
	after := h.call(t, http.MethodGet, h.object("files/plan.md")+"?versions=1", nil)
	decode(t, after, &page)
	if len(page.Entries) != 2 {
		t.Fatalf("the history after a restore is %+v", page.Entries)
	}
	for _, v := range page.Entries {
		if v.Checksum == digest("one") {
			t.Error("the restored version is still listed")
		}
	}
	if got := h.store.actions(); got[len(got)-1] != EventRestore {
		t.Errorf("the log holds %v", got)
	}
}

func TestAVersionThatIsNotThereAndAVersionNumberThatIsNotOneAreRefused(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "one")
	for target, want := range map[string]string{
		"?version=9":  api.CodeNotFound,
		"?version=0":  api.CodeInvalidField,
		"?version=-1": api.CodeInvalidField,
		"?version=x":  api.CodeInvalidField,
	} {
		if got := code(t, h.call(t, http.MethodGet, h.object("files/plan.md")+target, nil)); got != want {
			t.Errorf("a read of %s is %q, want %q", target, got, want)
		}
	}
	if got := code(t, h.post(t, "files/plan.md", `{"restore_version":9}`)); got != api.CodeNotFound {
		t.Error("a restore of a version that is not there was accepted")
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/plan.md")+"?versions=1&cursor=x", nil)); got != api.CodeInvalidField {
		t.Error("a cursor that is not a version number was accepted")
	}
	if got := code(t, h.post(t, "workspaces/build/main.go", `{"restore_version":1}`)); got != api.CodeInvalidPath {
		t.Error("a restore under the workspaces plane was accepted")
	}
}
