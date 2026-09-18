// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// seed writes one object under a workspace root and answers the id its bytes
// were written under, so a test can check the key a presign named.
func (h *harness) seed(t *testing.T, ws Workspace, path, checksum string, size int64) object.ID {
	t.Helper()
	id := h.store.put(h.subject, Root(ws.Slug)+path, checksum, size)
	if _, err := h.bucket.Put(t.Context(), id.Key("arca/"), strings.NewReader(strings.Repeat("x", int(size))), size, blob.PutOptions{}); err != nil {
		t.Fatalf("seed %q: %v", path, err)
	}
	return id
}

// materialize reads one attachment's snapshot.
func (h *harness) materialize(t *testing.T, ws Workspace, a Attachment) Manifest {
	t.Helper()
	got := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID+"/materialize?attachment="+a.ID, nil)
	if got.code != http.StatusOK {
		t.Fatalf("the materialize = %d: %s", got.code, got.body)
	}
	var m Manifest
	got.decode(t, &m)
	return m
}

func TestMaterializePinsToTheAttachmentAndSignsTheKeyTheRowNames(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	main := h.seed(t, ws, "src/main.go", "4f9a", 12)
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "ro")

	m := h.materialize(t, ws, a)
	if m.Root != "workspaces/build/" || m.PinnedAt == nil {
		t.Fatalf("the manifest is %+v", m)
	}
	if len(m.Files) != 2 {
		t.Fatalf("the manifest holds %d files", len(m.Files))
	}
	// A URL is signed against the key the object id derives, and never
	// against a key recomputed from the path: an object assembled from parts
	// carries a key the path does not predict.
	var signed string
	for _, f := range m.Files {
		if f.URL == "" {
			t.Errorf("%q carries no URL", f.Path)
		}
		if f.Path == "src/main.go" {
			signed = f.URL
			if f.Checksum != "4f9a" || f.Size != 12 {
				t.Errorf("the entry is %+v", f)
			}
		}
	}
	if !strings.Contains(signed, string(main)) {
		t.Errorf("the URL %q does not name the object %s", signed, main)
	}
	// One presign per file and no other bucket call: the bytes never pass
	// through arcad.
	if got := h.bucket.Calls(blob.MethodPresignGet); got != 2 {
		t.Errorf("the materialize presigned %d times", got)
	}
	if got := h.bucket.Calls(blob.MethodGet); got != 0 {
		t.Errorf("the materialize read %d objects through the server", got)
	}
}

func TestMaterializeOmitsAPathWhoseRowIsGone(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.seed(t, ws, "src/main.go", "4f9a", 12)
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "ro")

	// The snapshot was pinned at attach; the row goes afterwards.
	if _, _, err := h.store.Drop(t.Context(), nil, h.subject, []string{Root(ws.Slug) + "bin/app"}); err != nil {
		t.Fatal(err)
	}
	m := h.materialize(t, ws, a)
	if len(m.Files) != 1 || m.Files[0].Path != "src/main.go" {
		t.Fatalf("the manifest holds %+v", m.Files)
	}
	// And a path written after the snapshot is not in it: the attachment
	// sees the tree it attached to.
	h.seed(t, ws, "src/new.go", "beef", 4)
	if again := h.materialize(t, ws, a); len(again.Files) != 1 {
		t.Fatalf("the snapshot grew to %+v", again.Files)
	}
}

func TestMaterializeRefusesWithoutAnAttachmentItCanServe(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")
	other := h.create(t, "release")

	for _, c := range []struct {
		name, query string
		code        int
	}{
		{"no attachment named", "", http.StatusBadRequest},
		{"an attachment that is not there", "?attachment=att-9999", http.StatusNotFound},
	} {
		got := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID+"/materialize"+c.query, nil)
		if got.code != c.code {
			t.Errorf("%s = %d: %s", c.name, got.code, got.body)
		}
	}
	// An attachment of another workspace names nothing here.
	across := h.do(t, http.MethodGet, "/v1/workspaces/"+other.ID+"/materialize?attachment="+a.ID, nil)
	if across.code != http.StatusNotFound {
		t.Errorf("an attachment of another workspace = %d", across.code)
	}
	// A released attachment is gone: the sandbox must attach again rather
	// than read a snapshot it no longer holds a claim to.
	h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+a.ID, nil)
	released := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID+"/materialize?attachment="+a.ID, nil)
	if code := released.errorCode(t); released.code != http.StatusGone || code != "attachment_gone" {
		t.Errorf("a released attachment = %d %q", released.code, code)
	}
}

func TestASyncDeletesWhatTheManifestDropsAndKeepsTheRest(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	kept := h.seed(t, ws, "src/main.go", "4f9a", 12)
	dropped := h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "rw")
	h.seed(t, ws, "src/new.go", "beef", 4)

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files": []map[string]any{
			{"path": "src/main.go", "checksum": "4f9a", "size": 12},
			{"path": "src/new.go", "checksum": "beef", "size": 4},
		},
	})
	if got.code != http.StatusOK {
		t.Fatalf("the sync = %d: %s", got.code, got.body)
	}
	var result SyncResult
	got.decode(t, &result)
	if result.SyncedFiles != 2 || result.DeletedFiles != 1 || result.LastSync.IsZero() {
		t.Fatalf("the sync answered %+v", result)
	}
	rows, err := h.store.Manifest(t.Context(), nil, h.subject, Root(ws.Slug))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("the subtree holds %+v", rows)
	}
	// Rows first, keys after, which is invariant 1 of spec 001: the bytes
	// the rows no longer name are removed and the ones still named are not.
	if got := h.bucket.Calls(blob.MethodDeleteMany); got != 1 {
		t.Errorf("the sync made %d batch deletes", got)
	}
	if _, err := h.bucket.Head(t.Context(), dropped.Key("arca/")); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the dropped object's bytes are still there: %v", err)
	}
	if _, err := h.bucket.Head(t.Context(), kept.Key("arca/")); err != nil {
		t.Errorf("a kept object's bytes went: %v", err)
	}
	// The boundary is stamped and the usage the drop freed is given back.
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.LastSync == nil {
		t.Error("the sync stamped no boundary")
	}
	h.ledger.mu.Lock()
	freed := h.ledger.released[h.subject]
	h.ledger.mu.Unlock()
	if freed != 8 {
		t.Errorf("the sync gave back %d bytes, want the 8 it dropped", freed)
	}
	if !slices.Contains(h.ledger.actions(), ActionSync) {
		t.Errorf("the sync appended %v", h.ledger.actions())
	}
}

func TestASyncNamingAPathThatWasNeverUploadedWritesNothing(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.seed(t, ws, "src/main.go", "4f9a", 12)
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "rw")

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files": []map[string]any{
			{"path": "src/main.go", "checksum": "4f9a", "size": 12},
			{"path": "bin/never", "checksum": "0000", "size": 1},
			{"path": "bin/also-never", "checksum": "0000", "size": 1},
		},
	})
	if code := got.errorCode(t); got.code != http.StatusConflict || code != "manifest_incomplete" {
		t.Fatalf("a manifest naming an object nobody uploaded = %d %q", got.code, code)
	}
	// The missing paths are listed, so the client finishes its puts and
	// syncs again rather than guessing.
	if fields := got.fields(t); !slices.Equal(fields, []string{"bin/also-never", "bin/never"}) {
		t.Errorf("the refusal named %v", fields)
	}
	// The whole sync wrote nothing: the row the manifest dropped is still
	// there, the boundary is unstamped, and no byte left the bucket. This is
	// the bug the service Arca replaces carried, where the delete ran before
	// the check.
	rows, err := h.store.Manifest(t.Context(), nil, h.subject, Root(ws.Slug))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("a refused sync left %+v", rows)
	}
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.LastSync != nil {
		t.Error("a refused sync stamped a boundary")
	}
	if got := h.bucket.Calls(blob.MethodDeleteMany); got != 0 {
		t.Errorf("a refused sync deleted bytes %d times", got)
	}
	if slices.Contains(h.ledger.actions(), ActionSync) {
		t.Errorf("a refused sync appended %v", h.ledger.actions())
	}
}

func TestReplayingASyncChangesNothingButTheBoundary(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.seed(t, ws, "src/main.go", "4f9a", 12)
	a := h.attach(t, ws, "sbx_a", "rw")
	manifest := map[string]any{
		"attachment_id": a.ID,
		"files":         []map[string]any{{"path": "src/main.go", "checksum": "4f9a", "size": 12}},
	}
	first := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest)
	if first.code != http.StatusOK {
		t.Fatalf("the first sync = %d: %s", first.code, first.body)
	}
	before := h.bucket.Total()
	second := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest)
	if second.code != http.StatusOK {
		t.Fatalf("the replay = %d: %s", second.code, second.body)
	}
	var result SyncResult
	second.decode(t, &result)
	if result.DeletedFiles != 0 {
		t.Errorf("the replay deleted %d files", result.DeletedFiles)
	}
	if h.bucket.Total() != before {
		t.Errorf("the replay made %d bucket calls", h.bucket.Total()-before)
	}
	rows, err := h.store.Manifest(t.Context(), nil, h.subject, Root(ws.Slug))
	if err != nil || len(rows) != 1 {
		t.Fatalf("the replay left %+v, %v", rows, err)
	}
}

func TestASyncRePinsTheAttachmentToWhatTheRowsHold(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.seed(t, ws, "src/main.go", "4f9a", 12)
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "rw")

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files":         []map[string]any{{"path": "src/main.go", "checksum": "4f9a", "size": 12}},
	})
	if got.code != http.StatusOK {
		t.Fatalf("the sync = %d: %s", got.code, got.body)
	}
	// The same attachment materializes the post-sync state, so a runtime
	// that syncs periodically keeps one attachment for a whole run.
	m := h.materialize(t, ws, a)
	if len(m.Files) != 1 || m.Files[0].Path != "src/main.go" {
		t.Fatalf("the re-pinned snapshot is %+v", m.Files)
	}
	// And the pin moves to the boundary the sync took.
	if m.PinnedAt == nil || !m.PinnedAt.After(a.ExpiresAt.Add(-DefaultTTL)) {
		t.Errorf("the pin is %v", m.PinnedAt)
	}
}

func TestASyncThatIsNotTheWritersIsRefused(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	reader := h.attach(t, ws, "sbx_r", "ro")
	writer := h.attach(t, ws, "sbx_w", "rw")
	manifest := func(id string) map[string]any {
		return map[string]any{"attachment_id": id, "files": []map[string]any{}}
	}

	// A read-only attachment holds no lease, so it declares no boundary.
	fromReader := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest(reader.ID))
	if code := fromReader.errorCode(t); fromReader.code != http.StatusConflict || code != "lease_not_held" {
		t.Fatalf("a sync from a reader = %d %q", fromReader.code, code)
	}
	// A released writer's attachment is gone rather than forbidden: it must
	// attach again and materialize again.
	h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID+"/attach/"+writer.ID, nil)
	fromReleased := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest(writer.ID))
	if code := fromReleased.errorCode(t); fromReleased.code != http.StatusGone || code != "attachment_gone" {
		t.Fatalf("a sync from a released attachment = %d %q", fromReleased.code, code)
	}
	// A writer whose lease lapsed and moved on is told it no longer holds it.
	zombie := h.attach(t, ws, "sbx_z", "rw")
	h.travel(DefaultTTL + time.Minute)
	h.attach(t, ws, "sbx_next", "rw")
	fromZombie := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest(zombie.ID))
	if code := fromZombie.errorCode(t); fromZombie.code != http.StatusConflict || code != "lease_not_held" {
		t.Fatalf("a sync from a zombie writer = %d %q", fromZombie.code, code)
	}
	// And a sync naming no attachment at all.
	unnamed := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{"files": []map[string]any{}})
	if code := unnamed.errorCode(t); code != "missing_field" {
		t.Errorf("a sync naming no attachment = %d %q", unnamed.code, code)
	}
	absent := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", manifest("att-9999"))
	if absent.code != http.StatusNotFound {
		t.Errorf("a sync naming an attachment that is not there = %d", absent.code)
	}
}

func TestAManifestPathThatLeavesTheWorkspaceIsRefused(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	a := h.attach(t, ws, "sbx_a", "rw")
	for _, c := range []struct {
		path string
		code string
	}{
		{"../escape", "invalid_path"},
		{"src/../../escape", "invalid_path"},
		{"/absolute", "invalid_path"},
		{"", "invalid_path"},
	} {
		got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
			"attachment_id": a.ID,
			"files":         []map[string]any{{"path": c.path, "checksum": "0", "size": 0}},
		})
		if code := got.errorCode(t); code != c.code {
			t.Errorf("the path %q = %d %q, want %s", c.path, got.code, code, c.code)
		}
	}
	// A path named twice is a manifest that says two things about one file.
	twice := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files": []map[string]any{
			{"path": "src/main.go", "checksum": "a", "size": 1},
			{"path": "./src/main.go", "checksum": "b", "size": 2},
		},
	})
	if code := twice.errorCode(t); code != "invalid_field" {
		t.Errorf("a path named twice = %d %q", twice.code, code)
	}
	negative := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID,
		"files":         []map[string]any{{"path": "src/main.go", "checksum": "a", "size": -1}},
	})
	if code := negative.errorCode(t); code != "invalid_field" {
		t.Errorf("a negative size = %d %q", negative.code, code)
	}
}

func TestBytesAnotherRowStillNamesSurviveASync(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	shared := h.seed(t, ws, "bin/app", "11c0", 8)
	// A second row names the same object, which is what a copy leaves.
	h.store.mu.Lock()
	h.store.files[h.subject]["files/keepsake"] = store.WorkspaceFile{
		Path: "files/keepsake", Checksum: "11c0", Size: 8, ObjectID: shared,
	}
	h.store.mu.Unlock()
	a := h.attach(t, ws, "sbx_a", "rw")

	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID, "files": []map[string]any{},
	})
	if got.code != http.StatusOK {
		t.Fatalf("the sync = %d: %s", got.code, got.body)
	}
	if _, err := h.bucket.Head(t.Context(), shared.Key("arca/")); err != nil {
		t.Errorf("bytes another row still names were removed: %v", err)
	}
}

func TestASyncWhoseBucketRefusesStillRecordsTheBoundary(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "rw")
	// The rows are gone before the bucket is called, so a failure there
	// leaves an object with no row, which is what the reaper of spec 010
	// finds. Failing the request would tell a writer its sync did not happen
	// when it did.
	h.bucket.FailNth(blob.MethodDeleteMany, 1, errors.New("the bucket is unavailable"))
	got := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/sync", map[string]any{
		"attachment_id": a.ID, "files": []map[string]any{},
	})
	if got.code != http.StatusOK {
		t.Fatalf("a sync whose sweep failed = %d: %s", got.code, got.body)
	}
	rows, err := h.store.Manifest(t.Context(), nil, h.subject, Root(ws.Slug))
	if err != nil || len(rows) != 0 {
		t.Fatalf("the rows are %+v, %v", rows, err)
	}
}

func TestAMountFaultIsNeverAMissingWorkspace(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.seed(t, ws, "bin/app", "11c0", 8)
	a := h.attach(t, ws, "sbx_a", "rw")
	manifest := map[string]any{"attachment_id": a.ID, "files": []map[string]any{}}

	for _, c := range []struct {
		fail         string
		method, path string
		body         any
	}{
		{"Manifest", http.MethodGet, "/v1/workspaces/" + ws.ID + "/materialize?attachment=" + a.ID, nil},
		{"GetAttachment", http.MethodGet, "/v1/workspaces/" + ws.ID + "/materialize?attachment=" + a.ID, nil},
		{"Manifest", http.MethodPost, "/v1/workspaces/" + ws.ID + "/sync", manifest},
		{"Drop", http.MethodPost, "/v1/workspaces/" + ws.ID + "/sync", manifest},
		{"Unreferenced", http.MethodPost, "/v1/workspaces/" + ws.ID + "/sync", manifest},
		{"StampSync", http.MethodPost, "/v1/workspaces/" + ws.ID + "/sync", manifest},
		{"SetManifest", http.MethodPost, "/v1/workspaces/" + ws.ID + "/sync", manifest},
	} {
		h.store.fail = map[string]error{c.fail: errors.New("the connection failed")}
		got := h.do(t, c.method, c.path, c.body)
		if got.code < 500 {
			t.Errorf("%s while %s fails answered %d: %s", c.method, c.fail, got.code, got.body)
		}
		if strings.Contains(string(got.body), "connection failed") {
			t.Errorf("the 5xx named the store: %s", got.body)
		}
	}
	h.store.fail = map[string]error{}
	// A bucket that will not sign is a fault and not an empty manifest.
	h.bucket.FailNth(blob.MethodPresignGet, 1, errors.New("the bucket is unavailable"))
	got := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID+"/materialize?attachment="+a.ID, nil)
	if got.code < 500 {
		t.Errorf("a materialize that could not sign = %d: %s", got.code, got.body)
	}
	// A manifest the column does not hold as JSON is a fault of this server
	// and not a request a caller can fix.
	if _, err := decode(store.Attachment{Manifest: []byte("not json")}); err == nil {
		t.Error("a manifest that is not JSON decoded")
	}
}

func TestThePinIsWhenTheSnapshotWasTaken(t *testing.T) {
	attached := time.Now().Add(-time.Hour)
	synced := time.Now().Add(-time.Minute)
	holder := "sbx_a"
	elsewhere := "sbx_b"
	for _, c := range []struct {
		name string
		ws   store.Workspace
		a    store.Attachment
		want time.Time
	}{
		{
			"a reader pins at attach",
			store.Workspace{LastSync: &synced, WriterHolder: &holder},
			store.Attachment{Mode: store.ModeRead, Holder: holder, CreatedAt: attached},
			attached,
		},
		{
			"a writer that has not synced pins at attach",
			store.Workspace{WriterHolder: &holder},
			store.Attachment{Mode: store.ModeWrite, Holder: holder, CreatedAt: attached},
			attached,
		},
		{
			"a writer that synced pins at its boundary",
			store.Workspace{LastSync: &synced, WriterHolder: &holder},
			store.Attachment{Mode: store.ModeWrite, Holder: holder, CreatedAt: attached},
			synced,
		},
		{
			"a writer whose lease moved on pins at attach",
			store.Workspace{LastSync: &synced, WriterHolder: &elsewhere},
			store.Attachment{Mode: store.ModeWrite, Holder: holder, CreatedAt: attached},
			attached,
		},
		{
			"a boundary older than the attach pins at attach",
			store.Workspace{LastSync: &attached, WriterHolder: &holder},
			store.Attachment{Mode: store.ModeWrite, Holder: holder, CreatedAt: synced},
			synced,
		},
	} {
		if got := pinnedAt(c.ws, c.a); !got.Equal(c.want) {
			t.Errorf("%s: the pin is %v, want %v", c.name, got, c.want)
		}
	}
}
