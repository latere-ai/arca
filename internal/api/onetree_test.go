// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// retired is the word criterion 24 of spec 028 looks for, in any letter
// case: the name of the plane that spec removed.
const retired = "workspace"

// records are the places that keep the word because each is the record of
// what was removed: the deck, the changelog, and the migrations, which
// create the two tables and drop them. An entry that ends in a slash covers
// every file under that directory.
var records = []string{"specs/", "CHANGELOG.md", "internal/store/migrations/"}

// excepted are the files outside the records that name a workspace, each
// with the reason it has to. An entry that ends in a slash covers every file
// under that directory.
var excepted = []string{
	// This test spells the word it looks for.
	"internal/api/onetree_test.go",
	// The test of migration 0006 seeds the two tables the migration drops.
	"internal/store/onetree_tier_test.go",
	// The workflows read github.workspace, the runner's checkout directory.
	".github/workflows/",
	// The finished migration from the predecessor and the page about it,
	// which wrote into the schema as it was before migration 0006.
	"tools/migrate-drive/",
	"docs/internals/drive-migration.md",
	// The upgrade section an operator reads before running migration 0006,
	// which names the paths and routes clients move away from.
	"docs/operations.md",
	// The compatibility assertions of spec 028: a path under workspaces/ is
	// unknown_plane, and nothing is registered under /v1/workspaces.
	"test/conformance/cases028.go",
	"internal/files/onetree_test.go",
}

// TestNothingNamesAWorkspace is criterion 24 of spec 028: no file of the
// tree outside the deck, the changelog and the migrations names a workspace,
// in its path or in its content. The plane is removed, so a file that still
// names it describes something this build does not have, and whoever reads
// that file goes looking for it.
//
// The tree is read from the disk and not asked of git, because the gate runs
// the suite with only the Go toolchain on PATH. What .gitignore lists is not
// the tree and is passed over, so a build output or a coverage profile left
// in a checkout is not read.
func TestNothingNamesAWorkspace(t *testing.T) {
	root := moduleRoot(t)
	rules := ignoreRules(t, root)
	kept := slices.Concat(records, excepted)
	var named []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file another process removed between the listing of its
			// directory and this call names nothing.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		out, err := passedOver(rules, rel, d.IsDir())
		if err != nil {
			return err
		}
		// .git is the repository's own data: a directory in a clone and a
		// file in a worktree.
		if out || d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || covered(kept, rel) {
			return nil
		}
		if strings.Contains(strings.ToLower(rel), retired) {
			named = append(named, rel+" (its path)")
			return nil
		}
		raw, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if at := bytes.Index(bytes.ToLower(raw), []byte(retired)); at >= 0 {
			line := 1 + bytes.Count(raw[:at], []byte("\n"))
			named = append(named, fmt.Sprintf("%s (line %d)", rel, line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	slices.Sort(named)
	for _, file := range named {
		t.Errorf("%s names a workspace", file)
	}
	if len(named) > 0 {
		t.Errorf("%d files name the plane spec 028 removed; reword each, or add it to excepted with the reason it keeps the word", len(named))
	}
}

// moduleRoot answers the repository root from this file's own location,
// two directories above the package. It is read through runtime.Caller and
// not through the working directory, so the test reads the tree it was
// built from wherever it is run.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test binary carries no source position")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root: %v", root, err)
	}
	return root
}

// covered reports whether one of the entries names the file: the file
// itself, or a directory above it where the entry ends in a slash.
func covered(entries []string, rel string) bool {
	for _, entry := range entries {
		if rel == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(rel, entry)) {
			return true
		}
	}
	return false
}

// ignoreRule is one line of .gitignore.
type ignoreRule struct {
	// pattern is one path segment, with the globs of path.Match.
	pattern string
	// negated is a line that begins with "!" and takes a path back in.
	negated bool
	// anchored is a line that begins with "/" and matches at the root only.
	anchored bool
	// directory is a line that ends in "/" and matches a directory alone.
	directory bool
}

// ignoreRules reads the root .gitignore in the forms that file uses: a name
// or a glob matched at any depth, a leading slash that anchors it at the
// root, a trailing slash that makes it a directory, and "!" that takes a
// path back in. A line in any other form fails the test, so a rule this
// reader would misread is noticed when it is written and not when a file it
// meant to ignore is read.
func ignoreRules(t *testing.T, root string) []ignoreRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	var rules []ignoreRule
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var rule ignoreRule
		rule.pattern, rule.negated = strings.CutPrefix(line, "!")
		rule.pattern, rule.anchored = strings.CutPrefix(rule.pattern, "/")
		rule.pattern, rule.directory = strings.CutSuffix(rule.pattern, "/")
		if _, err := path.Match(rule.pattern, ""); err != nil || rule.pattern == "" ||
			strings.Contains(rule.pattern, "/") || strings.Contains(rule.pattern, "**") {
			t.Fatalf(".gitignore holds %q, a form this test does not read; teach ignoreRules the form", line)
		}
		rules = append(rules, rule)
	}
	return rules
}

// passedOver reports whether the rules ignore a path. The last rule that
// matches decides, which is how git reads the file.
func passedOver(rules []ignoreRule, rel string, dir bool) (bool, error) {
	out := false
	for _, rule := range rules {
		if rule.directory && !dir {
			continue
		}
		// A pattern is one segment and a glob does not cross a slash, so an
		// anchored rule read against the whole path matches at the root only.
		subject := path.Base(rel)
		if rule.anchored {
			subject = rel
		}
		ok, err := path.Match(rule.pattern, subject)
		if err != nil {
			return false, err
		}
		if ok {
			out = !rule.negated
		}
	}
	return out, nil
}
