// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"strings"
	"testing"
)

// The rows of spec 008 the wire shows: a grant names a subtree, a grantee
// and a rung of the ladder, is read back and revoked, and shows in the
// grantee's own listing and in nobody else's; and a link is a token grant
// that an unauthenticated caller redeems, which is the one exception to
// invariant 5 of spec 001.

func cases008() []testCase {
	shares := []string{"POST /v1/shares", "GET /v1/shares", "GET /v1/shares/{id}", "DELETE /v1/shares/{id}"}
	links := []string{"POST /v1/shares/links", "GET /v1/shares/links", "DELETE /v1/shares/links/{id}"}
	redeem := []string{"GET /v1/shares/links/{token}/meta", "GET /v1/shares/links/{token}"}
	return []testCase{
		{name: "Grant", group: GroupShares, routes: append(append([]string{}, shares...),
			"POST /v1/workspaces", "DELETE /v1/workspaces/{id}"), run: case008Grant},
		{name: "WithMe", group: GroupShares, routes: append(append([]string{}, shares...),
			"GET /v1/shares/with-me", "POST /v1/workspaces", "DELETE /v1/workspaces/{id}"), run: case008WithMe},
		{name: "Ladder", group: GroupShares, routes: shares,
			codes: []string{CodeInvalidField, CodeUnknownPlane}, run: case008Ladder},
		{name: "Link", group: GroupLinks, routes: append(append([]string{}, links...), redeem...),
			run: case008Link},
		{name: "LinkReadOnly", group: GroupLinks, routes: []string{"POST /v1/shares/links"},
			codes: []string{CodeLinkReadOnly}, run: case008LinkReadOnly},
		{name: "LinkRevoked", group: GroupLinks, routes: append(append([]string{}, links...), redeem...),
			codes: []string{CodeNotFound}, run: case008LinkRevoked},
		{name: "LinkFile", group: GroupLinks, routes: append(append([]string{}, links...),
			"GET /v1/shares/links/{token}/files/{path...}",
			"PUT /v1/files/{owner}/{path...}"), run: case008LinkFile},
	}
}

// grantPrefix is a subtree of the caller's own files plane, under the run's
// own name, so a grant the run makes covers nothing anybody else stored.
func (s *session) grantPrefix(label string) string { return "files/" + s.name(label) }

// grant makes one and records it for the cleanup.
func (s *session) grant(t *testing.T, principal, prefix, grantee, permission string) map[string]any {
	t.Helper()
	r := s.call(t, principal, http.MethodPost, "/v1/shares",
		body(fields{"owner": "me", "path_prefix": prefix, "grantee": grantee, "permission": permission}))
	expectStatus(t, r, http.StatusCreated)
	id := str(r.json, "id")
	failIf(t, id == "", "the created grant carries no id: %s", r.body)
	s.record("share "+id, func(t testing.TB) error {
		s.call(t, principal, http.MethodDelete, "/v1/shares/"+id, "")
		return nil
	})
	return r.json
}

// case008Grant: a grant carries the fields spec 013's shape names, is read
// back by id, shows in the space's listing, and a revoke takes effect on the
// next request rather than eventually.
func case008Grant(t *testing.T, s *session) {
	prefix := s.grantPrefix("grant")
	bob := s.subject(t, Bob)
	made := s.grant(t, Alice, prefix, bob, "read")
	id := str(made, "id")
	failIf(t, str(made, "path_prefix") != prefix, "the grant covers %q, want %q", str(made, "path_prefix"), prefix)
	failIf(t, str(made, "permission") != "read", "the grant names the permission %q, want read", str(made, "permission"))
	failIf(t, str(made, "owner") == "", "the grant names no owner, and spec 013 renders a subject in full: %v", made)

	read := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/shares/"+id, ""), http.StatusOK)
	failIf(t, str(read.json, "id") != id, "GET by id answered the grant %q, want %q", str(read.json, "id"), id)

	page := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/shares?owner=me&limit=1000", ""), http.StatusOK)
	found := false
	for _, e := range list(page.json, "entries") {
		if str(e, "id") == id {
			found = true
		}
	}
	failIf(t, !found, "the grant the run made is not in its space's listing")

	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/shares/"+id, ""), http.StatusNoContent)
	gone := s.call(t, Alice, http.MethodGet, "/v1/shares/"+id, "")
	failIf(t, gone.status == http.StatusOK && str(gone.json, "status") == "active",
		"a revoked grant is still active on the next request: %s", gone.body)
}

// case008WithMe: the shared-with-me listing shows a grant to its grantee and
// to nobody else. A grantee seeing somebody else's grant is the same leak as
// a space being readable, and the listing is the one place a caller learns
// what was shared with them.
func case008WithMe(t *testing.T, s *session) {
	prefix := s.grantPrefix("with-me")
	bob := s.subject(t, Bob)
	made := s.grant(t, Alice, prefix, bob, "read")
	id := str(made, "id")

	mine := expectStatus(t, s.call(t, Bob, http.MethodGet, "/v1/shares/with-me?limit=1000", ""), http.StatusOK)
	found := false
	for _, e := range list(mine.json, "entries") {
		if str(e, "id") == id {
			found = true
		}
	}
	failIf(t, !found, "the grantee's shared-with-me listing does not hold the grant made to them")

	theirs := s.call(t, Alice, http.MethodGet, "/v1/shares/with-me?limit=1000", "")
	if theirs.status != http.StatusOK {
		return
	}
	for _, e := range list(theirs.json, "entries") {
		failIf(t, str(e, "id") == id,
			"a grant to another principal is in this caller's shared-with-me listing")
	}
}

// case008Ladder: each rung of the permission ladder is accepted under its
// own name and a rung that is not on it is refused, so a caller cannot
// invent a permission the ladder does not define.
func case008Ladder(t *testing.T, s *session) {
	bob := s.subject(t, Bob)
	for _, rung := range []string{"read", "write", "manage"} {
		made := s.grant(t, Alice, s.grantPrefix("ladder-"+rung), bob, rung)
		failIf(t, str(made, "permission") != rung, "a %q grant answered the permission %q", rung, str(made, "permission"))
	}
	refused := s.call(t, Alice, http.MethodPost, "/v1/shares",
		body(fields{"owner": "me", "path_prefix": s.grantPrefix("ladder-none"), "grantee": bob, "permission": "own"}))
	failIf(t, refused.status == http.StatusCreated, "a permission outside the ladder was granted")
	expectError(t, refused, CodeInvalidField)

	// A prefix under no plane is not a subtree of this space, and the two
	// refusals are told apart so an operator reading the second learns the
	// planes are two.
	outside := s.call(t, Alice, http.MethodPost, "/v1/shares",
		body(fields{"owner": "me", "path_prefix": "secrets/" + s.run, "grantee": bob, "permission": "read"}))
	expectError(t, outside, CodeUnknownPlane)
}

// case008Link: a link is a token grant, the token is answered once at
// creation, and the three redemption routes serve it to a caller with no
// bearer. The token is what authorizes, so a caller holding it needs
// nothing else, and a caller without it learns nothing.
func case008Link(t *testing.T, s *session) {
	prefix := s.grantPrefix("link")
	made := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/shares/links",
		body(fields{"owner": "me", "path_prefix": prefix, "permission": "read"})), http.StatusCreated)
	id, token := str(made.json, "id"), str(made.json, "token")
	failIf(t, id == "" || token == "", "a minted link carries no id or no token: %s", made.body)
	s.record("link "+id, func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, "/v1/shares/links/"+id, "")
		return nil
	})

	page := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/shares/links?owner=me&limit=1000", ""), http.StatusOK)
	for _, e := range list(page.json, "entries") {
		if str(e, "id") == id {
			failIf(t, str(e, "token") != "",
				"the listing answers the token again, and spec 013 shows it once at creation: %v", e)
		}
	}

	if !s.options.Anonymous {
		s.unverifiable(t, "a public link serves an unauthenticated caller",
			"Options.Anonymous is not set, so the target does not offer public reading")
		return
	}
	meta := s.do(t, request{method: http.MethodGet, path: "/v1/shares/links/" + token + "/meta"})
	expectStatus(t, meta, http.StatusOK)
	failIf(t, str(meta.json, "path_prefix") != prefix,
		"the link's meta names the subtree %q, want %q", str(meta.json, "path_prefix"), prefix)
	failIf(t, str(meta.json, "kind") == "", "the link's meta names no kind: %s", meta.body)

	listing := s.do(t, request{method: http.MethodGet, path: "/v1/shares/links/" + token})
	expectStatus(t, listing, http.StatusOK)

	// A link grants reading and nothing more, which the wire shows as a
	// write through a redemption route being refused rather than ignored.
	write := s.do(t, request{method: http.MethodDelete, path: "/v1/shares/links/" + token})
	failIf(t, write.status < 400, "a write through a redemption route answered %d", write.status)
}

// case008LinkReadOnly: a token grant carries reading and nothing more, so a
// mint that asks for a rung above it is refused at creation rather than
// quietly narrowed. A link that granted a write would be a credential
// anybody who saw the URL could write with, which is why spec 013 has a code
// written for this rule alone.
func case008LinkReadOnly(t *testing.T, s *session) {
	for _, rung := range []string{"write", "manage"} {
		r := s.call(t, Alice, http.MethodPost, "/v1/shares/links",
			body(fields{"owner": "me", "path_prefix": s.grantPrefix("link-" + rung), "permission": rung}))
		if r.status == http.StatusCreated {
			s.record("link "+str(r.json, "id"), func(t testing.TB) error {
				s.call(t, Alice, http.MethodDelete, "/v1/shares/links/"+str(r.json, "id"), "")
				return nil
			})
			t.Fatalf("a link was minted carrying %q, and a token grant carries reading", rung)
		}
		details := expectError(t, r, CodeLinkReadOnly)
		fields := expectFields(t, details)
		failIf(t, !strings.Contains(strings.Join(fields, ","), "permission"),
			"the refusal names the fields %v and not permission", fields)
	}
}

// case008LinkRevoked: a revoked token is a missing object on the next
// request, and a token that never existed is the same answer, so a caller
// probing tokens learns nothing about which ones were real.
func case008LinkRevoked(t *testing.T, s *session) {
	prefix := s.grantPrefix("link-revoked")
	made := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/shares/links",
		body(fields{"owner": "me", "path_prefix": prefix, "permission": "read"})), http.StatusCreated)
	id, token := str(made.json, "id"), str(made.json, "token")

	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/shares/links/"+id, ""), http.StatusNoContent)

	revoked := s.do(t, request{method: http.MethodGet, path: "/v1/shares/links/" + token + "/meta"})
	absent := s.do(t, request{method: http.MethodGet, path: "/v1/shares/links/" + s.name("never-minted") + "/meta"})
	expectError(t, revoked, CodeNotFound)
	expectError(t, absent, CodeNotFound)
	failIf(t, revoked.status != absent.status || revoked.code() != absent.code() || revoked.message() != absent.message(),
		"a revoked token answers %d %s and one that never existed %d %s; the two are one answer",
		revoked.status, revoked.code(), absent.status, absent.code())
}

// case008LinkFile: one object under the token's subtree is served through
// the redemption route to a caller with no bearer, and an object outside the
// subtree is not. The token names a subtree, so what it reaches is the whole
// of what it grants and a path that leaves the subtree is a missing object.
func case008LinkFile(t *testing.T, s *session) {
	prefix := s.grantPrefix("link-file")
	inside := prefix + "/inside.txt"
	outside := s.grantPrefix("link-file-outside") + "/outside.txt"
	expectAWrite(t, s.put(t, Alice, inside, "inside the subtree\n", "text/plain"))
	expectAWrite(t, s.put(t, Alice, outside, "outside the subtree\n", "text/plain"))

	made := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/shares/links",
		body(fields{"owner": "me", "path_prefix": prefix, "permission": "read"})), http.StatusCreated)
	id, token := str(made.json, "id"), str(made.json, "token")
	s.record("link "+id, func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, "/v1/shares/links/"+id, "")
		return nil
	})

	served := s.do(t, request{method: http.MethodGet, path: "/v1/shares/links/" + token + "/files/" + inside + "?inline=1"})
	failIf(t, served.status == http.StatusUnauthorized,
		"a redemption route asked for a bearer, and the token in the URL is the whole of its authorization")
	expectStatus(t, served, http.StatusOK)
	failIf(t, string(served.body) != "inside the subtree\n", "the link served %q", served.body)

	// A path that leaves the subtree the token names is a missing object,
	// whether it is a sibling or a traversal. The traversal is sent with its
	// dots percent-encoded, because a router removes a dot segment from a
	// request line before any handler reads it: what is asserted here is the
	// answer of the route, and a redirect to the cleaned path is the
	// router's answer and not the route's.
	for _, escaping := range []string{outside, prefix + "/%2e%2e/" + outside} {
		r := s.do(t, request{method: http.MethodGet, path: "/v1/shares/links/" + token + "/files/" + escaping})
		failIf(t, r.status == http.StatusOK, "a link served an object outside the subtree it names: %q", escaping)
		expectError(t, r, CodeNotFound)
	}
}
