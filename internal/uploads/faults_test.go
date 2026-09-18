// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
)

// TestASessionRowThatCannotBeReadIsAnOutageAndNeverAMissingSession: the
// database decides whether a session exists, so a database that answers
// nothing decides nothing.
func TestASessionRowThatCannotBeReadIsAnOutageAndNeverAMissingSession(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 1024)
	h.store.failGet = errOutage
	if got := code(t, h.call(t, http.MethodDelete, "/v1/uploads/"+session.ID, nil)); got != api.CodeStorageUnavailable {
		t.Errorf("an abort against a database that answers nothing is %q", got)
	}
	completing := h.call(t, http.MethodPost, "/v1/uploads/"+session.ID+"/complete",
		strings.NewReader(`{"parts":[{"n":1,"etag":"x"}]}`))
	if got := code(t, completing); got != api.CodeStorageUnavailable {
		t.Errorf("a completion against a database that answers nothing is %q", got)
	}
}

// TestThePathBehindASessionThatCannotBeReadIsAnOutageToo: the precondition
// is read against the row before the parts are assembled, so a database that
// will not answer takes the completion down before the store is asked.
func TestThePathBehindASessionThatCannotBeReadIsAnOutageToo(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 1024)
	first := h.upload(t, session, 1, "x")
	h.store.failPath = errOutage
	before := h.bucket.Calls(blob.MethodCompleteMultipart)
	if got := code(t, h.finish(t, session, []string{first})); got != api.CodeStorageUnavailable {
		t.Fatalf("a completion whose path could not be read is %q", got)
	}
	if h.bucket.Calls(blob.MethodCompleteMultipart) != before {
		t.Error("a completion whose path could not be read assembled its parts anyway")
	}
}

// TestWhatCannotBeGivenBackIsLeftForTheReaper: an abort the store refuses,
// and an assembled object the bucket will not drop, both leave the reaper
// something to collect and neither changes what the caller is told.
func TestWhatCannotBeGivenBackIsLeftForTheReaper(t *testing.T) {
	h := newHarness(t)
	taken := h.open(t, "files/plan.md", 1024)
	part := h.upload(t, taken, 1, "first")
	if w := h.finish(t, taken, []string{part}); w.Code != http.StatusCreated {
		t.Fatalf("the first completion answered %d: %s", w.Code, w.Body)
	}

	// A create-only completion onto a live path is refused, and the store
	// will take back neither the parts nor the object.
	doomed := h.open(t, "files/plan.md", 1024)
	part = h.upload(t, doomed, 1, "second")
	h.bucket.FailNth(blob.MethodAbortMultipart, 1, errOutage)
	w := h.finish(t, doomed, []string{part}, api.HeaderIfNoneMatch, "*")
	if got := code(t, w); got != api.CodePreconditionFailed {
		t.Fatalf("a create-only completion onto a live path is %q", got)
	}
	if len(h.store.sessions) != 1 {
		t.Error("a session the store would not discard was dropped anyway")
	}
	// The refusal names what the path holds, which is what a caller needs to
	// retry.
	if !strings.Contains(w.Body.String(), "the path holds") {
		t.Errorf("the refusal reads %s", w.Body)
	}
}

// TestASessionThatCouldNotBeOpenedLeavesNoMultipartAndNoCharge.
func TestASessionThatCouldNotBeOpenedLeavesNoMultipartAndNoCharge(t *testing.T) {
	h := newHarness(t)
	h.store.failWrite = errOutage
	h.bucket.FailNth(blob.MethodAbortMultipart, 1, errOutage)
	body := `{"owner":"me","path":"files/a.mp4","size":1024}`
	w := h.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))
	if got := code(t, w); got != api.CodeStorageUnavailable {
		t.Fatalf("a session whose row could not be written is %q", got)
	}
	if held := h.store.usage[h.owner]; held != 0 {
		t.Errorf("a session that never opened left the space holding %d bytes", held)
	}
}

// TestAnAssembledObjectNothingCanRemoveIsLeftForTheReaper.
func TestAnAssembledObjectNothingCanRemoveIsLeftForTheReaper(t *testing.T) {
	h := newHarness(t)
	first := h.open(t, "files/plan.md", 1024)
	part := h.upload(t, first, 1, "first")
	if w := h.finish(t, first, []string{part}); w.Code != http.StatusCreated {
		t.Fatalf("the first completion answered %d: %s", w.Code, w.Body)
	}
	doomed := h.open(t, "files/plan.md", 1024)
	part = h.upload(t, doomed, 1, "second")
	h.bucket.FailNth(blob.MethodDelete, 1, errOutage)
	w := h.finish(t, doomed, []string{part}, api.HeaderIfMatch, `"nothing"`)
	if got := code(t, w); got != api.CodePreconditionFailed {
		t.Fatalf("a stale If-Match at completion is %q", got)
	}
}
