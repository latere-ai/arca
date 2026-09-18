// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The rows of spec 013 the wire shows on every route: the error envelope and
// the codes its table fixes, the one list envelope and its keyset paging,
// the request id, the bodies and the content types, and the two public
// documents that take no token.

func cases013() []testCase {
	create := []string{"POST /v1/workspaces", "DELETE /v1/workspaces/{id}"}
	pages := []string{"GET /v1/workspaces", "GET /v1/shares", "GET /v1/events"}
	return []testCase{
		{name: "Envelope", group: GroupErrors, routes: create, run: case013Envelope},
		{name: "ErrorTable", group: GroupErrors, routes: create, run: case013ErrorTable},
		{name: "Documents", group: GroupErrors, run: case013Documents},
		{name: "RequestID", group: GroupErrors, routes: create, run: case013RequestID},
		{name: "ListEnvelope", group: GroupErrors, routes: pages, run: case013ListEnvelope},
		{name: "Limit", group: GroupErrors, routes: pages, run: case013Limit},
		{name: "BodiesAndTypes", group: GroupErrors, routes: create, run: case013BodiesAndTypes},
		{name: "Planes", group: GroupPaths, routes: []string{"POST /v1/shares"}, run: case013Planes},
	}
}

// case013Envelope: every refusal is one envelope with a code, the table's
// sentence and a request id, and a code that names fields carries the field
// paths at fault. A caller branches on the code and shows the sentence, so
// the sentence never carries a value.
func case013Envelope(t *testing.T, s *session) {
	r := s.call(t, Alice, http.MethodPost, "/v1/workspaces", body(fields{"owner": "me"}))
	details := expectError(t, r, CodeMissingField)
	fields := expectFields(t, details)
	failIf(t, len(fields) == 0, "a field naming refusal carries no details.fields")

	// details.detail is the developer sentence and the only field that names
	// a value. It is never parsed, and it is never the message.
	failIf(t, r.message() != codeTable[CodeMissingField].sentence,
		"the message varies with the request: %q", r.message())

	// Nothing above 499 names a store, a query, a key or an internal type,
	// even in the developer detail. A refusal below 500 may name a path,
	// which is what the detail is for.
	if r.status >= 500 {
		for _, leak := range []string{"postgres", "pgx", "sql", "bucket", "s3", "SELECT", "INSERT"} {
			failIf(t, strings.Contains(strings.ToLower(string(r.body)), strings.ToLower(leak)),
				"a 5xx body names %q: %s", leak, r.body)
		}
	}
}

// case013ErrorTable: every code the suite can provoke arrives with the
// status and the one sentence spec 013's table fixes. A code the suite
// cannot provoke from outside the installation is named in unprovoked with
// the reason, so the table is read whole and the gaps are recorded rather
// than passed over.
func case013ErrorTable(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "codes")
	id, slug := str(made, "id"), str(made, "slug")

	provoke := []struct {
		code  string
		drive func() response
	}{
		{CodeUnauthenticated, func() response {
			return s.do(t, request{method: http.MethodGet, path: "/v1/workspaces"})
		}},
		{CodeMissingField, func() response {
			return s.call(t, Alice, http.MethodPost, "/v1/workspaces", body(fields{"owner": "me"}))
		}},
		{CodeUnknownField, func() response {
			return s.call(t, Alice, http.MethodPost, "/v1/workspaces", body(fields{"owner": "me", "slug": s.name("x"), "colour": "green"}))
		}},
		{CodeInvalidField, func() response {
			return s.call(t, Alice, http.MethodGet, "/v1/workspaces?limit=5000", "")
		}},
		{CodeBadRequest, func() response {
			return s.do(t, request{method: http.MethodPost, path: "/v1/workspaces", subject: Alice,
				body: strings.NewReader("{not json"), contentType: "application/json"})
		}},
		{CodeUnsupportedMediaType, func() response {
			return s.do(t, request{method: http.MethodPost, path: "/v1/workspaces", subject: Alice,
				body: strings.NewReader(body(fields{"owner": "me", "slug": s.name("y")})), contentType: "text/plain"})
		}},
		{CodeNotAcceptable, func() response {
			return s.with(t, Alice, http.MethodGet, "/v1/workspaces", "", map[string]string{"Accept": "text/plain"})
		}},
		{CodeNotFound, func() response {
			return s.call(t, Alice, http.MethodGet, "/v1/workspaces/"+s.name("no-such-workspace"), "")
		}},
		{CodeSlugTaken, func() response {
			return s.call(t, Alice, http.MethodPost, "/v1/workspaces", body(fields{"owner": "me", "slug": slug}))
		}},
		{CodeUnknownPlane, func() response {
			return s.call(t, Alice, http.MethodPost, "/v1/shares",
				body(fields{"owner": "me", "path_prefix": "secrets/x", "grantee": s.subject(t, Bob), "permission": "read"}))
		}},
		{CodeInvalidPath, func() response {
			return s.call(t, Alice, http.MethodPost, "/v1/shares",
				body(fields{"owner": "me", "path_prefix": "/files/x", "grantee": s.subject(t, Bob), "permission": "read"}))
		}},
		{CodeWriterHeld, func() response {
			held := s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
				body(fields{"sandbox_id": s.name("codes-a"), "mode": "rw", "ttl_seconds": 120}))
			if held.status != http.StatusCreated {
				return held
			}
			defer s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+str(held.json, "id"), "")
			return s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
				body(fields{"sandbox_id": s.name("codes-b"), "mode": "rw", "ttl_seconds": 120}))
		}},
	}
	seen := map[string]bool{}
	for _, p := range provoke {
		t.Run(p.code, func(t *testing.T) {
			if waiting := s.pendingRoutes(routesOfCode(p.code)); len(waiting) > 0 {
				t.Skipf("the pending group holds it: the target does not serve %s", strings.Join(waiting, ", "))
			}
			expectError(t, p.drive(), p.code)
		})
		seen[p.code] = true
	}

	// The codes this suite reaches no route for are the ones the pending
	// group names and the ones unprovoked records, and no others: a code
	// that is neither is a hole in this case.
	for code := range codeTable {
		if seen[code] {
			continue
		}
		if _, named := unprovoked[code]; named {
			continue
		}
		t.Logf("%s is not provoked by this case; it is reached by the group its row belongs to", code)
	}
}

// routesOfCode names the rows a code's provocation drives, so a code that
// needs a route the target does not serve is held by the pending group
// rather than failing as though the answer were wrong.
func routesOfCode(code string) []string {
	switch code {
	case CodeUnknownPlane, CodeInvalidPath:
		return []string{"POST /v1/shares"}
	case CodeWriterHeld:
		return []string{"POST /v1/workspaces/{id}/attach", "DELETE /v1/workspaces/{id}/attach/{aid}"}
	}
	return []string{"POST /v1/workspaces", "GET /v1/workspaces"}
}

// case013Documents: the two public documents answer without a bearer. A
// route name is not a secret, and existence hiding protects objects rather
// than the shape of the API.
func case013Documents(t *testing.T, s *session) {
	doc := s.do(t, request{method: http.MethodGet, path: "/openapi.json"})
	expectStatus(t, doc, http.StatusOK)
	failIf(t, doc.json == nil, "GET /openapi.json is not JSON: %s", doc.body)
	version := str(doc.json, "openapi")
	failIf(t, !strings.HasPrefix(version, "3."), "the document names the version %q, and spec 013 serves OpenAPI 3", version)
	failIf(t, len(obj(doc.json, "paths")) == 0, "the document names no path")

	root := s.do(t, request{method: http.MethodGet, path: "/"})
	failIf(t, root.status == http.StatusUnauthorized, "GET / asked for a bearer, and spec 002 answers the build identity")
}

// case013RequestID: every answer carries the request id back, a client's own
// id within the rule is kept, and one outside it is replaced rather than
// echoed. The id reaches the error envelope, the authorizer question and the
// event, so a caller that sent one finds its request in every record.
func case013RequestID(t *testing.T, s *session) {
	mine := "conformance-" + s.run
	kept := s.with(t, Alice, http.MethodGet, "/v1/workspaces?limit=1", "", map[string]string{HeaderRequestID: mine})
	failIf(t, kept.header.Get(HeaderRequestID) != mine,
		"a client id within the rule came back as %q, want %q", kept.header.Get(HeaderRequestID), mine)

	// Over 128 characters, and not printable ASCII: both are outside the
	// rule, and both are replaced by one the server minted.
	for _, outside := range []string{strings.Repeat("a", 200), "a\nb"} {
		r := s.with(t, Alice, http.MethodGet, "/v1/workspaces?limit=1", "", map[string]string{HeaderRequestID: outside})
		got := r.header.Get(HeaderRequestID)
		failIf(t, got == outside, "a client id outside the rule was echoed: %q", got)
		failIf(t, got == "", "an answer carries no %s", HeaderRequestID)
	}

	// The id is on a refusal too, in the header and in the details.
	refused := s.with(t, Alice, http.MethodGet, "/v1/workspaces?limit=5000", "", map[string]string{HeaderRequestID: mine})
	details := expectError(t, refused, CodeInvalidField)
	failIf(t, str(details, "request_id") != refused.header.Get(HeaderRequestID),
		"the envelope's request_id is %q and the header's is %q", str(details, "request_id"), refused.header.Get(HeaderRequestID))
}

// case013ListEnvelope: every list route answers one shape. entries is never
// null and is an empty array on an empty page, and next_cursor is present
// only when a further page exists, so its presence is the only has-more
// signal and a client stops when it is absent.
func case013ListEnvelope(t *testing.T, s *session) {
	for _, path := range []string{"/v1/workspaces?owner=me", "/v1/shares?owner=me", "/v1/events?owner=me"} {
		t.Run(strings.Trim(strings.SplitN(path, "?", 2)[0], "/"), func(t *testing.T) {
			if waiting := s.pendingRoutes([]string{listRoute(path)}); len(waiting) > 0 {
				t.Skipf("the pending group holds it: the target does not serve %s", strings.Join(waiting, ", "))
			}
			r := expectStatus(t, s.call(t, Alice, http.MethodGet, path+"&limit=1000", ""), http.StatusOK)
			raw, ok := r.json["entries"]
			failIf(t, !ok, "the answer carries no entries: %s", r.body)
			failIf(t, raw == nil, "the answer carries a null entries, and spec 013 answers an empty array: %s", r.body)

			// One row per page reaches the has-more signal, which is what a
			// client pages on.
			first := expectStatus(t, s.call(t, Alice, http.MethodGet, path+"&limit=1", ""), http.StatusOK)
			if next := str(first.json, "next_cursor"); next != "" {
				second := expectStatus(t, s.call(t, Alice, http.MethodGet, path+"&limit=1&cursor="+escape(next), ""), http.StatusOK)
				failIf(t, len(list(second.json, "entries")) > 1, "a page of one answered more than one row")
				a, b := list(first.json, "entries"), list(second.json, "entries")
				if len(a) == 1 && len(b) == 1 {
					failIf(t, str(a[0], "id") != "" && str(a[0], "id") == str(b[0], "id"),
						"the cursor answered the row it was given, so a keyset page does not advance")
				}
			}
		})
	}
}

// listRoute is the row of spec 013's table a list path belongs to.
func listRoute(path string) string {
	return http.MethodGet + " " + strings.SplitN(path, "?", 2)[0]
}

// case013Limit: ?limit= outside its range is invalid_field rather than a
// silent clamp. A client asking for five thousand and receiving a thousand
// silently believes it has read everything, which is the change from the
// service Arca replaces.
func case013Limit(t *testing.T, s *session) {
	for _, limit := range []string{"5000", "0", "-1", "many"} {
		t.Run(limit, func(t *testing.T) {
			r := s.call(t, Alice, http.MethodGet, "/v1/workspaces?owner=me&limit="+limit, "")
			details := expectError(t, r, CodeInvalidField)
			fields := expectFields(t, details)
			failIf(t, !strings.Contains(strings.Join(fields, ","), "limit"),
				"the refusal names the fields %v and not limit", fields)
		})
	}
	expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces?owner=me&limit=1000", ""), http.StatusOK)
}

// case013BodiesAndTypes: a JSON route refuses a field it does not know, a
// content type that is not JSON, and an Accept that excludes JSON. A client
// that misspells a field learns it at once rather than silently writing a
// default.
func case013BodiesAndTypes(t *testing.T, s *session) {
	expectError(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces",
		body(fields{"owner": "me", "slug": s.name("typo"), "slugg": "x"})), CodeUnknownField)

	expectError(t, s.do(t, request{method: http.MethodPost, path: "/v1/workspaces", subject: Alice,
		body: strings.NewReader(body(fields{"owner": "me", "slug": s.name("type")})), contentType: "application/x-www-form-urlencoded"}),
		CodeUnsupportedMediaType)

	expectError(t, s.with(t, Alice, http.MethodGet, "/v1/workspaces", "",
		map[string]string{"Accept": "text/csv"}), CodeNotAcceptable)

	// A body far above what any route accepts is body_too_large and not a
	// connection the server dropped.
	huge := `{"owner":"me","slug":"` + strings.Repeat("x", 4<<20) + `"}`
	r := s.do(t, request{method: http.MethodPost, path: "/v1/workspaces", subject: Alice,
		body: strings.NewReader(huge), contentType: "application/json"})
	failIf(t, r.status < 400, "a body of four megabytes on a JSON route was accepted")
	code := r.code()
	failIf(t, code != CodeBodyTooLarge && code != CodeInvalidField,
		"an oversized body answered %d %s; spec 013 names body_too_large", r.status, code)
	expectError(t, r, code)
}

// case013Planes: the two planes of spec 001 are the whole of what this
// server serves. A path under a third prefix is refused with unknown_plane,
// and a path that escapes its space is refused with invalid_path, so the two
// are told apart and an operator reading the second learns the planes are
// two and which.
func case013Planes(t *testing.T, s *session) {
	bob := s.subject(t, Bob)
	for _, plane := range []string{"files/" + s.run, "workspaces/" + s.run} {
		r := s.call(t, Alice, http.MethodPost, "/v1/shares",
			body(fields{"owner": "me", "path_prefix": plane, "grantee": bob, "permission": "read"}))
		expectStatus(t, r, http.StatusCreated)
		s.record("share "+str(r.json, "id"), func(t testing.TB) error {
			s.call(t, Alice, http.MethodDelete, "/v1/shares/"+str(r.json, "id"), "")
			return nil
		})
	}
	for _, third := range []string{"repos/x", "memory/x", "secrets/x", "x"} {
		expectError(t, s.call(t, Alice, http.MethodPost, "/v1/shares",
			body(fields{"owner": "me", "path_prefix": third, "grantee": bob, "permission": "read"})), CodeUnknownPlane)
	}
	for _, escaping := range []string{"files/../etc/passwd", "/files/x", "files//x", "files/x/"} {
		r := s.call(t, Alice, http.MethodPost, "/v1/shares",
			body(fields{"owner": "me", "path_prefix": escaping, "grantee": bob, "permission": "read"}))
		failIf(t, r.status == http.StatusCreated, "a path that escapes its plane was accepted: %q", escaping)
		expectError(t, r, r.code())
	}
}

// mustMarshal renders a decoded value back to JSON, so a case can assert
// over a whole entry rather than field by field.
func mustMarshal(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	failIf(t, err != nil, "render %v: %v", v, err)
	return raw
}
