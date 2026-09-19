// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// The grant on the resource of spec 006: the rung the caller holds on a
// prefix of the resource's path rides on every question about a file and a
// workspace, so an operator's endpoint can tell a grantee from a stranger.
//
// The wire form is read off the stub's record of what it was sent, which is
// the same bytes an endpoint reads.

// lookup is the grants table as this package reads it, and a record of what
// it was asked, so a case states the question the seam put as well as the
// answer it took.
type lookup struct {
	held  auth.Permission
	err   error
	asked []string
}

func (l *lookup) Permission(_ context.Context, owner, subject, path string) (auth.Permission, error) {
	l.asked = append(l.asked, owner+" "+subject+" "+path)
	if l.err != nil {
		return auth.PermissionNone, l.err
	}
	return l.held, nil
}

// aWorkspace is the resource of a question about one workspace, whose grant
// is read against the subtree the slug owns.
func aWorkspace(id string) authz.Resource {
	return authorizer.Workspace{ID: id, Owner: alice, Slug: "build"}.Resource()
}

// aShare is the resource of a question no grant reaches.
func aShare(id string) authz.Resource {
	return authorizer.Share{
		ID: id, Owner: alice, Path: "files/reports", Grantee: carol, Permission: "read",
	}.Resource()
}

// sent answers the resource of the one question the stub was asked.
func sent(t *testing.T, s *stub.Server) authz.Resource {
	t.Helper()
	seen := s.Requests()
	if len(seen) != 1 {
		t.Fatalf("the endpoint was asked %d questions, want one", len(seen))
	}
	return seen[0].Resource
}

// TestTheQuestionCarriesTheCallersGrant: a grantee's question names the rung
// they hold, and a stranger's names none, so the two are different questions
// to an endpoint that reads Arca's resource and nothing else.
func TestTheQuestionCarriesTheCallersGrant(t *testing.T) {
	for _, c := range []struct {
		name     string
		held     auth.Permission
		resource authz.Resource
		subject  string
		want     string
		asked    string
	}{
		{"a grantee's file", auth.PermissionRead, aFile("01J8R4"), carol, "read",
			alice + " " + carol + " files/reports/q3.pdf"},
		{"a grantee's workspace", auth.PermissionWrite, aWorkspace("01J8R7"), carol, "write",
			alice + " " + carol + " workspaces/build"},
		{"the highest rung", auth.PermissionManage, aFile("01J8R4"), carol, "manage",
			alice + " " + carol + " files/reports/q3.pdf"},
		{"a stranger", auth.PermissionNone, aFile("01J8R4"), carol, "",
			alice + " " + carol + " files/reports/q3.pdf"},
		// A share, a link, an event and a space are powers over a space and
		// not over a subtree of it, so no grant reaches their actions and the
		// table is not read at all.
		{"a share", auth.PermissionManage, aShare("01J8R5"), carol, "", ""},
		// The three public link routes carry no caller, and a table read for
		// nobody would answer for nobody.
		{"an anonymous caller", auth.PermissionRead, aFile("01J8R4"), "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			table := &lookup{held: c.held}
			a := asking(t, s, nil).WithGrants(table)

			action := authorizer.ActionFileRead
			switch c.resource.Kind {
			case authorizer.KindWorkspace:
				action = authorizer.ActionWorkspaceRead
			case authorizer.KindShare:
				action = authorizer.ActionShareRead
			}
			if _, err := a.Decide(serving(c.subject), action, c.resource); err != nil {
				t.Fatalf("the question was refused: %v", err)
			}

			got := sent(t, s)
			if got.String(auth.GrantField) != c.want {
				t.Errorf("the question carries the grant %q, want %q", got.String(auth.GrantField), c.want)
			}
			if _, present := got.Fields[auth.GrantField]; present != (c.want != "") {
				t.Errorf("the grant field is present=%v on a question whose rung is %q", present, c.want)
			}
			if asked := strings.Join(table.asked, "; "); asked != c.asked {
				t.Errorf("the table was asked %q, want %q", asked, c.asked)
			}
		})
	}
}

// TestTheResourceTheHandlerBuiltIsNotChanged: the grant is added to a copy,
// so a handler that reuses one resource for two questions does not find the
// first question's answer on it.
func TestTheResourceTheHandlerBuiltIsNotChanged(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	a := asking(t, s, nil).WithGrants(&lookup{held: auth.PermissionRead})

	res := aFile("01J8R4")
	if _, err := a.Decide(serving(carol), authorizer.ActionFileRead, res); err != nil {
		t.Fatalf("the question was refused: %v", err)
	}
	if _, present := res.Fields[auth.GrantField]; present {
		t.Error("the resource the handler built came back with a grant on it")
	}
}

// TestNoGrantsTableIsNoGrant: an installation that reads no table asks the
// question it always asked, which is what a build without spec 008's table
// does.
func TestNoGrantsTableIsNoGrant(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	a := asking(t, s, nil)

	if _, err := a.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the question was refused: %v", err)
	}
	if _, present := sent(t, s).Fields[auth.GrantField]; present {
		t.Error("a node that reads no grants table sent a grant anyway")
	}
}

// TestAGrantsTableThatCannotAnswerStopsTheQuestion: a table that fails is no
// decision, so the request is authorizer_unavailable and the endpoint is not
// asked a question missing the grant the caller holds. A wrong answer is
// worse than none.
func TestAGrantsTableThatCannotAnswerStopsTheQuestion(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	a := asking(t, s, nil).WithGrants(&lookup{err: errors.New("the database is gone")})

	_, err := a.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4"))
	if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
		t.Errorf("a table that cannot answer gave %v, want %s", err, auth.CodeAuthorizerUnavailable)
	}
	if seen := s.Requests(); len(seen) != 0 {
		t.Errorf("the endpoint was asked %d questions with the grant unresolved", len(seen))
	}

	// A lookup at resolve time refuses the same way, so a reference another
	// request named is not admitted on a table that did not answer either.
	_, err = a.Lookup(serving(carol), authorizer.ActionFileRead, aFile("01J8R4"))
	if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
		t.Errorf("a lookup on a table that cannot answer gave %v, want %s", err, auth.CodeAuthorizerUnavailable)
	}
}

// TestBothModesResolveTheGrant: the field is resolved by the node and not by
// whichever authorizer it runs, so an installation on the owner policy reads
// its table for the field as one on an operator's endpoint does. Without
// that, a platform's cutover to an endpoint would silently stop seeing
// grantees.
func TestBothModesResolveTheGrant(t *testing.T) {
	iss := issuer(t)

	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	endpointTable := &lookup{held: auth.PermissionRead}
	asked, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: audience,
		AuthorizerURL: s.URL(), AuthorizerToken: s.Token(), Grants: endpointTable,
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if asked.Mode != auth.ModeAuthorizer {
		t.Fatalf("the mode is %q, want %q", asked.Mode, auth.ModeAuthorizer)
	}
	if _, err := asked.Authorizer.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the question was refused: %v", err)
	}
	if got := sent(t, s).String(auth.GrantField); got != string(auth.PermissionRead) {
		t.Errorf("the endpoint was asked with the grant %q, want read", got)
	}

	// The owner policy answers in process, so what proves the field was
	// resolved is the table being read for it: the policy's own grant step
	// is reached only once the frame has refused, and the owner's question
	// below is one the frame allows.
	policyTable := &lookup{held: auth.PermissionRead}
	own, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audience: audience, Grants: policyTable,
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if own.Mode != auth.ModeOwnerPolicy {
		t.Fatalf("the mode is %q, want %q", own.Mode, auth.ModeOwnerPolicy)
	}
	if _, err := own.Authorizer.Decide(serving(alice), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the owner's own question was refused: %v", err)
	}
	if len(policyTable.asked) != 1 {
		t.Errorf("the owner policy read the grants table %d times for the field, want once", len(policyTable.asked))
	}
}
