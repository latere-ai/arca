// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWalkSourcesReadsThisTreeAndNotACopyUnderAHiddenDirectory(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.go", "a_test.go", "sub/b.go", ".claude/worktrees/x/c.go", ".hidden/d.go"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	if err := walkSources(root, func(path string) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		seen = append(seen, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(seen)
	if want := []string{"a.go", "sub/b.go"}; !slices.Equal(seen, want) {
		t.Errorf("walked %v, want %v: tests and hidden directories are not sources of this tree", seen, want)
	}
}
