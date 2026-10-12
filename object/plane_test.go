// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import "testing"

// TestThePlaneIsFiles is criterion 1 of spec 028: a space is one tree, so
// the type has one valid value and every other first segment is no plane.
func TestThePlaneIsFiles(t *testing.T) {
	if !PlaneFiles.Valid() {
		t.Errorf("%q is not valid", PlaneFiles)
	}
	if got, want := PlaneFiles.Prefix(), "files/"; got != want {
		t.Errorf("Prefix() = %q, want %q", got, want)
	}
	for _, p := range []Plane{"", "memory", "repos", "agents", "Files"} {
		if p.Valid() {
			t.Errorf("%q is valid, and the plane is files", p)
		}
	}
}

func TestSplitPathReadsThePlaneAndLeavesTheRestAlone(t *testing.T) {
	for path, want := range map[string]string{
		"files/notes/todo.md":       "notes/todo.md",
		"files/memory/agent.json":   "memory/agent.json",
		"files/repos/build/main.go": "repos/build/main.go",
		"files/ ":                   " ",
	} {
		plane, rest, err := SplitPath(path)
		if err != nil || plane != PlaneFiles || rest != want {
			t.Errorf("SplitPath(%q) = %q, %q, %v", path, plane, rest, err)
		}
	}
	for _, path := range []string{"", "files", "files/", "repos/x", "/files/x", "memory/a", "filesystem/a"} {
		if plane, rest, err := SplitPath(path); err == nil {
			t.Errorf("SplitPath(%q) = %q, %q, want an error", path, plane, rest)
		}
	}
}
