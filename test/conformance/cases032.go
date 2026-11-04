// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"testing"
)

// The cases of spec 032: a whole space deleted by an administrator.
//
// The space deleted is one a throwaway principal owns, so no other case of
// the run loses what it wrote. A target whose tokens are a fixed set holds
// no such principal, and the case says so rather than deleting Alice's.
func cases032() []testCase {
	return []testCase{
		{name: "SpaceDelete", group: GroupAdministraton, routes: []string{
			"DELETE /v1/admin/spaces/{owner}",
			"PUT /v1/files/{owner}/{path...}", "GET /v1/files/{owner}/{path...}",
			"DELETE /v1/files/{owner}/{path...}", "GET /v1/trash", "GET /v1/events",
		}, codes: []string{CodeInvalidField, CodeNotFound}, run: case032SpaceDelete},
	}
}

// counts reads the per-kind counts of a deletion's answer.
func counts(t testing.TB, r response) map[string]any {
	t.Helper()
	c, ok := r.json["counts"].(map[string]any)
	failIf(t, !ok, "the deletion answers no counts: %s", r.body)
	return c
}

// case032SpaceDelete: an administrator deletes a space, and the space then
// holds nothing. Its files, its trash and its history are gone, its log
// holds the one record of the deletion and no path, a repeat answers zero
// counts, and a space that never held anything answers zero counts too. A
// caller who is not the administrator deletes nothing where the target can
// tell, and a deletion that names no subject is refused before it asks anything.
func case032SpaceDelete(t *testing.T, s *session) {
	principal := "erased-" + s.run
	if _, err := s.options.Token(s.context(), principal); err != nil {
		s.unverifiable(t, "a deleted space holds nothing",
			"the target mints no token for a throwaway principal, and deleting a fixed principal's space would break the cases after it")
		return
	}
	owner := s.subject(t, principal)
	kept, trashed := s.filePath("erase-kept.txt"), s.filePath("erase-trashed.txt")
	expectAWrite(t, s.put(t, principal, kept, "the first content\n", "text/plain"))
	expectAWrite(t, s.put(t, principal, kept, "the second content\n", "text/plain"))
	expectAWrite(t, s.put(t, principal, trashed, "in the trash\n", "text/plain"))
	expectStatus(t, s.call(t, principal, http.MethodDelete, s.fileRoute(owner, trashed), ""), http.StatusNoContent)

	route := "/v1/admin/spaces/" + pathParam(owner)

	// An ordinary caller deletes nothing. The space asked about is one that
	// holds nothing, so a target that treats this caller as an administrator
	// loses nothing by the answer and the case says what it could not check.
	empty := "/v1/admin/spaces/" + pathParam(owner+"-empty")
	if refused := s.call(t, principal, http.MethodDelete, empty, ""); refused.status == http.StatusOK {
		s.unverifiable(t, "an ordinary caller deletes no space", "the target treats this caller as an administrator")
	} else {
		failIf(t, refused.status != http.StatusForbidden && refused.status != http.StatusNotFound,
			"an ordinary caller's deletion answered %d: %s", refused.status, refused.body)
	}

	// A name that is not a subject is a malformed request, the alias included.
	expectError(t, s.call(t, s.options.Admin, http.MethodDelete, "/v1/admin/spaces/me", ""), CodeInvalidField)
	expectError(t, s.call(t, s.options.Admin, http.MethodDelete, "/v1/admin/spaces/no-issuer", ""), CodeInvalidField)

	done := expectStatus(t, s.call(t, s.options.Admin, http.MethodDelete, route, ""), http.StatusOK)
	failIf(t, str(done.json, "owner") != owner || str(done.json, "status") != "deleted",
		"the deletion answered %s, want the owner %q and the status deleted", done.body, owner)
	c := counts(t, done)
	failIf(t, num(c, "files") != 1 || num(c, "trashed") != 1,
		"the deletion counted %v, want one live file and one trashed", c)

	s.expectGone(t, principal, owner, kept)
	s.expectGone(t, principal, owner, trashed)
	log, _ := s.tail(t, principal, "")
	failIf(t, len(log) != 1 || str(log[0], "action") != "space_delete" || str(log[0], "path") != "",
		"the space's log holds %v, want the one record of the deletion and no path", log)

	// Whatever is left to count is nothing: a repeat, and a space that never
	// held anything, both succeed with zero counts.
	for _, name := range []string{owner, owner + "-never-held"} {
		r := expectStatus(t, s.call(t, s.options.Admin, http.MethodDelete,
			"/v1/admin/spaces/"+pathParam(name), ""), http.StatusOK)
		for kind, n := range counts(t, r) {
			v, _ := n.(float64)
			failIf(t, v != 0, "a deletion of %q with nothing left counted %v of %s, want none", name, v, kind)
		}
	}
}
