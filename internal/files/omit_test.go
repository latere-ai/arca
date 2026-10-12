// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
)

// limits.omit of spec 028 at the unit tier: an answer leaves subtrees out
// of what its caller enumerates. Every case holds a space with objects
// inside and outside a prefix, one caller whose answer omits the prefix, and
// a second with no omission, who sees everything.
//
// The two callers are subjects other than the space's owner, and the
// recorder in front of the Decide seam gives each the limits an authorizer
// would have answered it with.

// The prefix the cases omit, and the tree around it. A sibling whose name
// begins with the prefix is outside it: the match is on whole segments.
const omittedPrefix = "files/team/payroll"

// shared seeds the space the cases enumerate and gives carol an answer that
// omits the prefix. dave is the second caller, with no omission.
func shared(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for path, content := range map[string]string{
		"files/team/plan.md":               "plan",
		"files/team/payroll/may.pdf":       "may pay",
		"files/team/payroll/2026/june.pdf": "june pay!",
		"files/team/payroll-notes.md":      "notes",
		"files/other.md":                   "o",
	} {
		h.seed(t, path, content)
	}
	h.asked.limitsFor = map[string]string{h.subject("carol"): `{"omit":["` + omittedPrefix + `"]}`}
	return h
}

// pathsIn reads the paths of a list answer's entries, sorted.
func pathsIn(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var page struct {
		Entries []struct {
			Path string `json:"path"`
		} `json:"entries"`
	}
	decode(t, w, &page)
	out := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, e.Path)
	}
	slices.Sort(out)
	return out
}

// TestAListingLeavesOutWhatTheAnswerOmits: the folder listing and the
// manifest of GET /v1/files/materialize hold nothing under an omitted
// prefix and synthesize no folder for it, a second caller sees everything,
// and the usage a root listing carries stays the whole space's.
func TestAListingLeavesOutWhatTheAnswerOmits(t *testing.T) {
	h := shared(t)
	list := func(sub, prefix string) Listing {
		t.Helper()
		w := h.as(t, sub, http.MethodGet, h.object(prefix)+"?list=1", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s listing %s answered %d: %s", sub, prefix, w.Code, w.Body)
		}
		var page Listing
		decode(t, w, &page)
		return page
	}
	names := func(page Listing) []string {
		out := make([]string, 0, len(page.Entries))
		for _, e := range page.Entries {
			out = append(out, e.Path)
		}
		return out
	}

	narrowed := list("carol", "files/team")
	if got, want := names(narrowed), []string{"files/team/payroll-notes.md", "files/team/plan.md"}; !slices.Equal(got, want) {
		t.Errorf("the listing with the prefix omitted holds %v, want %v", got, want)
	}
	if len(narrowed.Prefixes) != 0 {
		t.Errorf("the listing synthesized the folders %v for a subtree it leaves out", narrowed.Prefixes)
	}
	whole := list("dave", "files/team")
	if got, want := names(whole), []string{
		"files/team/payroll-notes.md", "files/team/payroll/2026/june.pdf",
		"files/team/payroll/may.pdf", "files/team/plan.md",
	}; !slices.Equal(got, want) {
		t.Errorf("the listing with no omission holds %v, want %v", got, want)
	}
	if !slices.Equal(whole.Prefixes, []string{"files/team/payroll/"}) {
		t.Errorf("the listing with no omission synthesized %v", whole.Prefixes)
	}

	// A page is as full as the rows the caller may read allow: the limit
	// counts what is listed and the cursor resumes on it.
	first := h.as(t, "carol", http.MethodGet, h.object("files")+"?list=1&limit=2", "")
	var page Listing
	decode(t, first, &page)
	if got := names(page); !slices.Equal(got, []string{"files/other.md", "files/team/payroll-notes.md"}) || page.NextCursor == "" {
		t.Fatalf("the first page of two holds %v with the cursor %q", got, page.NextCursor)
	}
	second := h.as(t, "carol", http.MethodGet, h.object("files")+"?list=1&limit=2&cursor="+url.QueryEscape(page.NextCursor), "")
	var rest Listing
	decode(t, second, &rest)
	if got := names(rest); !slices.Equal(got, []string{"files/team/plan.md"}) || rest.NextCursor != "" {
		t.Errorf("the second page holds %v with the cursor %q", got, rest.NextCursor)
	}

	// The root listing's usage is the whole space's: it is the counter the
	// space is charged against, and it is not narrowed.
	root := list("carol", "files")
	total := int64(len("plan") + len("may pay") + len("june pay!") + len("notes") + len("o"))
	if root.Space == nil || root.Space.Bytes != total || root.Space.Files != 5 {
		t.Errorf("the root listing with the prefix omitted carries %+v, want %d bytes in 5 files", root.Space, total)
	}
	if got := names(root); len(got) != 3 {
		t.Errorf("the root listing holds %v", got)
	}

	manifest := func(sub string) []string {
		t.Helper()
		w := h.as(t, sub, http.MethodGet, "/v1/files/materialize?owner="+url.QueryEscape(h.owner)+"&prefix=team", "")
		if w.Code != http.StatusOK {
			t.Fatalf("the manifest answered %d: %s", w.Code, w.Body)
		}
		var out Manifest
		decode(t, w, &out)
		paths := make([]string, 0, len(out.Files))
		for _, f := range out.Files {
			paths = append(paths, f.Path)
		}
		return paths
	}
	if got, want := manifest("carol"), []string{"team/payroll-notes.md", "team/plan.md"}; !slices.Equal(got, want) {
		t.Errorf("the manifest with the prefix omitted holds %v, want %v", got, want)
	}
	if got := manifest("dave"); len(got) != 4 {
		t.Errorf("the manifest with no omission holds %v", got)
	}
}

// TestAQuestionAboutOneObjectReadsNoOmission: the limit shapes
// enumerations. A read, a write, a move, a delete and a copy of one object
// inside an omitted subtree are each their own question, which the
// authorizer allowed here, and a listing of a folder inside it is shaped by
// what that listing's own answer omits.
func TestAQuestionAboutOneObjectReadsNoOmission(t *testing.T) {
	h := shared(t)
	inside := h.object(omittedPrefix + "/may.pdf")

	if w := h.as(t, "carol", http.MethodGet, inside, ""); w.Code != http.StatusOK || w.Body.String() != "may pay" {
		t.Errorf("a read inside the omitted subtree answered %d: %s", w.Code, w.Body)
	}
	if w := h.as(t, "carol", http.MethodHead, inside, ""); w.Code != http.StatusOK {
		t.Errorf("a head inside the omitted subtree answered %d", w.Code)
	}
	if w := h.as(t, "carol", http.MethodGet, inside+"?versions=1", ""); w.Code != http.StatusOK {
		t.Errorf("the versions of one path inside the omitted subtree answered %d: %s", w.Code, w.Body)
	}
	if w := h.as(t, "carol", http.MethodPost, inside, `{"copy_to":"files/team/payroll/may-copy.pdf"}`); w.Code != http.StatusOK {
		t.Errorf("a copy of one object inside the omitted subtree answered %d: %s", w.Code, w.Body)
	}
	if w := h.as(t, "carol", http.MethodPost, inside, `{"move_to":"files/team/payroll/may-2026.pdf"}`); w.Code != http.StatusOK {
		t.Errorf("a move inside the omitted subtree answered %d: %s", w.Code, w.Body)
	}
	if w := h.as(t, "carol", http.MethodDelete, h.object(omittedPrefix+"/may-2026.pdf"), ""); w.Code != http.StatusNoContent {
		t.Errorf("a delete inside the omitted subtree answered %d: %s", w.Code, w.Body)
	}
	if got := h.trashed(); !slices.Equal(got, []string{omittedPrefix + "/may-2026.pdf"}) {
		t.Errorf("the trash holds %v", got)
	}

	// The folder itself, listed: the answer to this question carries the
	// same omission, so its page holds nothing. With an answer that omits
	// nothing the same listing is whole.
	var narrowed, whole Listing
	decode(t, h.as(t, "carol", http.MethodGet, h.object(omittedPrefix)+"?list=1", ""), &narrowed)
	if len(narrowed.Entries) != 0 {
		t.Errorf("a listing of the omitted folder under an answer that omits it holds %d entries", len(narrowed.Entries))
	}
	decode(t, h.as(t, "dave", http.MethodGet, h.object(omittedPrefix)+"?list=1", ""), &whole)
	if len(whole.Entries) != 2 {
		t.Errorf("a listing of the folder under an answer that omits nothing holds %d entries, want 2", len(whole.Entries))
	}
}

// TestTheTrashLeavesOutWhatTheAnswerOmits: the trash listing holds nothing
// under an omitted prefix, emptying the trash leaves what is under it in
// the trash and out of the count, and a purge of one named path is that
// path's own question.
func TestTheTrashLeavesOutWhatTheAnswerOmits(t *testing.T) {
	h := shared(t)
	for _, path := range []string{"files/team/plan.md", omittedPrefix + "/may.pdf", omittedPrefix + "/2026/june.pdf"} {
		if w := h.call(t, http.MethodDelete, h.object(path), nil); w.Code != http.StatusNoContent {
			t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
		}
	}
	trash := "/v1/trash?owner=" + url.QueryEscape(h.owner)
	listed := func(sub string) []string {
		t.Helper()
		w := h.as(t, sub, http.MethodGet, trash, "")
		if w.Code != http.StatusOK {
			t.Fatalf("the trash listing answered %d: %s", w.Code, w.Body)
		}
		return pathsIn(t, w)
	}
	if got := listed("carol"); !slices.Equal(got, []string{"files/team/plan.md"}) {
		t.Errorf("the trash with the prefix omitted lists %v", got)
	}
	if got := listed("dave"); len(got) != 3 {
		t.Errorf("the trash with no omission lists %v", got)
	}

	var out struct {
		Purged int `json:"purged"`
	}
	w := h.as(t, "carol", http.MethodDelete, trash, "")
	decode(t, w, &out)
	if w.Code != http.StatusOK || out.Purged != 1 {
		t.Fatalf("emptying the trash with the prefix omitted answered %d, purged %d: %s", w.Code, out.Purged, w.Body)
	}
	if got, want := h.trashed(), []string{omittedPrefix + "/2026/june.pdf", omittedPrefix + "/may.pdf"}; !slices.Equal(got, want) {
		t.Errorf("the trash holds %v after it was emptied, want %v", got, want)
	}
	// One named path is a question about that path.
	w = h.as(t, "carol", http.MethodDelete, trash+"&path="+url.QueryEscape(omittedPrefix+"/may.pdf"), "")
	decode(t, w, &out)
	if w.Code != http.StatusOK || out.Purged != 1 {
		t.Errorf("a purge of one path inside the omitted subtree answered %d, purged %d: %s", w.Code, out.Purged, w.Body)
	}
	w = h.as(t, "dave", http.MethodDelete, trash, "")
	decode(t, w, &out)
	if out.Purged != 1 || len(h.trashed()) != 0 {
		t.Errorf("emptying the trash with no omission purged %d and left %v", out.Purged, h.trashed())
	}
}

// TestTheStarListingLeavesOutWhatTheAnswerOmits: a star whose path is under
// an omitted prefix is not listed, and a second caller with the same stars
// and no omission lists them all.
func TestTheStarListingLeavesOutWhatTheAnswerOmits(t *testing.T) {
	h := shared(t)
	for _, sub := range []string{"carol", "dave"} {
		for _, path := range []string{"files/team/plan.md", omittedPrefix + "/may.pdf", "files/team/payroll-notes.md"} {
			body := `{"owner":"` + h.owner + `","path":"` + path + `"}`
			if w := h.as(t, sub, http.MethodPut, "/v1/stars", body); w.Code != http.StatusNoContent {
				t.Fatalf("%s starring %s answered %d: %s", sub, path, w.Code, w.Body)
			}
		}
	}
	listed := func(sub string) []string {
		t.Helper()
		w := h.as(t, sub, http.MethodGet, "/v1/stars", "")
		if w.Code != http.StatusOK {
			t.Fatalf("the star listing answered %d: %s", w.Code, w.Body)
		}
		return pathsIn(t, w)
	}
	if got, want := listed("carol"), []string{"files/team/payroll-notes.md", "files/team/plan.md"}; !slices.Equal(got, want) {
		t.Errorf("the stars with the prefix omitted are %v, want %v", got, want)
	}
	if got := listed("dave"); len(got) != 3 {
		t.Errorf("the stars with no omission are %v", got)
	}
}

// TestAFolderDeleteLeavesAnOmittedSubtreeWhereItIs: an omitted subtree is
// neither trashed nor removed by a caller it is omitted for, the grants
// under the folder are removed around it, and the event's counts leave it
// out. The second caller's delete takes the whole folder.
func TestAFolderDeleteLeavesAnOmittedSubtreeWhereItIs(t *testing.T) {
	kept := []string{omittedPrefix + "/2026/june.pdf", omittedPrefix + "/may.pdf"}

	soft := shared(t)
	if w := soft.as(t, "carol", http.MethodDelete, soft.object("files/team")+"?recursive=1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
	}
	if got, want := soft.live(), append([]string{"files/other.md"}, kept...); !slices.Equal(got, want) {
		t.Errorf("the space holds %v live, want %v", got, want)
	}
	if got, want := soft.trashed(), []string{"files/team/payroll-notes.md", "files/team/plan.md"}; !slices.Equal(got, want) {
		t.Errorf("the trash holds %v, want %v", got, want)
	}
	if e := soft.last(t); e.Detail["files"] != int64(2) || e.Detail["bytes"] != int64(len("plan")+len("notes")) {
		t.Errorf("the event counts %v", e.Detail)
	}

	hard := shared(t)
	keys := len(hard.objects.Keys())
	if w := hard.as(t, "carol", http.MethodDelete, hard.object("files/team")+"?recursive=1&permanent=1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("the permanent folder delete answered %d: %s", w.Code, w.Body)
	}
	if got, want := hard.live(), append([]string{"files/other.md"}, kept...); !slices.Equal(got, want) {
		t.Errorf("the space holds %v live, want %v", got, want)
	}
	if got := len(hard.objects.Keys()); got != keys-2 {
		t.Errorf("the bucket lost %d keys, want the two outside the omitted subtree", keys-got)
	}
	if want := []string{hard.owner + " files/team " + omittedPrefix}; !slices.Equal(hard.grants.removed, want) {
		t.Errorf("the grants were removed as %v, want %v", hard.grants.removed, want)
	}
	if e := hard.last(t); e.Detail["files"] != int64(2) || e.Detail["trashed"] != false {
		t.Errorf("the event counts %v", e.Detail)
	}
	if got := hard.store.usage[hard.owner]; got != int64(len("may pay")+len("june pay!")+len("o")) {
		t.Errorf("the space holds %d bytes after the delete", got)
	}

	// The second caller's answer omits nothing, so the folder is emptied.
	if w := hard.as(t, "dave", http.MethodDelete, hard.object("files/team")+"?recursive=1&permanent=1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("the second folder delete answered %d: %s", w.Code, w.Body)
	}
	if got := hard.live(); !slices.Equal(got, []string{"files/other.md"}) {
		t.Errorf("the space holds %v after a delete with no omission", got)
	}
}

// TestAFolderCopyLeavesOutWhatTheAnswerOmitsAtBothEnds: an object is not
// copied when the answer omits its source path or its destination path, and
// the answer's counts and the event's leave it out. A folder all of whose
// objects are left out is the answer a folder with nothing in it gives.
func TestAFolderCopyLeavesOutWhatTheAnswerOmitsAtBothEnds(t *testing.T) {
	h := shared(t)
	var out struct {
		Path  string `json:"path"`
		Files int    `json:"files"`
		Bytes int64  `json:"bytes"`
	}
	w := h.as(t, "carol", http.MethodPost, h.object("files/team"), `{"copy_to":"files/copy","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the copy answered %d: %s", w.Code, w.Body)
	}
	decode(t, w, &out)
	if out.Files != 2 || out.Bytes != int64(len("plan")+len("notes")) {
		t.Errorf("the copy with the prefix omitted answered %+v", out)
	}
	var copied []string
	for _, path := range h.live() {
		if strings.HasPrefix(path, "files/copy/") {
			copied = append(copied, path)
		}
	}
	if want := []string{"files/copy/payroll-notes.md", "files/copy/plan.md"}; !slices.Equal(copied, want) {
		t.Errorf("the copy wrote %v, want %v", copied, want)
	}
	if e := h.last(t); e.Action != EventCopy || e.Detail["files"] != 2 {
		t.Errorf("the event is %+v", e)
	}
	// The question's size was counted before the answer, so it is the whole
	// folder's.
	q := h.asked.asked[len(h.asked.asked)-1]
	if q.res.Fields["size"] != int64(len("plan")+len("may pay")+len("june pay!")+len("notes")) {
		t.Errorf("the question carried the size %v", q.res.Fields["size"])
	}

	// The destination end: what would land under an omitted prefix is left
	// out, and a destination that holds something there is not asked about.
	h.asked.limitsFor[h.subject("carol")] = `{"omit":["files/mirror/payroll"]}`
	h.seed(t, "files/mirror/payroll/may.pdf", "something the caller cannot see")
	w = h.as(t, "carol", http.MethodPost, h.object("files/team"), `{"copy_to":"files/mirror","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the copy onto an omitted destination answered %d: %s", w.Code, w.Body)
	}
	decode(t, w, &out)
	if out.Files != 2 {
		t.Errorf("the copy onto an omitted destination answered %+v", out)
	}
	if row, _ := h.read(t, "files/mirror/payroll/may.pdf"); row.Checksum != digest("something the caller cannot see") {
		t.Error("the copy wrote under a destination the answer omits")
	}

	// Everything left out: nothing to copy.
	h.asked.limitsFor[h.subject("carol")] = `{"omit":["` + omittedPrefix + `"]}`
	before := h.bucket.Calls(blob.MethodCopy)
	if got := code(t, h.as(t, "carol", http.MethodPost, h.object(omittedPrefix), `{"copy_to":"files/elsewhere","recursive":true}`)); got != api.CodeNotFound {
		t.Errorf("a copy of a folder the answer omits whole is %q", got)
	}
	if h.bucket.Calls(blob.MethodCopy) != before {
		t.Error("a copy with nothing left in it reached the bucket")
	}
	// The second caller copies the whole folder.
	w = h.as(t, "dave", http.MethodPost, h.object("files/team"), `{"copy_to":"files/whole","recursive":true}`)
	decode(t, w, &out)
	if w.Code != http.StatusOK || out.Files != 4 {
		t.Errorf("the copy with no omission answered %d %+v", w.Code, out)
	}
}
