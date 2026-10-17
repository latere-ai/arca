// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The name search and the newest files of spec 029 against a real Postgres.
// A fake proves what each statement binds; only the database proves what
// its LIKE, its lower() and its sort mean over real rows.
package store

import (
	"slices"
	"testing"
	"time"
)

// stampModified sets the time a row was last modified, so a case orders rows
// without waiting between writes.
func stampModified(t *testing.T, db *DB, owner, path string, at time.Time) {
	t.Helper()
	tag, err := db.Querier().Exec(t.Context(),
		`UPDATE files SET updated_at = $3 WHERE owner = $1 AND path = $2`, owner, path, at)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("stamp %q: %v, %d rows", path, err, tag.RowsAffected())
	}
}

// searchAll walks every page of one search and answers the paths in the
// order the pages gave them, failing a page before the last that is short.
func searchAll(t *testing.T, db *DB, owner, prefix, text string, limit int, omit []string) []string {
	t.Helper()
	var walked []string
	cursor := ""
	for range 20 {
		rows, next, err := NewFiles().SearchPrefix(t.Context(), db.Querier(), owner, prefix, text, cursor, limit, omit)
		if err != nil {
			t.Fatalf("search %q under %q: %v", text, prefix, err)
		}
		walked = append(walked, pathsOfFiles(rows)...)
		if next == "" {
			return walked
		}
		if len(rows) != limit {
			t.Errorf("a page of the search before the last holds %d rows, want %d", len(rows), limit)
		}
		cursor = next
	}
	t.Fatalf("the search of %q under %q did not end", text, prefix)
	return nil
}

// TestStoreASearchMatchesTheNameUnderThePrefix: the last segment holds the
// text ignoring case, a folder's own name is not matched, a sibling of the
// prefix and a trashed row are absent, and the pages walk the path order.
func TestStoreASearchMatchesTheNameUnderThePrefix(t *testing.T) {
	db := tier(t)
	owner := aSpaceOf(t)
	for _, path := range []string{
		"files/notes/a-Plan.md",
		"files/notes/b/old-PLAN.md",
		"files/notes/c-project-plan.txt",
		"files/notes/plan/readme.md",
		"files/notes/d-trashed-plan.md",
		"files/notes-plan.md",
		"files/other/plan.md",
	} {
		reaperFile(t, db, owner, path, 1)
	}
	reaperTrash(t, db, owner, "files/notes/d-trashed-plan.md", time.Now())

	want := []string{"files/notes/a-Plan.md", "files/notes/b/old-PLAN.md", "files/notes/c-project-plan.txt"}
	if got := searchAll(t, db, owner, "files/notes/", "plan", 100, nil); !slices.Equal(got, want) {
		t.Errorf("the search holds %v, want %v", got, want)
	}
	if got := searchAll(t, db, owner, "files/notes/", "plan", 2, nil); !slices.Equal(got, want) {
		t.Errorf("the search in pages of two walked %v, want %v", got, want)
	}
	if got := searchAll(t, db, owner, "files/notes/", "PLAN.MD", 100, nil); !slices.Equal(got, want[:2]) {
		t.Errorf("the search in upper case holds %v, want %v", got, want[:2])
	}
	// Another space holding the same names answers nothing of them.
	if got := searchAll(t, db, aSpaceOf(t), "files/", "plan", 100, nil); len(got) != 0 {
		t.Errorf("a search of an empty space holds %v", got)
	}
}

// TestStoreASearchTakesTheTextLiterally: the two LIKE wildcards and the
// escape character in a text match themselves and nothing else.
func TestStoreASearchTakesTheTextLiterally(t *testing.T) {
	db := tier(t)
	owner := aSpaceOf(t)
	for _, path := range []string{
		"files/a/50%_off.pdf", "files/a/50xoff.pdf",
		"files/a/5_0.txt", "files/a/5x0.txt",
		`files/a/back\slash.md`, "files/a/backslash.md",
	} {
		reaperFile(t, db, owner, path, 1)
	}
	for text, want := range map[string][]string{
		"%":     {"files/a/50%_off.pdf"},
		"50%":   {"files/a/50%_off.pdf"},
		"_":     {"files/a/50%_off.pdf", "files/a/5_0.txt"},
		"5_0":   {"files/a/5_0.txt"},
		"%_":    {"files/a/50%_off.pdf"},
		`k\s`:   {`files/a/back\slash.md`},
		`\`:     {`files/a/back\slash.md`},
		"slash": {`files/a/back\slash.md`, "files/a/backslash.md"},
	} {
		got := searchAll(t, db, owner, "files/", text, 100, nil)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("the search for %q holds %v, want %v", text, got, want)
		}
	}
}

// TestStoreASearchFoldsTheCaseOfANameBeyondASCII: on a database whose
// character classification is a UTF-8 locale, which the tier's is, a letter
// beyond ASCII matches its other case on either side.
func TestStoreASearchFoldsTheCaseOfANameBeyondASCII(t *testing.T) {
	db := tier(t)
	owner := aSpaceOf(t)
	for _, path := range []string{
		"files/docs/Ärger.md", "files/docs/ÉTÉ-plan.txt", "files/docs/ΣΟΦΙΑ.txt", "files/docs/other.md",
	} {
		reaperFile(t, db, owner, path, 1)
	}
	for text, want := range map[string]string{
		"ärger": "files/docs/Ärger.md",
		"ÄRGER": "files/docs/Ärger.md",
		"été":   "files/docs/ÉTÉ-plan.txt",
		"σοφια": "files/docs/ΣΟΦΙΑ.txt",
	} {
		if got := searchAll(t, db, owner, "files/", text, 100, nil); !slices.Equal(got, []string{want}) {
			t.Errorf("the search for %q holds %v, want %s; the database folds only what its LC_CTYPE folds", text, got, want)
		}
	}
	recent, err := NewFiles().RecentPrefix(t.Context(), db.Querier(), owner, "files/", "ärger", 10, nil)
	if err != nil || !slices.Equal(pathsOfFiles(recent), []string{"files/docs/Ärger.md"}) {
		t.Errorf("the newest page for %q holds %v, %v", "ärger", pathsOfFiles(recent), err)
	}
}

// TestStoreRecentIsNewestFirstAndBounded: the newest modification first,
// two of one instant in path order, at most limit rows, nothing trashed and
// nothing beside the prefix, and a text keeps the names that hold it.
func TestStoreRecentIsNewestFirstAndBounded(t *testing.T) {
	db := tier(t)
	owner := aSpaceOf(t)
	now := time.Now().UTC().Truncate(time.Second)
	for path, age := range map[string]time.Duration{
		"files/r/a-report.md":         5 * time.Hour,
		"files/r/b-notes.md":          time.Hour,
		"files/r/deep/c-report.md":    3 * time.Hour,
		"files/r/d-notes.md":          time.Hour,
		"files/r/e-report.md":         2 * time.Hour,
		"files/r/f-trashed-report.md": 0,
		"files/r-report.md":           0,
	} {
		reaperFile(t, db, owner, path, 1)
		stampModified(t, db, owner, path, now.Add(-age))
	}
	reaperTrash(t, db, owner, "files/r/f-trashed-report.md", now)

	recent := func(text string, limit int) []string {
		t.Helper()
		rows, err := NewFiles().RecentPrefix(t.Context(), db.Querier(), owner, "files/r/", text, limit, nil)
		if err != nil {
			t.Fatalf("the newest %d for %q: %v", limit, text, err)
		}
		return pathsOfFiles(rows)
	}
	all := []string{
		"files/r/b-notes.md", "files/r/d-notes.md", "files/r/e-report.md",
		"files/r/deep/c-report.md", "files/r/a-report.md",
	}
	if got := recent("", 100); !slices.Equal(got, all) {
		t.Errorf("the newest page holds %v, want %v", got, all)
	}
	if got := recent("", 3); !slices.Equal(got, all[:3]) {
		t.Errorf("the newest three are %v, want %v", got, all[:3])
	}
	if got, want := recent("REPORT", 100), []string{
		"files/r/e-report.md", "files/r/deep/c-report.md", "files/r/a-report.md",
	}; !slices.Equal(got, want) {
		t.Errorf("the newest reports are %v, want %v", got, want)
	}
}

// TestStoreAnOmittedSubtreeIsLeftOutOfASearchAndRecent: a match inside an
// omitted subtree is never answered, a page stays as full as what may be
// read allows, and the newest page counts its limit over what may be read.
func TestStoreAnOmittedSubtreeIsLeftOutOfASearchAndRecent(t *testing.T) {
	db := tier(t)
	owner := aSpaceOf(t)
	now := time.Now().UTC().Truncate(time.Second)
	for path, age := range map[string]time.Duration{
		"files/team/payroll/plan-a.md":      0,
		"files/team/payroll/2026/plan-b.md": time.Minute,
		"files/team/plan.md":                time.Hour,
		"files/team/payroll-plan.md":        2 * time.Hour,
		"files/zz/plan-c.md":                3 * time.Hour,
	} {
		reaperFile(t, db, owner, path, 1)
		stampModified(t, db, owner, path, now.Add(-age))
	}
	outside := []string{"files/team/payroll-plan.md", "files/team/plan.md", "files/zz/plan-c.md"}

	if got := searchAll(t, db, owner, "files/", "plan", 1, omitting); !slices.Equal(got, outside) {
		t.Errorf("the search in pages of one with the prefix omitted walked %v, want %v", got, outside)
	}
	if got := searchAll(t, db, owner, "files/", "plan", 100, nil); len(got) != 5 {
		t.Errorf("the search with no omission holds %v", got)
	}
	if got := searchAll(t, db, owner, "files/team/payroll/", "plan", 100, omitting); len(got) != 0 {
		t.Errorf("a search inside the omitted prefix holds %v", got)
	}

	for _, text := range []string{"", "plan"} {
		rows, err := NewFiles().RecentPrefix(t.Context(), db.Querier(), owner, "files/", text, 2, omitting)
		if err != nil || !slices.Equal(pathsOfFiles(rows), []string{"files/team/plan.md", "files/team/payroll-plan.md"}) {
			t.Errorf("the newest two for %q with the prefix omitted are %v, %v", text, pathsOfFiles(rows), err)
		}
	}
	whole, err := NewFiles().RecentPrefix(t.Context(), db.Querier(), owner, "files/", "", 2, nil)
	if err != nil || !slices.Equal(pathsOfFiles(whole), []string{"files/team/payroll/plan-a.md", "files/team/payroll/2026/plan-b.md"}) {
		t.Errorf("the newest two with no omission are %v, %v", pathsOfFiles(whole), err)
	}
}
