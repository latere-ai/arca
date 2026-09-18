// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/authorizer"
)

// Spec 012 through the binary: `arcad check` against the whole stack, the
// same command against one broken variable, and the administrative overview
// answered by a running server over a real database.

// overviewRow is one row of GET /v1/admin/overview as a caller reads it. It
// is written out here rather than imported, so the tier is held to the wire
// and not to the server's own type.
type overviewRow struct {
	Owner        string     `json:"owner"`
	Files        int64      `json:"files"`
	Bytes        int64      `json:"bytes"`
	TrashedBytes int64      `json:"trashed_bytes"`
	Workspaces   int64      `json:"workspaces"`
	Leases       int64      `json:"leases"`
	Links        int64      `json:"links"`
	LastWriteAt  *time.Time `json:"last_write_at"`
}

// TestE2ECheckPassesAgainstTheStack is criterion 12 of spec 012 where an
// operator meets it: every requirement of an installation reached for real,
// one line each, exit 0.
func TestE2ECheckPassesAgainstTheStack(t *testing.T) {
	i := start(t)
	out, err := i.command(t, "check")
	if err != nil {
		t.Fatalf("arcad check against a healthy installation: %v\n%s", err, out)
	}
	for _, requirement := range []string{"bucket", "database", "issuer", "authorizer", "public-url"} {
		if !strings.Contains(out, requirement) {
			t.Errorf("the report holds no %q line:\n%s", requirement, out)
		}
	}
	if strings.Contains(out, "fail ") {
		t.Errorf("a healthy installation failed a requirement:\n%s", out)
	}
	if !strings.Contains(out, "5 checks passed") {
		t.Errorf("the summary is missing:\n%s", out)
	}
	// Two runs of a healthy installation print identical output, which is
	// what lets an operator diff one against the next.
	again, err := i.command(t, "check")
	if err != nil {
		t.Fatalf("the second run: %v\n%s", err, again)
	}
	if again != out {
		t.Errorf("two runs printed\n%s\nand\n%s", out, again)
	}
}

// TestE2ECheckFailsOnABrokenVariable is criterion 13 where an operator meets
// it: one variable pointed somewhere else, and the command exits 1 naming the
// requirement that failed and no other.
func TestE2ECheckFailsOnABrokenVariable(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value, requirement string
	}{
		{
			name: "the bucket does not exist", variable: "ARCA_BUCKET",
			value: "no-such-bucket", requirement: "bucket",
		},
		{
			name: "the database is somewhere else", variable: "ARCA_DATABASE_URL",
			value: "postgres://arca:arca@127.0.0.1:1/arca?sslmode=disable", requirement: "database",
		},
		{
			name: "the issuer does not answer", variable: "ARCA_OIDC_ISSUERS",
			value: "http://127.0.0.1:1", requirement: "issuer",
		},
		{
			name: "the authorizer does not answer", variable: "ARCA_AUTHORIZER_URL",
			value: "http://127.0.0.1:1", requirement: "authorizer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := start(t)
			out, err := i.commandWith(t, []string{tc.variable + "=" + tc.value}, "check")
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("a broken %s exited %v, want 1\n%s", tc.variable, err, out)
			}
			if !strings.Contains(out, "fail  "+tc.requirement) {
				t.Errorf("the report does not fail the %s line:\n%s", tc.requirement, out)
			}
			if !strings.Contains(out, "of 5 checks failed") {
				t.Errorf("the summary is missing:\n%s", out)
			}
		})
	}
}

// TestE2ETheAdministrativeOverviewReadsAcrossSpaces is criteria 1, 2, 5 and 6
// of spec 012 through the running server: the route is registered by the
// node, asks space.admin, and answers one row per space that holds anything
// with the counters a direct count of the fixtures gives.
func TestE2ETheAdministrativeOverviewReadsAcrossSpaces(t *testing.T) {
	i := start(t)
	i.allowEverything()
	db := i.database(t)

	// Two objects and a workspace in the caller's own space, seeded the way
	// the routes of spec 005 would leave them.
	first := i.put(t, db, "files/reports/q3.pdf", "the third quarter")
	second := i.put(t, db, "files/reports/q4.pdf", "the fourth quarter")
	if _, err := db.Querier().Exec(t.Context(),
		`INSERT INTO space_usage (owner, bytes) VALUES ($1, $2)
		 ON CONFLICT (owner) DO UPDATE SET bytes = EXCLUDED.bytes`,
		i.subject(), first.size+second.size); err != nil {
		t.Fatalf("seed the ledger: %v", err)
	}
	var workspace struct {
		ID string `json:"id"`
	}
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces",
		map[string]any{"slug": "build"}, &workspace)

	var overview struct {
		Entries    []overviewRow `json:"entries"`
		NextCursor string        `json:"next_cursor"`
	}
	i.expect(t, http.StatusOK, http.MethodGet, "/v1/admin/overview", nil, &overview)

	var row *overviewRow
	for j := range overview.Entries {
		if overview.Entries[j].Owner == i.subject() {
			row = &overview.Entries[j]
		}
	}
	if row == nil {
		t.Fatalf("the overview holds %d rows and none is the caller's space", len(overview.Entries))
	}
	if row.Files != 2 {
		t.Errorf("the space holds %d live paths, want 2", row.Files)
	}
	if row.Bytes != first.size+second.size {
		t.Errorf("the space's usage is %d, want the ledger's %d", row.Bytes, first.size+second.size)
	}
	if row.TrashedBytes != 0 {
		t.Errorf("the space's trashed bytes are %d, want 0", row.TrashedBytes)
	}
	if row.Workspaces != 1 {
		t.Errorf("the space holds %d live workspaces, want 1", row.Workspaces)
	}
	if row.Leases != 0 {
		t.Errorf("the space holds %d leases and nothing attached", row.Leases)
	}
	if row.Links != 0 {
		t.Errorf("the space holds %d links and none was issued", row.Links)
	}
	if row.LastWriteAt == nil {
		t.Error("the row names no last write and the space holds two paths")
	}
}

// TestE2EADeniedAdministratorIsForbidden is criterion 2 through the running
// server: a deny is 403 and not the 404 the service Arca replaces answered to
// hide the surface.
func TestE2EADeniedAdministratorIsForbidden(t *testing.T) {
	i := start(t)
	i.allowEverything()
	i.authorizer.Deny(stub.Rule{Subject: "*", Action: authorizer.ActionSpaceAdmin, Resource: "*"}, "no rule allows it")
	code, body := i.api(t, http.MethodGet, "/v1/admin/overview", nil)
	if code != http.StatusForbidden {
		t.Fatalf("a denied overview answered %d: %s", code, body)
	}
	if got := errorCode(t, body); got != "forbidden" {
		t.Errorf("a denied overview answered %q", got)
	}
}

// TestE2ETheRestoreAcrossOwnersIsRegisteredAndUnbound: the row is registered
// at its right place and answers the one code spec 013 reserves for a route
// whose behaviour has not landed, so the surface is settled before the
// handler is.
func TestE2ETheRestoreAcrossOwnersIsRegisteredAndUnbound(t *testing.T) {
	i := start(t)
	i.allowEverything()
	code, body := i.api(t, http.MethodPost, "/v1/admin/spaces/me/restore", map[string]any{"id": "01J8R4A"})
	if code != http.StatusNotImplemented {
		t.Fatalf("the restore answered %d: %s", code, body)
	}
	if got := errorCode(t, body); got != "not_implemented" {
		t.Errorf("the restore answered %q", got)
	}
}
