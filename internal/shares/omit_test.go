// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
)

// limits.omit of spec 028 on the pages of this package: the grants of a
// space, what is shared with a caller, the links of a space, and the
// listing a link answers. Each leaves out what is at or under an omitted
// prefix for the caller whose answer omits it, and a second caller with no
// omission reads everything.

// omittedPrefix is the subtree the cases leave out.
const omittedPrefix = "files/team/payroll"

// omitFor gives one subject's answers the omission, after the allow-all
// rule the harness starts with, so the later rule wins for that subject.
func omitFor(h *harness, subject string, limits map[string]any) {
	h.endpoint.Allow(stub.Rule{Subject: subject, Action: "*", Resource: "*", Limits: limits})
}

// prefixesOf reads the path_prefix of every entry of a page, sorted.
func prefixesOf(t *testing.T, page api.Page[map[string]any]) []string {
	t.Helper()
	var out []string
	for _, e := range page.Entries {
		out = append(out, e["path_prefix"].(string))
	}
	slices.Sort(out)
	return out
}

// TestTheGrantPagesLeaveOutWhatTheAnswerOmits: GET /v1/shares and GET
// /v1/shares/with-me hold no grant whose prefix is at or under the omitted
// prefix. A grant above it is listed: its own prefix is not inside what the
// answer leaves out.
func TestTheGrantPagesLeaveOutWhatTheAnswerOmits(t *testing.T) {
	h := newHarness(t)
	for _, prefix := range []string{"files/team", omittedPrefix, omittedPrefix + "/2026", "files/team/payroll-notes.md"} {
		if w := h.do(t, http.MethodPost, "/v1/shares", "alice", body(prefix, h.subject("carol"), "read")); w.Code != http.StatusCreated {
			t.Fatalf("granting %s = %d: %s", prefix, w.Code, w.Body)
		}
	}
	space := "/v1/shares?owner=" + url.QueryEscape(h.subject("alice"))
	omitFor(h, h.subject("carol"), map[string]any{"omit": []string{omittedPrefix}})

	kept := []string{"files/team", "files/team/payroll-notes.md"}
	if got := prefixesOf(t, pageOf[map[string]any](t, h.do(t, http.MethodGet, space, "carol", nil))); !slices.Equal(got, kept) {
		t.Errorf("the grants of the space with the prefix omitted are %v, want %v", got, kept)
	}
	if got := prefixesOf(t, pageOf[map[string]any](t, h.do(t, http.MethodGet, "/v1/shares/with-me", "carol", nil))); !slices.Equal(got, kept) {
		t.Errorf("what is shared with the caller is %v, want %v", got, kept)
	}
	if got := prefixesOf(t, pageOf[map[string]any](t, h.do(t, http.MethodGet, space, "dave", nil))); len(got) != 4 {
		t.Errorf("the grants of the space with no omission are %v", got)
	}

	// Limits this server cannot read are no decision: nothing is listed.
	omitFor(h, h.subject("carol"), map[string]any{"omit": []string{"files"}})
	for _, target := range []string{space, "/v1/shares/with-me"} {
		w := h.do(t, http.MethodGet, target, "carol", nil)
		if got := refusalOf(t, w); got != api.CodeStorageUnavailable {
			t.Errorf("%s under an omission that is not a path is %q", target, got)
		}
	}
}

// TestTheLinkPagesLeaveOutWhatTheAnswerOmits: GET /v1/shares/links holds no
// link whose prefix is at or under the omitted prefix, and the listing a
// link answers holds no object under it. The second is asked with no
// subject at all, so the answer that omits is the one to link.read.
func TestTheLinkPagesLeaveOutWhatTheAnswerOmits(t *testing.T) {
	h := newHarness(t)
	everything := mint(t, h, "files/team", "")
	for _, prefix := range []string{omittedPrefix, "files/team/payroll-notes.md"} {
		mint(t, h, prefix, "")
	}
	for _, path := range []string{"files/team/plan.md", omittedPrefix + "/may.pdf", omittedPrefix + "/2026/june.pdf", "files/team/payroll-notes.md"} {
		anObject(h, path)
	}
	links := "/v1/shares/links?owner=" + url.QueryEscape(h.subject("alice"))
	omitFor(h, h.subject("carol"), map[string]any{"omit": []string{omittedPrefix}})

	if got := prefixesOf(t, pageOf[map[string]any](t, h.do(t, http.MethodGet, links, "carol", nil))); !slices.Equal(got, []string{"files/team", "files/team/payroll-notes.md"}) {
		t.Errorf("the links of the space with the prefix omitted are %v", got)
	}
	if got := prefixesOf(t, pageOf[map[string]any](t, h.do(t, http.MethodGet, links, "dave", nil))); len(got) != 3 {
		t.Errorf("the links of the space with no omission are %v", got)
	}

	// The listing a link answers is asked with no subject, so the answer
	// that omits is the one to link.read. Answers are cached for their ttl,
	// so the second reader is a node of its own whose endpoint carries the
	// omission from the first question.
	paths := func(h *harness, token string) []string {
		t.Helper()
		w := h.do(t, http.MethodGet, "/v1/shares/links/"+token, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("the link's listing = %d: %s", w.Code, w.Body)
		}
		var out []string
		for _, e := range pageOf[map[string]any](t, w).Entries {
			out = append(out, e["path"].(string))
		}
		return out
	}
	if got := paths(h, everything.Token); len(got) != 4 {
		t.Fatalf("the link's listing with no omission holds %v", got)
	}
	narrowed := newHarness(t)
	listed := mint(t, narrowed, "files/team", "")
	for _, path := range []string{"files/team/plan.md", omittedPrefix + "/may.pdf", omittedPrefix + "/2026/june.pdf", "files/team/payroll-notes.md"} {
		anObject(narrowed, path)
	}
	narrowed.endpoint.Allow(stub.Rule{Action: "link.read", Limits: map[string]any{"omit": []string{omittedPrefix}}})
	if got, want := paths(narrowed, listed.Token), []string{"files/team/payroll-notes.md", "files/team/plan.md"}; !slices.Equal(got, want) {
		t.Errorf("the link's listing with the prefix omitted holds %v, want %v", got, want)
	}
	h = narrowed
	everything = listed
	// One object under the link is its own question, and is served.
	if w := h.do(t, http.MethodGet, "/v1/shares/links/"+everything.Token+"/files/"+omittedPrefix+"/may.pdf", "", nil); w.Code != http.StatusOK {
		t.Errorf("one object under the omitted prefix = %d: %s", w.Code, w.Body)
	}
}
