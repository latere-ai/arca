// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"testing"
	"time"

	"latere.ai/x/arca/internal/store"
)

// GET /v1/admin/overview through the surface: the seven counters, the
// envelope, the page, and what a failure answers.

// aMoment is the fixed clock the rows below carry, so a rendered timestamp
// is compared against a value and not against the wall.
var aMoment = time.Date(2026, 9, 17, 8, 41, 2, 0, time.UTC)

// spaces is the page the query set answers.
func spaces(owners ...string) []store.SpaceOverview {
	out := make([]store.SpaceOverview, 0, len(owners))
	for _, owner := range owners {
		out = append(out, store.SpaceOverview{Owner: owner, Files: 1, Bytes: 10})
	}
	return out
}

func TestTheOverviewAnswersSevenCountersPerSpace(t *testing.T) {
	h := newHarness(t)
	h.spaces.page = []store.SpaceOverview{{
		Owner: "https://issuer.example|9ab3", Files: 214, Bytes: 88213004,
		TrashedBytes: 402118, Workspaces: 2, Leases: 1, LastWriteAt: &aMoment,
	}}
	h.links.counts = map[string]int64{"https://issuer.example|9ab3": 3}

	got := h.do(t, http.MethodGet, "/v1/admin/overview", nil)
	if got.code != http.StatusOK {
		t.Fatalf("the overview answered %d: %s", got.code, got.body)
	}
	var p page
	got.decode(t, &p)
	if len(p.Entries) != 1 {
		t.Fatalf("the page holds %d rows, want 1", len(p.Entries))
	}
	row := p.Entries[0]
	want := Space{
		Owner: "https://issuer.example|9ab3", Files: 214, Bytes: 88213004,
		TrashedBytes: 402118, Workspaces: 2, Leases: 1, Links: 3, LastWriteAt: &aMoment,
	}
	if row.Owner != want.Owner || row.Files != want.Files || row.Bytes != want.Bytes {
		t.Errorf("the row is %+v, want %+v", row, want)
	}
	if row.TrashedBytes != want.TrashedBytes || row.Workspaces != want.Workspaces ||
		row.Leases != want.Leases || row.Links != want.Links {
		t.Errorf("the row's counters are %+v, want %+v", row, want)
	}
	if row.LastWriteAt == nil || !row.LastWriteAt.Equal(aMoment) {
		t.Errorf("the row's last write is %v, want %v", row.LastWriteAt, aMoment)
	}
	if p.NextCursor != "" {
		t.Errorf("a page with nothing after it answered the cursor %q", p.NextCursor)
	}
	if len(h.links.asked) != 1 || h.links.asked[0] != want.Owner {
		t.Errorf("the link count was asked about %v, want the page's owners", h.links.asked)
	}
}

// TestTheOverviewCountsNoLinkWhereNoneAreBound is the seam's own rule: a
// build that has issued no link counts none, which is the true count, and
// the row still carries the field.
func TestTheOverviewCountsNoLinkWhereNoneAreBound(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Links = nil })
	h.spaces.page = spaces("https://issuer.example|9ab3")

	var p page
	h.do(t, http.MethodGet, "/v1/admin/overview", nil).decode(t, &p)
	if len(p.Entries) != 1 || p.Entries[0].Links != 0 {
		t.Fatalf("an installation with no link table answered %+v", p.Entries)
	}
}

func TestTheOverviewPagesByTheSubject(t *testing.T) {
	h := newHarness(t)
	h.spaces.page = spaces(
		"https://issuer.example|a", "https://issuer.example|b", "https://issuer.example|c")

	var first page
	h.do(t, http.MethodGet, "/v1/admin/overview?limit=2", nil).decode(t, &first)
	if len(first.Entries) != 2 {
		t.Fatalf("the first page holds %d rows, want 2", len(first.Entries))
	}
	if first.NextCursor != "https://issuer.example|b" {
		t.Fatalf("the first page's cursor is %q", first.NextCursor)
	}
	// One row more than the page is read, so the presence of a further page
	// costs no second count.
	if h.spaces.limit != 3 {
		t.Errorf("the query asked for %d rows for a page of 2", h.spaces.limit)
	}

	var second page
	h.do(t, http.MethodGet, "/v1/admin/overview?limit=2&cursor="+first.NextCursor, nil).decode(t, &second)
	if len(second.Entries) != 1 || second.Entries[0].Owner != "https://issuer.example|c" {
		t.Fatalf("the second page holds %+v", second.Entries)
	}
	if second.NextCursor != "" {
		t.Errorf("the last page answered the cursor %q", second.NextCursor)
	}
}

func TestTheOverviewAnswersAnEmptyPageAsAnArray(t *testing.T) {
	h := newHarness(t)
	got := h.do(t, http.MethodGet, "/v1/admin/overview", nil)
	if got.code != http.StatusOK {
		t.Fatalf("the overview answered %d: %s", got.code, got.body)
	}
	if want := `{"entries":[]}`; string(got.body) != want+"\n" && string(got.body) != want {
		t.Errorf("an empty page is %s, want %s", got.body, want)
	}
}

func TestTheOverviewRefusesALimitOutsideTheBound(t *testing.T) {
	h := newHarness(t)
	for _, limit := range []string{"0", "1001", "half"} {
		got := h.do(t, http.MethodGet, "/v1/admin/overview?limit="+limit, nil)
		if got.code != http.StatusBadRequest {
			t.Errorf("limit=%s answered %d", limit, got.code)
		}
		if code := got.errorCode(t); code != "invalid_field" {
			t.Errorf("limit=%s answered %q", limit, code)
		}
		if fields := got.fields(t); len(fields) != 1 || fields[0] != "limit" {
			t.Errorf("limit=%s named the fields %v", limit, fields)
		}
	}
	if h.spaces.calls != 0 {
		t.Error("a refused page reached the database")
	}
}

func TestTheOverviewReportsAQueryItCouldNotRun(t *testing.T) {
	h := newHarness(t)
	h.spaces.err = errFault
	got := h.do(t, http.MethodGet, "/v1/admin/overview", nil)
	if got.code != http.StatusInternalServerError {
		t.Fatalf("a failed query answered %d: %s", got.code, got.body)
	}
	if code := got.errorCode(t); code != "internal" {
		t.Errorf("a failed query answered %q", code)
	}
}

func TestTheOverviewReportsALinkCountItCouldNotRun(t *testing.T) {
	h := newHarness(t)
	h.spaces.page = spaces("https://issuer.example|9ab3")
	h.links.err = errFault
	got := h.do(t, http.MethodGet, "/v1/admin/overview", nil)
	if got.code != http.StatusInternalServerError {
		t.Fatalf("a failed link count answered %d: %s", got.code, got.body)
	}
}

// TestTheOverviewRendersATimeInUTC: every timestamp of spec 013 is RFC 3339
// in UTC, whatever zone the database handed back.
func TestTheOverviewRendersATimeInUTC(t *testing.T) {
	h := newHarness(t)
	elsewhere := aMoment.In(time.FixedZone("elsewhere", 5*60*60))
	h.spaces.page = []store.SpaceOverview{{Owner: "https://issuer.example|9ab3", Files: 1, LastWriteAt: &elsewhere}}

	var p page
	h.do(t, http.MethodGet, "/v1/admin/overview", nil).decode(t, &p)
	if len(p.Entries) != 1 || p.Entries[0].LastWriteAt.Location() != time.UTC {
		t.Fatalf("the row's last write is %v", p.Entries[0].LastWriteAt)
	}
}
