// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
)

// DELETE /v1/admin/spaces/{owner} through the surface: the action it asks,
// who it refuses, what it validates before it asks, and what it answers.

// erasePath is the route for one space, with the subject encoded into the
// path the way a client sends it.
func erasePath(owner string) string { return "/v1/admin/spaces/" + url.PathEscape(owner) }

func TestADeletionAsksSpaceDeleteOnThatSpaceAndAnswersTheCounts(t *testing.T) {
	seam := &fakeEraser{counts: Erased{Files: 3, Trashed: 1, Versions: 2, Objects: 4, SharesHeld: 1}}
	h := newHarness(t, withEraser(seam))
	owner := "https://other.example|c1d0"
	h.forget()

	got := h.do(t, http.MethodDelete, erasePath(owner), nil)
	if got.code != http.StatusOK {
		t.Fatalf("the deletion answered %d: %s", got.code, got.body)
	}
	var body struct {
		Owner  string `json:"owner"`
		Status string `json:"status"`
		Counts Erased `json:"counts"`
	}
	got.decode(t, &body)
	if body.Owner != owner || body.Status != "deleted" || body.Counts != seam.counts {
		t.Errorf("the deletion answered %+v", body)
	}
	if seam.owner != owner || seam.calls != 1 {
		t.Errorf("the seam was asked to delete %q %d times", seam.owner, seam.calls)
	}
	asked := h.asked()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionSpaceDelete ||
		asked[0].Resource.Kind != authorizer.KindSpace || asked[0].Resource.String("owner") != owner {
		t.Errorf("the route asked %v, want space.delete on the Space of %q", asked, owner)
	}
}

func TestADeletionIsRefusedForACallerTheAuthorizerDeniesBeforeItActs(t *testing.T) {
	seam := &fakeEraser{}
	h := newHarness(t, withEraser(seam))
	h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")
	got := h.do(t, http.MethodDelete, erasePath("https://other.example|c1d0"), nil)
	if got.code != http.StatusForbidden || got.errorCode(t) != "forbidden" {
		t.Fatalf("a denied deletion answered %d: %s", got.code, got.body)
	}
	if seam.calls != 0 {
		t.Error("a denied deletion deleted")
	}
}

// TestAnAdministratorOfOnlyTheOtherActionDeletesNothing: space.admin is not
// space.delete, so a caller allowed one is refused the other.
func TestAnAdministratorOfOnlyTheOtherActionDeletesNothing(t *testing.T) {
	seam := &fakeEraser{}
	h := newHarness(t, withEraser(seam))
	h.endpoint.Deny(stub.Rule{Subject: "*", Action: authorizer.ActionSpaceDelete, Resource: "*"}, "not a deleter")
	if got := h.do(t, http.MethodGet, "/v1/admin/overview", nil); got.code != http.StatusOK {
		t.Fatalf("the overview answered %d: %s", got.code, got.body)
	}
	if got := h.do(t, http.MethodDelete, erasePath("https://other.example|c1d0"), nil); got.code != http.StatusForbidden {
		t.Fatalf("a caller refused space.delete answered %d: %s", got.code, got.body)
	}
	if seam.calls != 0 {
		t.Error("a caller refused space.delete deleted")
	}
}

func TestAMalformedOwnerIsRefusedBeforeAnythingIsAsked(t *testing.T) {
	for name, owner := range map[string]string{
		"the alias":         "me",
		"no issuer half":    "|9ab3",
		"no sub half":       "https://other.example|",
		"no separator":      "c1d0",
		"too long":          "https://other.example|" + strings.Repeat("a", 600),
		"a control byte":    "https://other.example|c1\x01d0",
		"a newline escaped": "https://other.example|c1\nd0",
	} {
		t.Run(name, func(t *testing.T) {
			seam := &fakeEraser{}
			h := newHarness(t, withEraser(seam))
			h.forget()
			got := h.do(t, http.MethodDelete, erasePath(owner), nil)
			if got.code != http.StatusBadRequest || got.errorCode(t) != "invalid_field" {
				t.Fatalf("%q answered %d: %s", owner, got.code, got.body)
			}
			if fields := got.fields(t); len(fields) != 1 || fields[0] != "owner" {
				t.Errorf("the refusal names the fields %v, want owner", fields)
			}
			if seam.calls != 0 || len(h.asked()) != 0 {
				t.Error("a malformed owner reached the authorizer or the seam")
			}
		})
	}
}

func TestADeletionWithNoSeamBoundIsNotImplemented(t *testing.T) {
	h := newHarness(t)
	got := h.do(t, http.MethodDelete, erasePath("https://other.example|c1d0"), nil)
	if got.code != http.StatusNotImplemented || got.errorCode(t) != "not_implemented" {
		t.Fatalf("an unbound deletion answered %d: %s", got.code, got.body)
	}
}

func TestAFailureOfTheSeamIsAServerErrorThatNamesNoStore(t *testing.T) {
	h := newHarness(t, withEraser(&fakeEraser{err: errFault}))
	got := h.do(t, http.MethodDelete, erasePath("https://other.example|c1d0"), nil)
	if got.code != http.StatusInternalServerError {
		t.Fatalf("a failed deletion answered %d: %s", got.code, got.body)
	}
	if strings.Contains(string(got.body), errFault.Error()) {
		t.Errorf("the answer carries the failure's text: %s", got.body)
	}
}

// TestTheOwnerOfASpaceCannotDeleteItUnderTheOwnerPolicy: the owner policy
// reads ARCA_ADMIN_SUBJECTS for this action as it does for space.admin, and
// the space's own owner is refused unless listed.
func TestTheOwnerOfASpaceCannotDeleteItUnderTheOwnerPolicy(t *testing.T) {
	seam := &fakeEraser{}
	h := newHarness(t, ownerPolicy(), withEraser(seam))
	if got := h.do(t, http.MethodDelete, erasePath(h.subject), nil); got.code != http.StatusForbidden {
		t.Fatalf("an owner deleting their own space answered %d: %s", got.code, got.body)
	}
	h.service.authorizer = ownerPolicyFor(h.subject)
	if got := h.do(t, http.MethodDelete, erasePath("https://other.example|c1d0"), nil); got.code != http.StatusOK {
		t.Fatalf("a listed administrator answered %d: %s", got.code, got.body)
	}
	if seam.calls != 1 {
		t.Errorf("the seam was reached %d times, want once", seam.calls)
	}
}
