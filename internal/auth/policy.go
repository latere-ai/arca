// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
)

// The reasons the owner policy adds to the shared frame's. The frame names
// the probe, the anonymous subject and the object nobody owns; these are the
// rows spec 006 writes on top of it.
const (
	// ReasonUnknownAction is an action outside Arca's vocabulary. The client
	// refuses one before the wire, so reaching this is a caller inside the
	// process that skipped it, or an operator's endpoint serving this policy
	// to a core that is not Arca.
	ReasonUnknownAction = "unknown_action"
	// ReasonNotGranted is a caller who owns nothing here and holds no live
	// grant on a prefix of the path, and, for link.read, no link that
	// resolves. It is the last row of spec 006's flowchart.
	ReasonNotGranted = "not_granted"
)

// Permission is a rung of the share ladder of spec 008, which spec 006's
// owner policy reads a grant against: read < write < manage. The strings are
// the ones a grant row carries and a share body renders.
type Permission string

// The rungs, and the absence of one.
const (
	PermissionNone   Permission = ""
	PermissionRead   Permission = "read"
	PermissionWrite  Permission = "write"
	PermissionManage Permission = "manage"
)

// rung orders the ladder. A rung admits every action of the rungs below it,
// which is what "write adds" and "manage adds" mean in spec 006.
func rung(p Permission) int {
	switch p {
	case PermissionRead:
		return 1
	case PermissionWrite:
		return 2
	case PermissionManage:
		return 3
	}
	return 0
}

// ladder is spec 006's reading of the share ladder: the lowest rung a grant
// must reach for each action. An action absent here is reachable by no
// grant, which is the owner's or an administrator's alone: space.admin,
// workspace.create and workspace.delete, which spec 006 names, and
// workspace.restore, event.read and the three link.* actions, which follow
// from the same rule, since undoing a delete, reading a space's log and
// minting or revoking a public token are powers over the space rather than
// over a subtree of it.
var ladder = map[string]Permission{
	authorizer.ActionFileRead:    PermissionRead,
	authorizer.ActionFileList:    PermissionRead,
	authorizer.ActionFileWrite:   PermissionWrite,
	authorizer.ActionFileDelete:  PermissionWrite,
	authorizer.ActionFileRestore: PermissionWrite,
	authorizer.ActionUploadWrite: PermissionWrite,

	authorizer.ActionWorkspaceRead:   PermissionRead,
	authorizer.ActionWorkspaceList:   PermissionRead,
	authorizer.ActionWorkspaceWrite:  PermissionWrite,
	authorizer.ActionWorkspaceAttach: PermissionWrite,
	authorizer.ActionWorkspaceSync:   PermissionWrite,

	authorizer.ActionShareCreate: PermissionManage,
	authorizer.ActionShareRead:   PermissionManage,
	authorizer.ActionShareList:   PermissionManage,
	authorizer.ActionShareRevoke: PermissionManage,
}

// Granted is the rung a grant must reach for an action, and false where no
// grant reaches it.
func Granted(action string) (Permission, bool) {
	p, ok := ladder[action]
	return p, ok
}

// Admits reports whether a caller holding one rung may perform an action
// that needs another, which is the whole of what the ladder says. It is
// exported because an authorizer decides the same way off the grant the
// question of spec 006 carries, and a second copy of the comparison is a
// second answer waiting to disagree with this one.
func Admits(held, need Permission) bool { return rung(held) >= rung(need) }

// GrantLookup answers the grant step of spec 006's flowchart: the highest
// live permission subject holds on a prefix of path in owner's space.
// [PermissionNone] is no grant at all.
//
// It is an interface because the grants table is spec 008's and arrives with
// it; until then the owner policy runs with none, which is the answer for an
// installation that has issued no grant. The owner policy is the one place
// this core reads its own state to decide, and that is deliberate: a grant is
// a resource of the core, not a claim about a person, and a platform that
// wants grants decided elsewhere configures an authorizer and this policy is
// not consulted.
type GrantLookup interface {
	Permission(ctx context.Context, owner, subject, path string) (Permission, error)
}

// LinkResolver answers the link step: whether id names a live public link on
// owner's space that covers path. The handler of a public link route
// resolves the token in the URL first and answers not_found when it names
// nothing, so what reaches here is a link the request already found; this
// confirms it is still live and still covers what is being read, which is
// what lets an operator turn public reading off by denying link.read.
//
// Like [GrantLookup] it arrives with spec 008.
type LinkResolver interface {
	Live(ctx context.Context, id, owner, path string) (bool, error)
}

// OwnerPolicy is the policy arcad applies with ARCA_AUTHORIZER_URL unset. It
// is a policy with tests, not the absence of one: the rows of spec 006 in
// front of the shared frame of latere.ai/x/pkg/authz, which decides the
// probe, the anonymous subject, the administrator and the owner.
//
// The space's owner is read off the resource, because arcad built that
// resource from the path the request named before it asked. Every question
// carries an owner, a create included, since a space is addressed by subject
// and a new object is created in a space that already has one; so the frame
// needs no create action and the owner field decides every row of it.
type OwnerPolicy struct {
	// Admins are the rendered subjects of ARCA_ADMIN_SUBJECTS.
	Admins []string
	// Grants is the grants table, nil until spec 008 lands one.
	Grants GrantLookup
	// Links is the public link table, nil until spec 008 lands one.
	Links LinkResolver
}

// Authorize answers one request, so the owner policy and an operator's
// endpoint are one seam to everything above.
//
// The answer is the policy's own decision intersected with the grants the
// caller's token carries. A personal key is narrower than the person who
// holds it: the policy says what the person may do, and authz.Restrict
// removes what the credential was not granted. The conjunction turns an
// allow into a deny and never a deny into an allow, so a grant on a key is a
// restriction and never authority, and a token of any other credential class
// is decided by the policy alone.
func (p *OwnerPolicy) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	d, err := p.decide(ctx, req)
	if err != nil {
		return authz.Decision{}, err
	}
	return p.restrict(d, req), nil
}

// Decide is the same answer under the name latere.ai/x/pkg/authz/server
// calls a decider by, so the policy arcad runs in process is also the
// endpoint an operator serves from it, with no second implementation between
// the two.
func (p *OwnerPolicy) Decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	return p.Authorize(ctx, req)
}

// restrict is the intersection itself, the same shape the scaffold of
// latere.ai/x/pkg/authz/server applies to its own decider's answer.
//
// A claim that does not read as grants is a deny and not an error: an error
// is a decision point that produced no decision, which a core reads as an
// outage at the authorizer, and there is no outage. There is a credential
// whose reach nobody can read, and the closed answer is the only safe one.
func (p *OwnerPolicy) restrict(d authz.Decision, req authz.Request) authz.Decision {
	if !d.Allow {
		// A deny keeps the reason the policy gave it. There is nothing for
		// the grants to narrow, and the policy's reason is the one worth
		// reading.
		return d
	}
	grants, err := authz.ParseGrants(req.Claims)
	if err != nil {
		return authz.Decision{Reason: authz.ReasonGrant}
	}
	return authz.Restrict(authorizer.Core, d, req, grants)
}

// decide is the policy's own answer, before the grants narrow it: the
// flowchart of spec 006, in its order. The administrator, the owner, the
// grantee within the ladder, the link that resolves, and then a deny.
func (p *OwnerPolicy) decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	kind := authorizer.Kind(req.Action)
	if kind == "" || kind != req.Resource.Kind {
		return authz.Decision{Reason: ReasonUnknownAction}, nil
	}
	frame := authz.Policy{Admins: p.Admins}
	switch d := frame.Decide(req, object(req.Action, req.Resource)); {
	case d.Allow:
		return d, nil
	case d.Reason == authz.ReasonProbe:
		// The probe is denied before anything else and by everyone, so no
		// grant and no link reaches past it.
		return d, nil
	}
	if allowed, err := p.granted(ctx, req); err != nil {
		return authz.Decision{}, err
	} else if allowed {
		return authz.Decision{Allow: true}, nil
	}
	if allowed, err := p.linked(ctx, req); err != nil {
		return authz.Decision{}, err
	} else if allowed {
		return authz.Decision{Allow: true}, nil
	}
	if req.Subject == "" {
		return authz.Decision{Reason: authz.ReasonAnonymous}, nil
	}
	return authz.Decision{Reason: ReasonNotGranted}, nil
}

// granted is the grant step: a live grant on a prefix of the resource's path
// for this subject, at or above the rung the action needs.
func (p *OwnerPolicy) granted(ctx context.Context, req authz.Request) (bool, error) {
	need, ok := Granted(req.Action)
	if !ok || p.Grants == nil || req.Subject == "" {
		return false, nil
	}
	owner, path := req.Resource.String("owner"), grantPath(req.Resource)
	if owner == "" || path == "" {
		return false, nil
	}
	held, err := p.Grants.Permission(ctx, owner, req.Subject, path)
	if err != nil {
		return false, fmt.Errorf("the grants of %s: %w", owner, err)
	}
	return rung(held) >= rung(need), nil
}

// linked is the link step: link.read on a link that still resolves. It is
// the one step an anonymous caller reaches, because the three public link
// routes of spec 013 carry no bearer and the grant the token resolved to is
// the whole of the authorization.
func (p *OwnerPolicy) linked(ctx context.Context, req authz.Request) (bool, error) {
	if req.Action != authorizer.ActionLinkRead || p.Links == nil {
		return false, nil
	}
	owner := req.Resource.String("owner")
	if req.Resource.ID == "" || owner == "" {
		return false, nil
	}
	live, err := p.Links.Live(ctx, req.Resource.ID, owner, req.Resource.String("path"))
	if err != nil {
		return false, fmt.Errorf("the link %s: %w", req.Resource.ID, err)
	}
	return live, nil
}

// grantPath is the path a grant is read against. Every resource but a
// workspace names one; a workspace names its slug, and its subtree is the
// workspaces plane under that slug (spec 001), which is the prefix a grant
// on it covers.
func grantPath(res authz.Resource) string {
	if res.Kind == authorizer.KindWorkspace {
		if slug := res.String("slug"); slug != "" {
			return "workspaces/" + strings.Trim(slug, "/")
		}
		return ""
	}
	return res.String("path")
}

// object is what arcad found when it looked the resource up, read off the
// resource it built. Every question about a space carries its owner, so an
// object with no owner is one no space claims, which only the administrative
// overview across spaces asks about.
//
// space.admin is the one action the owner rung does not reach. Administration
// is a capability and not ownership: a space's owner is not an administrator
// of its own space, and the routes under /v1/admin read across other people's
// spaces and restore across owners, which the owner's own routes of specs 005
// and 009 do without asking this action (spec 012). The frame admits an owner
// for every action it is handed an owned object for, so this action is handed
// none and the administrator list decides it alone. The probe is unaffected:
// the frame refuses the reserved id before it reads the object, so no listed
// subject reaches past it.
func object(action string, res authz.Resource) authz.Object {
	if action == authorizer.ActionSpaceAdmin {
		return authz.Object{}
	}
	owner := res.String("owner")
	return authz.Object{Exists: owner != "", Owner: owner}
}
