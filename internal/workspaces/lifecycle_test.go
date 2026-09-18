// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
)

func TestAWorkspaceIsCreatedUnderTheCallersOwnSpaceWithADerivedRoot(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	if ws.Owner != h.subject {
		t.Errorf("the workspace is owned by %q, and the caller is %q", ws.Owner, h.subject)
	}
	// The root prefix derives from the slug and is never stored, which is
	// what makes a rename a row update.
	if ws.RootPrefix != "workspaces/build/" {
		t.Errorf("the root prefix is %q", ws.RootPrefix)
	}
	if ws.Lease != nil || ws.LastSync != nil {
		t.Errorf("a fresh workspace carries a lease or a sync: %+v", ws)
	}
	if ws.CreatedBy != h.subject {
		t.Errorf("the workspace was created by %q", ws.CreatedBy)
	}
	if got := h.asked(); !slices.Equal(got, []string{"workspace.create"}) {
		t.Errorf("the create asked %v", got)
	}
}

func TestASlugIsHeldToTheOneShapeARootDerivesFrom(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		slug string
		code string
	}{
		{"", "missing_field"},
		{"Build", "invalid_field"},
		{"-build", "invalid_field"},
		{"build/sub", "invalid_field"},
		{"build space", "invalid_field"},
		{strings.Repeat("a", 65), "invalid_field"},
	} {
		got := h.do(t, http.MethodPost, "/v1/workspaces", map[string]any{"slug": c.slug})
		if code := got.errorCode(t); code != c.code {
			t.Errorf("the slug %q = %d %q, want %s", c.slug, got.code, code, c.code)
		}
		if fields := got.fields(t); !slices.Contains(fields, "slug") {
			t.Errorf("the slug %q was refused naming %v", c.slug, fields)
		}
	}
}

func TestASlugCollidesAcrossLiveAndDeletedWorkspacesAlike(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	again := h.do(t, http.MethodPost, "/v1/workspaces", map[string]any{"slug": "build"})
	if code := again.errorCode(t); again.code != http.StatusConflict || code != "slug_taken" {
		t.Fatalf("a taken slug = %d %q", again.code, code)
	}
	// A tombstone keeps its slug reserved until purge, which is what makes
	// restore guard-free: nothing can have taken the name in the meantime.
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d: %s", got.code, got.body)
	}
	deleted := h.do(t, http.MethodPost, "/v1/workspaces", map[string]any{"slug": "build"})
	if code := deleted.errorCode(t); deleted.code != http.StatusConflict || code != "slug_taken" {
		t.Fatalf("a deleted workspace's slug = %d %q", deleted.code, code)
	}
}

func TestAWorkspaceReadCarriesTheCountersOfItsSubtree(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.store.put(h.subject, "workspaces/build/src/main.go", "4f9a", 2814)
	h.store.put(h.subject, "workspaces/build/bin/app", "11c0", 5120000)
	// An object outside the root is not this workspace's.
	h.store.put(h.subject, "files/notes.md", "aaaa", 10)

	got := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	if got.code != http.StatusOK {
		t.Fatalf("the read = %d: %s", got.code, got.body)
	}
	var read Workspace
	got.decode(t, &read)
	if read.Files == nil || *read.Files != 2 || read.Bytes == nil || *read.Bytes != 5122814 {
		t.Fatalf("the counters are %+v", read)
	}
	if got := h.asked(); !slices.Equal(got, []string{"workspace.create", "workspace.read"}) {
		t.Errorf("the read asked %v", got)
	}
}

func TestAWorkspaceThatIsNotThereAndOneThatIsRefusedAreOneAnswer(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	absent := h.do(t, http.MethodGet, "/v1/workspaces/ws-9999", nil)
	if absent.code != http.StatusNotFound {
		t.Fatalf("a workspace that is not there = %d", absent.code)
	}

	// A deny at lookup is the same not-found, byte for byte, which is
	// invariant 6 of spec 001: a request cannot be written to enumerate what
	// somebody else owns.
	h.endpoint.SetRules()
	h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")
	refused := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	if refused.code != http.StatusNotFound {
		t.Fatalf("a refused workspace = %d: %s", refused.code, refused.body)
	}
	if refused.errorCode(t) != "not_found" || absent.errorCode(t) != "not_found" {
		t.Fatalf("the two refusals carry different codes")
	}
}

func TestASoftDeletedWorkspaceIsHiddenFromEveryRouteButTheRestore(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d", got.code)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/workspaces/" + ws.ID, nil},
		{http.MethodPatch, "/v1/workspaces/" + ws.ID, map[string]any{"slug": "release"}},
		{http.MethodDelete, "/v1/workspaces/" + ws.ID, nil},
		{http.MethodPost, "/v1/workspaces/" + ws.ID + "/attach", map[string]any{"sandbox_id": "sbx", "mode": "ro"}},
	} {
		got := h.do(t, c.method, c.path, c.body)
		if got.code != http.StatusNotFound {
			t.Errorf("%s %s against a deleted workspace = %d: %s", c.method, c.path, got.code, got.body)
		}
	}
	// It is listed where it can be found and restored, and nowhere else.
	var visible, tombstones page
	h.do(t, http.MethodGet, "/v1/workspaces", nil).decode(t, &visible)
	if len(visible.Entries) != 0 {
		t.Errorf("a deleted workspace is in the live listing: %+v", visible.Entries)
	}
	h.do(t, http.MethodGet, "/v1/workspaces/deleted", nil).decode(t, &tombstones)
	if len(tombstones.Entries) != 1 || tombstones.Entries[0].DeletedAt == nil {
		t.Fatalf("the deleted listing holds %+v", tombstones.Entries)
	}
}

func TestRestoreBringsBackADeletedWorkspaceAndRefusesALiveOne(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	live := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/restore", nil)
	if live.code != http.StatusConflict {
		t.Fatalf("restoring a live workspace = %d: %s", live.code, live.body)
	}
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d", got.code)
	}
	restored := h.do(t, http.MethodPost, "/v1/workspaces/"+ws.ID+"/restore", nil)
	if restored.code != http.StatusOK {
		t.Fatalf("the restore = %d: %s", restored.code, restored.body)
	}
	var back Workspace
	restored.decode(t, &back)
	if back.DeletedAt != nil || back.Slug != "build" {
		t.Fatalf("the restored workspace is %+v", back)
	}
	if !slices.Contains(h.ledger.actions(), ActionRestore) {
		t.Errorf("the restore appended %v", h.ledger.actions())
	}
	// A workspace the reaper already purged is gone, not a conflict.
	purged := h.do(t, http.MethodPost, "/v1/workspaces/ws-9999/restore", nil)
	if purged.code != http.StatusNotFound {
		t.Fatalf("restoring a purged id = %d", purged.code)
	}
}

func TestARenameMovesTheRowsAndReachesNoBucket(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.store.put(h.subject, "workspaces/build/src/main.go", "4f9a", 2814)
	h.store.put(h.subject, "workspaces/build/bin/app", "11c0", 5120000)

	got := h.do(t, http.MethodPatch, "/v1/workspaces/"+ws.ID, map[string]any{"slug": "release"})
	if got.code != http.StatusOK {
		t.Fatalf("the rename = %d: %s", got.code, got.body)
	}
	var renamed Workspace
	got.decode(t, &renamed)
	if renamed.Slug != "release" || renamed.RootPrefix != "workspaces/release/" {
		t.Fatalf("the renamed workspace is %+v", renamed)
	}
	moved, err := h.store.Manifest(t.Context(), nil, h.subject, "workspaces/release/")
	if err != nil || len(moved) != 2 {
		t.Fatalf("the subtree moved to %v, %v", moved, err)
	}
	// Invariant 8 of spec 001: a key derives from the object id the row
	// carries, so a rename of a whole subtree is rows and nothing else.
	if h.bucket.Total() != 0 {
		t.Errorf("the rename made %d bucket calls", h.bucket.Total())
	}
}

func TestARenameToTheSameSlugChangesNothing(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	before := h.store.txs
	got := h.do(t, http.MethodPatch, "/v1/workspaces/"+ws.ID, map[string]any{"slug": "build"})
	if got.code != http.StatusOK {
		t.Fatalf("the rename = %d: %s", got.code, got.body)
	}
	if h.store.txs != before {
		t.Errorf("a rename to the same slug opened %d transactions", h.store.txs-before)
	}
}

func TestARenameToATakenSlugIsAConflict(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.create(t, "release")
	got := h.do(t, http.MethodPatch, "/v1/workspaces/"+ws.ID, map[string]any{"slug": "release"})
	if code := got.errorCode(t); got.code != http.StatusConflict || code != "slug_taken" {
		t.Fatalf("a taken slug = %d %q", got.code, code)
	}
}

func TestARenameAndADeleteAreRefusedWhileAWriterHoldsTheWorkspace(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.attach(t, ws, "sbx_a", "rw")

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/v1/workspaces/" + ws.ID, map[string]any{"slug": "release"}},
		{http.MethodDelete, "/v1/workspaces/" + ws.ID, nil},
	} {
		got := h.do(t, c.method, c.path, c.body)
		if code := got.errorCode(t); got.code != http.StatusConflict || code != "writer_held" {
			t.Errorf("%s while the lease is held = %d %q: %s", c.method, got.code, code, got.body)
		}
	}
	// The workspace's own row is untouched: a refusal writes nothing.
	read := h.do(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	var after Workspace
	read.decode(t, &after)
	if after.Slug != "build" || after.Lease == nil {
		t.Fatalf("the refused rename changed the workspace: %+v", after)
	}
}

func TestALapsedLeaseDoesNotHoldARenameOrADelete(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	h.attach(t, ws, "sbx_a", "rw")
	// The lease is time bounded, so a holder past its deadline does not hold
	// the workspace and the rename is no longer refused.
	h.travel(DefaultTTL + time.Minute)
	got := h.do(t, http.MethodPatch, "/v1/workspaces/"+ws.ID, map[string]any{"slug": "release"})
	if got.code != http.StatusOK {
		t.Fatalf("a rename against a lapsed lease = %d: %s", got.code, got.body)
	}
}

func TestAListingPagesByKeysetAndHoldsItsLimits(t *testing.T) {
	h := newHarness(t)
	for _, slug := range []string{"one", "two", "three", "four"} {
		h.create(t, slug)
	}
	var first, second page
	h.do(t, http.MethodGet, "/v1/workspaces?limit=2", nil).decode(t, &first)
	if len(first.Entries) != 2 || first.NextCursor == "" {
		t.Fatalf("the first page is %+v", first)
	}
	h.do(t, http.MethodGet, "/v1/workspaces?limit=2&cursor="+first.NextCursor, nil).decode(t, &second)
	if len(second.Entries) != 2 {
		t.Fatalf("the second page is %+v", second)
	}
	// The cursor is absent on the last page, which is the only has-more
	// signal a client reads.
	if second.NextCursor != "" {
		t.Errorf("the last page carries the cursor %q", second.NextCursor)
	}
	for _, row := range second.Entries {
		if slices.ContainsFunc(first.Entries, func(w Workspace) bool { return w.ID == row.ID }) {
			t.Errorf("the walk returned %s twice", row.ID)
		}
	}
	// A listing counts nothing, so it carries no counters.
	if second.Entries[0].Files != nil {
		t.Errorf("a listing row carries counters: %+v", second.Entries[0])
	}
	over := h.do(t, http.MethodGet, "/v1/workspaces?limit=5000", nil)
	if code := over.errorCode(t); code != "invalid_field" {
		t.Errorf("a limit above the cap = %d %q", over.code, code)
	}
	empty := h.do(t, http.MethodGet, "/v1/workspaces", nil)
	if !strings.Contains(string(empty.body), `"entries"`) {
		t.Errorf("the envelope is %s", empty.body)
	}
}

func TestAListingIsScopedToTheSpaceItNames(t *testing.T) {
	h := newHarness(t)
	h.create(t, "mine")
	// The alias resolves to the caller's own space, and a subject names any
	// space the authorizer allows. A response renders the subject in full.
	var mine, theirs page
	h.do(t, http.MethodGet, "/v1/workspaces?owner=me", nil).decode(t, &mine)
	if len(mine.Entries) != 1 || mine.Entries[0].Owner != h.subject {
		t.Fatalf("the alias listed %+v", mine.Entries)
	}
	h.do(t, http.MethodGet, "/v1/workspaces?owner=https%3A%2F%2Felsewhere.example%7C7", nil).decode(t, &theirs)
	if len(theirs.Entries) != 0 {
		t.Fatalf("another space's listing holds %+v", theirs.Entries)
	}
}

func TestEveryRouteRefusesABodyItCannotRead(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")

	// A field the endpoint does not know is named rather than silently
	// dropped: the service Arca replaces took a kind here, and a client that
	// still sends one learns it at once.
	unknown := h.do(t, http.MethodPost, "/v1/workspaces",
		map[string]any{"slug": "other", "kind": "workspace"})
	if code := unknown.errorCode(t); code != "unknown_field" {
		t.Fatalf("an unknown field = %d %q", unknown.code, code)
	}
	if fields := unknown.fields(t); !slices.Contains(fields, "kind") {
		t.Errorf("the refusal named %v", fields)
	}

	// A body that is not JSON, and a content type that is not JSON.
	raw := httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader("{"))
	raw.Header.Set("Content-Type", "application/json")
	raw.Header.Set("Authorization", "Bearer "+h.bearer())
	if code := h.send(t, raw).errorCode(t); code != "bad_request" {
		t.Errorf("a body that is not JSON = %q", code)
	}
	form := httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader("slug=build"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.Header.Set("Authorization", "Bearer "+h.bearer())
	if code := h.send(t, form).errorCode(t); code != "unsupported_media_type" {
		t.Errorf("a form body = %q", code)
	}
	none := httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader("{}"))
	none.Header.Set("Authorization", "Bearer "+h.bearer())
	if code := h.send(t, none).errorCode(t); code != "unsupported_media_type" {
		t.Errorf("a body with no content type = %q", code)
	}
	twice := httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader(`{"slug":"a"}{"slug":"b"}`))
	twice.Header.Set("Content-Type", "application/json")
	twice.Header.Set("Authorization", "Bearer "+h.bearer())
	if code := h.send(t, twice).errorCode(t); code != "bad_request" {
		t.Errorf("two documents in one body = %q", code)
	}

	// A caller that takes no JSON is told before the handler reads a row.
	picky := h.request(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	picky.Header.Set("Accept", "text/csv")
	if code := h.send(t, picky).errorCode(t); code != "not_acceptable" {
		t.Errorf("a caller that accepts no JSON = %q", code)
	}
	broad := h.request(t, http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	broad.Header.Set("Accept", "text/html, */*;q=0.8")
	if got := h.send(t, broad); got.code != http.StatusOK {
		t.Errorf("a caller that accepts anything = %d", got.code)
	}
}

func TestAStoreFaultIsNeverAMissingWorkspace(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	for _, method := range []string{"Get", "Stat", "List", "Create", "GetForUpdate", "SoftDelete", "Restore"} {
		h.store.fail = map[string]error{method: errors.New("the connection failed")}
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/v1/workspaces/" + ws.ID, nil},
			{http.MethodGet, "/v1/workspaces", nil},
			{http.MethodPost, "/v1/workspaces", map[string]any{"slug": "other"}},
			{http.MethodDelete, "/v1/workspaces/" + ws.ID, nil},
			{http.MethodPost, "/v1/workspaces/" + ws.ID + "/restore", nil},
		} {
			got := h.do(t, c.method, c.path, c.body)
			if got.code == http.StatusNotFound {
				t.Errorf("%s while %s fails answered not-found", c.method, method)
			}
			if got.code >= 500 && strings.Contains(string(got.body), "connection failed") {
				t.Errorf("a 5xx named the store: %s", got.body)
			}
		}
	}
}

func TestTheServiceRefusesToBuildWithoutASeamItWouldReachThrough(t *testing.T) {
	full := Options{
		DB: newMemory(), Workspaces: newMemory(), Attachments: attachmentSet{newMemory()},
		Objects: newMemory(), Bucket: blob.NewMemory(), Authorizer: nil,
	}
	if _, err := New(full); err == nil {
		t.Fatal("a service with no authorizer was built")
	}
	for _, strip := range []func(*Options){
		func(o *Options) { o.DB = nil },
		func(o *Options) { o.Workspaces = nil },
		func(o *Options) { o.Attachments = nil },
		func(o *Options) { o.Objects = nil },
		func(o *Options) { o.Bucket = nil },
	} {
		o := full
		o.Authorizer = &auth.Authorizer{}
		strip(&o)
		if _, err := New(o); err == nil {
			t.Error("a service with a missing seam was built")
		} else if !strings.HasPrefix(err.Error(), "workspaces: ") {
			t.Errorf("the failure reads %q", err)
		}
	}
	// A service with every seam and no ledger writes nothing rather than
	// reaching through a nil.
	o := full
	o.Authorizer = &auth.Authorizer{}
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.ledger.Append(t.Context(), nil, Event{}); err != nil {
		t.Errorf("the silent ledger refused an append: %v", err)
	}
	if err := s.ledger.Release(t.Context(), nil, "owner", 1); err != nil {
		t.Errorf("the silent ledger refused a release: %v", err)
	}
	if s.now().IsZero() {
		t.Error("a service with no clock reads no time")
	}
}

// The id-addressed restore of spec 012, which the node binds as the
// workspace arm of the administrative restore across owners. It asks
// nothing: the caller asked space.admin before it got here, and
// workspace.restore is the owner's question, which an administrator acting
// on somebody else's space would be refused.
func TestRestoreDeletedBringsBackATombstoneOfTheSpaceItNames(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d", got.code)
	}
	// Every question is denied from here on. The restore still runs, which
	// is the property: it asks nothing, because the caller was allowed
	// space.admin before it reached this arm and workspace.restore is the
	// owner's question.
	h.endpoint.Deny(stub.Rule{Subject: "*", Action: "*", Resource: "*"}, "no rule allows it")

	back, err := h.service.RestoreDeleted(t.Context(), h.subject, ws.ID)
	if err != nil {
		t.Fatalf("RestoreDeleted: %v", err)
	}
	if back.Slug != "build" || back.DeletedAt != nil {
		t.Fatalf("the restored workspace is %+v", back)
	}
	read, err := h.store.Get(t.Context(), nil, ws.ID)
	if err != nil || read.DeletedAt != nil {
		t.Fatalf("the row is %+v, %v after the restore", read, err)
	}
	if !slices.Contains(h.ledger.actions(), ActionRestore) {
		t.Errorf("the restore appended %v and no restore row", h.ledger.actions())
	}
}

// The space is checked against the row and never taken from it. An id of
// another owner is a workspace nothing authorized this caller to touch, and
// it answers as an id that names nothing, so the node's adapter renders one
// refusal rather than restoring across a space boundary nobody asked about.
func TestRestoreDeletedRefusesWhatTheSpaceCannotBringBack(t *testing.T) {
	h := newHarness(t)
	live := h.create(t, "live")
	gone := h.create(t, "gone")
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+gone.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d", got.code)
	}
	for _, tc := range []struct {
		name  string
		owner string
		id    string
	}{
		{"another space's tombstone", "https://issuer.example|c1d0", gone.ID},
		{"an id that names nothing", h.subject, "no-such-workspace"},
		{"a live workspace", h.subject, live.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.service.RestoreDeleted(t.Context(), tc.owner, tc.id); !errors.Is(err, ErrNotDeleted) {
				t.Fatalf("RestoreDeleted answered %v", err)
			}
		})
	}
}

// A store that will not answer is a fault and not a missing workspace: the
// node's adapter must not read it as an id that names nothing.
func TestRestoreDeletedCarriesAStoreFailure(t *testing.T) {
	h := newHarness(t)
	ws := h.create(t, "build")
	if got := h.do(t, http.MethodDelete, "/v1/workspaces/"+ws.ID, nil); got.code != http.StatusNoContent {
		t.Fatalf("the delete = %d", got.code)
	}
	h.store.fail = map[string]error{"Restore": errors.New("the connection failed")}
	_, err := h.service.RestoreDeleted(t.Context(), h.subject, ws.ID)
	if err == nil || errors.Is(err, ErrNotDeleted) {
		t.Fatalf("a failed restore answered %v", err)
	}
}
