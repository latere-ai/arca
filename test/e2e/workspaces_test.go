// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
	"latere.ai/x/arca/test/stubs/issuer"
)

// The e2e tier of spec 009: arcad as a process, against a real Postgres, a
// real MinIO, the stub issuer and the stub authorizer. Nothing here reaches
// inside the server; what a test sees is what a sandbox runtime sees.
//
// The objects a workspace holds are seeded straight into the two stores,
// because the put of spec 005 and the multipart session of spec 007 are not
// in this build. That is the one thing these tests stand in for, and it is
// what lets the manifest of a multipart-written object be proved here:
// such an object carries a key its path does not predict, which is the case
// materialize has to get right.

// api drives one request against the running server with the caller's
// bearer and answers the status and the body.
func (i *installation) api(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, i.publicURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+i.issuer.Mint(issuer.Claims{Sub: "dev"}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, read
}

// expect drives one request and fails the test when the status is not the
// one wanted, decoding the body into out when it is given.
func (i *installation) expect(t *testing.T, want int, method, path string, body, out any) []byte {
	t.Helper()
	code, read := i.api(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, code, want, read)
	}
	if out != nil {
		if err := json.Unmarshal(read, out); err != nil {
			t.Fatalf("%s %s answered %s: %v", method, path, read, err)
		}
	}
	return read
}

// errorCode reads the code of a refusal in the envelope of spec 013.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				RequestID string   `json:"request_id"`
				Fields    []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("the refusal is not the envelope: %s", body)
	}
	if envelope.Error.Code != "" && envelope.Error.Details.RequestID == "" {
		t.Errorf("the refusal carries no request id: %s", body)
	}
	return envelope.Error.Code
}

// missingPaths reads the paths a manifest_incomplete named.
func missingPaths(t *testing.T, body []byte) []string {
	t.Helper()
	var envelope struct {
		Error struct {
			Details struct {
				Fields []string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Error.Details.Fields
}

// subject is the space every request of these tiers acts in.
func (i *installation) subject() string { return i.issuer.URL() + "|dev" }

// allowEverything puts one rule in the stub's table, so the tiers below
// exercise the routes rather than the ladder. The ladder itself is the unit
// tier's and the conformance suite's.
func (i *installation) allowEverything() {
	i.authorizer.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
}

// bucket opens the run's own bucket, for seeding and for reading back what a
// sync removed.
func (i *installation) bucket(t *testing.T) blob.Store {
	t.Helper()
	b, err := blob.NewS3(t.Context(), blob.Options{
		Bucket: i.stack.bucket, Endpoint: i.stack.endpoint, Region: "us-east-1",
		AccessKey: i.stack.key, SecretKey: i.stack.secret, PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	return b
}

// database opens the run's own schema.
func (i *installation) database(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), i.databaseURL)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// seeded is one object a tier wrote straight into the two stores.
type seeded struct {
	path     string
	id       object.ID
	checksum string
	size     int64
	bytes    []byte
}

// put writes one object in one piece and the row that names it, which is
// what a put of spec 005 leaves behind.
func (i *installation) put(t *testing.T, db *store.DB, path, content string) seeded {
	t.Helper()
	id := object.NewID()
	written, err := i.bucket(t).Put(t.Context(), id.Key(i.prefix), strings.NewReader(content), int64(len(content)), blob.PutOptions{})
	if err != nil {
		t.Fatalf("seed %q: %v", path, err)
	}
	return i.row(t, db, path, id, written.SHA256, int64(len(content)), object.ChecksumSHA256, []byte(content))
}

// putInParts writes one object as a multipart upload and the row that names
// it. Such an object carries the store's composite label rather than a
// digest of its bytes, and a key the path does not predict, which is the
// case materialize has to presign from the row.
func (i *installation) putInParts(t *testing.T, db *store.DB, path, content string) seeded {
	t.Helper()
	b := i.bucket(t)
	id := object.NewID()
	key := id.Key(i.prefix)
	upload, err := b.CreateMultipart(t.Context(), key, blob.PutOptions{})
	if err != nil {
		t.Fatalf("open the multipart upload: %v", err)
	}
	url, err := b.PresignPart(t.Context(), key, upload, 1)
	if err != nil {
		t.Fatalf("sign the part: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send the part: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("the part answered %d: %s", resp.StatusCode, body)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	assembled, err := b.CompleteMultipart(t.Context(), key, upload, []blob.Part{{Number: 1, ETag: etag}})
	if err != nil {
		t.Fatalf("assemble the parts: %v", err)
	}
	if !strings.Contains(assembled.ETag, "-") {
		t.Fatalf("the assembled object's label is %q, which is not a composite", assembled.ETag)
	}
	return i.row(t, db, path, id, assembled.ETag, int64(len(content)), object.ChecksumETag, []byte(content))
}

// row writes the metadata half of a seeded object.
func (i *installation) row(t *testing.T, db *store.DB, path string, id object.ID, checksum string, size int64, kind object.ChecksumKind, content []byte) seeded {
	t.Helper()
	created, err := store.NewFiles().Insert(t.Context(), db.Querier(), store.File{
		Owner: i.subject(), Path: path, ObjectID: id, CreatedBy: i.subject(),
		SizeBytes: size, Checksum: checksum, ChecksumKind: kind,
	})
	if err != nil || !created {
		t.Fatalf("seed the row for %q: %v, %v", path, created, err)
	}
	return seeded{path: path, id: id, checksum: checksum, size: size, bytes: content}
}

// fetch reads a presigned URL the way a sandbox does: straight from the
// bucket, with no bearer of Arca's.
func fetch(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// The shapes these tiers read back, named here rather than imported so the
// test reads the wire and not the server's own types.
type (
	workspaceView struct {
		ID         string `json:"id"`
		Owner      string `json:"owner"`
		Slug       string `json:"slug"`
		RootPrefix string `json:"root_prefix"`
		Files      *int64 `json:"files"`
		Bytes      *int64 `json:"bytes"`
		Lease      *struct {
			Holder    string    `json:"holder"`
			Mode      string    `json:"mode"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"lease"`
		LastSync  *time.Time `json:"last_sync"`
		DeletedAt *time.Time `json:"deleted_at"`
	}
	entryView struct {
		Path     string `json:"path"`
		Checksum string `json:"checksum"`
		Size     int64  `json:"size"`
	}
	attachmentView struct {
		ID          string      `json:"id"`
		WorkspaceID string      `json:"workspace_id"`
		Mode        string      `json:"mode"`
		ExpiresAt   time.Time   `json:"expires_at"`
		Manifest    []entryView `json:"manifest"`
	}
	manifestView struct {
		Root     string     `json:"root"`
		PinnedAt *time.Time `json:"pinned_at"`
		Files    []struct {
			entryView
			URL string `json:"url"`
		} `json:"files"`
	}
	syncView struct {
		SyncedFiles  int       `json:"synced_files"`
		DeletedFiles int       `json:"deleted_files"`
		LastSync     time.Time `json:"last_sync"`
	}
	pageView struct {
		Entries    []workspaceView `json:"entries"`
		NextCursor string          `json:"next_cursor"`
	}
)

// TestE2EAWorkspaceIsAttachedMaterializedSyncedAndReleased is the run a
// sandbox makes, against the binary: the lease, the snapshot, the write-back
// and the release, with the two stores behind them.
func TestE2EAWorkspaceIsAttachedMaterializedSyncedAndReleased(t *testing.T) {
	i := start(t)
	i.allowEverything()
	db := i.database(t)

	var ws workspaceView
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces", map[string]any{"slug": "build"}, &ws)
	if ws.Owner != i.subject() || ws.RootPrefix != "workspaces/build/" || ws.Lease != nil {
		t.Fatalf("the workspace is %+v", ws)
	}
	// One object written in one piece and one assembled from parts. The
	// second is the case that matters: its key is not the one a path would
	// predict, so a manifest that recomputed a key would presign a 404.
	main := i.put(t, db, ws.RootPrefix+"src/main.go", "package main\n")
	binary := i.putInParts(t, db, ws.RootPrefix+"bin/app", strings.Repeat("build output\n", 64))

	var attached attachmentView
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_a", "mode": "rw", "ttl_seconds": 3600}, &attached)
	if attached.Mode != "rw" || attached.WorkspaceID != ws.ID || len(attached.Manifest) != 2 {
		t.Fatalf("the attachment is %+v", attached)
	}

	// A second writer is a conflict while the first holds the lease, which
	// is invariant 8 of spec 001 through the binary.
	code, body := i.api(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_b", "mode": "rw"})
	if code != http.StatusConflict || errorCode(t, body) != "writer_held" {
		t.Fatalf("the second writer = %d %s", code, body)
	}
	// A reader attaches beside it and takes no lease.
	var reader attachmentView
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_r", "mode": "ro"}, &reader)

	// Materialize, and fetch every URL the way a sandbox does: straight from
	// the bucket, with no bearer of Arca's.
	var snapshot manifestView
	i.expect(t, http.StatusOK, http.MethodGet,
		"/v1/workspaces/"+ws.ID+"/materialize?attachment="+attached.ID, nil, &snapshot)
	if snapshot.Root != ws.RootPrefix || snapshot.PinnedAt == nil || len(snapshot.Files) != 2 {
		t.Fatalf("the snapshot is %+v", snapshot)
	}
	want := map[string]seeded{"src/main.go": main, "bin/app": binary}
	for _, f := range snapshot.Files {
		seed, named := want[f.Path]
		if !named {
			t.Fatalf("the snapshot names %q", f.Path)
		}
		if f.Checksum != seed.checksum || f.Size != seed.size {
			t.Errorf("%q is %+v, want the checksum %q and %d bytes", f.Path, f.entryView, seed.checksum, seed.size)
		}
		status, got := fetch(t, f.URL)
		if status != http.StatusOK {
			t.Fatalf("fetching %q answered %d: %s", f.Path, status, got)
		}
		if !bytes.Equal(got, seed.bytes) {
			t.Errorf("%q fetched %d bytes, want %d", f.Path, len(got), len(seed.bytes))
		}
	}

	// A renew pushes the deadline forward and moves the lease with it.
	var renewed struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	i.expect(t, http.StatusOK, http.MethodPost,
		"/v1/workspaces/"+ws.ID+"/attach/"+attached.ID+"/renew", map[string]any{"ttl_seconds": 7200}, &renewed)
	if renewed.ID != attached.ID || !renewed.ExpiresAt.After(attached.ExpiresAt) {
		t.Fatalf("the renew answered %+v against %v", renewed, attached.ExpiresAt)
	}
	var held workspaceView
	i.expect(t, http.StatusOK, http.MethodGet, "/v1/workspaces/"+ws.ID, nil, &held)
	if held.Lease == nil || held.Lease.Holder != "sbx_a" || !held.Lease.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("the lease is %+v", held.Lease)
	}
	if held.Files == nil || *held.Files != 2 || held.Bytes == nil || *held.Bytes != main.size+binary.size {
		t.Fatalf("the counters are %+v", held)
	}

	// A manifest naming a path nobody uploaded is refused with the missing
	// paths listed, and the whole sync writes nothing.
	code, body = i.api(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": attached.ID,
		"files": []map[string]any{
			{"path": "src/main.go", "checksum": main.checksum, "size": main.size},
			{"path": "bin/never", "checksum": "0000", "size": 1},
		},
	})
	if code != http.StatusConflict || errorCode(t, body) != "manifest_incomplete" {
		t.Fatalf("a manifest naming an object nobody uploaded = %d %s", code, body)
	}
	if got := missingPaths(t, body); !slices.Equal(got, []string{"bin/never"}) {
		t.Errorf("the refusal named %v", got)
	}
	var untouched workspaceView
	i.expect(t, http.StatusOK, http.MethodGet, "/v1/workspaces/"+ws.ID, nil, &untouched)
	if untouched.Files == nil || *untouched.Files != 2 || untouched.LastSync != nil {
		t.Fatalf("a refused sync changed the workspace: %+v", untouched)
	}
	if _, err := i.bucket(t).Head(t.Context(), binary.id.Key(i.prefix)); err != nil {
		t.Fatalf("a refused sync removed bytes: %v", err)
	}

	// The real sync drops the object the manifest no longer names, rows
	// first and keys after.
	manifest := map[string]any{
		"attachment_id": attached.ID,
		"files":         []map[string]any{{"path": "src/main.go", "checksum": main.checksum, "size": main.size}},
	}
	var result syncView
	i.expect(t, http.StatusOK, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest, &result)
	if result.SyncedFiles != 1 || result.DeletedFiles != 1 || result.LastSync.IsZero() {
		t.Fatalf("the sync answered %+v", result)
	}
	if _, err := i.bucket(t).Head(t.Context(), binary.id.Key(i.prefix)); err == nil {
		t.Error("the dropped object's bytes are still in the bucket")
	}
	if _, err := i.bucket(t).Head(t.Context(), main.id.Key(i.prefix)); err != nil {
		t.Errorf("a kept object's bytes went: %v", err)
	}

	// Replaying the same manifest changes nothing but the boundary.
	var replay syncView
	i.expect(t, http.StatusOK, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest, &replay)
	if replay.DeletedFiles != 0 || replay.SyncedFiles != 1 {
		t.Fatalf("the replay answered %+v", replay)
	}
	if !replay.LastSync.After(result.LastSync) && !replay.LastSync.Equal(result.LastSync) {
		t.Errorf("the replay moved the boundary backwards: %v after %v", replay.LastSync, result.LastSync)
	}

	// The same attachment materializes the post-sync state.
	var after manifestView
	i.expect(t, http.StatusOK, http.MethodGet,
		"/v1/workspaces/"+ws.ID+"/materialize?attachment="+attached.ID, nil, &after)
	if len(after.Files) != 1 || after.Files[0].Path != "src/main.go" {
		t.Fatalf("the re-pinned snapshot is %+v", after.Files)
	}
	// And the reader's, pinned at its own attach, omits the path whose row
	// the sync removed rather than serving a URL that answers nothing.
	var readerSnapshot manifestView
	i.expect(t, http.StatusOK, http.MethodGet,
		"/v1/workspaces/"+ws.ID+"/materialize?attachment="+reader.ID, nil, &readerSnapshot)
	if len(readerSnapshot.Files) != 1 || readerSnapshot.Files[0].Path != "src/main.go" {
		t.Fatalf("the reader's snapshot is %+v", readerSnapshot.Files)
	}

	// A sync from the reader holds no lease.
	code, body = i.api(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": reader.ID, "files": []map[string]any{},
	})
	if code != http.StatusConflict || errorCode(t, body) != "lease_not_held" {
		t.Fatalf("a sync from a reader = %d %s", code, body)
	}

	// The release frees the lease and is idempotent, and the next writer
	// takes the workspace.
	for range 2 {
		code, body = i.api(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+attached.ID, nil)
		if code != http.StatusNoContent {
			t.Fatalf("the release = %d %s", code, body)
		}
	}
	var free workspaceView
	i.expect(t, http.StatusOK, http.MethodGet, "/v1/workspaces/"+ws.ID, nil, &free)
	if free.Lease != nil {
		t.Fatalf("the release left the lease held: %+v", free.Lease)
	}
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_b", "mode": "rw"}, nil)

	// A released attachment is gone: the sandbox attaches again rather than
	// continuing against a snapshot it no longer holds a claim to.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/workspaces/" + ws.ID + "/materialize?attachment=" + attached.ID, nil},
		{http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach/" + attached.ID + "/renew", nil},
		{http.MethodPost, "/v1/workspaces/" + ws.ID + "/sync", manifest},
	} {
		code, body = i.api(t, c.method, c.path, c.body)
		if code != http.StatusGone || errorCode(t, body) != "attachment_gone" {
			t.Errorf("%s against a released attachment = %d %s", c.method, code, body)
		}
	}
}

// TestE2EAWorkspaceIsDeletedListedAndRestored is the other half of the
// record's life through the binary: the soft delete, the listing of what is
// still restorable, and the restore.
func TestE2EAWorkspaceIsDeletedListedAndRestored(t *testing.T) {
	i := start(t)
	i.allowEverything()

	var ws workspaceView
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces", map[string]any{"slug": "scratch"}, &ws)

	// A delete while the lease is held is refused: the writer's view of its
	// own paths would change underneath it.
	var attached attachmentView
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces/"+ws.ID+"/attach",
		map[string]any{"sandbox_id": "sbx_a", "mode": "rw"}, &attached)
	code, body := i.api(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil)
	if code != http.StatusConflict || errorCode(t, body) != "writer_held" {
		t.Fatalf("a delete while the lease is held = %d %s", code, body)
	}
	code, body = i.api(t, http.MethodPatch, "/v1/workspaces/"+ws.ID, map[string]any{"slug": "renamed"})
	if code != http.StatusConflict || errorCode(t, body) != "writer_held" {
		t.Fatalf("a rename while the lease is held = %d %s", code, body)
	}
	i.expect(t, http.StatusNoContent, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+attached.ID, nil, nil)

	// A rename moves the record and the slug it held is free.
	var renamed workspaceView
	i.expect(t, http.StatusOK, http.MethodPatch, "/v1/workspaces/"+ws.ID,
		map[string]any{"slug": "kept"}, &renamed)
	if renamed.Slug != "kept" || renamed.RootPrefix != "workspaces/kept/" {
		t.Fatalf("the renamed workspace is %+v", renamed)
	}

	i.expect(t, http.StatusNoContent, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil, nil)
	if code, _ := i.api(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil); code != http.StatusNotFound {
		t.Fatalf("a deleted workspace reads %d", code)
	}
	var live, tombstones pageView
	i.expect(t, http.StatusOK, http.MethodGet, "/v1/workspaces", nil, &live)
	if slices.ContainsFunc(live.Entries, func(w workspaceView) bool { return w.ID == ws.ID }) {
		t.Errorf("a deleted workspace is in the live listing: %+v", live.Entries)
	}
	i.expect(t, http.StatusOK, http.MethodGet, "/v1/workspaces/deleted", nil, &tombstones)
	found := slices.IndexFunc(tombstones.Entries, func(w workspaceView) bool { return w.ID == ws.ID })
	if found < 0 || tombstones.Entries[found].DeletedAt == nil {
		t.Fatalf("the deleted listing holds %+v", tombstones.Entries)
	}
	// The tombstone keeps its slug reserved, which is what makes the restore
	// guard-free.
	code, body = i.api(t, http.MethodPost, "/v1/workspaces", map[string]any{"slug": "kept"})
	if code != http.StatusConflict || errorCode(t, body) != "slug_taken" {
		t.Fatalf("a tombstone's slug = %d %s", code, body)
	}
	var back workspaceView
	i.expect(t, http.StatusOK, http.MethodPost, "/v1/workspaces/"+ws.ID+"/restore", nil, &back)
	if back.DeletedAt != nil || back.Slug != "kept" {
		t.Fatalf("the restored workspace is %+v", back)
	}
	// Restoring a live workspace is a conflict; restoring one the reaper
	// purged is not-found.
	if code, _ = i.api(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/restore", nil); code != http.StatusConflict {
		t.Errorf("restoring a live workspace = %d", code)
	}
	gone := fmt.Sprintf("/v1/workspaces/%s/restore", "2b7e1f4c-4a6d-4c3e-9b1e-1f4c4a6d4c3e")
	if code, _ = i.api(t, http.MethodPost, gone, nil); code != http.StatusNotFound {
		t.Errorf("restoring a purged id = %d", code)
	}
}

// TestE2ETheWorkspaceRoutesAreDescribedByTheDocumentTheServerServes is the
// e2e half of criterion 13 of spec 013 for this prefix: the description a
// client generates against names every route the server registers, and it is
// served without a token.
func TestE2ETheWorkspaceRoutesAreDescribedByTheDocumentTheServerServes(t *testing.T) {
	i := start(t)
	code, body := get(t, i.publicURL+"/openapi.json")
	if code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d", code)
	}
	var document struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	for _, want := range []struct{ path, method string }{
		{"/v1/workspaces", "post"},
		{"/v1/workspaces", "get"},
		{"/v1/workspaces/deleted", "get"},
		{"/v1/workspaces/{id}", "get"},
		{"/v1/workspaces/{id}", "patch"},
		{"/v1/workspaces/{id}", "delete"},
		{"/v1/workspaces/{id}/restore", "post"},
		{"/v1/workspaces/{id}/attach", "post"},
		{"/v1/workspaces/{id}/attach/{aid}/renew", "post"},
		{"/v1/workspaces/{id}/attach/{aid}", "delete"},
		{"/v1/workspaces/{id}/materialize", "get"},
		{"/v1/workspaces/{id}/sync", "post"},
	} {
		if _, described := document.Paths[want.path][want.method]; !described {
			t.Errorf("the document does not describe %s %s", strings.ToUpper(want.method), want.path)
		}
	}
}

// TestE2EAWorkspaceRouteRefusesACallerTheAuthorizerDoesNot is invariant 6 of
// spec 001 through the binary: a deny at lookup is a 404 and not a 403, so a
// refused workspace and a missing one are one answer.
func TestE2EAWorkspaceRouteRefusesACallerTheAuthorizerDoesNot(t *testing.T) {
	i := start(t)
	i.allowEverything()
	var ws workspaceView
	i.expect(t, http.StatusCreated, http.MethodPost, "/v1/workspaces", map[string]any{"slug": "private"}, &ws)

	// The endpoint refuses the one action, and a later rule wins over an
	// earlier one, so the narrow refusal goes last.
	i.authorizer.SetRules()
	i.allowEverything()
	i.authorizer.Deny(stub.Rule{Subject: "*", Action: "workspace.read", Resource: "*"}, "no rule allows it")
	code, refused := i.api(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	if code != http.StatusNotFound || errorCode(t, refused) != "not_found" {
		t.Fatalf("a denied workspace = %d %s", code, refused)
	}
	// A workspace that is not there answers the same code, so a request
	// cannot be written to enumerate what somebody else owns.
	absent, _ := i.api(t, http.MethodGet, "/v1/workspaces/2b7e1f4c-4a6d-4c3e-9b1e-1f4c4a6d4c3e", nil)
	if absent != http.StatusNotFound {
		t.Errorf("a workspace that is not there = %d", absent)
	}
	// A request with no bearer at all meets the verifier before it meets a
	// route, so whether a workspace exists is not something an
	// unauthenticated caller learns.
	if bare, _ := get(t, i.publicURL+"/v1/workspaces/"+ws.ID); bare != http.StatusUnauthorized {
		t.Errorf("an unauthenticated read = %d", bare)
	}
}
