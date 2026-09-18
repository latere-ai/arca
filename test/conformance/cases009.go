// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"testing"
)

// The rows of spec 009 the wire shows: a workspace is created, renamed,
// soft deleted and restored; two attaches yield one lease and the second is
// refused, which is invariant 7 of spec 001 seen from outside; a renew
// pushes the expiry forward and a release ends the lease; and materialize
// and sync round-trip a subtree.

func cases009() []testCase {
	create := []string{"POST /v1/workspaces", "DELETE /v1/workspaces/{id}"}
	attach := append(append([]string{}, create...),
		"POST /v1/workspaces/{id}/attach",
		"POST /v1/workspaces/{id}/attach/{aid}/renew",
		"DELETE /v1/workspaces/{id}/attach/{aid}")
	return []testCase{
		{name: "Lifecycle", group: GroupWorkspaces, routes: append(append([]string{}, create...),
			"GET /v1/workspaces/{id}", "PATCH /v1/workspaces/{id}", "GET /v1/workspaces"), run: case009Lifecycle},
		{name: "SlugTaken", group: GroupWorkspaces, routes: create, run: case009SlugTaken},
		{name: "Deleted", group: GroupWorkspaces, routes: append(append([]string{}, create...),
			"GET /v1/workspaces/deleted", "POST /v1/workspaces/{id}/restore"), run: case009Deleted},
		{name: "OneWriter", group: GroupWorkspaces, routes: attach, run: case009OneWriter},
		{name: "Lease", group: GroupWorkspaces, routes: append(append([]string{}, attach...),
			"GET /v1/workspaces/{id}"), run: case009Lease},
		{name: "Materialize", group: GroupWorkspaces, routes: append(append([]string{}, attach...),
			"GET /v1/workspaces/{id}/materialize", "POST /v1/workspaces/{id}/sync"), run: case009Materialize},
	}
}

// workspace creates one under the run's own prefix and records it for the
// cleanup, so the run deletes what it made by id and nothing else.
func (s *session) workspace(t *testing.T, principal, label string) map[string]any {
	t.Helper()
	r := s.call(t, principal, http.MethodPost, "/v1/workspaces", body(fields{"owner": "me", "slug": s.name(label)}))
	expectStatus(t, r, http.StatusCreated)
	id := str(r.json, "id")
	failIf(t, id == "", "the created workspace carries no id: %s", r.body)
	s.record("workspace "+id, func(t testing.TB) error {
		s.call(t, principal, http.MethodDelete, "/v1/workspaces/"+id+"?permanent=1", "")
		s.call(t, principal, http.MethodDelete, "/v1/workspaces/"+id, "")
		return nil
	})
	return r.json
}

// case009Lifecycle: a workspace is created with the fields spec 013's shape
// names, read back by id, renamed, and found in the listing, which pages
// through the one envelope of that spec.
func case009Lifecycle(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "lifecycle")
	id := str(made, "id")
	for _, field := range []string{"id", "owner", "slug", "root_prefix", "created_at"} {
		failIf(t, str(made, field) == "" && num(made, field) == 0,
			"the created workspace carries no %s, and spec 013's shape names it: %v", field, made)
	}
	failIf(t, made["lease"] != nil, "a workspace nobody attached to carries a lease: %v", made["lease"])

	read := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces/"+id, ""), http.StatusOK)
	failIf(t, str(read.json, "id") != id, "GET by id answered %q, want %q", str(read.json, "id"), id)
	failIf(t, str(read.json, "owner") != str(made, "owner"),
		"the create and the read render the owner differently: %q and %q", str(made, "owner"), str(read.json, "owner"))

	renamed := s.name("renamed")
	patched := expectStatus(t, s.call(t, Alice, http.MethodPatch, "/v1/workspaces/"+id, body(fields{"slug": renamed})), http.StatusOK)
	failIf(t, str(patched.json, "slug") != renamed, "the rename answered the slug %q, want %q", str(patched.json, "slug"), renamed)

	page := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces?owner=me&limit=1000", ""), http.StatusOK)
	entries := list(page.json, "entries")
	failIf(t, entries == nil, "the listing answers no entries array, and spec 013 never answers null: %s", page.body)
	found := false
	for _, e := range entries {
		if str(e, "id") == id {
			found = true
		}
	}
	failIf(t, !found, "the workspace the run made is not in its owner's listing")
}

// case009SlugTaken: a second workspace with a slug the space already holds
// is refused with slug_taken, which is a 409 and not a 404.
func case009SlugTaken(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "taken")
	again := s.call(t, Alice, http.MethodPost, "/v1/workspaces", body(fields{"owner": "me", "slug": str(made, "slug")}))
	expectError(t, again, CodeSlugTaken)
}

// case009Deleted: a soft delete takes the workspace out of the listing and
// puts it in the deleted one, and a restore undoes it.
func case009Deleted(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "deleted")
	id := str(made, "id")
	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id, ""), http.StatusNoContent)

	gone := s.call(t, Alice, http.MethodGet, "/v1/workspaces/"+id, "")
	failIf(t, gone.status == http.StatusOK, "a soft deleted workspace is still read by id")

	deleted := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces/deleted?owner=me", ""), http.StatusOK)
	found := false
	for _, e := range list(deleted.json, "entries") {
		if str(e, "id") == id {
			found = true
		}
	}
	failIf(t, !found, "a soft deleted workspace is not in the deleted listing, and spec 009 makes it restorable")

	expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/restore", ""), http.StatusOK)
	expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces/"+id, ""), http.StatusOK)
}

// case009OneWriter: two write attaches to one workspace yield one lease and
// the second is refused with writer_held. This is invariant 7 of spec 001
// seen from outside, and it is the whole of what a workspace's exclusion
// promises a caller.
func case009OneWriter(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "one-writer")
	id := str(made, "id")

	first := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
		body(fields{"sandbox_id": s.name("sandbox-a"), "mode": "rw", "ttl_seconds": 120})), http.StatusCreated)
	aid := str(first.json, "id")
	failIf(t, aid == "", "the attachment carries no id: %s", first.body)

	second := s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
		body(fields{"sandbox_id": s.name("sandbox-b"), "mode": "rw", "ttl_seconds": 120}))
	expectError(t, second, CodeWriterHeld)

	// A reader is not a writer, so a read attach is admitted beside the one
	// live lease rather than refused by it.
	reader := s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
		body(fields{"sandbox_id": s.name("sandbox-r"), "mode": "ro", "ttl_seconds": 120}))
	expectStatus(t, reader, http.StatusCreated)
	s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+str(reader.json, "id"), "")

	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+aid, ""), http.StatusNoContent)
	// A release is idempotent, which is what a sandbox that retries needs.
	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+aid, ""), http.StatusNoContent)

	after := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
		body(fields{"sandbox_id": s.name("sandbox-c"), "mode": "rw", "ttl_seconds": 120})), http.StatusCreated)
	s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+str(after.json, "id"), "")
}

// case009Lease: the workspace carries the lease its writer holds, a renew
// pushes the expiry forward, and a release takes the lease away. The lease
// is one nested object rather than three fields, which is spec 013's shape.
func case009Lease(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "lease")
	id := str(made, "id")
	holder := s.name("sandbox-lease")

	attached := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
		body(fields{"sandbox_id": holder, "mode": "rw", "ttl_seconds": 60})), http.StatusCreated)
	aid := str(attached.json, "id")

	held := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces/"+id, ""), http.StatusOK)
	lease := obj(held.json, "lease")
	failIf(t, lease == nil, "a workspace under a write attach carries no lease: %s", held.body)
	failIf(t, str(lease, "holder") != holder, "the lease names the holder %q, want %q", str(lease, "holder"), holder)
	failIf(t, str(lease, "mode") != "rw", "the lease names the mode %q, want rw", str(lease, "mode"))
	before := str(lease, "expires_at")
	failIf(t, before == "", "the lease carries no expires_at: %s", held.body)

	renewed := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach/"+aid+"/renew",
		body(fields{"ttl_seconds": 600})), http.StatusOK)
	after := str(renewed.json, "expires_at")
	failIf(t, after == "" || after <= before,
		"a renew answered the expiry %q, which is not later than %q; an RFC 3339 UTC timestamp sorts as it reads", after, before)

	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+aid, ""), http.StatusNoContent)
	released := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces/"+id, ""), http.StatusOK)
	failIf(t, released.json["lease"] != nil, "the lease survived its release: %v", released.json["lease"])

	// A released attachment is gone, and a renew of it says so rather than
	// answering as though it still held the lease.
	expired := s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach/"+aid+"/renew", body(fields{"ttl_seconds": 60}))
	failIf(t, expired.status == http.StatusOK, "a released attachment was renewed")
	code := expired.code()
	failIf(t, code != CodeAttachmentGone && code != CodeNotFound && code != CodeLeaseNotHeld,
		"a renew of a released attachment answered %d %s; spec 013 names attachment_gone, lease_not_held and not_found", expired.status, code)
	expectError(t, expired, code)
}

// case009Materialize: an attachment's manifest is the pinned view of the
// subtree with a presigned URL per object, and a sync declares the
// post-state the server reconciles against. An empty workspace is the case
// every target can run, because the file routes that fill one are spec
// 005's and the manifest's shape is spec 013's either way.
func case009Materialize(t *testing.T, s *session) {
	made := s.workspace(t, Alice, "materialize")
	id := str(made, "id")

	attached := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/attach",
		body(fields{"sandbox_id": s.name("sandbox-m"), "mode": "rw", "ttl_seconds": 300})), http.StatusCreated)
	aid := str(attached.json, "id")
	defer s.call(t, Alice, http.MethodDelete, "/v1/workspaces/"+id+"/attach/"+aid, "")

	manifest := expectStatus(t, s.call(t, Alice, http.MethodGet,
		"/v1/workspaces/"+id+"/materialize?attachment="+aid, ""), http.StatusOK)
	root := str(manifest.json, "root")
	failIf(t, root == "", "the manifest names no root: %s", manifest.body)
	failIf(t, root != str(made, "root_prefix"),
		"the manifest's root is %q and the workspace's root_prefix is %q", root, str(made, "root_prefix"))
	files, ok := manifest.json["files"]
	failIf(t, !ok || files == nil, "the manifest answers no files array, and spec 013 never answers null: %s", manifest.body)
	for _, f := range list(manifest.json, "files") {
		failIf(t, str(f, "url") == "", "a manifest entry carries no presigned url, and invariant 4 puts the bytes off the hot path: %v", f)
		failIf(t, str(f, "checksum") == "", "a manifest entry carries no checksum: %v", f)
	}

	// The post-state of an empty attachment is an empty manifest, which is a
	// sync that removes nothing and writes nothing. It is the one sync every
	// target can make without the file routes of spec 005.
	synced := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/sync",
		body(fields{"attachment_id": aid, "files": []any{}})), http.StatusOK)
	failIf(t, str(synced.json, "last_sync") == "", "the sync answers no last_sync: %s", synced.body)

	// A sync that names no attachment does not hold the lease, whatever it
	// declares.
	orphan := s.call(t, Alice, http.MethodPost, "/v1/workspaces/"+id+"/sync",
		body(fields{"attachment_id": s.name("no-such-attachment"), "files": []any{}}))
	failIf(t, orphan.status == http.StatusOK, "a sync from an attachment that does not exist was applied")
	expectError(t, orphan, orphan.code())
}
