// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"strings"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/object"
)

// cleanPrefix reads the subtree a grant covers off a request.
//
// A prefix is a plane rooted path with no trailing slash, and a grant on one
// covers that path and everything under it. The whole of the plane is a
// prefix too, because a space's owner may hand out the plane; nothing
// shallower exists, since a path outside the plane is a path this server
// does not serve.
//
// The refusals are spec 013's: a path that is not one this server accepts is
// invalid_path, and one that begins with something that is not the plane is
// unknown_plane. The two are told apart because an operator reading the
// second learns what a path begins with.
func cleanPrefix(raw string) (string, error) {
	prefix := strings.TrimSuffix(raw, "/")
	if prefix == "" {
		return "", api.Refuse(api.CodeMissingField, "a grant covers a subtree, and none is named").About("path_prefix")
	}
	if err := usablePath(prefix); err != nil {
		return "", err
	}
	// The plane on its own is the whole of it, and anything deeper is read
	// for the plane it is rooted in. Which prefix is the plane is the
	// frame's answer rather than this package's own reading of spec 001, so
	// the plane stays one fact for every package above the frame.
	if !api.PlaneServed(prefix) {
		return "", api.Refuse(api.CodeUnknownPlane,
			"a path begins with %q, and %q does not",
			object.PlaneFiles.Prefix(), prefix).About("path_prefix")
	}
	return prefix, nil
}

// usablePath refuses what no path of this server may hold: an empty segment,
// a relative segment, a leading slash, and a control character. It judges the
// shape and not the plane, so one rule serves a prefix and the path a link
// route is asked for.
func usablePath(path string) error {
	refuse := func(why string) error {
		return api.Refuse(api.CodeInvalidPath, "%q %s", path, why).About("path")
	}
	switch {
	case path == "":
		return refuse("is empty")
	case strings.HasPrefix(path, "/"):
		return refuse("begins with a slash, and a path is rooted in a plane")
	case strings.HasSuffix(path, "/"):
		return refuse("ends with a slash, and a path names an object")
	}
	for segment := range strings.SplitSeq(path, "/") {
		switch segment {
		case "":
			return refuse("has an empty segment")
		case ".", "..":
			return refuse("has a relative segment, and a path is not resolved")
		}
	}
	if strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return refuse("holds a control character")
	}
	return nil
}

// covers reports whether a grant on prefix covers path: the path is the
// prefix, or the path is inside the prefix's subtree.
//
// The match is by segment, which is the whole of the rule: files/reports
// covers files/reports and files/reports/q3.pdf and does not cover
// files/reports-archive.
func covers(prefix, path string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
