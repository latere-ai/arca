// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"errors"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
)

// The three subjects of the policy cases: the space's owner, the grantee,
// and a caller who holds nothing.
const (
	owner     = "https://issuer.example|alice"
	grantee   = "https://issuer.example|carol"
	stranger  = "https://issuer.example|mallory"
	linkToken = "t0ken"
)

// policy builds the owner policy of spec 006 over a real grants table: the
// seams that spec left for spec 008, bound to the queries of this one.
func policy(t *table) *auth.OwnerPolicy {
	db := &database{table: t}
	return &auth.OwnerPolicy{Grants: shares.Grants(db, t), Links: shares.Links(db, t)}
}

// ask puts one question to a policy.
func ask(t *testing.T, p *auth.OwnerPolicy, subject, action string, res authz.Resource) bool {
	t.Helper()
	d, err := p.Authorize(t.Context(), authz.Request{
		Subject: subject, Action: action, Resource: res, Claims: map[string]any{},
	})
	if err != nil {
		t.Fatalf("the policy produced no decision for %s: %v", action, err)
	}
	return d.Allow
}

// file is the resource of a file question under the granted subtree.
func file(path string) authz.Resource {
	return authorizer.File{ID: "01J8FILE", Owner: owner, Path: path, Plane: "files"}.Resource()
}

// TestAGranteeActsWithinTheLadder is criterion 8 of spec 006 driven against
// the real lookup: the rung a grant carries is the whole of what its holder
// may do on the subtree, and no grant reaches the eight actions that are
// powers over the space.
func TestAGranteeActsWithinTheLadder(t *testing.T) {
	share := authorizer.Share{
		ID: "01J8GRANT", Owner: owner, Path: "files/reports",
		Grantee: stranger, Permission: "read",
	}.Resource()
	workspace := authorizer.Workspace{ID: "01J8WS", Owner: owner, Slug: "build"}.Resource()

	for _, c := range []struct {
		held   string
		action string
		res    authz.Resource
		allow  bool
	}{
		{"read", authorizer.ActionFileRead, file("files/reports/q3.pdf"), true},
		{"read", authorizer.ActionFileList, file("files/reports"), true},
		{"read", authorizer.ActionFileWrite, file("files/reports/q3.pdf"), false},
		{"read", authorizer.ActionFileDelete, file("files/reports/q3.pdf"), false},
		{"write", authorizer.ActionFileRead, file("files/reports/q3.pdf"), true},
		{"write", authorizer.ActionFileWrite, file("files/reports/q3.pdf"), true},
		{"write", authorizer.ActionFileDelete, file("files/reports/q3.pdf"), true},
		{"write", authorizer.ActionUploadWrite,
			authorizer.Upload{Owner: owner, Path: "files/reports/q3.pdf"}.Resource(), true},
		{"write", authorizer.ActionShareCreate, share, false},
		{"manage", authorizer.ActionFileWrite, file("files/reports/q3.pdf"), true},
		{"manage", authorizer.ActionShareCreate, share, true},
		{"manage", authorizer.ActionShareRead, share, true},
		{"manage", authorizer.ActionShareList, share, true},
		{"manage", authorizer.ActionShareRevoke, share, true},

		// The eight a grant does not reach. Minting a token anyone may read
		// with, making or removing a workspace, undoing a delete and reading
		// a space's log are powers over the space and not over a subtree of
		// it, so the highest rung does not confer them.
		{"manage", authorizer.ActionLinkCreate, authorizer.Link{Owner: owner, Path: "files/reports"}.Resource(), false},
		{"manage", authorizer.ActionLinkRevoke, authorizer.Link{ID: "01J8LINK", Owner: owner, Path: "files/reports"}.Resource(), false},
		{"manage", authorizer.ActionWorkspaceCreate, workspace, false},
		{"manage", authorizer.ActionWorkspaceDelete, workspace, false},
		{"manage", authorizer.ActionWorkspaceRestore, workspace, false},
		{"manage", authorizer.ActionEventRead, authorizer.Event{Owner: owner}.Resource(), false},
		{"manage", authorizer.ActionSpaceAdmin, authorizer.Space{Owner: owner}.Resource(), false},
	} {
		t.Run(c.held+" "+c.action, func(t *testing.T) {
			tbl := newTable()
			tbl.put(store.Grant{
				Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
				Grantee: grantee, Permission: c.held, CreatedBy: owner,
			})
			if got := ask(t, policy(tbl), grantee, c.action, c.res); got != c.allow {
				t.Errorf("a %s grantee asking %s = %t, want %t", c.held, c.action, got, c.allow)
			}
		})
	}
}

// TestAGrantOnAWorkspaceIsReadAgainstItsSubtree: a workspace names a slug,
// and the prefix a grant on it covers is the subtree of the workspaces plane
// that workspace owns (spec 006).
func TestAGrantOnAWorkspaceIsReadAgainstItsSubtree(t *testing.T) {
	tbl := newTable()
	tbl.put(store.Grant{
		Owner: owner, PathPrefix: "workspaces/build", GranteeKind: store.GranteeSubject,
		Grantee: grantee, Permission: "write", CreatedBy: owner,
	})
	p := policy(tbl)
	build := authorizer.Workspace{ID: "01J8WS", Owner: owner, Slug: "build"}.Resource()
	if !ask(t, p, grantee, authorizer.ActionWorkspaceAttach, build) {
		t.Error("a write grant on a workspace does not admit an attach")
	}
	other := authorizer.Workspace{ID: "01J8WS2", Owner: owner, Slug: "build-archive"}.Resource()
	if ask(t, p, grantee, authorizer.ActionWorkspaceRead, other) {
		t.Error("a grant on workspaces/build admits workspaces/build-archive")
	}
}

// TestWhatTheGrantStepDoesNotAdmit: the segment rule, the expiry, the
// revoke, another space's grant, another subject's grant, and a token grant,
// which is the link step's and not this one's.
func TestWhatTheGrantStepDoesNotAdmit(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	for _, c := range []struct {
		name    string
		grant   store.Grant
		subject string
		path    string
	}{
		{"a sibling whose name begins with the prefix", store.Grant{
			Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
			Grantee: grantee, Permission: "manage", CreatedBy: owner,
		}, grantee, "files/reports-archive/q3.pdf"},
		{"a path above the prefix", store.Grant{
			Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
			Grantee: grantee, Permission: "manage", CreatedBy: owner,
		}, grantee, "files/q3.pdf"},
		{"another subject's grant", store.Grant{
			Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
			Grantee: grantee, Permission: "read", CreatedBy: owner,
		}, stranger, "files/reports/q3.pdf"},
		{"a grant that expired", store.Grant{
			Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
			Grantee: grantee, Permission: "read", CreatedBy: owner, ExpiresAt: &past,
		}, grantee, "files/reports/q3.pdf"},
		{"a token grant", store.Grant{
			Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteePublic,
			Permission: "read", Token: linkToken, CreatedBy: owner,
		}, grantee, "files/reports/q3.pdf"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tbl := newTable()
			tbl.put(c.grant)
			if ask(t, policy(tbl), c.subject, authorizer.ActionFileRead, file(c.path)) {
				t.Error("the grant step admitted it")
			}
		})
	}
}

// TestARevokedGrantStopsGrantingOnTheNextQuestion: the effect is immediate
// and there is no grace window.
func TestARevokedGrantStopsGrantingOnTheNextQuestion(t *testing.T) {
	tbl := newTable()
	held := tbl.put(store.Grant{
		Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeSubject,
		Grantee: grantee, Permission: "read", CreatedBy: owner,
	})
	p := policy(tbl)
	if !ask(t, p, grantee, authorizer.ActionFileRead, file("files/reports/q3.pdf")) {
		t.Fatal("the grantee could not read what it holds")
	}
	if _, err := tbl.Revoke(t.Context(), nil, held.ID); err != nil {
		t.Fatal(err)
	}
	if ask(t, p, grantee, authorizer.ActionFileRead, file("files/reports/q3.pdf")) {
		t.Error("a revoked grant still grants")
	}
}

// TestTheLinkStepAnswersLinkReadAndNothingElse: the one step an anonymous
// caller reaches, and only for the link that resolved.
func TestTheLinkStepAnswersLinkReadAndNothingElse(t *testing.T) {
	tbl := newTable()
	link := tbl.put(store.Grant{
		Owner: owner, PathPrefix: "files/reports", GranteeKind: store.GranteeLink,
		Permission: "read", Token: linkToken, CreatedBy: owner,
	})
	p := policy(tbl)
	resource := func(id, path string) authz.Resource {
		return authorizer.Link{ID: id, Owner: owner, Path: path}.Resource()
	}
	if !ask(t, p, "", authorizer.ActionLinkRead, resource(link.ID, "files/reports")) {
		t.Error("a link that resolves does not read what it names")
	}
	if !ask(t, p, "", authorizer.ActionLinkRead, resource(link.ID, "files/reports/q3.pdf")) {
		t.Error("a link does not read a path inside its subtree")
	}
	if ask(t, p, "", authorizer.ActionLinkRead, resource(link.ID, "files/elsewhere")) {
		t.Error("a link reads outside its subtree")
	}
	if ask(t, p, "", authorizer.ActionLinkRead, resource("01J8NOBODY", "files/reports")) {
		t.Error("a link that resolves to nothing reads")
	}
	if ask(t, p, "", authorizer.ActionLinkRevoke, resource(link.ID, "files/reports")) {
		t.Error("an anonymous caller revokes a link")
	}
	if ask(t, p, "", authorizer.ActionFileRead, file("files/reports/q3.pdf")) {
		t.Error("an anonymous caller reads a file through the link step")
	}

	if _, err := tbl.Revoke(t.Context(), nil, link.ID); err != nil {
		t.Fatal(err)
	}
	if ask(t, p, "", authorizer.ActionLinkRead, resource(link.ID, "files/reports")) {
		t.Error("a revoked link still reads")
	}
}

// TestATableThatCannotAnswerIsNoDecision: a store failure in either step
// reaches the seam as an error, which spec 006 turns into
// authorizer_unavailable and never into a deny.
func TestATableThatCannotAnswerIsNoDecision(t *testing.T) {
	boom := errors.New("the connection went away")
	tbl := newTable()
	tbl.failCovering = boom
	p := policy(tbl)
	if _, err := p.Authorize(t.Context(), authz.Request{
		Subject: grantee, Action: authorizer.ActionFileRead,
		Resource: file("files/reports/q3.pdf"), Claims: map[string]any{},
	}); !errors.Is(err, boom) {
		t.Errorf("a grants table that cannot answer = %v", err)
	}
}

// TestTheStepsAnswerNothingForAQuestionThatNamesNothing: a question with no
// space, no subject, or no path reaches neither table.
func TestTheStepsAnswerNothingForAQuestionThatNamesNothing(t *testing.T) {
	tbl := newTable()
	grants := shares.Grants(&database{table: tbl}, tbl)
	for _, c := range []struct{ owner, subject, path string }{
		{"", grantee, "files/reports"},
		{owner, "", "files/reports"},
		{owner, grantee, ""},
	} {
		got, err := grants.Permission(t.Context(), c.owner, c.subject, c.path)
		if err != nil || got != auth.PermissionNone {
			t.Errorf("Permission(%q, %q, %q) = %q, %v", c.owner, c.subject, c.path, got, err)
		}
	}
	links := shares.Links(&database{table: tbl}, tbl)
	for _, c := range []struct{ id, owner string }{{"", owner}, {"01J8LINK", ""}} {
		live, err := links.Live(t.Context(), c.id, c.owner, "files/reports")
		if err != nil || live {
			t.Errorf("Live(%q, %q) = %t, %v", c.id, c.owner, live, err)
		}
	}
}
