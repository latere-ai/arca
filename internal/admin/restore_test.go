// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// POST /v1/admin/spaces/{owner}/restore through the surface: the seam bound
// and unbound, the two kinds one id names, and what an id past the window
// answers.

// restorePath is the route for one space, with the subject encoded into the
// path the way a client sends it.
func restorePath(owner string) string {
	return "/v1/admin/spaces/" + url.PathEscape(owner) + "/restore"
}

func TestARestoreBringsBackWhatTheIdNames(t *testing.T) {
	for _, tc := range []struct {
		kind string
		id   string
	}{
		{kind: KindFile, id: "01J8R4A"},
		{kind: KindWorkspace, id: "01J8R4B"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			seam := &fakeRestorer{back: Restored{ID: tc.id, Kind: tc.kind}}
			h := newHarness(t, withRestorer(seam))
			owner := "https://other.example|c1d0"

			got := h.do(t, http.MethodPost, restorePath(owner), map[string]any{"id": tc.id})
			if got.code != http.StatusOK {
				t.Fatalf("the restore answered %d: %s", got.code, got.body)
			}
			var back struct {
				ID     string `json:"id"`
				Kind   string `json:"kind"`
				Status string `json:"status"`
			}
			got.decode(t, &back)
			if back.ID != tc.id || back.Kind != tc.kind || back.Status != "restored" {
				t.Errorf("the restore answered %+v", back)
			}
			// The restore crosses owners, which is the whole reason it exists
			// beside the owner's own.
			if seam.owner != owner || seam.id != tc.id {
				t.Errorf("the seam was asked to restore %q of %q", seam.id, seam.owner)
			}
		})
	}
}

// TestARestoreWithNoSeamBoundIsNotImplemented: the row is registered at its
// right place and answers the one code spec 013 reserves for a route whose
// behavior has not landed.
func TestARestoreWithNoSeamBoundIsNotImplemented(t *testing.T) {
	h := newHarness(t)
	got := h.do(t, http.MethodPost, restorePath("https://other.example|c1d0"), map[string]any{"id": "01J8R4A"})
	if got.code != http.StatusNotImplemented {
		t.Fatalf("an unbound restore answered %d: %s", got.code, got.body)
	}
	if code := got.errorCode(t); code != "not_implemented" {
		t.Errorf("an unbound restore answered %q", code)
	}
}

// TestAnIdPastTheWindowIsNotFoundAndNamesTheWindow: an administrator is not
// left guessing between a typo and an expiry.
func TestAnIdPastTheWindowIsNotFoundAndNamesTheWindow(t *testing.T) {
	h := newHarness(t, withRestorer(&fakeRestorer{err: ErrNotRestorable}))
	got := h.do(t, http.MethodPost, restorePath("https://other.example|c1d0"), map[string]any{"id": "01J8R4A"})
	if got.code != http.StatusNotFound {
		t.Fatalf("a purged id answered %d: %s", got.code, got.body)
	}
	if code := got.errorCode(t); code != "not_found" {
		t.Errorf("a purged id answered %q", code)
	}
	if detail := got.detail(t); !strings.Contains(detail, "ARCA_TRASH_RETENTION") {
		t.Errorf("the developer detail is %q and does not name the window", detail)
	}
}

func TestARestoreNamesTheIdToBringBack(t *testing.T) {
	seam := &fakeRestorer{}
	h := newHarness(t, withRestorer(seam))
	got := h.do(t, http.MethodPost, restorePath("https://other.example|c1d0"), map[string]any{})
	if got.code != http.StatusBadRequest {
		t.Fatalf("a restore naming no id answered %d: %s", got.code, got.body)
	}
	if code := got.errorCode(t); code != "missing_field" {
		t.Errorf("a restore naming no id answered %q", code)
	}
	if fields := got.fields(t); len(fields) != 1 || fields[0] != "id" {
		t.Errorf("the refusal named the fields %v", fields)
	}
	if seam.calls != 0 {
		t.Error("a refused restore reached the seam")
	}
}

// TestARestoreOnTheAliasNamesTheCallersOwnSpace: a client that has not read
// its own subject back still addresses its space.
func TestARestoreOnTheAliasNamesTheCallersOwnSpace(t *testing.T) {
	seam := &fakeRestorer{}
	h := newHarness(t, withRestorer(seam))
	got := h.do(t, http.MethodPost, "/v1/admin/spaces/me/restore", map[string]any{"id": "01J8R4A"})
	if got.code != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", got.code, got.body)
	}
	if seam.owner != h.subject {
		t.Errorf("the alias resolved to %q, want the caller's own space %q", seam.owner, h.subject)
	}
}

func TestARestoreReportsASeamThatFailed(t *testing.T) {
	h := newHarness(t, withRestorer(&fakeRestorer{err: errFault}))
	got := h.do(t, http.MethodPost, restorePath("https://other.example|c1d0"), map[string]any{"id": "01J8R4A"})
	if got.code != http.StatusInternalServerError {
		t.Fatalf("a failed restore answered %d: %s", got.code, got.body)
	}
}

func TestARestoreRefusesABodyItCannotRead(t *testing.T) {
	seam := &fakeRestorer{}
	h := newHarness(t, withRestorer(seam))
	got := h.do(t, http.MethodPost, restorePath("https://other.example|c1d0"), map[string]any{"workspace_id": "01J8R4A"})
	if got.code != http.StatusBadRequest {
		t.Fatalf("a body with an unknown field answered %d: %s", got.code, got.body)
	}
	if code := got.errorCode(t); code != "unknown_field" {
		t.Errorf("a body with an unknown field answered %q", code)
	}
	if seam.calls != 0 {
		t.Error("a refused restore reached the seam")
	}
}
