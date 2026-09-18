// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import (
	"fmt"
	"strings"
)

// Plane is one of the two prefixes a path is rooted in. A space holds
// nothing outside them.
//
//	files/         objects a person, an application, or an agent stores
//	workspaces/    a durable subtree a sandbox attaches to
//
// The predecessor had four planes. A repository's history lives on a git
// host, so there is no plane for it, and memory files are a convention under
// files/ rather than a plane of their own (spec 019).
type Plane string

// The two planes. Their rules are spec 005's and spec 009's.
const (
	PlaneFiles      Plane = "files"
	PlaneWorkspaces Plane = "workspaces"
)

// Prefix is what a path in the plane begins with.
func (p Plane) Prefix() string { return string(p) + "/" }

// Valid reports whether p is one of the two planes.
func (p Plane) Valid() bool { return p == PlaneFiles || p == PlaneWorkspaces }

// SplitPath reads the plane a path is rooted in and the rest of the path.
// The rest is what the plane's own spec gives meaning to; this function
// refuses an empty rest and an unknown plane and judges nothing else.
func SplitPath(path string) (Plane, string, error) {
	for _, p := range []Plane{PlaneFiles, PlaneWorkspaces} {
		rest, ok := strings.CutPrefix(path, p.Prefix())
		if !ok {
			continue
		}
		if rest == "" {
			return "", "", fmt.Errorf("object: path %q names the plane and nothing in it", path)
		}
		return p, rest, nil
	}
	return "", "", fmt.Errorf("object: path %q is in no plane; a path begins with %q or %q", path, PlaneFiles.Prefix(), PlaneWorkspaces.Prefix())
}
