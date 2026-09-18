// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"testing"
)

// The rows of spec 012 the wire shows: the overview pages one row per space
// with its usage, a restore brings back an object of a space the caller does
// not own, an administrator's own moderation delete is on that space's event
// tail, and each of those is a missing object for a caller who is not an
// administrator.
//
// Every administrative read other than the two routes below is an ordinary
// route of another spec asked about a space the caller does not own, so the
// cases here drive the two and the ordinary routes with ?owner=.
//
// These cases are written from spec 013's shapes. A target that does not
// answer the two admin routes holds them in the pending group, and a target
// that names no administrator in [Options.Admin] skips the group with that
// as the reason.

func cases012() []testCase {
	admin := []string{"GET /v1/admin/overview", "POST /v1/admin/spaces/{owner}/restore"}
	return []testCase{
		{name: "Overview", group: GroupAdministraton, routes: admin[:1],
			codes: []string{CodeInvalidField}, run: case012Overview},
		{name: "NotAnAdministrator", group: GroupAdministraton, routes: admin,
			codes: []string{CodeNotFound}, run: case012NotAnAdministrator},
		{name: "AcrossSpaces", group: GroupAdministraton, routes: append(append([]string{}, admin...),
			"GET /v1/events", "POST /v1/workspaces", "DELETE /v1/workspaces/{id}"), run: case012AcrossSpaces},
	}
}

// case012Overview: one row per space with its usage and its counts, paged
// through the one envelope of spec 013. There is no route that reads or
// writes a limit, because Arca stores none; the usage is a count and the
// limit is the authorizer's.
func case012Overview(t *testing.T, s *session) {
	r := expectStatus(t, s.call(t, s.options.Admin, http.MethodGet, "/v1/admin/overview?limit=1000", ""), http.StatusOK)
	failIf(t, r.json["entries"] == nil, "the overview answers a null entries: %s", r.body)
	for _, e := range list(r.json, "entries") {
		failIf(t, str(e, "owner") == "", "an overview row names no space: %v", e)
		if _, ok := e["bytes"]; !ok {
			t.Fatalf("an overview row carries no usage, and the overview is where a space's usage is read: %v", e)
		}
	}
	// The overview applies no filter, because it asks space.admin and not a
	// list action, so a limit outside the range is still a field with a
	// value it cannot take.
	expectError(t, s.call(t, s.options.Admin, http.MethodGet, "/v1/admin/overview?limit=5000", ""), CodeInvalidField)
}

// case012NotAnAdministrator: an administrative route is a missing object for
// a caller who is not one, and never a refused one. An administrator's
// surface is not something an ordinary caller learns the shape of.
func case012NotAnAdministrator(t *testing.T, s *session) {
	overview := s.call(t, Bob, http.MethodGet, "/v1/admin/overview", "")
	if overview.status == http.StatusOK {
		s.unverifiable(t, "an administrative route is a missing object for an ordinary caller",
			"the target treats this caller as an administrator")
		return
	}
	expectError(t, overview, CodeNotFound)

	restore := s.call(t, Bob, http.MethodPost,
		"/v1/admin/spaces/"+pathParam(s.subject(t, Alice))+"/restore", body(fields{"path": s.filePath("nothing.txt")}))
	expectError(t, restore, CodeNotFound)
}

// case012AcrossSpaces: an administrator reads another space through that
// space's own routes, and what an administrator did is on that space's event
// tail rather than on a second log.
func case012AcrossSpaces(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "admin-sees")
	owner := s.subject(t, Alice)

	seen := s.call(t, s.options.Admin, http.MethodGet, "/v1/events?owner="+escape(owner)+"&limit=1000", "")
	expectStatus(t, seen, http.StatusOK)
	failIf(t, seen.json["entries"] == nil, "an administrator reading another space's log got a null entries: %s", seen.body)

	listed := s.call(t, s.options.Admin, http.MethodGet, "/v1/workspaces?owner="+escape(owner)+"&limit=1000", "")
	expectStatus(t, listed, http.StatusOK)
	found := false
	for _, e := range list(listed.json, "entries") {
		if str(e, "id") == str(made, "id") {
			found = true
		}
	}
	failIf(t, !found, "an administrator listing another space does not see its workspace")
}
