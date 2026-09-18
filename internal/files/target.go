// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/object"
)

// Target is the space and the path one request names, after the rules of
// spec 005 have been applied to both. Every handler resolves one before it
// asks its question, because a path that is not a path is a refusal the
// authorizer should never be asked about.
type Target struct {
	// Owner is the rendered subject of the space, never the alias.
	Owner string
	// Path is plane rooted.
	Path string
	// Plane is the plane the path is rooted in.
	Plane object.Plane
}

// MaxSubject is the bound spec 004 puts on a subject column. A longer owner
// names no space this server could have written, so it is refused before it
// reaches a query.
const MaxSubject = 512

// meAlias is the one alias the owner takes: the caller's own space. A
// response renders the subject in full and never an alias, so a client that
// stores what it read can send it back (spec 013).
const meAlias = "me"

// Target resolves the space and the path a request named and answers the
// workspace liveness rule with them: a path under workspaces/ whose
// workspace does not exist or is soft deleted is a missing object to
// everyone, which is spec 009's rule and spec 005 applies it.
func (s *Service) Target(ctx context.Context, owner, path string) (Target, error) {
	return s.resolve(ctx, owner, path, ValidatePath)
}

// Subtree resolves what a listing names, which is a path or a prefix above
// one. A listing of a whole plane is a legitimate question and a path naming
// a plane and nothing in it is not, so the two rules are not one function.
func (s *Service) Subtree(ctx context.Context, owner, prefix string) (Target, error) {
	return s.resolve(ctx, owner, prefix, ValidatePrefix)
}

// resolve is the half the two share.
func (s *Service) resolve(ctx context.Context, owner, path string, valid func(string) (object.Plane, error)) (Target, error) {
	subject, err := s.owner(ctx, owner)
	if err != nil {
		return Target{}, err
	}
	plane, err := valid(path)
	if err != nil {
		return Target{}, err
	}
	t := Target{Owner: subject, Path: path, Plane: plane}
	slug := workspaceSlug(path)
	if plane != object.PlaneWorkspaces || slug == "" {
		return t, nil
	}
	live, err := s.workspaces.Live(ctx, s.db.Querier(), subject, slug)
	if err != nil {
		return Target{}, fault(ctx, "read the workspace", err)
	}
	if !live {
		return Target{}, api.Refuse(api.CodeNotFound, "the workspace of %q is not there", path)
	}
	return t, nil
}

// owner resolves the space a request addressed: the alias me, or a rendered
// subject the caller sent back.
func (s *Service) owner(ctx context.Context, raw string) (string, error) {
	caller := Caller(ctx)
	if raw == "" || raw == meAlias {
		if caller == "" {
			return "", api.Refuse(api.CodeUnauthenticated, "the alias me names the caller's own space and this request carries no caller")
		}
		return caller, nil
	}
	switch {
	case len(raw) > MaxSubject:
		return "", api.Refuse(api.CodeInvalidField,
			"the owner is %d bytes and a subject is at most %d", len(raw), MaxSubject).About("owner")
	case !printable(raw):
		return "", api.Refuse(api.CodeInvalidField, "the owner holds a character a subject cannot carry").About("owner")
	}
	return raw, nil
}

// ValidatePath is spec 005's rule for a path, in the order the rule is
// written: the shape first, so a path that is not one is never read for its
// plane, and the plane second.
//
//   - no leading or trailing slash, no empty segment, no . and no ..
//   - nothing a database column or a log line cannot carry
//   - the first segment is a plane of spec 001
//   - under workspaces/ at least three segments, because bytes live inside
//     a workspace and not beside one
func ValidatePath(path string) (object.Plane, error) {
	switch {
	case path == "":
		return "", api.Refuse(api.CodeInvalidPath, "the path is empty").About("path")
	case strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/"):
		return "", api.Refuse(api.CodeInvalidPath, "the path %q is rooted or ends in a slash", path).About("path")
	case !printable(path):
		return "", api.Refuse(api.CodeInvalidPath, "the path holds a character this server does not store").About("path")
	}
	for segment := range strings.SplitSeq(path, "/") {
		switch segment {
		case "":
			return "", api.Refuse(api.CodeInvalidPath, "the path %q has an empty segment", path).About("path")
		case ".", "..":
			return "", api.Refuse(api.CodeInvalidPath, "the path %q names a segment that is not a name", path).About("path")
		}
	}
	plane, rest, err := object.SplitPath(path)
	if err != nil {
		return "", api.Refuse(api.CodeUnknownPlane,
			"the path %q is in no plane; a path begins with %q or %q",
			path, object.PlaneFiles.Prefix(), object.PlaneWorkspaces.Prefix()).About("path")
	}
	if plane == object.PlaneWorkspaces && !strings.Contains(rest, "/") {
		return "", api.Refuse(api.CodeInvalidPath,
			"the path %q names a workspace and nothing in it; bytes live inside a workspace", path).About("path")
	}
	return plane, nil
}

// ValidatePrefix is the rule a listing reads. It is ValidatePath with one
// row removed: a prefix may name a plane and nothing in it, because a
// listing of a whole plane is a question and an object at a plane root is
// not a path.
func ValidatePrefix(prefix string) (object.Plane, error) {
	if plane := object.Plane(prefix); plane.Valid() {
		return plane, nil
	}
	plane, err := ValidatePath(prefix)
	if err == nil {
		return plane, nil
	}
	// A workspace root is a subtree a listing may name, and the path rule
	// refuses it because bytes live inside a workspace and not beside one.
	if p, rest, split := object.SplitPath(prefix); split == nil &&
		p == object.PlaneWorkspaces && !strings.Contains(rest, "/") && shaped(prefix) {
		return p, nil
	}
	return "", err
}

// shaped reports whether a prefix holds no segment a path may not hold,
// which is the half of the path rule a prefix keeps.
func shaped(prefix string) bool {
	if strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") || !printable(prefix) {
		return false
	}
	for segment := range strings.SplitSeq(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// workspaceSlug is the workspace a path under workspaces/ belongs to, and ""
// for a prefix that names the plane and no workspace in it.
func workspaceSlug(path string) string {
	_, rest, err := object.SplitPath(path)
	if err != nil {
		return ""
	}
	slug, _, _ := strings.Cut(rest, "/")
	return slug
}

// printable reports whether a value holds only characters a column, a key
// and a log line can all carry. A control character in a path is refused
// here rather than at the database, where a NUL is a fault and not a
// verdict.
func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// fault renders a failure of either store. The caller reads that storage is
// unavailable and retries; the developer detail names the operation and no
// store, query, key or internal type, which is spec 013's rule for anything
// above 499. The error itself goes to the log, which is where an operator
// reads it.
func fault(ctx context.Context, what string, err error) error {
	slog.ErrorContext(ctx, "files: "+what, "error", err)
	return api.Refuse(api.CodeStorageUnavailable, "could not %s", what)
}

// Committed renders what a transaction answered. A refusal the body decided
// is the caller's own answer; anything else is the transaction itself
// failing to open or to commit, which is the store and not the request.
func Committed(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, refusal := errors.AsType[*api.Refusal](err); refusal {
		return err
	}
	return fault(ctx, "record the write", err)
}

// Caller is the subject the verified token identifies, and "" outside a
// verified request. Spec 007's package reads it through this.
func Caller(ctx context.Context) string { return auth.CallerFrom(ctx).Subject }
