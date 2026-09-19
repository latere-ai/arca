// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"
	"maps"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
)

// The grant step of the question (spec 006). Every question about a file or
// a workspace carries the highest live grant the caller holds on a prefix of
// the resource's path, in the caller's favour, resolved here before the
// question goes out.
//
// It is resolved in both modes, and that is the point. With
// ARCA_AUTHORIZER_URL set the owner policy is not consulted at all, so an
// endpoint that sees an owner, a path, a plane and a size has nothing to
// tell a grantee from a stranger with: every read of a shared object is a
// deny, and POST /v1/shares writes a row no decision consults. The endpoint
// still decides what the rung admits; this says what the caller holds.
//
// Arca reads no claim for meaning to fill it. It reads its own grants table,
// through the same [GrantLookup] seam the owner policy reads, which spec 006
// already allows: a grant is a resource of the core, not a claim about a
// person.
//
// The owner policy keeps its own grant step. This field is what an endpoint
// reads; that step is what the policy decides on, and folding one into the
// other would make the policy's answer depend on a field the policy itself
// filled in.

// WithGrants binds the grants table the decide path resolves the resource's
// grant through, and answers the authorizer so a node builds it in one
// expression. Nil reads no table, which is an installation that has issued
// no grant and is every installation until spec 008's table holds a row.
func (a *Authorizer) WithGrants(g GrantLookup) *Authorizer {
	a.grants = g
	return a
}

// granted answers the resource with the caller's rung on it, and the
// resource unchanged where there is no rung to add: a kind no grant reaches,
// a question that names no path, an anonymous caller, a caller asking about
// its own space, or a caller who holds nothing here.
//
// A caller's own space is settled without reading the table, which is spec
// 006's sentence that ownership is not a grant. It is also the question this
// core is asked most: an owner holds the whole space, which is strictly more
// than any grant on a subtree of it could confer, so the field would carry
// nothing an endpoint could act on and the query would be pure cost on the
// hot read path.
//
// A table that cannot answer is no decision. The failure reaches the caller
// as authorizer_unavailable and never as a deny, which is the rule spec 006
// puts on the owner policy's own grant step: a question sent without a grant
// the caller holds is a question the endpoint answers wrong, and a wrong
// answer is worse than none.
func (a *Authorizer) granted(ctx context.Context, res authz.Resource) (authz.Resource, error) {
	if a.grants == nil || !carriesGrant(res.Kind) {
		return res, nil
	}
	subject := CallerFrom(ctx).Subject
	owner, path := res.String("owner"), grantPath(res)
	if subject == "" || owner == "" || path == "" || subject == owner {
		return res, nil
	}
	held, err := a.grants.Permission(ctx, owner, subject, path)
	if err != nil {
		return res, fmt.Errorf("the grants of %s: %w", owner, err)
	}
	if held == PermissionNone {
		return res, nil
	}
	fields := maps.Clone(res.Fields)
	if fields == nil {
		fields = map[string]any{}
	}
	fields[GrantField] = string(held)
	return authz.NewResource(res.Kind, res.ID, fields), nil
}

// GrantField is the member of the resource the rung is sent as, the name
// spec 006's table gives it and `authorizer.File.Grant` renders.
const GrantField = "grant"

// carriesGrant reports whether a kind carries the field. A file and a
// workspace name a subtree a grant can cover; a share, a link, an event and
// a space are powers over a space rather than over a subtree of it, so no
// grant reaches their actions and a field there would be read as if one did.
func carriesGrant(kind string) bool {
	return kind == authorizer.KindFile || kind == authorizer.KindWorkspace
}
