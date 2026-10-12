// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import (
	"fmt"
	"strings"
)

// Plane is the prefix a path is rooted in. A space is one tree, and every
// path of it begins with the one plane:
//
//	files/    objects a person, an application, or an agent stores
//
// The type stays a type so a question and a metric keep naming the plane
// they always named. The predecessor had four planes: a repository's history
// lives on a git host, memory files are a convention under files/ rather
// than a plane of their own (spec 019), and a subtree a program manages is a
// folder like any other (spec 028).
type Plane string

// PlaneFiles is the one plane. Its rules are spec 005's.
const PlaneFiles Plane = "files"

// Prefix is what a path in the plane begins with.
func (p Plane) Prefix() string { return string(p) + "/" }

// Valid reports whether p is the plane.
func (p Plane) Valid() bool { return p == PlaneFiles }

// SplitPath reads the plane a path is rooted in and the rest of the path.
// The rest is what spec 005 gives meaning to; this function refuses an empty
// rest and a path outside the plane and judges nothing else.
func SplitPath(path string) (Plane, string, error) {
	rest, ok := strings.CutPrefix(path, PlaneFiles.Prefix())
	switch {
	case !ok:
		return "", "", fmt.Errorf("object: path %q is in no plane; a path begins with %q", path, PlaneFiles.Prefix())
	case rest == "":
		return "", "", fmt.Errorf("object: path %q names the plane and nothing in it", path)
	}
	return PlaneFiles, rest, nil
}
