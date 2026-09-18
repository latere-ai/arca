// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// carol is the third subject of the policy table: neither the owner of the
// space nor an administrator of the installation.
const carol = "https://issuer.example|carol"

// grants is a grants table for a test: one permission, held by one subject,
// on one prefix of one space. The real one arrives with spec 008.
type grants struct {
	owner, subject, prefix string
	held                   auth.Permission
	err                    error
}

func (g grants) Permission(_ context.Context, owner, subject, path string) (auth.Permission, error) {
	switch {
	case g.err != nil:
		return auth.PermissionNone, g.err
	case owner != g.owner || subject != g.subject:
		return auth.PermissionNone, nil
	case path != g.prefix && !strings.HasPrefix(path, g.prefix+"/"):
		return auth.PermissionNone, nil
	}
	return g.held, nil
}

// links is a public link table for a test: one live link on one space.
type links struct {
	id, owner string
	err       error
}

func (l links) Live(_ context.Context, id, owner, _ string) (bool, error) {
	if l.err != nil {
		return false, l.err
	}
	return id == l.id && owner == l.owner, nil
}

// policy is the owner policy with alice as the space's owner, bob as an
// administrator, and carol holding whatever the case grants her.
func policy(held auth.Permission, prefix string) *auth.OwnerPolicy {
	return &auth.OwnerPolicy{
		Admins: []string{bob},
		Grants: grants{owner: alice, subject: carol, prefix: prefix, held: held},
		Links:  links{id: "01J8LINK", owner: alice},
	}
}

// ask puts one question to a policy and reports the verdict and the reason.
func ask(t *testing.T, p *auth.OwnerPolicy, subject, action string, res authz.Resource) (bool, string) {
	t.Helper()
	d, err := p.Authorize(t.Context(), authz.Request{
		Subject: subject, Action: action, Resource: res, Claims: map[string]any{},
	})
	if err != nil {
		t.Fatalf("the policy produced no decision for %s: %v", action, err)
	}
	return d.Allow, d.Reason
}

// TestTheOwnerPolicyTable is criterion 8 of spec 006: the flowchart, row by
// row. An administrator acts on every space, an owner on its own, a grantee
// within the ladder, a link that resolves reads what it names, and
// everything else is denied.
func TestTheOwnerPolicyTable(t *testing.T) {
	file := func(owner string) authz.Resource {
		return authorizer.File{ID: "01J8R4", Owner: owner, Path: "files/reports/q3.pdf", Plane: "files"}.Resource()
	}
	cases := []struct {
		name    string
		policy  *auth.OwnerPolicy
		subject string
		action  string
		res     authz.Resource
		allow   bool
		reason  string
	}{
		{
			name: "the owner of the space", policy: policy(auth.PermissionNone, ""),
			subject: alice, action: authorizer.ActionFileWrite, res: file(alice), allow: true,
		},
		{
			name: "an administrator on a space it does not own", policy: policy(auth.PermissionNone, ""),
			subject: bob, action: authorizer.ActionFileDelete, res: file(alice), allow: true,
		},
		{
			name: "an administrator on the overview across spaces", policy: policy(auth.PermissionNone, ""),
			subject: bob, action: authorizer.ActionSpaceAdmin, res: authorizer.Space{}.Resource(), allow: true,
		},
		{
			name: "a stranger", policy: policy(auth.PermissionNone, ""),
			subject: carol, action: authorizer.ActionFileRead, res: file(alice),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "an owner who is not an administrator, on the overview", policy: policy(auth.PermissionNone, ""),
			subject: alice, action: authorizer.ActionSpaceAdmin, res: authorizer.Space{}.Resource(),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "a grantee reading within a read grant", policy: policy(auth.PermissionRead, "files/reports"),
			subject: carol, action: authorizer.ActionFileRead, res: file(alice), allow: true,
		},
		{
			name: "a grantee writing under a read grant", policy: policy(auth.PermissionRead, "files/reports"),
			subject: carol, action: authorizer.ActionFileWrite, res: file(alice),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "a grantee reading outside the granted subtree", policy: policy(auth.PermissionRead, "files/invoices"),
			subject: carol, action: authorizer.ActionFileRead, res: file(alice),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "a grantee writing under a write grant", policy: policy(auth.PermissionWrite, "files/reports"),
			subject: carol, action: authorizer.ActionFileWrite, res: file(alice), allow: true,
		},
		{
			name: "a grantee sharing under a write grant", policy: policy(auth.PermissionWrite, "files/reports"),
			subject: carol, action: authorizer.ActionShareCreate,
			res:    authorizer.Share{Owner: alice, Path: "files/reports", Grantee: bob, Permission: "read"}.Resource(),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "a grantee sharing under a manage grant", policy: policy(auth.PermissionManage, "files/reports"),
			subject: carol, action: authorizer.ActionShareCreate,
			res:   authorizer.Share{Owner: alice, Path: "files/reports", Grantee: bob, Permission: "read"}.Resource(),
			allow: true,
		},
		{
			name: "a grantee attaching to a workspace under a write grant", policy: policy(auth.PermissionWrite, "workspaces/build"),
			subject: carol, action: authorizer.ActionWorkspaceAttach,
			res:   authorizer.Workspace{ID: "01J8W1", Owner: alice, Slug: "build"}.Resource(),
			allow: true,
		},
		{
			name: "a grantee attaching to another workspace", policy: policy(auth.PermissionWrite, "workspaces/build"),
			subject: carol, action: authorizer.ActionWorkspaceAttach,
			res:    authorizer.Workspace{ID: "01J8W2", Owner: alice, Slug: "release"}.Resource(),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "a grantee deleting a workspace under a manage grant", policy: policy(auth.PermissionManage, "workspaces/build"),
			subject: carol, action: authorizer.ActionWorkspaceDelete,
			res:    authorizer.Workspace{ID: "01J8W1", Owner: alice, Slug: "build"}.Resource(),
			reason: auth.ReasonNotGranted,
		},
		{
			name: "an anonymous caller reading a link that resolves", policy: policy(auth.PermissionNone, ""),
			action: authorizer.ActionLinkRead,
			res:    authorizer.Link{ID: "01J8LINK", Owner: alice, Path: "files/reports"}.Resource(),
			allow:  true,
		},
		{
			name: "an anonymous caller reading a link that does not resolve", policy: policy(auth.PermissionNone, ""),
			action: authorizer.ActionLinkRead,
			res:    authorizer.Link{ID: "01J8GONE", Owner: alice, Path: "files/reports"}.Resource(),
			reason: authz.ReasonAnonymous,
		},
		{
			name: "an anonymous caller reading an object directly", policy: policy(auth.PermissionNone, ""),
			action: authorizer.ActionFileRead, res: file(alice),
			reason: authz.ReasonAnonymous,
		},
		{
			name: "an anonymous caller minting a link", policy: policy(auth.PermissionNone, ""),
			action: authorizer.ActionLinkCreate,
			res:    authorizer.Link{Owner: alice, Path: "files/reports"}.Resource(),
			reason: authz.ReasonAnonymous,
		},
		{
			name: "the probe, asked by an administrator", policy: policy(auth.PermissionNone, ""),
			subject: bob, action: authorizer.ActionSpaceAdmin, res: authorizer.Probe(),
			reason: authz.ReasonProbe,
		},
		{
			name: "an action outside the vocabulary", policy: policy(auth.PermissionNone, ""),
			subject: alice, action: "quota.write", res: file(alice),
			reason: auth.ReasonUnknownAction,
		},
		{
			name: "an action asked about the wrong kind", policy: policy(auth.PermissionNone, ""),
			subject: alice, action: authorizer.ActionFileRead,
			res:    authorizer.Workspace{ID: "01J8W1", Owner: alice, Slug: "build"}.Resource(),
			reason: auth.ReasonUnknownAction,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allow, reason := ask(t, c.policy, c.subject, c.action, c.res)
			if allow != c.allow {
				t.Fatalf("the policy answered allow=%v, want %v (reason %q)", allow, c.allow, reason)
			}
			if !allow && reason != c.reason {
				t.Errorf("the deny names the reason %q, want %q", reason, c.reason)
			}
			if allow && reason != "" {
				t.Errorf("an allow carries the reason %q", reason)
			}
		})
	}
}

// TestTheLadderMatchesSpec006 reads the owner policy's ladder out of
// specs/006-identity.md and holds the package equal to it: an action a rung
// admits in the spec and not here is a grantee refused what they were
// granted, and one here and not there is a grantee given what nobody wrote
// down.
func TestTheLadderMatchesSpec006(t *testing.T) {
	rungs, ownerActions := ladderOfSpec006(t)
	for _, rung := range []struct {
		name auth.Permission
		up   []auth.Permission
	}{
		{auth.PermissionRead, []auth.Permission{auth.PermissionRead}},
		{auth.PermissionWrite, []auth.Permission{auth.PermissionRead, auth.PermissionWrite}},
		{auth.PermissionManage, []auth.Permission{auth.PermissionRead, auth.PermissionWrite, auth.PermissionManage}},
	} {
		var want []string
		for _, r := range rung.up {
			want = append(want, rungs[r]...)
		}
		slices.Sort(want)
		var got []string
		for _, action := range authorizer.Actions() {
			if need, ok := auth.Granted(action); ok && admits(rung.name, need) {
				got = append(got, action)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("a %s grant admits %v; spec 006's ladder admits %v", rung.name, got, want)
		}
	}
	var reached []string
	for _, action := range authorizer.Actions() {
		if _, ok := auth.Granted(action); !ok {
			reached = append(reached, action)
		}
	}
	slices.Sort(reached)
	slices.Sort(ownerActions)
	if !slices.Equal(reached, ownerActions) {
		t.Errorf("no grant reaches %v; spec 006 names %v as the owner's or an administrator's", reached, ownerActions)
	}
}

// admits reports whether a grant at held reaches an action needing need.
func admits(held, need auth.Permission) bool {
	order := map[auth.Permission]int{
		auth.PermissionRead: 1, auth.PermissionWrite: 2, auth.PermissionManage: 3,
	}
	return order[held] >= order[need]
}

// TestTheLadderCoversTheWholeVocabulary: every one of the twenty-three is
// either reachable by a grant at a named rung or the owner's and an
// administrator's, with nothing left undecided.
func TestTheLadderCoversTheWholeVocabulary(t *testing.T) {
	for _, action := range authorizer.Actions() {
		p, ok := auth.Granted(action)
		if ok && p != auth.PermissionRead && p != auth.PermissionWrite && p != auth.PermissionManage {
			t.Errorf("%s needs the permission %q, which is not a rung of the ladder", action, p)
		}
	}
}

// TestAGrantsTableThatCannotAnswerIsNoDecision: a store that fails is an
// outage, not a deny. The seam turns it into authorizer_unavailable, and a
// caller is never told no because a query failed.
func TestAGrantsTableThatCannotAnswerIsNoDecision(t *testing.T) {
	boom := errors.New("the connection is gone")
	p := &auth.OwnerPolicy{Admins: []string{bob}, Grants: grants{err: boom}}
	_, err := p.Authorize(t.Context(), authz.Request{
		Subject: carol, Action: authorizer.ActionFileRead, Claims: map[string]any{},
		Resource: authorizer.File{ID: "01J8R4", Owner: alice, Path: "files/x", Plane: "files"}.Resource(),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("a failing grants table answered %v", err)
	}

	p = &auth.OwnerPolicy{Links: links{err: boom}}
	_, err = p.Authorize(t.Context(), authz.Request{
		Action: authorizer.ActionLinkRead, Claims: map[string]any{},
		Resource: authorizer.Link{ID: "01J8LINK", Owner: alice, Path: "files/x"}.Resource(),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("a failing link table answered %v", err)
	}
}

// TestNoTableIsNoGrantAndNoLink: an installation that has issued neither is
// the owner policy with both nil, which decides by owner and administrator
// alone and never dereferences what it does not have.
func TestNoTableIsNoGrantAndNoLink(t *testing.T) {
	p := &auth.OwnerPolicy{Admins: []string{bob}}
	if allow, _ := ask(t, p, alice, authorizer.ActionFileRead,
		authorizer.File{ID: "01J8R4", Owner: alice, Path: "files/x", Plane: "files"}.Resource()); !allow {
		t.Error("the owner was refused its own space with no tables wired")
	}
	if allow, reason := ask(t, p, "", authorizer.ActionLinkRead,
		authorizer.Link{ID: "01J8LINK", Owner: alice, Path: "files/x"}.Resource()); allow {
		t.Errorf("a link resolved with no link table, reason %q", reason)
	}
}

// TestDecideIsAuthorize: the policy arcad runs in process is the endpoint an
// operator serves from it, with no second implementation between the two.
func TestDecideIsAuthorize(t *testing.T) {
	p := policy(auth.PermissionNone, "")
	req := authz.Request{
		Subject: alice, Action: authorizer.ActionFileRead, Claims: map[string]any{},
		Resource: authorizer.File{ID: "01J8R4", Owner: alice, Path: "files/x", Plane: "files"}.Resource(),
	}
	a, err1 := p.Authorize(t.Context(), req)
	b, err2 := p.Decide(t.Context(), req)
	if err1 != nil || err2 != nil {
		t.Fatalf("the policy produced no decision: %v, %v", err1, err2)
	}
	if a.Allow != b.Allow || a.Reason != b.Reason {
		t.Errorf("Authorize answered %+v and Decide answered %+v", a, b)
	}
}

// tickedAction matches one backticked action name. An action name carries a
// full stop of its own, so the clauses below are cut at their markers rather
// than at punctuation.
var tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")

// The markers the ladder sentence of spec 006 is read between, in the order
// the spec writes them.
const (
	readRung   = "`read` admits"
	writeRung  = "`write` adds"
	manageRung = "`manage` adds"
	ownerOnly  = "No grant reaches the remaining"
	afterOwner = "Making a workspace"
)

// ladderOfSpec006 reads the owner policy's ladder out of the spec: the
// actions each rung adds, and the actions the spec says no grant reaches.
func ladderOfSpec006(t *testing.T) (map[auth.Permission][]string, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "006-identity.md"))
	if err != nil {
		t.Fatalf("spec 006: %v", err)
	}
	// The sentences wrap across lines, so the spec is read as one run of
	// text and cut at the words that open and close each clause.
	text := strings.Join(strings.Fields(string(raw)), " ")
	rungs := map[auth.Permission][]string{
		auth.PermissionRead:   actionsIn(t, between(t, text, readRung, writeRung)),
		auth.PermissionWrite:  actionsIn(t, between(t, text, writeRung, manageRung)),
		auth.PermissionManage: actionsIn(t, between(t, text, manageRung, ownerOnly)),
	}
	for name, actions := range rungs {
		if len(actions) == 0 {
			t.Fatalf("spec 006's %s rung names no action", name)
		}
	}
	return rungs, actionsIn(t, between(t, text, ownerOnly, afterOwner))
}

// between is the run of text one clause occupies: everything after the
// marker that opens it and before the one that closes it.
func between(t *testing.T, text, from, to string) string {
	t.Helper()
	i := strings.Index(text, from)
	if i < 0 {
		t.Fatalf("spec 006 no longer says %q, so the ladder cannot be read from it", from)
	}
	rest := text[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("spec 006 no longer says %q after %q", to, from)
	}
	return rest[:j]
}

// actionsIn is the backticked action names of one clause, in order, each
// held to the vocabulary so a typo in the spec is a failure here rather than
// a rung that silently admits nothing.
func actionsIn(t *testing.T, clause string) []string {
	t.Helper()
	var out []string
	for _, m := range tickedAction.FindAllStringSubmatch(clause, -1) {
		if !authorizer.Known(m[1]) {
			t.Errorf("spec 006 names %s, which is not one of the vocabulary", m[1])
			continue
		}
		out = append(out, m[1])
	}
	return out
}
