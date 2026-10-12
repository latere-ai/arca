// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"strings"
	"testing"
)

// The rows of spec 010 the wire shows: every mutation a run makes appears on
// the space's log in the order it was made, the cursor resumes from a saved
// position, no event of another principal's space appears, nothing a caller
// planted in a name is echoed as a secret, and a write past the limit the
// authorizer's answer carried is refused for size.

func cases010() []testCase {
	events := []string{"GET /v1/events"}
	writes := []string{"GET /v1/events", "PUT /v1/files/{owner}/{path...}", "DELETE /v1/files/{owner}/{path...}"}
	return []testCase{
		{name: "Tail", group: GroupEvents, routes: writes, run: case010Tail},
		{name: "Cursor", group: GroupEvents, routes: writes, run: case010Cursor},
		{name: "OneSpace", group: GroupEvents, routes: writes, run: case010OneSpace},
		{name: "NoSecretInAName", group: GroupEvents, routes: writes, run: case010NoSecretInAName},
		{name: "QuotaExceeded", group: GroupUsage, routes: events,
			codes: []string{CodeQuotaExceeded}, run: case010QuotaExceeded},
		{name: "NoLimitNoRefusal", group: GroupUsage, routes: writes, run: case010NoLimitNoRefusal},
	}
}

// tail reads a space's whole log from a cursor, following next_cursor until
// it is absent, which is the one has-more signal spec 013 gives.
func (s *session) tail(t *testing.T, principal, cursor string) ([]map[string]any, string) {
	t.Helper()
	var all []map[string]any
	for range 100 {
		path := "/v1/events?owner=me&limit=1000"
		if cursor != "" {
			path += "&cursor=" + escape(cursor)
		}
		r := expectStatus(t, s.call(t, principal, http.MethodGet, path, ""), http.StatusOK)
		entries := list(r.json, "entries")
		failIf(t, r.json["entries"] == nil, "the log answers a null entries, and spec 013 never does: %s", r.body)
		all = append(all, entries...)
		next := str(r.json, "next_cursor")
		if next == "" {
			return all, cursor
		}
		failIf(t, next == cursor, "the log answered the cursor it was given, so a reader never finishes")
		cursor = next
	}
	t.Fatalf("the log did not end after a hundred pages")
	return nil, ""
}

// case010Tail: the mutations a run makes appear on its own space's log, in
// the order they were made, with the fields spec 013's shape names.
func case010Tail(t *testing.T, s *session) {
	before, cursor := s.tail(t, Alice, "")
	_ = before
	first, second := s.filePath("tail-one.txt"), s.filePath("tail-two.txt")
	expectAWrite(t, s.put(t, Alice, first, "one\n", "text/plain"))
	expectAWrite(t, s.put(t, Alice, second, "two\n", "text/plain"))

	after, _ := s.tail(t, Alice, cursor)
	failIf(t, len(after) < 2, "two objects were written and the log gained %d entries", len(after))
	for _, e := range after {
		failIf(t, str(e, "action") == "", "an event names no action: %v", e)
		failIf(t, str(e, "owner") == "", "an event names no owner: %v", e)
		failIf(t, str(e, "at") == "", "an event carries no timestamp: %v", e)
	}

	// The order is the order the run made them, which a keyset log answers
	// oldest first.
	firstAt, secondAt := -1, -1
	for i, e := range after {
		if str(e, "path") == first {
			firstAt = i
		}
		if str(e, "path") == second {
			secondAt = i
		}
	}
	failIf(t, firstAt < 0 || secondAt < 0,
		"the log holds no event at %q or none at %q, and a write a caller made is on its space's log", first, second)
	failIf(t, firstAt > secondAt, "the log answers the second write before the first; spec 013 tails oldest first")
}

// case010Cursor: a cursor saved from one page resumes the tail exactly
// where it stopped, which is what a consumer tailing a log relies on, and
// next_cursor is absent on the last page rather than repeated.
func case010Cursor(t *testing.T, s *session) {
	expectAWrite(t, s.put(t, Alice, s.filePath("cursor-one.txt"), "one\n", "text/plain"))
	_, at := s.tail(t, Alice, "")
	expectAWrite(t, s.put(t, Alice, s.filePath("cursor-two.txt"), "two\n", "text/plain"))

	resumed, _ := s.tail(t, Alice, at)
	for _, e := range resumed {
		failIf(t, str(e, "at") == "", "a resumed page holds an event with no timestamp: %v", e)
	}

	last := "/v1/events?owner=me&limit=1000"
	if at != "" {
		last += "&cursor=" + escape(at)
	}
	r := expectStatus(t, s.call(t, Alice, http.MethodGet, last, ""), http.StatusOK)
	if _, present := r.json["next_cursor"]; present {
		failIf(t, str(r.json, "next_cursor") == "",
			"the answer carries an empty next_cursor; its presence is the only has-more signal: %s", r.body)
	}
}

// case010OneSpace: a space's log holds that space's events and no other's.
// A consumer tailing its own log learning what happened in somebody else's
// is the same leak as reading their objects.
func case010OneSpace(t *testing.T, s *session) {
	mine := s.filePath("one-space.txt")
	expectAWrite(t, s.put(t, Alice, mine, "one space\n", "text/plain"))

	page := s.call(t, Bob, http.MethodGet, "/v1/events?owner=me&limit=1000", "")
	if page.status != http.StatusOK {
		s.unverifiable(t, "no event of another space appears on this one's log",
			"the second principal's own log is not readable on this target")
		return
	}
	alice := s.subject(t, Alice)
	for _, e := range list(page.json, "entries") {
		failIf(t, str(e, "owner") == alice, "an event of another principal's space is on this caller's log: %v", e)
		failIf(t, strings.Contains(string(mustMarshal(t, e)), mine),
			"an event naming another space's object is on this caller's log: %v", e)
	}
}

// case010NoSecretInAName: a value a caller planted in a name is not echoed
// into the log as anything but that name. The log is read by a consumer and
// kept, so a token that reached it once is a token that leaked.
func case010NoSecretInAName(t *testing.T, s *session) {
	planted := s.filePath("plant-" + strings.Repeat("z", 8) + ".txt")
	expectAWrite(t, s.put(t, Alice, planted, "planted\n", "text/plain"))
	entries, _ := s.tail(t, Alice, "")
	for _, e := range entries {
		raw := string(mustMarshal(t, e))
		failIf(t, strings.Contains(raw, "Bearer ") || strings.Contains(raw, "Authorization"),
			"an event carries a credential of the request that made it: %v", e)
	}
}

// case010QuotaExceeded: a limit reaches Arca on the authorizer's answer and
// nowhere else, and a write that would cross it is refused with
// quota_exceeded, which is a 413 and not a class of its own. With no limit
// on the answer, no write is refused for size.
func case010QuotaExceeded(t *testing.T, s *session) {
	// The write that crosses a limit is a put of object bytes, which is spec
	// 005's route. Until it lands the byte half of this group has nothing to
	// drive, and the pending group reports the route.
	if waiting := s.pendingRoutes([]string{"PUT /v1/files/{owner}/{path...}"}); len(waiting) > 0 {
		s.unverifiable(t, "a write past the answer's quota_bytes is refused with quota_exceeded",
			"the target does not serve "+strings.Join(waiting, ", ")+", which the pending group reports")
		return
	}
	// The limit reaches the target on its authorizer's answer, so a run with
	// no stub to put one there has no way to make the target refuse.
	if s.options.AuthorizerControl == "" {
		s.unverifiable(t, "a write past the answer's quota_bytes is refused with quota_exceeded",
			"Options.AuthorizerControl is not set, so no answer of this target's authorizer carries a limit the suite chose")
		return
	}
	s.quota(t, 1)
	defer s.setRules(t)
	path := "files/" + s.name("quota.txt")
	r := s.await(t, func() response {
		return s.put(t, Alice, path, strings.Repeat("x", 4096), "text/plain")
	}, func(r response) bool { return r.status != http.StatusOK && r.status != http.StatusCreated })
	expectError(t, r, CodeQuotaExceeded)
}

// case010NoLimitNoRefusal: with no limit on the authorizer's answer, a space
// has none, because Arca stores none. A target that refuses a write for size
// with no limit in the answer is storing one.
func case010NoLimitNoRefusal(t *testing.T, s *session) {
	if s.options.AuthorizerControl != "" {
		s.quota(t, -1)
		defer s.setRules(t)
	}
	written := s.put(t, Alice, s.filePath("no-limit.txt"), strings.Repeat("x", 4096), "text/plain")
	failIf(t, written.status != http.StatusOK && written.status != http.StatusCreated,
		"a write was refused with no limit on the answer: %d %s", written.status, written.body)
}
