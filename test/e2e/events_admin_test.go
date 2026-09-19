// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Criteria 9 and 10 of spec 012 through the binary: the record of what an
// administrator did is the event log of spec 010 and no table of its own,
// the same route serves it to an administrator for any space and to an owner
// for its own, the mark says which touches neither ownership nor a grant
// explains, and nothing in this spec's surface removes a row from it.

// moderator is a subject that owns nothing here and is allowed on somebody
// else's space, which is what makes a call administrative.
const moderator = "moderator"

// logEntry is one row of GET /v1/events as a consumer reads it, written out
// here so the tier is held to the wire and not to the server's own type.
type logEntry struct {
	ID     int64          `json:"id"`
	Action string         `json:"action"`
	Owner  string         `json:"owner"`
	Path   string         `json:"path"`
	Actor  string         `json:"actor"`
	Detail map[string]any `json:"detail"`
}

// tail reads one page of a space's log as one subject.
func (i *installation) tail(t *testing.T, sub, owner string) []logEntry {
	t.Helper()
	code, body, _ := i.sharesCall(t, http.MethodGet,
		"/v1/events?owner="+url.QueryEscape(owner), sub, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/events as %s = %d: %s", sub, code, body)
	}
	var page struct {
		Entries []logEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("the page is not the envelope: %s", body)
	}
	return page.Entries
}

// deleted answers the log's delete row for one path, or fails.
func deleted(t *testing.T, entries []logEntry, path string) logEntry {
	t.Helper()
	for _, e := range entries {
		if e.Action == "delete" && e.Path == path {
			return e
		}
	}
	t.Fatalf("the log holds no delete of %q: %+v", path, entries)
	return logEntry{}
}

func TestE2ETheAdministrativeRecordIsTheEventLog(t *testing.T) {
	i := start(t)
	i.allowEverything()
	db := i.database(t)

	owner := i.subject()
	moderated := "files/reports/q3.pdf"
	own := "files/reports/q4.pdf"
	i.put(t, db, moderated, "the third quarter")
	i.put(t, db, own, "the fourth quarter")

	// A moderation delete is the ordinary route of spec 005 asked on
	// somebody else's space. There is no administrative copy of it, which is
	// why there is one delete implementation.
	code, body, _ := i.sharesCall(t, http.MethodDelete,
		"/v1/files/"+url.PathEscape(owner)+"/"+moderated, moderator, nil)
	if code != http.StatusNoContent {
		t.Fatalf("the moderation delete = %d: %s", code, body)
	}
	// The same route, asked by the space's own owner about its own path.
	i.expect(t, http.StatusNoContent, http.MethodDelete,
		"/v1/files/"+url.PathEscape(owner)+"/"+own, nil, nil)

	// Criterion 10: the administrator reads the space's log through the one
	// route a consumer reads any log through.
	asAdministrator := i.tail(t, moderator, owner)
	marked := deleted(t, asAdministrator, moderated)
	if marked.Owner != owner {
		t.Errorf("the row names the space %q and the space is %q", marked.Owner, owner)
	}
	if marked.Actor != i.sharesSubject(moderator) {
		t.Errorf("the row names the actor %q and the caller was %q",
			marked.Actor, i.sharesSubject(moderator))
	}

	// Criterion 9: the mark is on the touch neither ownership nor a grant
	// explains, and on no other.
	if marked.Detail["admin"] != true {
		t.Errorf("the moderation delete carries the detail %v and no admin mark", marked.Detail)
	}
	if mine := deleted(t, asAdministrator, own); mine.Detail["admin"] != nil {
		t.Errorf("the owner's own delete carries %v", mine.Detail)
	}

	// Criterion 10 again, the other reader: the owner reads its own space
	// through the same route, and sees the same rows.
	asOwner := i.tail(t, "dev", "me")
	if got := deleted(t, asOwner, moderated); got.ID != marked.ID {
		t.Errorf("the owner reads the moderation as row %d and the administrator read row %d",
			got.ID, marked.ID)
	}

	// And nothing removed a row: the two readers see the same log after both
	// of them read it.
	if len(i.tail(t, moderator, owner)) != len(asAdministrator) {
		t.Error("a read of the log changed what the log holds")
	}
}

// TestE2ENoRouteDeletesFromTheLog is the second half of criterion 10, read
// off the document the server serves rather than off a probe: a route that
// is not registered and a route that refuses answer the same way over the
// wire, so the surface is what has to be checked.
func TestE2ENoRouteDeletesFromTheLog(t *testing.T) {
	i := start(t)
	code, body := i.api(t, http.MethodGet, "/openapi.json", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", code, body)
	}
	var document struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if len(document.Paths) == 0 {
		t.Fatal("the document declares no path")
	}
	for path, operations := range document.Paths {
		if _, refuses := operations["delete"]; !refuses {
			continue
		}
		if strings.HasPrefix(path, "/v1/events") || strings.HasPrefix(path, "/v1/admin") {
			t.Errorf("%s declares a delete; the log is a tail nothing in spec 012 removes from", path)
		}
	}
}
