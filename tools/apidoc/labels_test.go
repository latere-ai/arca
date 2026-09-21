// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestOperationNavigationLabels checks the YAML consumers render in navigation,
// including the alternatives and qualifications retained in the full description.
func TestOperationNavigationLabels(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Summary     string `yaml:"summary"`
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(Render(), &doc); err != nil {
		t.Fatal(err)
	}
	verbs := strings.Fields("Write Read Get Move Delete List Restore Empty Star Unstar Create Revoke Rename Attach Renew Release Sync Start Complete Abort")
	for path, methods := range doc.Paths {
		for method, op := range methods {
			words := strings.Fields(op.Summary)
			if len(words) == 0 || len(words) > 4 {
				t.Errorf("%s %s summary must contain one to four words: %q", method, path, op.Summary)
				continue
			}
			if !slices.Contains(verbs, words[0]) {
				t.Errorf("%s %s summary must begin with an action: %q", method, path, op.Summary)
			}
		}
	}
	cases := []struct{ method, path, summary, detail string }{
		{"PUT", "/v1/files/{owner}/{path}", "Write file", "Write one object; the body is the bytes."},
		{"GET", "/v1/files/{owner}/{path}", "Read or list files", "Read one object, its subtree with ?list=1, which carries the space's usage at a plane root, or its versions with ?versions=1."},
		{"HEAD", "/v1/files/{owner}/{path}", "Get file headers", "The object's headers with no body."},
		{"POST", "/v1/files/{owner}/{path}", "Move or restore file", "Move the object with move_to, or bring a version forward with restore_version."},
		{"DELETE", "/v1/files/{owner}/{path}", "Delete file", "Trash the object; ?permanent=1 removes it and ?version=N prunes one version."},
		{"GET", "/v1/files/materialize", "Get space manifest", "One space as a manifest of presigned URLs."},
		{"GET", "/v1/trash", "List trash", "What is trashed and still restorable, newest first."},
		{"POST", "/v1/trash/restore", "Restore file", "Return one object to its path."},
		{"DELETE", "/v1/trash", "Empty trash", "Empty the trash, or one path with ?path=."},
		{"PUT", "/v1/stars", "Star file", "Star an object; idempotent."},
		{"DELETE", "/v1/stars", "Unstar file", "Unstar an object; idempotent."},
		{"GET", "/v1/stars", "List starred files", "The caller's stars across every space."},
		{"GET", "/v1/events", "List events", "One page of a space's log, oldest first."},
		{"GET", "/v1/shares/links/{token}/meta", "Get link metadata", "What a link token names, before anything is fetched."},
		{"GET", "/v1/shares/links/{token}", "List linked files", "A listing of the subtree a link token names."},
		{"GET", "/v1/shares/links/{token}/files/{path}", "Read linked file", "One object under the subtree a link token names."},
		{"POST", "/v1/shares", "Create grant", "Grant a subject a permission on a subtree of a space."},
		{"GET", "/v1/shares", "List grants", "The grants on a space."},
		{"GET", "/v1/shares/with-me", "List received grants", "The grants whose grantee is the caller."},
		{"GET", "/v1/shares/{id}", "Get grant", "One grant."},
		{"DELETE", "/v1/shares/{id}", "Revoke grant", "Revoke a grant."},
		{"POST", "/v1/shares/links", "Create link", "Mint a token grant on a subtree; the token is answered once."},
		{"GET", "/v1/shares/links", "List links", "The token grants on a space."},
		{"DELETE", "/v1/shares/links/{id}", "Revoke link", "Revoke a token grant."},
		{"POST", "/v1/workspaces", "Create workspace", "Create a workspace in a space."},
		{"GET", "/v1/workspaces", "List workspaces", "List the workspaces of a space."},
		{"GET", "/v1/workspaces/deleted", "List deleted workspaces", "List the soft deleted workspaces of a space that are still restorable."},
		{"GET", "/v1/workspaces/{id}", "Get workspace", "Read one workspace with its lease and the counters of its subtree."},
		{"PATCH", "/v1/workspaces/{id}", "Rename workspace", "Rename a workspace, which moves its rows and no bucket key."},
		{"DELETE", "/v1/workspaces/{id}", "Delete workspace", "Soft delete a workspace, leaving it restorable."},
		{"POST", "/v1/workspaces/{id}/restore", "Restore workspace", "Undo a soft delete."},
		{"POST", "/v1/workspaces/{id}/attach", "Attach workspace", "Open an attachment, taking the writer lease for a rw mount."},
		{"POST", "/v1/workspaces/{id}/attach/{aid}/renew", "Renew attachment", "Push the attachment's deadline forward."},
		{"DELETE", "/v1/workspaces/{id}/attach/{aid}", "Release attachment", "Release an attachment and the lease it held."},
		{"GET", "/v1/workspaces/{id}/materialize", "Get workspace manifest", "The attachment's pinned manifest with a presigned read per file."},
		{"POST", "/v1/workspaces/{id}/sync", "Sync workspace", "Declare the post-state manifest of the subtree; the server reconciles."},
		{"POST", "/v1/uploads", "Start upload", "Open an upload session and answer its presigned part URLs."},
		{"POST", "/v1/uploads/{id}/complete", "Complete upload", "Assemble the parts into the object at the session's path."},
		{"DELETE", "/v1/uploads/{id}", "Abort upload", "Abort the session and discard its parts."},
		{"GET", "/v1/admin/overview", "List space usage", "One row per space with its usage and its counts."},
		{"POST", "/v1/admin/spaces/{owner}/restore", "Restore deleted item", "Restore one deleted object or workspace of a space."},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			op := doc.Paths[tc.path][strings.ToLower(tc.method)]
			if op.Summary != tc.summary {
				t.Errorf("summary = %q, want %q", op.Summary, tc.summary)
			}
			if !strings.Contains(op.Description, tc.detail) {
				t.Errorf("description lost %q: %q", tc.detail, op.Description)
			}
			if !strings.Contains(op.Description, "Asks the authorizer for ") && !strings.Contains(op.Description, "Carries no bearer token:") {
				t.Errorf("description lost authorization details: %q", op.Description)
			}
		})
	}
}
