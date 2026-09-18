// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/api"
)

// The object plane's rows of spec 018's table. What the registry does with a
// label is that package's own tests'; what is proved here is that the number
// moves where this package moves bytes, and only there.

// TestTheBytesOfAPutAndAnInlineReadAreCounted: arca_bytes_in_total{kind=
// "inline"} and arca_bytes_out_total{kind="inline"} are the bytes this
// server carried, so a put counts what it committed and a read counts what
// it streamed.
func TestTheBytesOfAPutAndAnInlineReadAreCounted(t *testing.T) {
	h := newHarness(t)
	in := map[string]string{"kind": "inline"}

	if w := h.put(t, "files/notes/plan.md", "first"); w.Code != http.StatusCreated {
		t.Fatalf("the create answered %d: %s", w.Code, w.Body)
	}
	if got := h.counters.BytesIn.Value(in); got != 5 {
		t.Fatalf("a put of five bytes counted %d in", got)
	}
	// An overwrite is five more bytes accepted. The counter is a rate of
	// arrival and never the size of the space, which is what the ledger
	// holds and what arca_space_usage_bytes samples.
	if w := h.put(t, "files/notes/plan.md", "again"); w.Code != http.StatusOK {
		t.Fatalf("the overwrite answered %d: %s", w.Code, w.Body)
	}
	if got := h.counters.BytesIn.Value(in); got != 10 {
		t.Fatalf("two puts of five bytes counted %d in", got)
	}

	if got := h.counters.BytesOut.Value(in); got != 0 {
		t.Fatalf("a path nobody read counted %d out", got)
	}
	w := h.call(t, http.MethodGet, h.object("files/notes/plan.md"), nil)
	if w.Code != http.StatusOK || w.Body.String() != "again" {
		t.Fatalf("the read answered %d: %s", w.Code, w.Body)
	}
	if got := h.counters.BytesOut.Value(in); got != 5 {
		t.Fatalf("a read of five bytes counted %d out", got)
	}
	// A head answers the size and carries no body, so nothing left.
	if w := h.call(t, http.MethodHead, h.object("files/notes/plan.md"), nil); w.Code != http.StatusOK {
		t.Fatalf("the head answered %d", w.Code)
	}
	if got := h.counters.BytesOut.Value(in); got != 5 {
		t.Fatalf("a head counted bytes out; the counter reads %d", got)
	}
}

// TestAReadAboveTheInlineSizeCountsNoBytesOut: above the boundary the read is
// a redirect and the transfer is between the client and the bucket, which is
// invariant 4 of spec 001. It is counted as a signed URL by the decorator of
// spec 018 and never as bytes this server served.
func TestAReadAboveTheInlineSizeCountsNoBytesOut(t *testing.T) {
	h := newHarness(t)
	// A put above the boundary is refused, so the object arrives the way one
	// that large really arrives: through the session API of spec 007, whose
	// bytes are counted as parts. Here it is written into both stores
	// directly, because what this case reads is the answer and not the write.
	h.write(t, "files/large.md", strings.Repeat("x", inlineBytes+1))
	if got := h.counters.BytesIn.Value(map[string]string{"kind": "inline"}); got != 0 {
		t.Fatalf("a write that took no handler counted %d bytes in", got)
	}
	if w := h.call(t, http.MethodGet, h.object("files/large.md"), nil); w.Code != http.StatusFound {
		t.Fatalf("a read above the boundary answered %d: %s", w.Code, w.Body)
	}
	if got := h.counters.BytesOut.Value(map[string]string{"kind": "inline"}); got != 0 {
		t.Fatalf("a redirect counted %d bytes out", got)
	}
}

// TestAWriteTheLimitRefusedIsCountedOnce: Arca stores no limit, so the count
// of refusals is the only thing about one it can publish, and a write the
// bucket already took but the ledger refused counts no bytes in.
func TestAWriteTheLimitRefusedIsCountedOnce(t *testing.T) {
	h := newHarness(t)
	h.endpoint.Allow(stub.Rule{
		Subject: "*", Action: "*", Resource: "*", Allow: true,
		Limits: map[string]any{"quota_bytes": 4},
	})
	w := h.put(t, "files/plan.md", "five!")
	if got := code(t, w); got != api.CodeQuotaExceeded {
		t.Fatalf("a write past the answer's limit is %q", got)
	}
	if got := h.counters.LimitRejections.Value(nil); got != 1 {
		t.Fatalf("a refused write was counted %d times", got)
	}
	if got := h.counters.BytesIn.Value(map[string]string{"kind": "inline"}); got != 0 {
		t.Fatalf("a refused write counted %d bytes into the space", got)
	}
	// A write the limit admits moves the one counter and not the other.
	h.endpoint.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	if again := h.put(t, "files/plan.md", "five!"); again.Code != http.StatusCreated {
		t.Fatalf("the write answered %d: %s", again.Code, again.Body)
	}
	if got := h.counters.LimitRejections.Value(nil); got != 1 {
		t.Fatalf("an admitted write was counted as a refusal; the counter reads %d", got)
	}
}

// TestASurfaceWithNoRecorderRecordsNothing: the seam has a no-op default, so
// every call site is one line rather than a branch and a unit test that binds
// no registry is unchanged by this spec.
func TestASurfaceWithNoRecorderRecordsNothing(t *testing.T) {
	s := New(Options{DB: newMemory()})
	if s.metrics == nil {
		t.Fatal("a surface built with no recording surface bound none")
	}
	s.metrics.In("inline", 1)
	s.metrics.Out("inline", 1)
	s.metrics.LimitRejected()
}
