// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/store"
)

func TestTwoWritersOnOneWorkspaceLeaveOneLease(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	first := h.attach(t, ws, "sbx_a", "rw")

	second := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_b", "mode": "rw"})
	if code := second.errorCode(t); second.code != http.StatusConflict || code != "writer_held" {
		t.Fatalf("the second writer = %d %q: %s", second.code, code, second.body)
	}
	// The holder is named in the developer detail, so a runtime fails a
	// sandbox create with something a person can act on.
	var envelope struct {
		Error struct {
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	second.decode(t, &envelope)
	if !slices.Contains([]string{"the workspace is held by sbx_a"}, envelope.Error.Details.Detail) {
		t.Errorf("the refusal reads %q", envelope.Error.Details.Detail)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease == nil || after.Lease.Holder != "sbx_a" || after.Lease.Mode != "rw" {
		t.Fatalf("the lease is %+v", after.Lease)
	}
	if first.Mode != "rw" || first.WorkspaceID != ws.ID {
		t.Fatalf("the first attachment is %+v", first)
	}
}

func TestManyReadersAttachAtOnceAndNoneTouchesTheLease(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	for _, sandbox := range []string{"sbx_a", "sbx_b", "sbx_c"} {
		a := h.attach(t, ws, sandbox, "ro")
		if a.Mode != "ro" {
			t.Fatalf("a read-only attach is %+v", a)
		}
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease != nil {
		t.Fatalf("a read-only attach took the lease: %+v", after.Lease)
	}
	// A writer still reaches a workspace every reader is holding.
	h.attach(t, ws, "sbx_w", "rw")
}

func TestTheModeOfAnAttachPicksTheActionItAsks(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	for _, c := range []struct{ mode, action string }{
		{"ro", "workspace.read"},
		{"rw", "workspace.attach"},
	} {
		h.forget()
		h.attach(t, ws, "sbx_"+c.mode, c.mode)
		if got := h.asked(); !slices.Equal(got, []string{c.action}) {
			t.Errorf("a %s attach asked %v, want %q", c.mode, got, c.action)
		}
	}
	// The ladder of spec 008 falls out with no second rule: a caller the
	// authorizer allows workspace.read and refuses workspace.attach mounts
	// read-only and cannot take the lease. The workspace is a fresh one, so
	// the refusal is the lease and not a writer already holding it.
	grantee := newHarness(t)
	shared := grantee.create(t, "shared")
	// A later rule wins over an earlier one, so the narrow refusal goes last.
	grantee.endpoint.SetRules()
	grantee.endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	grantee.endpoint.Deny(stub.Rule{Subject: "*", Action: "workspace.attach", Resource: "*"}, "read only")
	reader := grantee.do(t, http.MethodPost, "/v1/workspaces/"+shared.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_r", "mode": "ro"})
	if reader.code != http.StatusCreated {
		t.Fatalf("a read grantee could not mount read-only: %d %s", reader.code, reader.body)
	}
	writer := grantee.do(t, http.MethodPost, "/v1/workspaces/"+shared.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_x", "mode": "rw"})
	if writer.code != http.StatusNotFound {
		t.Fatalf("a read grantee took the lease: %d %s", writer.code, writer.body)
	}
}

func TestAnAttachRefusesABodyItCannotAct(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	for _, c := range []struct {
		body any
		code string
	}{
		{map[string]any{"mode": "rw"}, "missing_field"},
		{map[string]any{"sandbox_id": "sbx", "mode": "rwx"}, "invalid_field"},
		{map[string]any{"sandbox_id": "sbx", "mode": ""}, "invalid_field"},
		{map[string]any{"sandbox_id": "sbx", "mode": "rw", "ttl_seconds": -1}, "invalid_field"},
	} {
		got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach", c.body)
		if code := got.errorCode(t); code != c.code {
			t.Errorf("%v = %d %q, want %s", c.body, got.code, code, c.code)
		}
	}
	// A refused attach asks nothing and takes no lease.
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease != nil {
		t.Fatalf("a refused attach took the lease: %+v", after.Lease)
	}
}

func TestTheLeaseIsBoundedByTheCeilingAndDefaultsToAnHour(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	// The clock is fixed first, so the deadlines below are read against the
	// instant the attach was measured on and not against a wall clock that
	// moved between the two reads.
	h.travel(0)
	start := h.at()

	byDefault := h.attach(t, ws, "sbx_a", "ro")
	if got := byDefault.ExpiresAt.Sub(start); got != DefaultTTL {
		t.Errorf("the default lease lasts %v, want %v", got, DefaultTTL)
	}
	// A client asks in seconds and gets the smaller of its ask and the
	// ceiling, so no client can ask for a lease long enough to wedge a
	// workspace for a week.
	asked := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_b", "mode": "ro", "ttl_seconds": 7 * 24 * 3600})
	var capped Attachment
	asked.decode(t, &capped)
	if got := capped.ExpiresAt.Sub(start); got != MaxTTL {
		t.Errorf("an ask above the ceiling lasts %v, want %v", got, MaxTTL)
	}
	short := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_c", "mode": "ro", "ttl_seconds": 60})
	var brief Attachment
	short.decode(t, &brief)
	if got := brief.ExpiresAt.Sub(start); got != time.Minute {
		t.Errorf("a minute's ask lasts %v", got)
	}
}

func TestAnAttachPinsTheManifestItWasHandedAndAppendsTheLog(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.store.put(h.subject, "workspaces/build/src/main.go", "4f9a", 2814)
	h.store.put(h.subject, "workspaces/build/bin/app", "11c0", 5120000)
	h.store.put(h.subject, "files/elsewhere.md", "aaaa", 1)

	a := h.attach(t, ws, "sbx_a", "rw")
	// The paths are relative to the root: what a sandbox writes on its own
	// disk is the subtree and not the plane.
	want := []Entry{
		{Path: "bin/app", Checksum: "11c0", Size: 5120000},
		{Path: "src/main.go", Checksum: "4f9a", Size: 2814},
	}
	if !slices.Equal(a.Manifest, want) {
		t.Fatalf("the pinned manifest is %+v", a.Manifest)
	}
	if got := h.ledger.actions(); !slices.Contains(got, ActionAttach) {
		t.Errorf("the attach appended %v", got)
	}
	h.ledger.mu.Lock()
	detail := h.ledger.events[len(h.ledger.events)-1].Detail
	h.ledger.mu.Unlock()
	if detail["mode"] != "rw" || detail["sandbox_id"] != "sbx_a" || detail["attachment_id"] != a.ID {
		t.Errorf("the attach row carries %v", detail)
	}
}

func TestARenewExtendsTheDeadlineAndMovesTheLeaseWithIt(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")

	h.travel(30 * time.Minute)
	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID+"/renew",
		map[string]any{"ttl_seconds": 3600})
	if got.code != http.StatusOK {
		t.Fatalf("the renew = %d: %s", got.code, got.body)
	}
	var renewed Renewal
	got.decode(t, &renewed)
	if renewed.ID != a.ID || !renewed.ExpiresAt.After(a.ExpiresAt) {
		t.Fatalf("the renew answered %+v against %v", renewed, a.ExpiresAt)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease == nil || !after.Lease.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the lease did not move with the attachment: %+v", after.Lease)
	}
	// An empty body is the default lease, which is what a runtime that
	// renews on a timer sends.
	bare := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID+"/renew", nil)
	if bare.code != http.StatusOK {
		t.Fatalf("a renew with no body = %d: %s", bare.code, bare.body)
	}
}

func TestAReleaseClearsTheLeaseAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")

	first := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID, nil)
	if first.code != http.StatusNoContent {
		t.Fatalf("the release = %d: %s", first.code, first.body)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease != nil {
		t.Fatalf("the release left the lease held: %+v", after.Lease)
	}
	// The end state is what the caller asked for, so a second release is the
	// same answer and not a refusal.
	again := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID, nil)
	if again.code != http.StatusNoContent {
		t.Fatalf("a second release = %d: %s", again.code, again.body)
	}
	if got := h.ledger.actions(); !slices.Contains(got, ActionRelease) {
		t.Errorf("the release appended %v", got)
	}
	// The workspace takes a new writer once the lease is free.
	h.attach(t, ws, "sbx_b", "rw")
}

func TestARenewAgainstAnAttachmentThatEndedIsGone(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the release = %d", got.code)
	}
	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID+"/renew", nil)
	if code := got.errorCode(t); got.code != http.StatusGone || code != "attachment_gone" {
		t.Fatalf("a renew against a released attachment = %d %q", got.code, code)
	}
}

func TestAnAttachmentOfAnotherWorkspaceIsNotFound(t *testing.T) {
	h := newHarness(t)
	build := h.create(t, "build")
	release := h.create(t, "release")
	a := h.attach(t, build, "sbx_a", "rw")

	for _, path := range []string{
		"/v1/workspaces/" + release.ID + "/attach/" + a.ID + "/renew",
		"/v1/workspaces/" + build.ID + "/attach/att-9999/renew",
	} {
		got := h.do(t, http.MethodPost, path, nil)
		if got.code != http.StatusNotFound {
			t.Errorf("POST %s = %d: %s", path, got.code, got.body)
		}
	}
	gone := h.do(t, http.MethodDelete, "/v1/workspaces/"+release.ID+"/attach/"+a.ID, nil)
	if gone.code != http.StatusNotFound {
		t.Errorf("a release across workspaces = %d", gone.code)
	}
}

func TestTheRenewAndTheReleaseAskTheActionTheirAttachAsked(t *testing.T) {
	for _, c := range []struct{ mode, action string }{
		{"ro", "workspace.read"},
		{"rw", "workspace.attach"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			// Each mode gets a workspace and a harness of its own. An allow
			// is cached by the client of spec 006 for the answer's ttl, so
			// the question a renew puts reaches the stub only when it is the
			// first one about that workspace.
			h := newHarness(t)
			ws := h.create(t, "build")
			a := h.store.open(ws.ID, "sbx_"+c.mode, store.Mode(c.mode), h.at().Add(DefaultTTL))
			h.forget()
			if got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID+"/renew", nil); got.code != http.StatusOK {
				t.Fatalf("the renew = %d: %s", got.code, got.body)
			}
			if got := h.asked(); !slices.Equal(got, []string{c.action}) {
				t.Errorf("a %s renew asked %v, want %q", c.mode, got, c.action)
			}

			release := newHarness(t)
			other := release.create(t, "build")
			b := release.store.open(other.ID, "sbx_"+c.mode, store.Mode(c.mode), release.at().Add(DefaultTTL))
			release.forget()
			if got := release.do(t, http.MethodDelete, "/v1/workspaces/"+other.ID+"/attach/"+b.ID, nil); got.code != http.StatusNoContent {
				t.Fatalf("the release = %d: %s", got.code, got.body)
			}
			if got := release.asked(); !slices.Equal(got, []string{c.action}) {
				t.Errorf("a %s release asked %v, want %q", c.mode, got, c.action)
			}
		})
	}
}

func TestAZombieWriterWhoseLeaseMovedOnRenewsNothing(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	zombie := h.attach(t, ws, "sbx_a", "rw")
	// The lease lapses and the next writer takes it. The zombie's
	// attachment is still active, so its renew reaches the lease check and
	// is told it no longer holds one.
	h.travel(DefaultTTL + time.Minute)
	h.attach(t, ws, "sbx_b", "rw")

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach/"+zombie.ID+"/renew", nil)
	if code := got.errorCode(t); got.code != http.StatusConflict || code != "lease_not_held" {
		t.Fatalf("the zombie's renew = %d %q: %s", got.code, code, got.body)
	}
	// And its release frees the next writer's lease from nobody.
	if out := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+zombie.ID, nil); out.code != http.StatusNoContent {
		t.Fatalf("the zombie's release = %d", out.code)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease == nil || after.Lease.Holder != "sbx_b" {
		t.Fatalf("the zombie's release took the next writer's lease: %+v", after.Lease)
	}
}

func TestTheReaperPassEndsWhatOutlivedItsDeadline(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	writer := h.attach(t, ws, "sbx_a", "rw")
	reader := h.attach(t, ws, "sbx_r", "ro")

	// Nothing has lapsed yet.
	if n, err := h.service.ExpireLeases(t.Context(), h.at()); err != nil || n != 0 {
		t.Fatalf("a pass over live attachments ended %d: %v", n, err)
	}
	h.travel(DefaultTTL + time.Minute)
	n, err := h.service.ExpireLeases(t.Context(), h.at())
	if err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	if n != 2 {
		t.Fatalf("the pass ended %d attachments, want the writer and the reader", n)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease != nil {
		t.Fatalf("the pass left a lease held: %+v", after.Lease)
	}
	// The loss is visible rather than silent: a reap row per attachment.
	reaps := 0
	for _, action := range h.ledger.actions() {
		if action == ActionReap {
			reaps++
		}
	}
	if reaps != 2 {
		t.Errorf("the pass appended %d reap rows", reaps)
	}
	// The zombie's next request is gone, not forbidden.
	for _, id := range []string{writer.ID, reader.ID} {
		got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach/"+id+"/renew", nil)
		if code := got.errorCode(t); got.code != http.StatusGone || code != "attachment_gone" {
			t.Errorf("the reaped attachment's renew = %d %q", got.code, code)
		}
	}
	// A second pass ends nothing: the rows are already reaped.
	if again, err := h.service.ExpireLeases(t.Context(), h.at()); err != nil || again != 0 {
		t.Fatalf("a second pass ended %d: %v", again, err)
	}
}

func TestTheReaperPassFreesALeaseNoAttachmentHolds(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	// A lease can sit on the row with no attachment behind it, which is what
	// a repair write leaves. The sweep frees it or the workspace wedges.
	holder := "sbx_orphan"
	lapsed := h.at().Add(-time.Minute)
	h.store.mu.Lock()
	row := h.store.workspaces[ws.ID]
	row.WriterHolder, row.WriterExpiresAt = &holder, &lapsed
	h.store.workspaces[ws.ID] = row
	h.store.mu.Unlock()

	n, err := h.service.ExpireLeases(t.Context(), h.at())
	if err != nil || n != 1 {
		t.Fatalf("the pass freed %d leases: %v", n, err)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Lease != nil {
		t.Fatalf("the orphan lease survived: %+v", after.Lease)
	}
}

func TestAReaperPassReportsAStoreFaultRatherThanSwallowingIt(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.attach(t, ws, "sbx_a", "rw")
	h.travel(DefaultTTL + time.Minute)

	for _, method := range []string{"Expired", "Reap", "ExpiredLeases"} {
		h.store.fail = map[string]error{method: errors.New("the connection failed")}
		if _, err := h.service.ExpireLeases(t.Context(), h.at()); err == nil {
			t.Errorf("a pass with %s failing reported success", method)
		}
	}
	// A fresh harness, because the loop above already reaped what this one
	// needs to find.
	logged := newHarness(t)
	other := logged.create(t, "build")
	logged.attach(t, other, "sbx_a", "rw")
	logged.travel(DefaultTTL + time.Minute)
	logged.ledger.fail = errors.New("the log is unavailable")
	if _, err := logged.service.ExpireLeases(t.Context(), logged.at()); err == nil {
		t.Error("a pass whose log refused reported success")
	}
}
