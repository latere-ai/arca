// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
)

// The session API's rows of spec 018's table. A session is counted once by
// what became of it, so the four outcomes partition the sessions this surface
// ever opened.

// outcomes reads the four values of arca_upload_sessions_total.
func (h *harness) outcomes() map[string]uint64 {
	out := map[string]uint64{}
	for _, o := range []string{sessionCreated, sessionCompleted, sessionAborted, sessionExpired} {
		out[o] = h.recorder.UploadSessions.Value(map[string]string{"outcome": o})
	}
	return out
}

// TestASessionIsCountedOnceByWhatBecameOfIt: created at the open, completed
// at the assembly, and the bytes of the object as parts in. The part counter
// moves with the URLs signed and the parts assembled.
func TestASessionIsCountedOnceByWhatBecameOfIt(t *testing.T) {
	h := newHarness(t)
	content := strings.Repeat("p", 4096)
	session := h.open(t, "files/video/keynote.mp4", int64(len(content)))
	if got := h.outcomes(); got[sessionCreated] != 1 || got[sessionCompleted] != 0 {
		t.Fatalf("an open session counted %v", got)
	}
	// One part per presigned URL the session answered.
	if got := h.recorder.UploadParts.Value(map[string]string{"outcome": partPresigned}); got != uint64(session.PartCount) {
		t.Fatalf("%d URLs were signed and %d parts counted", session.PartCount, got)
	}

	etag := h.upload(t, session, 1, content)
	if w := h.finish(t, session, []string{etag}); w.Code != http.StatusCreated {
		t.Fatalf("the completion answered %d: %s", w.Code, w.Body)
	}
	got := h.outcomes()
	if got[sessionCreated] != 1 || got[sessionCompleted] != 1 {
		t.Fatalf("a completed session counted %v", got)
	}
	if n := h.recorder.UploadParts.Value(map[string]string{"outcome": partCompleted}); n != 1 {
		t.Fatalf("one part assembled counted %d", n)
	}
	// The bytes are the head's and not the declaration's, and they are
	// counted as parts: no byte of this upload passed through the server.
	if n := h.recorder.BytesIn.Value(map[string]string{"kind": "part"}); n != uint64(len(content)) {
		t.Fatalf("an object of %d bytes counted %d in as parts", len(content), n)
	}
	if n := h.recorder.BytesIn.Value(map[string]string{"kind": "inline"}); n != 0 {
		t.Fatalf("a multipart upload counted %d bytes in as inline", n)
	}
}

// TestASessionTheCallerAbortsAndOneTheReaperSweepsAreDifferentOutcomes: both
// end the same way in the two stores, and an operator reading the series
// tells a client that changed its mind from one that went away.
func TestASessionTheCallerAbortsAndOneTheReaperSweepsAreDifferentOutcomes(t *testing.T) {
	h := newHarness(t)
	aborted := h.open(t, "files/video/one.mp4", 4096)
	if w := h.call(t, http.MethodDelete, "/v1/uploads/"+aborted.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("the abort answered %d: %s", w.Code, w.Body)
	}
	if got := h.outcomes(); got[sessionAborted] != 1 || got[sessionExpired] != 0 {
		t.Fatalf("an aborted session counted %v", got)
	}

	swept := h.open(t, "files/video/two.mp4", 4096)
	held := h.sessions(t, swept.ID)
	// A dry run counts and changes nothing, so it records nothing either:
	// the series is what an installation did and not what it would do.
	if n, err := h.service.Sweep(t.Context(), nil, held.ExpiresAt.Add(time.Hour), true); err != nil || n != 1 {
		t.Fatalf("the dry run swept %d, %v", n, err)
	}
	if got := h.outcomes(); got[sessionExpired] != 0 {
		t.Fatalf("a dry run counted %v", got)
	}
	if n, err := h.service.Sweep(t.Context(), nil, held.ExpiresAt.Add(time.Hour), false); err != nil || n != 1 {
		t.Fatalf("the sweep closed %d, %v", n, err)
	}
	got := h.outcomes()
	if got[sessionExpired] != 1 || got[sessionAborted] != 1 || got[sessionCreated] != 2 {
		t.Fatalf("a swept session counted %v", got)
	}
}

// TestACompletionTheStoreCouldNotAssembleCountsAMissingPart: the store does
// not say which part it could not find, so the completion is one missing part
// and not a number this package would be inventing.
func TestACompletionTheStoreCouldNotAssembleCountsAMissingPart(t *testing.T) {
	h := newHarness(t)
	session := h.open(t, "files/video/keynote.mp4", 4096)
	first := h.upload(t, session, 1, strings.Repeat("x", 4096))
	if w := h.finish(t, session, []string{first + "-wrong"}); w.Code != http.StatusBadRequest {
		t.Fatalf("a completion the store would not assemble answered %d: %s", w.Code, w.Body)
	}
	if n := h.recorder.UploadParts.Value(map[string]string{"outcome": partMissing}); n != 1 {
		t.Fatalf("a completion that did not assemble counted %d missing parts", n)
	}
	if got := h.outcomes(); got[sessionCompleted] != 0 {
		t.Fatalf("a completion that did not assemble counted %v", got)
	}
}

// TestASessionThePlanRefusesIsCountedAsARejection: the declared bytes are
// charged when the session opens, so a space with no room left is refused
// there and counted once.
func TestASessionThePlanRefusesIsCountedAsARejection(t *testing.T) {
	h := newHarness(t)
	h.endpoint.SetRules(stub.Rule{
		Subject: "*", Action: "*", Resource: "*", Allow: true,
		Limits: map[string]any{"quota_bytes": 1024},
	})
	body := `{"owner":"me","path":"files/video/keynote.mp4","size":4096}`
	w := h.call(t, http.MethodPost, "/v1/uploads", strings.NewReader(body))
	if got := code(t, w); got != api.CodeQuotaExceeded {
		t.Fatalf("a session past the answer's limit is %q", got)
	}
	if n := h.recorder.LimitRejections.Value(nil); n != 1 {
		t.Fatalf("a refused session was counted %d times", n)
	}
	if got := h.outcomes(); got[sessionCreated] != 0 {
		t.Fatalf("a refused session counted %v", got)
	}
}

// TestTheOpenSessionsAreTheDatabasesCount: arca_upload_sessions_open reads a
// number no replica keeps, because a session opened on one replica is open on
// all of them.
func TestTheOpenSessionsAreTheDatabasesCount(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	if n, err := h.service.Open(t.Context(), now); err != nil || n != 0 {
		t.Fatalf("Open with nothing open = %d, %v", n, err)
	}
	session := h.open(t, "files/video/keynote.mp4", 4096)
	if n, err := h.service.Open(t.Context(), now); err != nil || n != 1 {
		t.Fatalf("Open with one session = %d, %v", n, err)
	}
	// Past its deadline the same row is the reaper's and not an open session,
	// which is what makes the gauge and pass 4 read one partition.
	held := h.sessions(t, session.ID)
	if n, err := h.service.Open(t.Context(), held.ExpiresAt.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("Open past the deadline = %d, %v", n, err)
	}
	if n, err := h.service.Open(t.Context(), now); err != nil || n != 1 {
		t.Fatalf("Open = %d, %v", n, err)
	}
}
