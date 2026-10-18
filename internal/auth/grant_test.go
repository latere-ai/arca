// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/auth"
)

// The grant on the resource of spec 006: the rung the caller holds on a
// prefix of the resource's path rides on every question about a file and an
// upload, so an operator's endpoint can tell a grantee from a stranger.
//
// The wire form is read off the stub's record of what it was sent, which is
// the same bytes an endpoint reads.

// lookup is the grants table as this package reads it, and a record of what
// it was asked, so a case states the question the seam put as well as the
// answer it took.
type lookup struct {
	held auth.Permission
	// by answers one rung per path, for a case whose question names two
	// paths. A path it does not hold answers no rung. Nil answers held for
	// every path.
	by    map[string]auth.Permission
	err   error
	asked []string
	// holders is what the read of every holder answers, and holdersErr its
	// failure. They are recorded apart from asked, so a case about the
	// caller's own rung states the reads that rung took and no others.
	holders    []authorizer.Holder
	holdersErr error
	listed     []string
}

func (l *lookup) Holders(_ context.Context, owner, path, from string, limit int) ([]authorizer.Holder, error) {
	l.listed = append(l.listed, fmt.Sprintf("%s %s %s %d", owner, path, from, limit))
	if l.holdersErr != nil {
		return nil, l.holdersErr
	}
	return slices.Clone(l.holders), nil
}

func (l *lookup) Permission(_ context.Context, owner, subject, path string) (auth.Permission, error) {
	l.asked = append(l.asked, owner+" "+subject+" "+path)
	if l.err != nil {
		return auth.PermissionNone, l.err
	}
	if l.by != nil {
		return l.by[path], nil
	}
	return l.held, nil
}

// anUpload is the resource of a multipart write, which a grantee holding
// write reaches through upload.write when the object is too large to inline.
func anUpload() authz.Resource {
	return authorizer.Upload{Owner: alice, Path: "files/reports/q3.pdf"}.Resource()
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
		{"the highest rung", auth.PermissionManage, aFile("01J8R4"), carol, "manage",
			alice + " " + carol + " files/reports/q3.pdf"},
		{"a stranger", auth.PermissionNone, aFile("01J8R4"), carol, "",
			alice + " " + carol + " files/reports/q3.pdf"},
		// A share, a link, an event and a space are powers over a space and
		// not over a subtree of it, so no grant reaches their actions and the
		// table is not read at all.
		// upload.write is on the write rung's ladder, so a grantee's
		// multipart write carries the rung the same way a small write does.
		// Without it a grantee's writes would be admitted below the inline
		// bound and refused above it.
		{"a grantee's multipart write", auth.PermissionWrite, anUpload(), carol, "write",
			alice + " " + carol + " files/reports/q3.pdf"},
		{"a stranger's multipart write", auth.PermissionNone, anUpload(), carol, "",
			alice + " " + carol + " files/reports/q3.pdf"},
		{"a share", auth.PermissionManage, aShare("01J8R5"), carol, "", ""},
		// The three public link routes carry no caller, and a table read for
		// nobody would answer for nobody.
		{"an anonymous caller", auth.PermissionRead, aFile("01J8R4"), "", "", ""},
		// Ownership is not a grant, and it is strictly more than any grant on
		// a subtree of the space could confer, so the space's own owner is
		// settled without reading the table at all. That is also the question
		// this core is asked most, and the read it saves is on the hot path.
		{"the space's own owner", auth.PermissionManage, aFile("01J8R4"), alice, "", ""},
		{"the owner's own multipart write", auth.PermissionManage, anUpload(), alice, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			table := &lookup{held: c.held}
			a := asking(t, s, nil).WithGrants(table)

			action := authorizer.ActionFileRead
			switch c.resource.Kind {
			case authorizer.KindShare:
				action = authorizer.ActionShareRead
			case authorizer.KindUpload:
				action = authorizer.ActionUploadWrite
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
	got := sent(t, s)
	if _, present := got.Fields[auth.GrantField]; present {
		t.Error("a node that reads no grants table sent a grant anyway")
	}
	if _, present := got.Fields[auth.GrantsField]; present {
		t.Error("a node that reads no grants table sent the holders anyway")
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
		Issuers: []string{iss.URL()}, Audiences: []string{audience},
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
	// resolved is the table being read twice: once here for the field an
	// endpoint would read, and once by the policy's own grant step, which is
	// the step the policy decides on. The two are deliberately not folded
	// together: a policy deciding on a field it filled in itself would be
	// deciding on its own answer.
	policyTable := &lookup{held: auth.PermissionRead}
	own, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, Grants: policyTable,
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if own.Mode != auth.ModeOwnerPolicy {
		t.Fatalf("the mode is %q, want %q", own.Mode, auth.ModeOwnerPolicy)
	}
	if _, err := own.Authorizer.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the grantee's question was refused: %v", err)
	}
	if len(policyTable.asked) != 2 {
		t.Errorf("the owner policy read the grants table %d times, want twice: the field and its own grant step", len(policyTable.asked))
	}

	// And the owner's own question reads it for neither, because the frame
	// allows an owner before the grant step and ownership is not a grant.
	ownerTable := &lookup{held: auth.PermissionManage}
	plain, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, Grants: ownerTable,
	})
	if err != nil {
		t.Fatalf("the node would not start: %v", err)
	}
	if _, err := plain.Authorizer.Decide(serving(alice), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
		t.Fatalf("the owner's own question was refused: %v", err)
	}
	if len(ownerTable.asked) != 0 {
		t.Errorf("an owner's own question read the grants table %d times, want none", len(ownerTable.asked))
	}
}

// TestAGrantMustCoverBothEnds: a question that names a source beside its
// path carries the lower of the two rungs the caller holds, and the owner
// policy decides on the same rung, so both modes read one answer.
func TestAGrantMustCoverBothEnds(t *testing.T) {
	const source, destination = "files/reports/q3.pdf", "files/inbox/q3.pdf"
	moved := authorizer.File{
		ID: "01J8R4", Owner: alice, Path: destination, Plane: "files", From: source,
	}.Resource()
	both := alice + " " + carol + " " + destination + "; " + alice + " " + carol + " " + source

	for _, c := range []struct {
		name          string
		onSource      auth.Permission
		onDestination auth.Permission
		want          auth.Permission
		asked         string
	}{
		{"the same rung on both", auth.PermissionWrite, auth.PermissionWrite, auth.PermissionWrite, both},
		{"a higher rung on the source", auth.PermissionManage, auth.PermissionRead, auth.PermissionRead, both},
		{"a higher rung on the destination", auth.PermissionRead, auth.PermissionManage, auth.PermissionRead, both},
		{"no rung on the source", auth.PermissionNone, auth.PermissionWrite, auth.PermissionNone, both},
		// No rung on the destination settles the answer, so the source is
		// not read.
		{"no rung on the destination", auth.PermissionWrite, auth.PermissionNone, auth.PermissionNone,
			alice + " " + carol + " " + destination},
	} {
		t.Run(c.name, func(t *testing.T) {
			held := map[string]auth.Permission{source: c.onSource, destination: c.onDestination}

			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			table := &lookup{by: held}
			a := asking(t, s, nil).WithGrants(table)
			if _, err := a.Decide(serving(carol), authorizer.ActionFileWrite, moved); err != nil {
				t.Fatalf("the question was refused: %v", err)
			}
			got := sent(t, s)
			if got.String(auth.GrantField) != string(c.want) {
				t.Errorf("the question carries the grant %q, want %q", got.String(auth.GrantField), c.want)
			}
			if _, present := got.Fields[auth.GrantField]; present != (c.want != auth.PermissionNone) {
				t.Errorf("the grant field is present=%v on a question whose rung is %q", present, c.want)
			}
			if asked := strings.Join(table.asked, "; "); asked != c.asked {
				t.Errorf("the table was asked %q, want %q", asked, c.asked)
			}

			policy := &auth.OwnerPolicy{Grants: &lookup{by: held}}
			d, err := policy.Authorize(t.Context(), authz.Request{
				Subject: carol, Action: authorizer.ActionFileWrite, Resource: moved, Claims: map[string]any{},
			})
			if err != nil {
				t.Fatalf("the policy produced no decision: %v", err)
			}
			if want := auth.Admits(c.want, auth.PermissionWrite); d.Allow != want {
				t.Errorf("the owner policy answered allow=%t, want %t", d.Allow, want)
			}
		})
	}
}

// TestATableThatCannotAnswerForTheSourceStopsTheQuestion: the second read
// fails as the first does, so a question is never sent with the rung of one
// end.
func TestATableThatCannotAnswerForTheSourceStopsTheQuestion(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	table := &failingOn{path: "files/reports/q3.pdf", held: auth.PermissionWrite}
	a := asking(t, s, nil).WithGrants(table)

	_, err := a.Decide(serving(carol), authorizer.ActionFileWrite, authorizer.File{
		ID: "01J8R4", Owner: alice, Path: "files/inbox/q3.pdf", Plane: "files", From: "files/reports/q3.pdf",
	}.Resource())
	if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
		t.Errorf("a table that cannot answer for the source gave %v, want %s", err, auth.CodeAuthorizerUnavailable)
	}
	if seen := s.Requests(); len(seen) != 0 {
		t.Errorf("the endpoint was asked %d questions with the grant unresolved", len(seen))
	}
}

// failingOn is a grants table that answers one rung for every path but one,
// and fails on that one.
type failingOn struct {
	path string
	held auth.Permission
}

func (f *failingOn) Permission(_ context.Context, _, _, path string) (auth.Permission, error) {
	if path == f.path {
		return auth.PermissionNone, errors.New("the database is gone")
	}
	return f.held, nil
}

func (f *failingOn) Holders(_ context.Context, _, path, from string, _ int) ([]authorizer.Holder, error) {
	if path == f.path || from == f.path {
		return nil, errors.New("the database is gone")
	}
	return nil, nil
}

// wireGrants reads the grants member of a question as the endpoint decoded
// it, an array of {subject, permission} objects, and whether it was sent.
func wireGrants(t *testing.T, res authz.Resource) ([]authorizer.Holder, bool) {
	t.Helper()
	raw, present := res.Fields[auth.GrantsField]
	if !present {
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("grants went out as %T, want an array", raw)
	}
	out := []authorizer.Holder{}
	for _, entry := range list {
		h, ok := entry.(map[string]any)
		if !ok || len(h) != 2 {
			t.Fatalf("an entry of grants went out as %#v, want {subject, permission}", entry)
		}
		subject, _ := h["subject"].(string)
		permission, _ := h["permission"].(string)
		out = append(out, authorizer.Holder{Subject: subject, Permission: permission})
	}
	return out, true
}

// TestTheQuestionCarriesEveryHoldersGrant: beside the caller's own rung, a
// question about another's space names every subject's, so an endpoint
// that decides as someone other than the caller reads that subject's rung.
// The list is sorted by subject whatever order the table answered in, an
// empty list is sent where nobody holds a grant, and the questions that
// carry no grant carry no list either.
func TestTheQuestionCarriesEveryHoldersGrant(t *testing.T) {
	const dave = "https://issuer.example|dave"
	unsorted := []authorizer.Holder{{Subject: dave, Permission: "write"}, {Subject: bob, Permission: "read"}}
	sorted := []authorizer.Holder{{Subject: bob, Permission: "read"}, {Subject: dave, Permission: "write"}}
	moved := authorizer.File{
		ID: "01J8R4", Owner: alice, Path: "files/inbox/q3.pdf", Plane: "files", From: "files/reports/q3.pdf",
	}.Resource()
	for _, c := range []struct {
		name     string
		resource authz.Resource
		subject  string
		holders  []authorizer.Holder
		want     []authorizer.Holder
		sent     bool
		listed   string
	}{
		{"a stranger's read names the holders", aFile("01J8R4"), carol, unsorted, sorted, true,
			alice + " files/reports/q3.pdf  65"},
		{"a multipart write names them", anUpload(), carol, unsorted, sorted, true,
			alice + " files/reports/q3.pdf  65"},
		{"a move reads both ends in one read", moved, carol, unsorted, sorted, true,
			alice + " files/inbox/q3.pdf files/reports/q3.pdf 65"},
		{"nobody holds a grant", aFile("01J8R4"), carol, nil, []authorizer.Holder{}, true,
			alice + " files/reports/q3.pdf  65"},
		{"the space's own owner", aFile("01J8R4"), alice, unsorted, nil, false, ""},
		{"an anonymous caller", aFile("01J8R4"), "", unsorted, nil, false, ""},
		{"a share", aShare("01J8R5"), carol, unsorted, nil, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			table := &lookup{held: auth.PermissionRead, holders: c.holders}
			a := asking(t, s, nil).WithGrants(table)

			action := authorizer.ActionFileRead
			switch {
			case c.resource.Kind == authorizer.KindShare:
				action = authorizer.ActionShareRead
			case c.resource.Kind == authorizer.KindUpload:
				action = authorizer.ActionUploadWrite
			case c.resource.String(auth.FromField) != "":
				action = authorizer.ActionFileWrite
			}
			if _, err := a.Decide(serving(c.subject), action, c.resource); err != nil {
				t.Fatalf("the question was refused: %v", err)
			}
			got := sent(t, s)
			holders, present := wireGrants(t, got)
			if present != c.sent {
				t.Fatalf("grants is present=%v, want %v", present, c.sent)
			}
			if !slices.Equal(holders, c.want) {
				t.Errorf("the question names the holders %v, want %v", holders, c.want)
			}
			if _, truncated := got.Fields[auth.GrantsTruncatedField]; truncated {
				t.Error("a list within the bound is marked truncated")
			}
			if listed := strings.Join(table.listed, "; "); listed != c.listed {
				t.Errorf("the holders were read as %q, want %q", listed, c.listed)
			}
		})
	}
}

// TestTheHoldersAreBoundedAndMarkedTruncated: a path more subjects hold a
// grant on than the bound names the first MaxGrants by subject and says
// the list was cut, so an endpoint that does not find its subject there
// refuses rather than read it as none. A list exactly at the bound is
// whole.
func TestTheHoldersAreBoundedAndMarkedTruncated(t *testing.T) {
	many := func(n int) []authorizer.Holder {
		out := make([]authorizer.Holder, 0, n)
		for i := n - 1; i >= 0; i-- {
			out = append(out, authorizer.Holder{Subject: fmt.Sprintf("https://issuer.example|%03d", i), Permission: "read"})
		}
		return out
	}
	for _, c := range []struct {
		name      string
		answered  int
		truncated bool
	}{
		{"at the bound", authorizer.MaxGrants, false},
		{"one past the bound", authorizer.MaxGrants + 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := endpoint(t)
			s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
			a := asking(t, s, nil).WithGrants(&lookup{holders: many(c.answered)})
			if _, err := a.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4")); err != nil {
				t.Fatalf("the question was refused: %v", err)
			}
			got := sent(t, s)
			holders, _ := wireGrants(t, got)
			if len(holders) != authorizer.MaxGrants {
				t.Fatalf("the question names %d holders, want %d", len(holders), authorizer.MaxGrants)
			}
			if first, last := holders[0].Subject, holders[len(holders)-1].Subject; first != "https://issuer.example|000" || last != fmt.Sprintf("https://issuer.example|%03d", authorizer.MaxGrants-1) {
				t.Errorf("the list runs from %s to %s; it is the first by subject", first, last)
			}
			if flag, present := got.Fields[auth.GrantsTruncatedField]; present != c.truncated || (present && flag != true) {
				t.Errorf("grants_truncated is %v, present=%v, want present=%v", flag, present, c.truncated)
			}
		})
	}
}

// TestAGrantsTableThatCannotListTheHoldersStopsTheQuestion: a read of the
// holders that fails is no decision, as a read of the caller's own rung
// is: the request is authorizer_unavailable and the endpoint is not asked
// a question that says nobody holds a grant when the table did not say so.
func TestAGrantsTableThatCannotListTheHoldersStopsTheQuestion(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	a := asking(t, s, nil).WithGrants(&lookup{held: auth.PermissionRead, holdersErr: errors.New("the database is gone")})

	_, err := a.Decide(serving(carol), authorizer.ActionFileRead, aFile("01J8R4"))
	if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
		t.Errorf("a table that cannot list the holders gave %v, want %s", err, auth.CodeAuthorizerUnavailable)
	}
	if seen := s.Requests(); len(seen) != 0 {
		t.Errorf("the endpoint was asked %d questions with the holders unresolved", len(seen))
	}
}
