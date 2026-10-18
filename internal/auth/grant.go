// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
)

// The grant step of the question (spec 006). Every question about a file or
// an upload carries the highest live grant the caller holds on a prefix of
// the resource's path, in the caller's favor, resolved here before the
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
//
// Beside the caller's rung the question carries every holder's, as grants
// (amended 2026-10-18). An endpoint may decide a question as a subject
// other than the caller, such as an agent's question decided as the person
// the agent acts for, and Arca cannot know which subject that is without
// reading a claim for meaning. So it names them all, bounded, and the
// endpoint picks the one it decides as.

// WithGrants binds the grants table the decide path resolves the resource's
// grant through, and answers the authorizer so a node builds it in one
// expression. Nil reads no table, which is an installation that has issued
// no grant and is every installation until spec 008's table holds a row.
func (a *Authorizer) WithGrants(g GrantLookup) *Authorizer {
	a.grants = g
	return a
}

// granted answers the resource with the caller's rung on it and every
// holder's, and the resource unchanged where there is nothing to add: a
// kind no grant reaches, a question that names no path, an anonymous
// caller, or a caller asking about its own space. A caller who holds
// nothing here gets no grant, and the holders are sent all the same, an
// empty list where nobody holds one.
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
	held, err := heldOn(ctx, a.grants, owner, subject, res)
	if err != nil {
		return res, fmt.Errorf("the grants of %s: %w", owner, err)
	}
	holders, truncated, err := holdersOn(ctx, a.grants, owner, res)
	if err != nil {
		return res, fmt.Errorf("the holders of a grant in %s: %w", owner, err)
	}
	fields := maps.Clone(res.Fields)
	if fields == nil {
		fields = map[string]any{}
	}
	if held != PermissionNone {
		fields[GrantField] = string(held)
	}
	fields[GrantsField] = authorizer.Holders(holders)
	if truncated {
		fields[GrantsTruncatedField] = true
	}
	return authz.NewResource(res.Kind, res.ID, fields), nil
}

// holdersOn answers every subject's rung on a resource of owner's space,
// ordered by subject and at most [authorizer.MaxGrants] of them, and
// whether more hold one than the list names.
//
// The table is asked for one more than the bound, so a list that fills it
// is told apart from one that was cut without a count. The order is sorted
// again here, in byte order, because it is part of the wire: the client
// caches an answer by the bytes of its question, and one set of holders in
// two orders would be two questions.
func holdersOn(ctx context.Context, grants GrantLookup, owner string, res authz.Resource) ([]authorizer.Holder, bool, error) {
	list, err := grants.Holders(ctx, owner, grantPath(res), res.String(FromField), authorizer.MaxGrants+1)
	if err != nil {
		return nil, false, err
	}
	if list == nil {
		list = []authorizer.Holder{}
	}
	slices.SortFunc(list, func(a, b authorizer.Holder) int { return strings.Compare(a.Subject, b.Subject) })
	truncated := len(list) > authorizer.MaxGrants
	if truncated {
		list = list[:authorizer.MaxGrants]
	}
	// The bytes are bounded too, so subjects of any length a table holds
	// cannot carry the question past what an endpoint reads.
	bytes := 0
	for i, h := range list {
		bytes += len(h.Subject) + len(h.Permission)
		if bytes > authorizer.MaxGrantsBytes {
			return list[:i], true, nil
		}
	}
	return list, truncated, nil
}

// GrantField is the member of the resource the rung is sent as, the name
// spec 006's table gives it and `authorizer.File.Grant` renders.
const GrantField = "grant"

// GrantsField and GrantsTruncatedField are the members every holder's
// rung is sent as, the names spec 006's table gives them and
// `authorizer.File.Grants` and `authorizer.File.GrantsTruncated` render.
const (
	GrantsField          = "grants"
	GrantsTruncatedField = "grants_truncated"
)

// FromField is the member of the resource that names a second path, the
// source of an operation whose resource path is its destination.
const FromField = "from"

// heldOn answers the rung subject holds on a resource of owner's space. A
// resource that names a source beside its path is read on both paths, and
// the answer is the lower of the two rungs, with no rung below read: the
// question is about both ends, so the rung it carries is the one the caller
// holds on both.
//
// It is the one reading of the table the field above and the owner policy's
// grant step share, so the two cannot answer differently about one resource.
func heldOn(ctx context.Context, grants GrantLookup, owner, subject string, res authz.Resource) (Permission, error) {
	held, err := grants.Permission(ctx, owner, subject, grantPath(res))
	if err != nil || held == PermissionNone {
		return PermissionNone, err
	}
	from := res.String(FromField)
	if from == "" {
		return held, nil
	}
	source, err := grants.Permission(ctx, owner, subject, from)
	if err != nil {
		return PermissionNone, err
	}
	if rung(source) < rung(held) {
		return source, nil
	}
	return held, nil
}

// carriesGrant reports whether a kind carries the field. A file and an
// upload name a subtree a grant can cover; a share, a link, an event and a
// space are powers over a space rather than over a subtree of it, so no
// grant reaches their actions and a field there would be read as if one
// did.
//
// The upload is here because upload.write sits on the write rung's ladder.
// An object at or below the inline bound reaches the grantee through
// file.write and a larger one through this question, so leaving it out
// would have admitted a grantee's small writes and refused the same
// grantee's large ones.
func carriesGrant(kind string) bool {
	return kind == authorizer.KindFile || kind == authorizer.KindUpload
}
