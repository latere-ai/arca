// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import "testing"

func TestThePlanesAreTwo(t *testing.T) {
	for _, p := range []Plane{PlaneFiles, PlaneWorkspaces} {
		if !p.Valid() {
			t.Errorf("%q is not valid", p)
		}
		if got, want := p.Prefix(), string(p)+"/"; got != want {
			t.Errorf("Prefix() = %q, want %q", got, want)
		}
	}
	for _, p := range []Plane{"", "memory", "repos", "agents"} {
		if p.Valid() {
			t.Errorf("%q is valid, and the planes are files and workspaces", p)
		}
	}
}

func TestSplitPathReadsThePlaneAndLeavesTheRestAlone(t *testing.T) {
	for path, want := range map[string]struct {
		plane Plane
		rest  string
	}{
		"files/notes/todo.md":      {PlaneFiles, "notes/todo.md"},
		"files/memory/agent.json":  {PlaneFiles, "memory/agent.json"},
		"workspaces/build/main.go": {PlaneWorkspaces, "build/main.go"},
		"files/ ":                  {PlaneFiles, " "},
	} {
		plane, rest, err := SplitPath(path)
		if err != nil || plane != want.plane || rest != want.rest {
			t.Errorf("SplitPath(%q) = %q, %q, %v", path, plane, rest, err)
		}
	}
	for _, path := range []string{"", "files", "files/", "repos/x", "/files/x", "memory/a"} {
		if plane, rest, err := SplitPath(path); err == nil {
			t.Errorf("SplitPath(%q) = %q, %q, want an error", path, plane, rest)
		}
	}
}
