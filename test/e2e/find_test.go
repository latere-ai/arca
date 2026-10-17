// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The e2e tier of spec 029: arcad as a process answers a listing narrowed
// to the names that hold a text and to the newest files, against the real
// Postgres and under an answer that omits a subtree.
package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"
)

// findPage is the listing a search and a newest page answer.
type findPage struct {
	Entries []struct {
		Path string `json:"path"`
	} `json:"entries"`
	Prefixes   []string `json:"prefixes"`
	NextCursor string   `json:"next_cursor"`
	Space      *struct {
		Bytes int64 `json:"bytes"`
	} `json:"space"`
}

func TestE2EAFolderIsSearchedByNameAndListedNewestFirst(t *testing.T) {
	i := filesInstallation(t)
	// Written in this order, so the newest is the last.
	written := []string{
		"files/notes/a-Plan.md",
		"files/notes/b/old-PLAN.md",
		"files/notes/readme.md",
		"files/notes/" + url.PathEscape("Ärger.md"),
		"files/notes/secret/plan-z.md",
		"files/notes/c-project-plan.txt",
	}
	for _, path := range written {
		if resp := i.filesPut(t, path, "x"); resp.StatusCode != http.StatusCreated {
			t.Fatalf("the put of %s answered %d: %s", path, resp.StatusCode, filesRead(t, resp))
		}
	}
	// Every listing question of the run is answered with a subtree omitted.
	i.authorizer.Allow(stub.Rule{
		Subject: "*", Action: "file.list", Resource: "*", Allow: true,
		Limits: map[string]any{"omit": []string{"files/notes/secret"}},
	})

	read := func(query string) findPage {
		t.Helper()
		resp := i.filesDo(t, http.MethodGet, i.filesObject("files/notes")+"?list=1&"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("?%s answered %d: %s", query, resp.StatusCode, filesRead(t, resp))
		}
		var page findPage
		filesJSON(t, resp, &page)
		return page
	}
	paths := func(page findPage) []string {
		out := make([]string, 0, len(page.Entries))
		for _, e := range page.Entries {
			out = append(out, e.Path)
		}
		return out
	}

	search := read("q=plan")
	if got, want := paths(search), []string{
		"files/notes/a-Plan.md", "files/notes/b/old-PLAN.md", "files/notes/c-project-plan.txt",
	}; !slices.Equal(got, want) || search.Prefixes == nil || len(search.Prefixes) != 0 {
		t.Errorf("the search holds %v with the prefixes %v, want %v and none", got, search.Prefixes, want)
	}
	first := read("q=PLAN&limit=2")
	if len(first.Entries) != 2 || first.NextCursor != "files/notes/b/old-PLAN.md" {
		t.Fatalf("the first page of two is %+v", first)
	}
	if rest := read("q=PLAN&limit=2&cursor=" + url.QueryEscape(first.NextCursor)); !slices.Equal(paths(rest), []string{"files/notes/c-project-plan.txt"}) {
		t.Errorf("the second page is %v", paths(rest))
	}
	if got := paths(read("q=" + url.QueryEscape("ärger"))); !slices.Equal(got, []string{"files/notes/Ärger.md"}) {
		t.Errorf("the search for a name beyond ASCII in the other case holds %v", got)
	}

	recent := read("recent=1&limit=3")
	if got, want := paths(recent), []string{
		"files/notes/c-project-plan.txt", "files/notes/Ärger.md", "files/notes/readme.md",
	}; !slices.Equal(got, want) || recent.NextCursor != "" {
		t.Errorf("the newest three are %v with the cursor %q, want %v", got, recent.NextCursor, want)
	}
	if got, want := paths(read("recent=1&q=plan")), []string{
		"files/notes/c-project-plan.txt", "files/notes/b/old-PLAN.md", "files/notes/a-Plan.md",
	}; !slices.Equal(got, want) {
		t.Errorf("the newest plans are %v, want %v", got, want)
	}
	for _, query := range []string{"q=plan", "q=z", "recent=1", "recent=1&q=plan"} {
		for _, path := range paths(read(query)) {
			if strings.HasPrefix(path, "files/notes/secret/") {
				t.Errorf("?%s answered %s, which the answer omits", query, path)
			}
		}
	}

	resp := i.filesDo(t, http.MethodGet, i.filesObject("files/notes")+"?list=1&q="+strings.Repeat("a", 201), nil)
	if body := filesRead(t, resp); resp.StatusCode != http.StatusBadRequest || errorCode(t, []byte(body)) != "invalid_field" {
		t.Errorf("a text past the bound answered %d: %s", resp.StatusCode, body)
	}
}
