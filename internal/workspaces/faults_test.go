// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/store"
)

// What a handler does when a store, the log, or the caller's own request is
// not what it needs. Two rules run through every case: a fault is never a
// missing workspace, and a refusal writes nothing.

func TestAFaultInsideATransactionWritesNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		fail   string
		drive  func(*harness, Workspace) answer
		intact func(*testing.T, *harness, Workspace)
	}{
		{
			name: "a rename whose subtree will not move",
			fail: "MoveSubtree",
			drive: func(h *harness, ws Workspace) answer {
				return h.do(t0(h), http.MethodPatch, "/v1/workspaces/"+ws.ID, map[string]any{"slug": "release"})
			},
			intact: func(t *testing.T, h *harness, ws Workspace) {
				h.store.mu.Lock()
				defer h.store.mu.Unlock()
				if got := h.store.workspaces[ws.ID].Slug; got != "build" {
					t.Errorf("the workspace is called %q after a failed rename", got)
				}
			},
		},
		{
			name: "an attach whose manifest cannot be read",
			fail: "Manifest",
			drive: func(h *harness, ws Workspace) answer {
				return h.do(t0(h), http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
					map[string]any{"sandbox_id": "sbx_a", "mode": "rw"})
			},
			intact: func(t *testing.T, h *harness, ws Workspace) {
				h.store.mu.Lock()
				defer h.store.mu.Unlock()
				if h.store.workspaces[ws.ID].WriterHolder != nil {
					t.Error("a failed attach left the lease taken")
				}
				if len(h.store.attachments) != 0 {
					t.Errorf("a failed attach left %d attachments", len(h.store.attachments))
				}
			},
		},
		{
			name: "an attach the log will not record",
			fail: "",
			drive: func(h *harness, ws Workspace) answer {
				h.ledger.fail = errors.New("the log is unavailable")
				return h.do(t0(h), http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
					map[string]any{"sandbox_id": "sbx_a", "mode": "rw"})
			},
			intact: func(t *testing.T, h *harness, ws Workspace) {
				h.store.mu.Lock()
				defer h.store.mu.Unlock()
				// The lease and the row recording it are written together or
				// not at all: a log that refuses is not an attach that half
				// happened.
				if h.store.workspaces[ws.ID].WriterHolder != nil {
					t.Error("an unrecorded attach left the lease taken")
				}
			},
		},
		{
			name: "a restore the log will not record",
			fail: "",
			drive: func(h *harness, ws Workspace) answer {
				h.do(t0(h), http.MethodDelete, "/v1/workspaces/"+ws.ID, nil)
				h.ledger.fail = errors.New("the log is unavailable")
				return h.do(t0(h), http.MethodPost, "/v1/workspaces/"+ws.ID+"/restore", nil)
			},
			intact: func(t *testing.T, h *harness, ws Workspace) {
				h.store.mu.Lock()
				defer h.store.mu.Unlock()
				if h.store.workspaces[ws.ID].DeletedAt == nil {
					t.Error("an unrecorded restore brought the workspace back")
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.t = t
			ws := h.create(t, "build")
			if c.fail != "" {
				h.store.fail = map[string]error{c.fail: errors.New("the connection failed")}
			}
			got := c.drive(h, ws)
			if got.code < 500 {
				t.Fatalf("a fault answered %d: %s", got.code, got.body)
			}
			// Nothing above 499 names a store, a query, or a key.
			for _, leak := range []string{"connection failed", "the log is unavailable", "workspace_attachments"} {
				if strings.Contains(string(got.body), leak) {
					t.Errorf("the 5xx names %q: %s", leak, got.body)
				}
			}
			c.intact(t, h, ws)
		})
	}
}

// t0 answers the test the harness was built with, so a table's drive
// function reaches one without carrying it twice.
func t0(h *harness) *testing.T { return h.t }

func TestALeaseFaultOnTheWayOutIsNeverAMissingAttachment(t *testing.T) {
	h := newHarness(t)
	h.t = t
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")

	for _, c := range []struct {
		fail         string
		method, path string
	}{
		{"GetAttachment", http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach/" + a.ID + "/renew"},
		{"SetExpiry", http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach/" + a.ID + "/renew"},
		{"RenewLease", http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach/" + a.ID + "/renew"},
		{"GetAttachment", http.MethodDelete, "/v1/workspaces/" + ws.ID + "/attach/" + a.ID},
		{"Release", http.MethodDelete, "/v1/workspaces/" + ws.ID + "/attach/" + a.ID},
		{"ReleaseLease", http.MethodDelete, "/v1/workspaces/" + ws.ID + "/attach/" + a.ID},
		{"TakeLease", http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach"},
		{"Insert", http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach"},
	} {
		h.store.fail = map[string]error{c.fail: errors.New("the connection failed")}
		var body any
		if strings.HasSuffix(c.path, "/attach") {
			body = map[string]any{"sandbox_id": "sbx_b", "mode": "rw"}
		}
		got := h.do(t, c.method, c.path, body)
		if got.code == http.StatusNotFound || got.code == http.StatusGone {
			t.Errorf("%s while %s fails answered %d", c.method, c.fail, got.code)
		}
	}
}

func TestARequestThatNamesNoSpaceAndCarriesNoSubjectIsRefused(t *testing.T) {
	// The alias resolves to the caller's own subject, so a request with none
	// names no space at all. Nothing under /v1 but the three link routes of
	// spec 013 reaches a handler without a subject, so this is the guard
	// rather than a path a caller drives.
	r := httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil)
	if _, err := space(r, ""); err == nil {
		t.Fatal("a request with no space and no subject named a space")
	}
	if _, err := space(r, OwnerAlias); err == nil {
		t.Fatal("the alias resolved with no subject behind it")
	}
	given, err := space(r, "https://issuer.example|9ab3")
	if err != nil || given != "https://issuer.example|9ab3" {
		t.Fatalf("a named space = %q, %v", given, err)
	}
}

func TestACursorFromNowhereIsABadRequestAndNotAFault(t *testing.T) {
	h := newHarness(t)
	h.store.fail = map[string]error{"List": store.ErrBadCursor}
	got := h.do(t, http.MethodGet, "/v1/workspaces?cursor=not-a-cursor", nil)
	if code := got.errorCode(t); got.code != http.StatusBadRequest || code != "invalid_field" {
		t.Fatalf("a cursor from nowhere = %d %q", got.code, code)
	}
}

func TestAHolderThatCannotBeReadStillRefusesTheSecondWriter(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.attach(t, ws, "sbx_a", "rw")
	// The conflict is already decided by the row count; naming the holder is
	// a courtesy, so a read that fails says something rather than turning a
	// conflict into a fault.
	if got := holderOf(t.Context(), h.service, h.store.Querier(), "ws-9999"); got != "another writer" {
		t.Errorf("an unreadable holder reads as %q", got)
	}
	if got := holderOf(t.Context(), h.service, h.store.Querier(), ws.ID); got != "sbx_a" {
		t.Errorf("the holder reads as %q", got)
	}
}

func TestAnAttachmentReadThatFailsIsAFaultAndNotAMissingOne(t *testing.T) {
	broken := errors.New("the connection failed")
	if err := attachmentGone(broken, "att-1"); errors.Is(err, broken) != true {
		t.Errorf("a connection failure was rendered as %v", err)
	}
}

func TestAPathUnderNoRootRendersAsItself(t *testing.T) {
	// A row the prefix query answered is always under the root, so this is
	// the guard rather than a case a caller reaches.
	if got := relative("workspaces/build/", "files/elsewhere"); got != "files/elsewhere" {
		t.Errorf("a path outside the root rendered as %q", got)
	}
	if got := relative("workspaces/build/", "workspaces/build/src/main.go"); got != "src/main.go" {
		t.Errorf("a path under the root rendered as %q", got)
	}
}

func TestTheLeaseIsRenderedOnlyWhileItIsHeld(t *testing.T) {
	h := newHarness(t)
	holder := "sbx_a"
	soon := h.at().Add(time.Minute)
	lapsed := h.at().Add(-time.Minute)
	for _, c := range []struct {
		name string
		row  store.Workspace
		want bool
	}{
		{"free", store.Workspace{}, false},
		{"held", store.Workspace{WriterHolder: &holder, WriterExpiresAt: &soon}, true},
		{"lapsed", store.Workspace{WriterHolder: &holder, WriterExpiresAt: &lapsed}, false},
		{"a holder with no deadline", store.Workspace{WriterHolder: &holder}, false},
	} {
		if got := h.service.lease(c.row) != nil; got != c.want {
			t.Errorf("a %s lease renders as %v", c.name, got)
		}
	}
	// The sync boundary and the soft delete render in UTC when they are
	// there and as null when they are not.
	at := time.Now()
	v := h.service.view(store.Workspace{LastSync: &at, DeletedAt: &at})
	if v.LastSync == nil || v.DeletedAt == nil || v.LastSync.Location() != time.UTC {
		t.Errorf("the view is %+v", v)
	}
}
