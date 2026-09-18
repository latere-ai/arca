// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
)

// listing reads one page of a subtree.
func (h *harness) listing(t *testing.T, target string) Listing {
	t.Helper()
	w := h.call(t, http.MethodGet, target, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the listing answered %d: %s", w.Code, w.Body)
	}
	var page Listing
	decode(t, w, &page)
	return page
}

func TestAListingPagesOnThePathAndSynthesisesTheDirectoriesBelowIt(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"files/notes/a.md", "files/notes/b.md",
		"files/notes/archive/old.md", "files/notes/drafts/new.md",
	} {
		h.seed(t, path, "x")
	}
	h.seed(t, "files/other.md", "x")

	page := h.listing(t, h.object("files/notes")+"?list=1")
	if len(page.Entries) != 4 {
		t.Fatalf("the listing is %+v", page.Entries)
	}
	if !slices.IsSortedFunc(page.Entries, func(a, b Object) int { return strings.Compare(a.Path, b.Path) }) {
		t.Errorf("the listing is not ordered by path: %+v", page.Entries)
	}
	want := []string{"files/notes/archive/", "files/notes/drafts/"}
	if !slices.Equal(page.Prefixes, want) {
		t.Fatalf("the listing synthesised %v, want %v", page.Prefixes, want)
	}
	// A listing of files/notes does not also return files/notes-archive.
	h.seed(t, "files/notes-archive.md", "x")
	if got := h.listing(t, h.object("files/notes")+"?list=1"); len(got.Entries) != 4 {
		t.Fatalf("the listing of a prefix took a sibling: %+v", got.Entries)
	}

	first := h.listing(t, h.object("files/notes")+"?list=1&limit=2")
	if len(first.Entries) != 2 || first.NextCursor == "" {
		t.Fatalf("the first page is %+v", first)
	}
	next := h.listing(t, h.object("files/notes")+"?list=1&limit=2&cursor="+first.NextCursor)
	if len(next.Entries) != 2 || next.NextCursor != "" {
		t.Fatalf("the second page is %+v", next)
	}
	if next.Entries[0].Path == first.Entries[0].Path {
		t.Error("the second page repeats the first")
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/notes")+"?list=1&limit=5000", nil)); got != api.CodeInvalidField {
		t.Errorf("a limit above the cap is %q", got)
	}
	// An empty subtree is an empty array and never null.
	empty := h.listing(t, h.object("files/nothing")+"?list=1")
	if empty.Entries == nil || len(empty.Entries) != 0 || empty.Prefixes == nil {
		t.Fatalf("an empty listing is %+v", empty)
	}
	// A trashed row leaves every listing.
	if w := h.call(t, http.MethodDelete, h.object("files/notes/a.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d", w.Code)
	}
	if got := h.listing(t, h.object("files/notes")+"?list=1"); len(got.Entries) != 3 {
		t.Fatalf("a trashed row is still listed: %+v", got.Entries)
	}
}

func TestAListingOutsideTheAnswersFilterIsAnEmptyPageAndNeverARefusal(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "x")
	h.endpoint.Allow(stub.Rule{
		Subject: "*", Action: "file.list", Resource: "*", Allow: true,
		Filter: &authz.Filter{Owners: []string{"https://issuer.example|someone-else"}},
	})
	page := h.listing(t, h.object("files")+"?list=1")
	if len(page.Entries) != 0 {
		t.Fatalf("a listing outside the filter answered %+v", page.Entries)
	}
	manifest := h.call(t, http.MethodGet, "/v1/files/materialize", nil)
	if manifest.Code != http.StatusOK {
		t.Fatalf("a manifest outside the filter answered %d", manifest.Code)
	}
	var out Manifest
	decode(t, manifest, &out)
	if len(out.Files) != 0 {
		t.Fatalf("a manifest outside the filter answered %+v", out.Files)
	}
}

func TestMaterializeAnswersOnePresignedURLPerObjectRelativeToTheRoot(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"files/a.md", "files/notes/b.md", "workspaces/build/main.go"} {
		h.write(t, path, "x")
	}
	w := h.call(t, http.MethodGet, "/v1/files/materialize", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the manifest answered %d: %s", w.Code, w.Body)
	}
	var out Manifest
	decode(t, w, &out)
	if out.Root != "files/" || out.PinnedAt != nil {
		t.Fatalf("the manifest is %+v", out)
	}
	if len(out.Files) != 2 {
		t.Fatalf("the manifest holds %+v, and the workspaces plane is not this route's", out.Files)
	}
	for _, f := range out.Files {
		if strings.HasPrefix(f.Path, "files/") {
			t.Errorf("a manifest path is not relative to the root: %q", f.Path)
		}
		if f.URL == "" || f.Checksum == "" {
			t.Errorf("a manifest entry is %+v", f)
		}
	}

	narrowed := h.call(t, http.MethodGet, "/v1/files/materialize?prefix=notes", nil)
	decode(t, narrowed, &out)
	if len(out.Files) != 1 || out.Files[0].Path != "notes/b.md" {
		t.Fatalf("a narrowed manifest is %+v", out.Files)
	}
	// materialize is a reserved word and not an owner: the literal wins
	// where it sits beside a wildcard (spec 013).
	if got := h.call(t, http.MethodGet, "/v1/files/materialize?prefix=../etc", nil); got.Code != http.StatusBadRequest {
		t.Errorf("a prefix that leaves the plane answered %d", got.Code)
	}
}

func TestStarsAreTheCallersOwnRowsAndDropOutWhenTheirTargetDoes(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/a.md", "one")
	h.seed(t, "files/b.md", "two")
	for _, path := range []string{"files/a.md", "files/b.md"} {
		body := `{"owner":"me","path":"` + path + `"}`
		if w := h.call(t, http.MethodPut, "/v1/stars", strings.NewReader(body),
			api.HeaderContentType, api.MediaJSON); w.Code != http.StatusNoContent {
			t.Fatalf("starring %q answered %d: %s", path, w.Code, w.Body)
		}
	}
	// Starring twice is one row.
	if w := h.call(t, http.MethodPut, "/v1/stars", strings.NewReader(`{"owner":"me","path":"files/a.md"}`),
		api.HeaderContentType, api.MediaJSON); w.Code != http.StatusNoContent {
		t.Fatalf("starring twice answered %d", w.Code)
	}

	var page struct {
		Entries    []Starred `json:"entries"`
		NextCursor string    `json:"next_cursor"`
	}
	decode(t, h.call(t, http.MethodGet, "/v1/stars", nil), &page)
	if len(page.Entries) != 2 || page.Entries[0].Path != "files/a.md" {
		t.Fatalf("the stars are %+v", page.Entries)
	}
	if page.Entries[0].Owner != h.owner || page.Entries[0].Checksum != digest("one") {
		t.Fatalf("a star entry is %+v", page.Entries[0])
	}

	decode(t, h.call(t, http.MethodGet, "/v1/stars?limit=1", nil), &page)
	if len(page.Entries) != 1 || page.NextCursor == "" {
		t.Fatalf("the first page of stars is %+v", page)
	}
	cursor := page.NextCursor
	decode(t, h.call(t, http.MethodGet, "/v1/stars?limit=1&cursor="+cursor, nil), &page)
	if len(page.Entries) != 1 || page.Entries[0].Path != "files/b.md" {
		t.Fatalf("the second page of stars is %+v", page)
	}
	if got := code(t, h.call(t, http.MethodGet, "/v1/stars?cursor=nonsense", nil)); got != api.CodeInvalidField {
		t.Errorf("a cursor this listing did not write is %q", got)
	}

	// A star whose target was trashed drops out of the listing at once,
	// and the row is the reaper's to prune.
	if w := h.call(t, http.MethodDelete, h.object("files/a.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d", w.Code)
	}
	decode(t, h.call(t, http.MethodGet, "/v1/stars", nil), &page)
	if len(page.Entries) != 1 || page.Entries[0].Path != "files/b.md" {
		t.Fatalf("a star on a trashed path is still listed: %+v", page.Entries)
	}

	// Unstarring is idempotent and reads no row, so a star whose target is
	// gone is still one the caller may drop.
	for range 2 {
		w := h.call(t, http.MethodDelete, "/v1/stars?owner=me&path=files/a.md", nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("unstarring answered %d: %s", w.Code, w.Body)
		}
	}
	if got := code(t, h.call(t, http.MethodPut, "/v1/stars",
		strings.NewReader(`{"owner":"me","path":"files/never.md"}`),
		api.HeaderContentType, api.MediaJSON)); got != api.CodeNotFound {
		t.Error("a star on a path that is not there was accepted")
	}
	if got := code(t, h.call(t, http.MethodDelete, "/v1/stars?owner=me", nil)); got != api.CodeInvalidPath {
		t.Error("an unstar with no path was accepted")
	}
}
