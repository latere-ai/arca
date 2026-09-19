// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// Criterion 9 of spec 012: an allow on a space the caller neither owns nor
// holds a covering grant on marks the request administrative, and the mark
// is written where the answer is read rather than by a handler. What reads
// it is the event log, so a route added later is recorded without being told
// to be.
//
// Arca cannot see why its authorizer said yes. What it records is that
// neither of the two explanations it can check is the reason.

// onDuty is the context a handler decides on, with the record the API's
// first middleware installs on every route under /v1.
func onDuty(subject string) context.Context {
	return auth.WithMarks(serving(subject))
}

func TestAnAllowNeitherOwnershipNorAGrantExplainsIsAdministrative(t *testing.T) {
	for _, c := range []struct {
		name   string
		caller string
		held   auth.Permission
		action string
		res    authz.Resource
		want   bool
	}{
		{
			name:   "a stranger the endpoint admits on another space",
			caller: bob, held: auth.PermissionNone,
			action: authorizer.ActionFileDelete, res: aFile("01J8R4"), want: true,
		},
		{
			name:   "the space's own owner",
			caller: alice, held: auth.PermissionNone,
			action: authorizer.ActionFileDelete, res: aFile("01J8R4"), want: false,
		},
		{
			name:   "a grantee holding a rung on the path",
			caller: bob, held: auth.PermissionWrite,
			action: authorizer.ActionFileDelete, res: aFile("01J8R4"), want: false,
		},
		{
			name:   "an administrator on another space, where no grant reaches the kind",
			caller: bob, held: auth.PermissionNone,
			action: authorizer.ActionSpaceAdmin, res: authorizer.Space{Owner: alice}.Resource(), want: true,
		},
		{
			name:   "the overview, which names no space at all",
			caller: bob, held: auth.PermissionNone,
			action: authorizer.ActionSpaceAdmin, res: authorizer.Space{}.Resource(), want: false,
		},
		{
			name:   "an anonymous caller on a public link",
			caller: "", held: auth.PermissionNone,
			action: authorizer.ActionLinkRead, res: aFile("01J8R4"), want: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			a := asking(t, s, nil).WithGrants(&lookup{held: c.held})

			ctx := onDuty(c.caller)
			if _, err := a.Decide(ctx, c.action, c.res); err != nil {
				t.Fatalf("the allow was refused: %v", err)
			}
			if got := auth.Administrative(ctx); got != c.want {
				t.Errorf("the request reads administrative=%v and the case holds %v", got, c.want)
			}
		})
	}
}

// TestADenyMarksNothing: the mark is a property of an allow. A refusal says
// nothing happened in the space, and an event nothing wrote cannot carry a
// detail either way; recording a deny would make a caller that probes a
// space it may not touch look like one that touched it.
func TestADenyMarksNothing(t *testing.T) {
	s := endpoint(t)
	s.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")
	a := asking(t, s, nil)

	ctx := onDuty(bob)
	if _, err := a.Decide(ctx, authorizer.ActionFileDelete, aFile("01J8R4")); err == nil {
		t.Fatal("the deny was read as an allow")
	}
	if auth.Administrative(ctx) {
		t.Error("a refused request reads administrative")
	}
}

// TestOneAdministrativeAllowStandsForTheRequest: a request asks more than
// one question, a resolve and then the action, and the mark is the request's
// and not the last question's. A later question about the caller's own space
// does not undo what it already did in somebody else's.
func TestOneAdministrativeAllowStandsForTheRequest(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	a := asking(t, s, nil)

	ctx := onDuty(bob)
	if _, err := a.Lookup(ctx, authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the lookup was refused: %v", err)
	}
	own := authorizer.File{ID: "01J8R5", Owner: bob, Path: "files/notes.md", Plane: "files"}.Resource()
	if _, err := a.Decide(ctx, authorizer.ActionFileRead, own); err != nil {
		t.Fatalf("the second question was refused: %v", err)
	}
	if !auth.Administrative(ctx) {
		t.Error("a request that read another space reads as one that did not")
	}
}

// TestACallOutsideARequestMarksNothing: the reaper decides nothing and
// appends events of its own, on a context no middleware touched. Reading or
// writing the mark there must be a no-op rather than a panic.
func TestACallOutsideARequestMarksNothing(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	a := asking(t, s, nil)

	ctx := serving(bob)
	if _, err := a.Decide(ctx, authorizer.ActionFileDelete, aFile("01J8R4")); err != nil {
		t.Fatalf("the allow was refused: %v", err)
	}
	if auth.Administrative(ctx) {
		t.Error("a context carrying no record answered a mark")
	}
}
