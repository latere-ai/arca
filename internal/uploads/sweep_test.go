// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"errors"
	"testing"
	"time"
)

// TestAnExpiredSessionIsSweptWithItsPartsAndItsCharge is pass 4 of spec 010
// bound to the table spec 007 owns: a session nobody completed leaves, its
// multipart is aborted, and the bytes it declared go back to the space.
//
// A session inside its deadline is untouched, which is the half that makes
// the sweep safe to run on every replica every few minutes.
func TestAnExpiredSessionIsSweptWithItsPartsAndItsCharge(t *testing.T) {
	h := newHarness(t)
	stale := h.open(t, "files/video/abandoned.mp4", 20<<20)
	h.upload(t, stale, 1, "the part that was uploaded")
	// The deadline is written as data, so the second session opens an hour
	// later and outlives the first by an hour.
	h.clock = h.clock.Add(time.Hour)
	live := h.open(t, "files/video/in-flight.mp4", 4<<20)
	if held := h.store.usage[h.owner]; held != 24<<20 {
		t.Fatalf("two open sessions leave the space holding %d bytes", held)
	}

	// A dry run counts and changes nothing, which this pass can say
	// honestly: what it would change is a query and not a write.
	past := stale.expiry(t).Add(time.Second)
	found, err := h.service.Sweep(t.Context(), nil, past, true)
	if err != nil {
		t.Fatalf("the dry sweep = %v", err)
	}
	if found != 1 {
		t.Fatalf("the dry sweep found %d sessions", found)
	}
	if len(h.store.sessions) != 2 || h.bucket.Calls("AbortMultipart") != 0 {
		t.Fatalf("the dry sweep changed the stores")
	}

	swept, err := h.service.Sweep(t.Context(), nil, past, false)
	if err != nil {
		t.Fatalf("the sweep = %v", err)
	}
	if swept != 1 {
		t.Fatalf("the sweep closed %d sessions", swept)
	}
	if h.bucket.Calls("AbortMultipart") != 1 {
		t.Errorf("the sweep made %d aborts", h.bucket.Calls("AbortMultipart"))
	}
	if _, ok := h.store.sessions[stale.ID]; ok {
		t.Error("the swept session still has a row")
	}
	if _, ok := h.store.sessions[live.ID]; !ok {
		t.Error("the sweep took a session inside its deadline")
	}
	// The declared bytes were charged when the session opened, and a session
	// that is gone holds none of them.
	if held := h.store.usage[h.owner]; held != 4<<20 {
		t.Errorf("after the sweep the space holds %d bytes", held)
	}
}

// TestASessionTheStoreWillNotDiscardKeepsItsRowForTheNextRun: the row is the
// only durable pointer to an upload's parts, so deleting it before the store
// has taken them strands them for the life of the bucket. The failure is
// reported and the rest of the page is still swept.
func TestASessionTheStoreWillNotDiscardKeepsItsRowForTheNextRun(t *testing.T) {
	h := newHarness(t)
	first := h.open(t, "files/video/one.mp4", 20<<20)
	h.open(t, "files/video/two.mp4", 20<<20)
	h.bucket.FailNth("AbortMultipart", 1, errOutage)

	past := first.expiry(t).Add(time.Second)
	swept, err := h.service.Sweep(t.Context(), nil, past, false)
	if err == nil {
		t.Fatal("the sweep reported a clean run over a store that refused")
	}
	if swept != 1 {
		t.Fatalf("the sweep closed %d of the two sessions", swept)
	}
	if len(h.store.sessions) != 1 {
		t.Fatalf("the sweep left %d rows", len(h.store.sessions))
	}
}

// TestASweepThatCannotReadTheTableReportsIt: a pass that cannot ask has
// found nothing, and saying so is what keeps a store outage from reading
// like a run with no expired sessions.
func TestASweepThatCannotReadTheTableReportsIt(t *testing.T) {
	h := newHarness(t)
	h.store.failExpired = errOutage
	found, err := h.service.Sweep(t.Context(), nil, time.Now(), true)
	if !errors.Is(err, errOutage) || found != 0 {
		t.Fatalf("the sweep = %d, %v", found, err)
	}
}

// expiry reads the deadline a session answered.
func (s Session) expiry(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s.ExpiresAt)
	if err != nil {
		t.Fatalf("the session expires at %q: %v", s.ExpiresAt, err)
	}
	return at
}
