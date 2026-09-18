// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"net/http"
	"testing"
)

// counted is spec 018's seam as a test reads it: the bytes each side of the
// boundary declared, by the vocabulary of that spec's table.
type counted struct {
	in  map[string]int64
	out map[string]int64
}

func newCounted() *counted {
	return &counted{in: map[string]int64{}, out: map[string]int64{}}
}

func (c *counted) In(kind string, n int64)  { c.in[kind] += n }
func (c *counted) Out(kind string, n int64) { c.out[kind] += n }

// TestTheBoundaryCountsTheBytesItDeclared is spec 018's arca_bytes_in_total
// and arca_bytes_out_total for this plane. Neither number is a transfer this
// process saw: a materialize hands out presigned URLs and a sync declares
// what the client already sent to the bucket, which is invariant 4 of spec
// 001.
func TestTheBoundaryCountsTheBytesItDeclared(t *testing.T) {
	c := newCounted()
	h := newHarness(t, func(o *Options) { o.Metrics = c })
	ws := h.create(t, "build")
	h.seed(t, ws, "src/main.go", "4f9a", 12)
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "rw")

	if got := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID+"/materialize?attachment="+a.ID, nil); got.code != http.StatusOK {
		t.Fatalf("the materialize = %d: %s", got.code, got.body)
	}
	if c.out["materialize"] != 20 {
		t.Errorf("the manifest handed out %d bytes, and the subtree holds 20", c.out["materialize"])
	}

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files": []map[string]any{
			{"path": "src/main.go", "checksum": "4f9a", "size": 12},
		},
	})
	if got.code != http.StatusOK {
		t.Fatalf("the sync = %d: %s", got.code, got.body)
	}
	if c.in["sync"] != 12 {
		t.Errorf("the sync declared %d bytes, and the manifest names 12", c.in["sync"])
	}
	if len(c.in) != 1 || len(c.out) != 1 {
		t.Errorf("the plane recorded %v in and %v out, and it owns one kind of each", c.in, c.out)
	}
}

// TestASyncThatWroteNothingCountsNothing: the count is taken after the
// boundary commits, so a manifest the server refused leaves the counter
// where it was and a client that repeats the sync is counted once.
func TestASyncThatWroteNothingCountsNothing(t *testing.T) {
	c := newCounted()
	h := newHarness(t, func(o *Options) { o.Metrics = c })
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files": []map[string]any{
			{"path": "src/never-put.go", "checksum": "dead", "size": 99},
		},
	})
	if got.code != http.StatusConflict {
		t.Fatalf("a manifest naming an object the space does not hold = %d: %s", got.code, got.body)
	}
	if c.in["sync"] != 0 {
		t.Errorf("a refused sync counted %d bytes", c.in["sync"])
	}
}

// TestAServiceWithNoSeamMovesBytesAnyway: the seam is optional, and a node
// that binds none serves every route.
func TestAServiceWithNoSeamMovesBytesAnyway(t *testing.T) {
	h := newHarness(t)
	if _, ok := h.service.metrics.(uncounted); !ok {
		t.Fatalf("the seam defaulted to %T", h.service.metrics)
	}
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")
	if got := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID+"/materialize?attachment="+a.ID, nil); got.code != http.StatusOK {
		t.Fatalf("the materialize = %d: %s", got.code, got.body)
	}
	h.service.metrics.In("sync", 1)
	h.service.metrics.Out("materialize", 1)
}
