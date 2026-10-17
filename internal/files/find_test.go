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
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
)

// The search by name and the newest files of spec 029 at the unit tier: what
// the handler reads off the request, what it asks, and the shape it answers.
// What Postgres folds and matches is the store tier's.

// stamp sets the time a row was last modified, so a case orders rows
// without waiting between writes.
func (h *harness) stamp(t *testing.T, path string, at time.Time) {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	row, ok := h.store.files[key(h.owner, path)]
	if !ok {
		t.Fatalf("there is no row at %q to stamp", path)
	}
	row.UpdatedAt = at
	h.store.files[key(h.owner, path)] = row
}

// namesOf reads the paths of a listing's entries, in its order.
func namesOf(page Listing) []string {
	out := make([]string, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, e.Path)
	}
	return out
}

// fieldsOf reads the fields a refusal names.
func fieldsOf(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Error struct {
			Details struct {
				Fields []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decode(t, w, &body)
	return body.Error.Details.Fields
}

// TestASearchKeepsTheNamesThatHoldTheText: the last segment holds the text
// ignoring case and taken as sent, a folder's own name is not matched, a
// sibling and a trashed file are absent, the pages walk the path cursor,
// and neither prefixes nor the space's usage are answered.
func TestASearchKeepsTheNamesThatHoldTheText(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"files/notes/a-Plan.md", "files/notes/b/old-PLAN.md", "files/notes/c-project-plan.txt",
		"files/notes/plan/readme.md", "files/notes/d-gone-plan.md", "files/notes-plan.md",
	} {
		h.seed(t, path, "x")
	}
	// Names a request line would carry escaped are written to the stores
	// directly.
	for _, path := range []string{"files/notes/Ärger.md", "files/notes/50%_off.pdf", "files/notes/50x_off.pdf"} {
		h.write(t, path, "x")
	}
	if w := h.call(t, http.MethodDelete, h.object("files/notes/d-gone-plan.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the delete answered %d", w.Code)
	}

	w := h.call(t, http.MethodGet, h.object("files/notes")+"?list=1&q=plan", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the search answered %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"prefixes":[]`) {
		t.Errorf("the search does not answer prefixes as an empty array: %s", w.Body)
	}
	var page Listing
	decode(t, w, &page)
	want := []string{"files/notes/a-Plan.md", "files/notes/b/old-PLAN.md", "files/notes/c-project-plan.txt"}
	if got := namesOf(page); !slices.Equal(got, want) || page.NextCursor != "" {
		t.Errorf("the search holds %v with the cursor %q, want %v", got, page.NextCursor, want)
	}

	first := h.listing(t, h.object("files/notes")+"?list=1&q=PLAN&limit=2")
	if got := namesOf(first); !slices.Equal(got, want[:2]) || first.NextCursor != want[1] {
		t.Fatalf("the first page of two holds %v with the cursor %q", got, first.NextCursor)
	}
	rest := h.listing(t, h.object("files/notes")+"?list=1&q=PLAN&limit=2&cursor="+url.QueryEscape(first.NextCursor))
	if got := namesOf(rest); !slices.Equal(got, want[2:]) || rest.NextCursor != "" {
		t.Errorf("the second page holds %v with the cursor %q", got, rest.NextCursor)
	}

	// The text reaches the statement as sent: a letter beyond ASCII and the
	// LIKE wildcards are not reinterpreted on the way.
	for text, want := range map[string][]string{
		"ärger":   {"files/notes/Ärger.md"},
		"50%_off": {"files/notes/50%_off.pdf"},
	} {
		got := h.listing(t, h.object("files/notes")+"?list=1&q="+url.QueryEscape(text))
		if !slices.Equal(namesOf(got), want) {
			t.Errorf("the search for %q holds %v, want %v", text, namesOf(got), want)
		}
	}

	// At the plane root a search answers no usage: the counters say what
	// the space holds, which a search does not ask.
	root := h.listing(t, h.object("files")+"?list=1&q=plan")
	if root.Space != nil || len(root.Prefixes) != 0 {
		t.Errorf("the search of the root carries %+v and %v", root.Space, root.Prefixes)
	}
	if got := namesOf(root); !slices.Contains(got, "files/notes-plan.md") || len(got) != 4 {
		t.Errorf("the search of the root holds %v", got)
	}
}

// TestABlankSearchIsThePlainListing: an empty text, and one of white space
// only, answer the listing with its folders and its usage.
func TestABlankSearchIsThePlainListing(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "x")
	h.seed(t, "files/notes/a.md", "x")
	plain := h.listing(t, h.object("files")+"?list=1")
	for _, q := range []string{"", "%20%20", "%09"} {
		page := h.listing(t, h.object("files")+"?list=1&q="+q)
		if !slices.Equal(namesOf(page), namesOf(plain)) || !slices.Equal(page.Prefixes, []string{"files/notes/"}) || page.Space == nil {
			t.Errorf("the listing with q=%q is %+v, want the plain listing %+v", q, page, plain)
		}
	}
}

// TestASearchOrRecentItCannotReadIsRefused: a text past the bound, one that
// is not text, a recent that is not 0 or 1, and a recent with a cursor are
// each refused with the field at fault, before the authorizer is asked.
func TestASearchOrRecentItCannotReadIsRefused(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "x")
	longest := strings.Repeat("é", MaxQueryBytes/2)
	for query, want := range map[string]struct {
		code   string
		fields []string
	}{
		"q=" + url.QueryEscape(longest+"x"):   {api.CodeInvalidField, []string{"q"}},
		"q=%FF":                               {api.CodeInvalidField, []string{"q"}},
		"q=a%00b":                             {api.CodeInvalidField, []string{"q"}},
		"recent=2":                            {api.CodeInvalidField, []string{"recent"}},
		"recent=yes&q=plan":                   {api.CodeInvalidField, []string{"recent"}},
		"recent=1&cursor=files%2Fplan.md":     {api.CodeExclusiveFields, []string{"recent", "cursor"}},
		"recent=1&q=plan&cursor=files%2Fa.md": {api.CodeExclusiveFields, []string{"recent", "cursor"}},
	} {
		asked := len(h.asked.asked)
		w := h.call(t, http.MethodGet, h.object("files")+"?list=1&"+query, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("?%s answered %d: %s", query, w.Code, w.Body)
			continue
		}
		if got := code(t, w); got != want.code {
			t.Errorf("?%s is %q, want %q", query, got, want.code)
		}
		if got := fieldsOf(t, w); !slices.Equal(got, want.fields) {
			t.Errorf("?%s names the fields %v, want %v", query, got, want.fields)
		}
		if len(h.asked.asked) != asked {
			t.Errorf("?%s asked the authorizer before it refused", query)
		}
	}
	// The bound is in bytes and admits a text of exactly that length, and a
	// recent page with an empty cursor is a first page.
	for _, query := range []string{"q=" + url.QueryEscape(longest), "recent=1&cursor=", "recent=0&cursor=files%2Fa.md"} {
		if w := h.call(t, http.MethodGet, h.object("files")+"?list=1&"+query, nil); w.Code != http.StatusOK {
			t.Errorf("?%s answered %d: %s", query, w.Code, w.Body)
		}
	}
}

// TestRecentAnswersTheNewestFilesFirst: newest first, two of one instant in
// path order, at most limit, one page with no cursor, nothing trashed and
// nothing beside the path, and with a text only the names that hold it.
func TestRecentAnswersTheNewestFilesFirst(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	for path, age := range map[string]time.Duration{
		"files/r/a-report.md":      5 * time.Hour,
		"files/r/b-notes.md":       time.Hour,
		"files/r/deep/c-report.md": 3 * time.Hour,
		"files/r/d-notes.md":       time.Hour,
		"files/r/e-report.md":      2 * time.Hour,
		"files/r/f-gone-report.md": 0,
		"files/r-report.md":        0,
	} {
		h.seed(t, path, "x")
		h.stamp(t, path, now.Add(-age))
	}
	if w := h.call(t, http.MethodDelete, h.object("files/r/f-gone-report.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the delete answered %d", w.Code)
	}

	all := []string{
		"files/r/b-notes.md", "files/r/d-notes.md", "files/r/e-report.md",
		"files/r/deep/c-report.md", "files/r/a-report.md",
	}
	w := h.call(t, http.MethodGet, h.object("files/r")+"?list=1&recent=1", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"prefixes":[]`) {
		t.Fatalf("the newest page answered %d: %s", w.Code, w.Body)
	}
	var page Listing
	decode(t, w, &page)
	if got := namesOf(page); !slices.Equal(got, all) || page.NextCursor != "" {
		t.Errorf("the newest page holds %v with the cursor %q, want %v", got, page.NextCursor, all)
	}
	if !slices.IsSortedFunc(page.Entries, func(a, b Object) int { return strings.Compare(b.Modified, a.Modified) }) {
		t.Errorf("the entries' modified times are not newest first: %+v", page.Entries)
	}
	three := h.listing(t, h.object("files/r")+"?list=1&recent=1&limit=3")
	if got := namesOf(three); !slices.Equal(got, all[:3]) || three.NextCursor != "" {
		t.Errorf("the newest three are %v with the cursor %q, want %v", got, three.NextCursor, all[:3])
	}
	reports := h.listing(t, h.object("files/r")+"?list=1&recent=1&q=REPORT")
	if got, want := namesOf(reports), []string{
		"files/r/e-report.md", "files/r/deep/c-report.md", "files/r/a-report.md",
	}; !slices.Equal(got, want) {
		t.Errorf("the newest reports are %v, want %v", got, want)
	}
	// recent=0 is the plain listing, in path order and with its folders.
	plain := h.listing(t, h.object("files/r")+"?list=1&recent=0")
	if !slices.IsSorted(namesOf(plain)) || !slices.Equal(plain.Prefixes, []string{"files/r/deep/"}) {
		t.Errorf("recent=0 answered %v with %v", namesOf(plain), plain.Prefixes)
	}
	if root := h.listing(t, h.object("files")+"?list=1&recent=1"); root.Space != nil || len(root.Entries) != 6 {
		t.Errorf("the newest page of the root is %+v", root)
	}
}

// TestASearchAndRecentLeaveOutWhatTheAnswerOmits: carol's answer omits a
// subtree that holds the best matches and the newest files, and none of them
// reaches her in a search or a newest page, while dave, whose answer omits
// nothing, reads them. The difference between the two is what proves the
// omission was read: an answer whose member the reader did not know would
// leave both callers the same page.
func TestASearchAndRecentLeaveOutWhatTheAnswerOmits(t *testing.T) {
	h := shared(t)
	now := time.Now().UTC().Truncate(time.Second)
	h.seed(t, "files/team/payroll/pay-slip.pdf", "slip")
	for path, age := range map[string]time.Duration{
		"files/team/payroll/pay-slip.pdf":  0,
		"files/team/payroll/2026/june.pdf": time.Minute,
		"files/team/payroll/may.pdf":       2 * time.Minute,
		"files/team/payroll-notes.md":      time.Hour,
		"files/team/plan.md":               2 * time.Hour,
		"files/other.md":                   3 * time.Hour,
	} {
		h.stamp(t, path, now.Add(-age))
	}
	read := func(sub, query string) Listing {
		t.Helper()
		w := h.as(t, sub, http.MethodGet, h.object("files")+"?list=1&"+query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s ?%s answered %d: %s", sub, query, w.Code, w.Body)
		}
		var page Listing
		decode(t, w, &page)
		return page
	}

	if got, want := namesOf(read("carol", "q=pay")), []string{"files/team/payroll-notes.md"}; !slices.Equal(got, want) {
		t.Errorf("carol's search holds %v, want %v", got, want)
	}
	if got, want := namesOf(read("dave", "q=pay")), []string{
		"files/team/payroll-notes.md", "files/team/payroll/pay-slip.pdf",
	}; !slices.Equal(got, want) {
		t.Errorf("dave's search holds %v, want %v", got, want)
	}
	if got := namesOf(read("carol", "q=.pdf")); len(got) != 0 {
		t.Errorf("carol's search for the files only the omitted subtree holds answered %v", got)
	}
	if got := namesOf(read("dave", "q=.pdf")); len(got) != 3 {
		t.Errorf("dave's search for the pdfs answered %v", got)
	}
	// A page of matches is as full as what carol may read allows, and the
	// cursor resumes on a file she read.
	first := read("carol", "q=a&limit=1")
	if got := namesOf(first); !slices.Equal(got, []string{"files/team/payroll-notes.md"}) || first.NextCursor == "" {
		t.Fatalf("carol's first page of one holds %v with the cursor %q", got, first.NextCursor)
	}
	second := read("carol", "q=a&limit=1&cursor="+url.QueryEscape(first.NextCursor))
	if got := namesOf(second); !slices.Equal(got, []string{"files/team/plan.md"}) {
		t.Errorf("carol's second page of one holds %v", got)
	}

	// The newest files are inside the omitted subtree, and carol's newest
	// page counts its limit over the files she may read.
	for _, query := range []string{"recent=1&limit=2", "recent=1&limit=2&q=."} {
		if got, want := namesOf(read("carol", query)), []string{
			"files/team/payroll-notes.md", "files/team/plan.md",
		}; !slices.Equal(got, want) {
			t.Errorf("carol's ?%s holds %v, want %v", query, got, want)
		}
	}
	if got, want := namesOf(read("dave", "recent=1&limit=2")), []string{
		"files/team/payroll/pay-slip.pdf", "files/team/payroll/2026/june.pdf",
	}; !slices.Equal(got, want) {
		t.Errorf("dave's newest two are %v, want %v", got, want)
	}
	for _, query := range []string{"q=pay", "q=.pdf", "recent=1", "recent=1&q=pdf"} {
		for _, e := range read("carol", query).Entries {
			if strings.HasPrefix(e.Path, omittedPrefix+"/") {
				t.Errorf("carol's ?%s answered %s", query, e.Path)
			}
		}
	}
}

// TestASearchAndRecentAskTheListingsQuestion: each asks one file.list about
// the path, and an answer whose filter does not admit the space is an empty
// page and never a refusal.
func TestASearchAndRecentAskTheListingsQuestion(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/notes/plan.md", "x")
	for _, query := range []string{"q=plan", "recent=1", "recent=1&q=plan"} {
		h.asked.asked = nil
		page := h.listing(t, h.object("files/notes")+"?list=1&"+query)
		if len(page.Entries) != 1 {
			t.Errorf("?%s holds %+v", query, page.Entries)
		}
		if len(h.asked.asked) != 1 {
			t.Fatalf("?%s asked %d questions, want one", query, len(h.asked.asked))
		}
		q := h.asked.asked[0]
		if q.action != authorizer.ActionFileList || q.res.Fields["path"] != "files/notes" || q.res.Fields["owner"] != h.owner {
			t.Errorf("?%s asked %s about %v", query, q.action, q.res.Fields)
		}
	}

	h.endpoint.Allow(stub.Rule{
		Subject: "*", Action: "file.list", Resource: "*", Allow: true,
		Filter: &authz.Filter{Owners: []string{"https://issuer.example|someone-else"}},
	})
	for _, query := range []string{"q=plan", "recent=1"} {
		page := h.listing(t, h.object("files/notes")+"?list=1&"+query)
		if len(page.Entries) != 0 || page.Prefixes == nil {
			t.Errorf("?%s outside the filter answered %+v", query, page)
		}
	}
}
