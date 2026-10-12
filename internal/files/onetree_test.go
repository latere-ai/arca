// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// Spec 028 at the unit tier: one plane, the two limits an answer turns
// versions and the trash off with, the folder delete, and the copy. The
// limit that leaves subtrees out of an enumeration is in omit_test.go.

// as drives one request as another of the issuer's subjects, which is how a
// case reaches the space of the harness as somebody who does not own it.
func (h *harness) as(t *testing.T, sub, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = h.request(method, target, strings.NewReader(body), api.HeaderContentType, api.JSONMediaType)
	} else {
		r = h.request(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer "+h.issuer.Mint(issuertest.Claims{Sub: sub}))
	return h.send(r)
}

// subject renders one of the issuer's subjects the way spec 006 does.
func (h *harness) subject(sub string) string { return h.issuer.URL() + "|" + sub }

// live answers the live paths of the harness's space, sorted.
func (h *harness) live() []string {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	var out []string
	for _, f := range h.store.files {
		if f.Owner == h.owner && f.DeletedAt == nil {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return out
}

// trashed answers the trashed paths of the harness's space, sorted.
func (h *harness) trashed() []string {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	var out []string
	for _, f := range h.store.files {
		if f.Owner == h.owner && f.DeletedAt != nil {
			out = append(out, f.Path)
		}
	}
	slices.Sort(out)
	return out
}

// last answers the last event the log recorded.
func (h *harness) last(t *testing.T) Event {
	t.Helper()
	if len(h.store.events) == 0 {
		t.Fatal("the log holds no event")
	}
	return h.store.events[len(h.store.events)-1]
}

// TestAPathOutsideFilesIsInNoPlane is criterion 1 of spec 028: a space is
// one tree under files/, so a path whose first segment is anything else,
// the removed second plane included, is unknown_plane on every route that
// takes a path, and nothing reaches the bucket.
func TestAPathOutsideFilesIsInNoPlane(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	before := h.bucket.Total()
	for _, path := range []string{"workspaces/build/main.go", "workspaces/build", "workspaces", "memory/agent.json"} {
		for _, r := range []struct {
			method, target, body string
		}{
			{http.MethodGet, h.object(path), ""},
			{http.MethodHead, h.object(path), ""},
			{http.MethodPut, h.object(path), "bytes"},
			{http.MethodDelete, h.object(path), ""},
			{http.MethodDelete, h.object(path) + "?recursive=1", ""},
			{http.MethodGet, h.object(path) + "?list=1", ""},
			{http.MethodPost, h.object(path), `{"move_to":"files/a.md"}`},
			{http.MethodPost, h.object("files/plan.md"), `{"move_to":"` + path + `"}`},
			{http.MethodPost, h.object("files/plan.md"), `{"copy_to":"` + path + `"}`},
		} {
			var w *httptest.ResponseRecorder
			switch {
			case r.method == http.MethodPost:
				w = h.post(t, strings.TrimPrefix(strings.TrimPrefix(r.target, h.object("")), "/"), r.body)
			case r.body != "":
				w = h.call(t, r.method, r.target, strings.NewReader(r.body))
			default:
				w = h.call(t, r.method, r.target, nil)
			}
			if r.method == http.MethodHead {
				if w.Code != http.StatusBadRequest {
					t.Errorf("HEAD %s answered %d", path, w.Code)
				}
				continue
			}
			if got := code(t, w); got != api.CodeUnknownPlane {
				t.Errorf("%s %s %s is %q, want %q", r.method, r.target, r.body, got, api.CodeUnknownPlane)
			}
		}
	}
	if got := h.bucket.Total() - before; got != 0 {
		t.Errorf("a path outside the plane reached the bucket %d times", got)
	}
	if got := h.live(); !slices.Equal(got, []string{"files/plan.md"}) {
		t.Errorf("the space holds %v", got)
	}
	if plane, err := ValidatePath("files/workspaces/build/main.go"); err != nil || plane != object.PlaneFiles {
		t.Errorf("a folder of that name under files/ is %v, %v; it is an ordinary folder", plane, err)
	}
}

// TestAWriteKeepsNoVersionWhenTheAnswerSaysSo is criterion 9: an overwrite
// whose answer carries limits.versions: false keeps no version, drops the
// bytes it replaced and charges the difference in size. With the member
// absent or true it keeps one, and the versions a path already has are
// untouched either way.
func TestAWriteKeepsNoVersionWhenTheAnswerSaysSo(t *testing.T) {
	for _, c := range []struct {
		name, limits string
		kept         bool
	}{
		{"the member is absent", ``, true},
		{"the member is true", `{"versions":true}`, true},
		{"the member is false", `{"versions":false}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			first := h.seed(t, "files/plan.md", "first")
			h.asked.limits = c.limits
			if w := h.put(t, "files/plan.md", "second, longer"); w.Code != http.StatusOK {
				t.Fatalf("the overwrite answered %d: %s", w.Code, w.Body)
			}
			_, held := h.objects.Bytes(first.ObjectID.Key("arca/"))
			if len(h.store.versions) != btoi(c.kept) || held != c.kept {
				t.Fatalf("the overwrite left %d versions and the replaced bytes held=%t, want kept=%t",
					len(h.store.versions), held, c.kept)
			}
			want := int64(len("second, longer"))
			if c.kept {
				want += int64(len("first"))
			}
			if got := h.store.usage[h.owner]; got != want {
				t.Errorf("the space holds %d bytes, want %d", got, want)
			}
		})
	}

	// A version the path already has stays when a later write keeps none.
	h := newHarness(t)
	h.seed(t, "files/plan.md", "one")
	h.seed(t, "files/plan.md", "two")
	h.asked.limits = `{"versions":false}`
	h.seed(t, "files/plan.md", "three")
	if len(h.store.versions) != 1 {
		t.Fatalf("a write that keeps no version left %d of the versions the path had", len(h.store.versions))
	}
	for _, v := range h.store.versions {
		if v.Checksum != digest("one") {
			t.Errorf("the version that stayed is %q, want the first content", v.Checksum)
		}
	}
}

// btoi counts a truth, for a case that reads a row count off one.
func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestAVersionBroughtForwardKeepsNoVersionWhenTheAnswerSaysSo: under
// limits.versions: false on the answer to file.restore the restored version
// becomes the content, and the content it replaces is dropped with its
// bytes and given back to the space rather than kept as a version.
func TestAVersionBroughtForwardKeepsNoVersionWhenTheAnswerSaysSo(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	second := h.seed(t, "files/plan.md", "second!")
	h.asked.limits = `{"versions":false}`

	w := h.post(t, "files/plan.md", `{"restore_version":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", w.Code, w.Body)
	}
	row, _ := h.read(t, "files/plan.md")
	if row.Checksum != digest("first") {
		t.Fatalf("the path holds %q after the restore", row.Checksum)
	}
	if len(h.store.versions) != 0 {
		t.Errorf("the restore left %d versions, and the answer keeps none", len(h.store.versions))
	}
	if _, held := h.objects.Bytes(second.ObjectID.Key("arca/")); held {
		t.Error("the content the restore replaced is still in the bucket")
	}
	if got, want := h.store.usage[h.owner], int64(len("first")); got != want {
		t.Errorf("the space holds %d bytes, want %d", got, want)
	}
}

// TestADeleteIsPermanentWhenTheAnswerSaysSo is criterion 10: a delete whose
// answer carries limits.trash: false removes the row, its versions and
// their bytes, exactly as ?permanent=1 does. With the member absent or true
// it trashes, and no value makes a delete soft that asked to be permanent.
func TestADeleteIsPermanentWhenTheAnswerSaysSo(t *testing.T) {
	for _, c := range []struct {
		name, limits, query string
		permanent           bool
	}{
		{"the member is absent", ``, "", false},
		{"the member is true", `{"trash":true}`, "", false},
		{"the member is false", `{"trash":false}`, "", true},
		{"the request asks, and the answer says the trash", `{"trash":true}`, "?permanent=1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			first := h.seed(t, "files/plan.md", "first")
			second := h.seed(t, "files/plan.md", "second")
			h.asked.limits = c.limits
			if w := h.call(t, http.MethodDelete, h.object("files/plan.md")+c.query, nil); w.Code != http.StatusNoContent {
				t.Fatalf("the delete answered %d: %s", w.Code, w.Body)
			}
			_, row := h.read(t, "files/plan.md")
			_, firstHeld := h.objects.Bytes(first.ObjectID.Key("arca/"))
			_, secondHeld := h.objects.Bytes(second.ObjectID.Key("arca/"))
			if c.permanent {
				if row || len(h.store.versions) != 0 || firstHeld || secondHeld || h.store.usage[h.owner] != 0 {
					t.Errorf("a permanent delete left row=%t versions=%d bytes=%t,%t usage=%d",
						row, len(h.store.versions), firstHeld, secondHeld, h.store.usage[h.owner])
				}
				return
			}
			if !row || !firstHeld || !secondHeld || !slices.Equal(h.trashed(), []string{"files/plan.md"}) {
				t.Errorf("a soft delete left row=%t bytes=%t,%t trashed=%v", row, firstHeld, secondHeld, h.trashed())
			}
		})
	}
}

// TestALimitThatDoesNotDecodeIsNoDecision is criterion 12: a member of the
// limits object that is not the type spec 006 gives it, and an omit entry
// that is not a path of a space, is refused with the 503 a byte limit that
// does not decode is refused with, on every route that reads the limits,
// and nothing is written, removed or listed.
func TestALimitThatDoesNotDecodeIsNoDecision(t *testing.T) {
	for _, limits := range []string{
		`{"versions":"no"}`,
		`{"trash":0}`,
		`{"quota_bytes":"plenty"}`,
		`{"omit":"files/team"}`,
		`{"omit":[7]}`,
		`{"omit":["files"]}`,
		`{"omit":["files/"]}`,
		`{"omit":["files/team/"]}`,
		`{"omit":["/files/team"]}`,
		`{"omit":["files//team"]}`,
		`{"omit":["files/../etc"]}`,
		`{"omit":["team/payroll"]}`,
		`{"omit":["files/team","nowhere/x"]}`,
		`not json`,
	} {
		t.Run(limits, func(t *testing.T) {
			h := newHarness(t)
			h.seed(t, "files/team/plan.md", "first")
			h.seed(t, "files/team/notes.md", "notes")
			h.seed(t, "files/gone.md", "gone")
			if w := h.call(t, http.MethodDelete, h.object("files/gone.md"), nil); w.Code != http.StatusNoContent {
				t.Fatalf("the trash answered %d", w.Code)
			}
			if w := h.call(t, http.MethodPut, "/v1/stars", strings.NewReader(
				`{"owner":"me","path":"files/team/plan.md"}`), api.HeaderContentType, api.JSONMediaType); w.Code != http.StatusNoContent {
				t.Fatalf("the star answered %d", w.Code)
			}
			h.seed(t, "files/team/plan.md", "second")
			rows, versions, keys := len(h.store.files), len(h.store.versions), len(h.objects.Keys())
			events := len(h.store.events)
			h.asked.limits = limits

			for _, r := range []struct {
				name, method, target, body string
			}{
				{"a write", http.MethodPut, h.object("files/team/new.md"), "bytes"},
				{"an overwrite", http.MethodPut, h.object("files/team/plan.md"), "third"},
				{"a delete", http.MethodDelete, h.object("files/team/plan.md"), ""},
				{"a folder delete", http.MethodDelete, h.object("files/team") + "?recursive=1&permanent=1", ""},
				{"a listing", http.MethodGet, h.object("files/team") + "?list=1", ""},
				{"a manifest", http.MethodGet, "/v1/files/materialize", ""},
				{"a trash listing", http.MethodGet, "/v1/trash", ""},
				{"an emptied trash", http.MethodDelete, "/v1/trash", ""},
				{"a star listing", http.MethodGet, "/v1/stars", ""},
				{"a version restore", http.MethodPost, h.object("files/team/plan.md"), `{"restore_version":1}`},
				{"a copy", http.MethodPost, h.object("files/team/plan.md"), `{"copy_to":"files/team/copy.md"}`},
				{"a folder copy", http.MethodPost, h.object("files/team"), `{"copy_to":"files/copied","recursive":true}`},
			} {
				var w *httptest.ResponseRecorder
				switch {
				case r.method == http.MethodPost:
					w = h.call(t, r.method, r.target, strings.NewReader(r.body), api.HeaderContentType, api.JSONMediaType)
				case r.body != "":
					w = h.call(t, r.method, r.target, strings.NewReader(r.body))
				default:
					w = h.call(t, r.method, r.target, nil)
				}
				if w.Code != http.StatusServiceUnavailable {
					t.Errorf("%s answered %d: %s", r.name, w.Code, w.Body)
					continue
				}
				if got := code(t, w); got != api.CodeStorageUnavailable {
					t.Errorf("%s is %q, want %q", r.name, got, api.CodeStorageUnavailable)
				}
			}
			if len(h.store.files) != rows || len(h.store.versions) != versions || len(h.objects.Keys()) != keys {
				t.Errorf("a request refused for its limits changed the stores: %d rows, %d versions, %d keys; were %d, %d, %d",
					len(h.store.files), len(h.store.versions), len(h.objects.Keys()), rows, versions, keys)
			}
			if len(h.store.events) != events {
				t.Errorf("a request refused for its limits appended %d events", len(h.store.events)-events)
			}
		})
	}
}

// folderOf seeds the tree the folder cases share: a folder with a file, a
// nested file and a trashed file in it, a file at the folder's own path, and
// a sibling whose name begins with the folder's.
func folderOf(t *testing.T, h *harness) {
	t.Helper()
	for path, content := range map[string]string{
		"files/notes/a.md":         "aaaa",
		"files/notes/deep/b.md":    "bbbbbb",
		"files/notes/old.md":       "old",
		"files/notes":              "a file named like the folder",
		"files/notes-archive/c.md": "cc",
		"files/other.md":           "o",
	} {
		h.seed(t, path, content)
	}
	if w := h.call(t, http.MethodDelete, h.object("files/notes/old.md"), nil); w.Code != http.StatusNoContent {
		t.Fatalf("the trash answered %d: %s", w.Code, w.Body)
	}
}

// TestAFolderDeleteTrashesTheSubtreeAndNothingElse is criterion 13: a soft
// folder delete trashes every live object under the folder, each by itself,
// and touches neither the object at the folder's own path nor a sibling
// whose name starts with it. No byte leaves and the usage stays.
func TestAFolderDeleteTrashesTheSubtreeAndNothingElse(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	usage, keys := h.store.usage[h.owner], len(h.objects.Keys())
	calls := h.bucket.Total()

	w := h.call(t, http.MethodDelete, h.object("files/notes")+"?recursive=1", nil)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
	}
	if got, want := h.live(), []string{"files/notes", "files/notes-archive/c.md", "files/other.md"}; !slices.Equal(got, want) {
		t.Errorf("the space holds %v live, want %v", got, want)
	}
	if got, want := h.trashed(), []string{"files/notes/a.md", "files/notes/deep/b.md", "files/notes/old.md"}; !slices.Equal(got, want) {
		t.Errorf("the trash holds %v, want %v", got, want)
	}
	if h.store.usage[h.owner] != usage || len(h.objects.Keys()) != keys || h.bucket.Total() != calls {
		t.Errorf("a soft folder delete moved bytes: usage %d was %d, %d keys were %d, %d bucket calls",
			h.store.usage[h.owner], usage, len(h.objects.Keys()), keys, h.bucket.Total()-calls)
	}
	// Each object is in the trash by itself and comes back one at a time.
	restore := h.call(t, http.MethodPost, "/v1/trash/restore", strings.NewReader(
		`{"owner":"me","path":"files/notes/deep/b.md"}`), api.HeaderContentType, api.JSONMediaType)
	if restore.Code != http.StatusOK {
		t.Fatalf("restoring one object answered %d: %s", restore.Code, restore.Body)
	}
	if got := h.trashed(); !slices.Equal(got, []string{"files/notes/a.md", "files/notes/old.md"}) {
		t.Errorf("after one restore the trash holds %v", got)
	}
}

// TestAPermanentFolderDeleteLeavesNothing is criterion 14: with permanent=1,
// or under an answer that carries limits.trash: false, a folder delete
// removes the live and the trashed rows, their versions, the grants and
// links under the folder and the bytes, gives the usage back, and makes one
// bucket call per thousand keys.
func TestAPermanentFolderDeleteLeavesNothing(t *testing.T) {
	for _, c := range []struct{ name, limits, query string }{
		{"the request asks", ``, "?recursive=1&permanent=1"},
		{"the answer says so", `{"trash":false}`, "?recursive=1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			folderOf(t, h)
			h.seed(t, "files/notes/a.md", "a second content")
			kept := int64(len("a file named like the folder") + len("cc") + len("o"))
			h.asked.limits = c.limits

			if w := h.call(t, http.MethodDelete, h.object("files/notes")+c.query, nil); w.Code != http.StatusNoContent {
				t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
			}
			if got, want := h.live(), []string{"files/notes", "files/notes-archive/c.md", "files/other.md"}; !slices.Equal(got, want) {
				t.Errorf("the space holds %v live, want %v", got, want)
			}
			if got := h.trashed(); len(got) != 0 {
				t.Errorf("the trash still holds %v", got)
			}
			if len(h.store.versions) != 0 {
				t.Errorf("%d versions outlived the folder", len(h.store.versions))
			}
			if got := len(h.objects.Keys()); got != 3 {
				t.Errorf("the bucket holds %d keys, want the three outside the folder", got)
			}
			if got := h.store.usage[h.owner]; got != kept {
				t.Errorf("the space holds %d bytes, want %d", got, kept)
			}
			if want := []string{h.owner + " files/notes "}; !slices.Equal(h.grants.removed, want) {
				t.Errorf("the grants under the folder were removed as %v, want %v", h.grants.removed, want)
			}
			if h.bucket.Calls(blob.MethodDeleteMany) != 1 || h.bucket.Calls(blob.MethodDelete) != 0 {
				t.Errorf("four objects left in %d batch calls and %d single ones, want one batch",
					h.bucket.Calls(blob.MethodDeleteMany), h.bucket.Calls(blob.MethodDelete))
			}
		})
	}

	// A thousand keys to a call: 2,300 objects leave in three.
	h := newHarness(t)
	for i := range 2300 {
		h.write(t, fmt.Sprintf("files/bulk/%04d.bin", i), "x")
	}
	before := h.bucket.Calls(blob.MethodDeleteMany)
	if w := h.call(t, http.MethodDelete, h.object("files/bulk")+"?recursive=1&permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
	}
	if got := h.bucket.Calls(blob.MethodDeleteMany) - before; got != 3 {
		t.Errorf("2,300 keys left in %d bucket calls, want 3", got)
	}
	if got := len(h.objects.Keys()); got != 0 {
		t.Errorf("the bucket still holds %d keys", got)
	}
}

// TestBytesAFolderDeleteCannotRemoveAreLeftForTheReaper: the rows are gone
// when the bytes are dropped, so a bucket that will not delete and a
// reference check that will not answer both leave bytes nothing points at,
// and the request the caller already had answered is not failed.
func TestBytesAFolderDeleteCannotRemoveAreLeftForTheReaper(t *testing.T) {
	stubborn := newHarness(t)
	stubborn.write(t, "files/notes/a.md", "aaaa")
	stubborn.bucket.FailNth(blob.MethodDeleteMany, 1, errOutage)
	if w := stubborn.call(t, http.MethodDelete, stubborn.object("files/notes")+"?recursive=1&permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
	}
	if len(stubborn.live()) != 0 || len(stubborn.objects.Keys()) != 1 {
		t.Errorf("a bucket that refused the delete left %v rows and %d keys", stubborn.live(), len(stubborn.objects.Keys()))
	}

	// A store that fails the transaction removes nothing and says so.
	broken := newHarness(t)
	broken.write(t, "files/notes/a.md", "aaaa")
	broken.store.failTx = errOutage
	if got := code(t, broken.call(t, http.MethodDelete, broken.object("files/notes")+"?recursive=1&permanent=1", nil)); got != api.CodeStorageUnavailable {
		t.Errorf("a folder delete against a store that will not commit is %q", got)
	}
	if len(broken.live()) != 1 || len(broken.objects.Keys()) != 1 {
		t.Error("a folder delete that did not commit removed something")
	}
	failing := newHarness(t)
	failing.write(t, "files/notes/a.md", "aaaa")
	failing.store.failWrite = errOutage
	for _, query := range []string{"?recursive=1", "?recursive=1&permanent=1"} {
		if got := code(t, failing.call(t, http.MethodDelete, failing.object("files/notes")+query, nil)); got != api.CodeStorageUnavailable {
			t.Errorf("a folder delete %s against a store that will not write is %q", query, got)
		}
	}
}

// TestAFolderDeleteIsIdempotent is criterion 15: a folder with nothing in it
// and a folder that never existed are the same thing, so the call is 204
// twice in a row; the plane itself is not a folder; and a version is not a
// folder's to name.
func TestAFolderDeleteIsIdempotent(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	for _, query := range []string{"?recursive=1", "?recursive=1", "?recursive=1&permanent=1", "?recursive=1&permanent=1"} {
		if w := h.call(t, http.MethodDelete, h.object("files/notes")+query, nil); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s answered %d: %s", query, w.Code, w.Body)
		}
	}
	if w := h.call(t, http.MethodDelete, h.object("files/never/was")+"?recursive=1", nil); w.Code != http.StatusNoContent {
		t.Errorf("a folder that never existed answered %d: %s", w.Code, w.Body)
	}
	for target, want := range map[string]string{
		h.object("files") + "?recursive=1":                     api.CodeInvalidPath,
		h.object("files") + "?recursive=1&permanent=1":         api.CodeInvalidPath,
		h.object("files/notes") + "?recursive=1&version=1":     api.CodeExclusiveFields,
		h.object("files/notes") + "?recursive=maybe":           api.CodeInvalidField,
		h.object("files/notes") + "?recursive=1&permanent=yes": api.CodeInvalidField,
		h.object("files/notes/") + "?recursive=1":              api.CodeInvalidPath,
	} {
		if got := code(t, h.call(t, http.MethodDelete, target, nil)); got != want {
			t.Errorf("DELETE %s is %q, want %q", target, got, want)
		}
	}
	// ?recursive=0 is a delete of one object, which the folder is not.
	if got := code(t, h.call(t, http.MethodDelete, h.object("files/never/was")+"?recursive=0", nil)); got != api.CodeNotFound {
		t.Errorf("?recursive=0 on a path that holds nothing is %q", got)
	}
	if got := h.live(); !slices.Equal(got, []string{"files/notes", "files/notes-archive/c.md", "files/other.md"}) {
		t.Errorf("the space holds %v", got)
	}
}

// TestAFolderDeleteAsksOnceAndRecordsOnce is criterion 16: one file.delete
// about the folder, with recursive on the question and no id and no size,
// and one delete event at the folder with the counts. A call that found
// nothing records nothing.
func TestAFolderDeleteAsksOnceAndRecordsOnce(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	h.asked.asked = nil
	events := len(h.store.events)

	if w := h.call(t, http.MethodDelete, h.object("files/notes")+"?recursive=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the folder delete answered %d: %s", w.Code, w.Body)
	}
	if len(h.asked.asked) != 1 {
		t.Fatalf("the folder delete asked %d questions: %v", len(h.asked.asked), h.asked.actions())
	}
	q := h.asked.asked[0]
	if q.action != authorizer.ActionFileDelete || q.res.ID != "" {
		t.Errorf("the question is %s about the id %q", q.action, q.res.ID)
	}
	want := map[string]any{"owner": h.owner, "path": "files/notes", "plane": "files", "recursive": true}
	if got := q.res.Fields; len(got) != len(want) || got["owner"] != want["owner"] || got["path"] != want["path"] ||
		got["plane"] != want["plane"] || got["recursive"] != true {
		t.Errorf("the question carries %v, want %v", got, want)
	}
	if len(h.store.events) != events+1 {
		t.Fatalf("the folder delete appended %d events", len(h.store.events)-events)
	}
	e := h.last(t)
	if e.Action != EventDelete || e.Path != "files/notes" || e.Actor != h.owner {
		t.Errorf("the event is %+v", e)
	}
	if e.Detail["recursive"] != true || e.Detail["files"] != int64(2) ||
		e.Detail["bytes"] != int64(len("aaaa")+len("bbbbbb")) || e.Detail["trashed"] != true {
		t.Errorf("the event's detail is %v", e.Detail)
	}

	// The permanent form counts the rows it took, live and trashed, and the
	// bytes the space got back.
	if w := h.call(t, http.MethodDelete, h.object("files/notes")+"?recursive=1&permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the permanent folder delete answered %d: %s", w.Code, w.Body)
	}
	e = h.last(t)
	if e.Detail["files"] != int64(3) || e.Detail["bytes"] != int64(len("aaaa")+len("bbbbbb")+len("old")) || e.Detail["trashed"] != false {
		t.Errorf("the permanent delete's detail is %v", e.Detail)
	}
	recorded := len(h.store.events)
	if w := h.call(t, http.MethodDelete, h.object("files/notes")+"?recursive=1&permanent=1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("the second delete answered %d", w.Code)
	}
	if len(h.store.events) != recorded {
		t.Error("a folder delete that found nothing appended an event")
	}

	// A refusal is the caller's own answer on its own space and a missing
	// object on another's, and nothing is touched either way.
	denied := newHarness(t)
	folderOf(t, denied)
	denied.asked.refuse = &auth.Error{Code: auth.CodeForbidden, Detail: "no rule allows it"}
	if w := denied.call(t, http.MethodDelete, denied.object("files/notes")+"?recursive=1&permanent=1", nil); w.Code != http.StatusForbidden {
		t.Errorf("a refused folder delete answered %d: %s", w.Code, w.Body)
	}
	denied.asked.refuse = &auth.Error{Code: auth.CodeNotFound, Detail: "no rule allows it"}
	if w := denied.as(t, "carol", http.MethodDelete, denied.object("files/notes")+"?recursive=1&permanent=1", ""); w.Code != http.StatusNotFound {
		t.Errorf("a refused folder delete of another space answered %d: %s", w.Code, w.Body)
	}
	if len(denied.live()) != 5 {
		t.Errorf("a refused folder delete removed something: %v", denied.live())
	}
}

// TestAFolderCopyMovesNoByteThroughTheServer is criterion 17: a folder copy
// writes every live object under the destination with a new object id and
// the same bytes, the bucket does the copying, and the server reads and
// writes no object body.
func TestAFolderCopyMovesNoByteThroughTheServer(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	usage := h.store.usage[h.owner]
	puts, gets := h.bucket.Calls(blob.MethodPut), h.bucket.Calls(blob.MethodGet)

	w := h.post(t, "files/notes", `{"copy_to":"files/reports/2026","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the copy answered %d: %s", w.Code, w.Body)
	}
	var out struct {
		Path  string `json:"path"`
		Files int    `json:"files"`
		Bytes int64  `json:"bytes"`
	}
	decode(t, w, &out)
	size := int64(len("aaaa") + len("bbbbbb"))
	if out.Path != "files/reports/2026" || out.Files != 2 || out.Bytes != size {
		t.Fatalf("the copy answered %+v", out)
	}
	if h.bucket.Calls(blob.MethodPut) != puts || h.bucket.Calls(blob.MethodGet) != gets {
		t.Errorf("the copy read or wrote an object body: %d puts, %d gets",
			h.bucket.Calls(blob.MethodPut)-puts, h.bucket.Calls(blob.MethodGet)-gets)
	}
	if got := h.bucket.Calls(blob.MethodCopy); got != 2 {
		t.Errorf("two objects were copied in %d bucket copies", got)
	}
	for rest, content := range map[string]string{"a.md": "aaaa", "deep/b.md": "bbbbbb"} {
		source, _ := h.read(t, "files/notes/"+rest)
		copied, held := h.read(t, "files/reports/2026/"+rest)
		if !held {
			t.Fatalf("the copy holds nothing at %s", rest)
		}
		if copied.ObjectID == source.ObjectID || copied.ID == source.ID {
			t.Errorf("%s shares an object with its source", rest)
		}
		if copied.Checksum != source.Checksum || copied.ChecksumKind != source.ChecksumKind ||
			copied.SizeBytes != source.SizeBytes || copied.ContentType != source.ContentType {
			t.Errorf("%s was copied as %+v from %+v", rest, copied, source)
		}
		if got, _ := h.objects.Bytes(copied.ObjectID.Key("arca/")); string(got) != content {
			t.Errorf("the copy of %s holds %q", rest, got)
		}
		if got, _ := h.objects.Bytes(source.ObjectID.Key("arca/")); string(got) != content {
			t.Errorf("the source of %s holds %q after the copy", rest, got)
		}
	}
	// The trashed object, the file at the folder's own path and the sibling
	// were not copied.
	for _, path := range []string{"files/reports/2026/old.md", "files/reports/2026", "files/reports/2026-archive/c.md"} {
		if _, held := h.read(t, path); held {
			t.Errorf("the copy wrote %s", path)
		}
	}
	if got := h.store.usage[h.owner]; got != usage+size {
		t.Errorf("the space holds %d bytes, want %d", got, usage+size)
	}
	e := h.last(t)
	if e.Action != EventCopy || e.Path != "files/reports/2026" || e.Detail["from"] != "files/notes" ||
		e.Detail["recursive"] != true || e.Detail["files"] != 2 || e.Detail["bytes"] != size {
		t.Errorf("the event is %+v", e)
	}

	// One object: the answer is the object, as a move answers it.
	one := h.post(t, "files/other.md", `{"copy_to":"files/reports/other.md"}`)
	if one.Code != http.StatusOK {
		t.Fatalf("the copy of one object answered %d: %s", one.Code, one.Body)
	}
	var copied Object
	decode(t, one, &copied)
	if copied.Path != "files/reports/other.md" || copied.Checksum != digest("o") || copied.Size != 1 {
		t.Errorf("the copy answered %+v", copied)
	}
	if got := one.Header().Get(api.HeaderETag); !strings.Contains(got, digest("o")) {
		t.Errorf("the copy's validator is %q", got)
	}
	if e := h.last(t); e.Action != EventCopy || e.Detail["recursive"] != false || e.Detail["from"] != "files/other.md" {
		t.Errorf("the event of the copy of one object is %+v", e)
	}
	if _, held := h.read(t, "files/other.md"); !held {
		t.Error("a copy removed its source")
	}
}

// TestACopyThatIsRefusedWritesNothing is criterion 18: a copy onto a path a
// live object holds is path_taken, a copy past the answer's byte limit is
// quota_exceeded, a store that fails is storage_unavailable, and each
// leaves no row and no key it wrote.
func TestACopyThatIsRefusedWritesNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(h *harness)
		body  string
		want  string
	}{
		{"a destination a live object holds", func(h *harness) {
			h.seed(t, "files/reports/deep/b.md", "taken")
		}, `{"copy_to":"files/reports","recursive":true}`, api.CodePathTaken},
		{"past the answer's byte limit", func(h *harness) {
			h.asked.limits = fmt.Sprintf(`{"quota_bytes":%d}`, h.store.usage[h.owner]+5)
		}, `{"copy_to":"files/reports","recursive":true}`, api.CodeQuotaExceeded},
		{"a bucket that will not copy", func(h *harness) {
			h.bucket.FailNth(blob.MethodCopy, 2, errOutage)
		}, `{"copy_to":"files/reports","recursive":true}`, api.CodeStorageUnavailable},
		{"a database that will not commit", func(h *harness) {
			h.store.failTx = errOutage
		}, `{"copy_to":"files/reports","recursive":true}`, api.CodeStorageUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			folderOf(t, h)
			c.setup(h)
			rows, keys, usage := len(h.store.files), len(h.objects.Keys()), h.store.usage[h.owner]
			events := len(h.store.events)

			if got := code(t, h.post(t, "files/notes", c.body)); got != c.want {
				t.Fatalf("the copy is %q, want %q", got, c.want)
			}
			if len(h.store.files) != rows || len(h.objects.Keys()) != keys || h.store.usage[h.owner] != usage {
				t.Errorf("a refused copy left %d rows, %d keys and %d bytes; were %d, %d and %d",
					len(h.store.files), len(h.objects.Keys()), h.store.usage[h.owner], rows, keys, usage)
			}
			if len(h.store.events) != events {
				t.Error("a refused copy appended an event")
			}
		})
	}

	// One object, the same refusals.
	h := newHarness(t)
	h.seed(t, "files/a.md", "aaaa")
	h.seed(t, "files/b.md", "taken")
	if got := code(t, h.post(t, "files/a.md", `{"copy_to":"files/b.md"}`)); got != api.CodePathTaken {
		t.Errorf("a copy onto a live object is %q", got)
	}
	if row, _ := h.read(t, "files/b.md"); row.Checksum != digest("taken") || len(h.objects.Keys()) != 2 {
		t.Errorf("a refused copy changed its destination or left a key: %+v, %d keys", row, len(h.objects.Keys()))
	}
	if got := code(t, h.post(t, "files/nothing.md", `{"copy_to":"files/c.md"}`)); got != api.CodeNotFound {
		t.Errorf("a copy of a path that holds nothing is %q", got)
	}
	if got := code(t, h.post(t, "files/nothing", `{"copy_to":"files/c","recursive":true}`)); got != api.CodeNotFound {
		t.Errorf("a copy of a folder that holds nothing is %q", got)
	}
	for body, want := range map[string]string{
		`{"copy_to":"files/a.md"}`:                      api.CodeInvalidField,
		`{"copy_to":"files/a.md/","recursive":true}`:    api.CodeInvalidPath,
		`{"copy_to":"files/x/../y"}`:                    api.CodeInvalidPath,
		`{"copy_to":"files/a.md/inside","recursive":1}`: api.CodeBadRequest,
	} {
		if got := code(t, h.post(t, "files/a.md", body)); got != want {
			t.Errorf("%s is %q, want %q", body, got, want)
		}
	}
	// The destination is not the folder and not inside it.
	nested := newHarness(t)
	folderOf(t, nested)
	for _, to := range []string{"files/notes", "files/notes/deep", "files/notes/copy/of/itself"} {
		if got := code(t, nested.post(t, "files/notes", `{"copy_to":"`+to+`","recursive":true}`)); got != api.CodeInvalidField {
			t.Errorf("a copy of a folder to %s is %q", to, got)
		}
	}
	if nested.bucket.Calls(blob.MethodCopy) != 0 {
		t.Error("a copy into its own source reached the bucket")
	}
}

// changing is a bucket whose first copy runs a callback before it answers,
// which is how a case changes the source of a copy while the copy runs.
type changing struct {
	blob.Store
	once func()
}

func (c *changing) Copy(ctx context.Context, from, to string, o blob.PutOptions) (blob.Object, error) {
	if c.once != nil {
		c.once()
		c.once = nil
	}
	return c.Store.Copy(ctx, from, to, o)
}

// TestASourceThatChangesWhileItIsCopiedRefusesTheCopy: the rows the bucket
// copied must be the rows the folder still holds when the destination rows
// are written. A write under the source in between is precondition_failed,
// nothing is written, and the caller retries.
func TestASourceThatChangesWhileItIsCopiedRefusesTheCopy(t *testing.T) {
	for _, c := range []struct{ name, source, body, changed string }{
		{"a folder", "files/notes", `{"copy_to":"files/reports","recursive":true}`, "files/notes/a.md"},
		{"one object", "files/notes/a.md", `{"copy_to":"files/reports/a.md"}`, "files/notes/a.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var h *harness
			racing := &changing{}
			h = newHarness(t, func(o *Options) {
				racing.Store = o.Bucket
				o.Bucket = racing
			})
			folderOf(t, h)
			racing.once = func() { h.seed(t, c.changed, "rewritten while the copy ran") }

			if got := code(t, h.post(t, c.source, c.body)); got != api.CodePreconditionFailed {
				t.Fatalf("the copy is %q, want %q", got, api.CodePreconditionFailed)
			}
			for _, path := range h.live() {
				if strings.HasPrefix(path, "files/reports") {
					t.Errorf("a refused copy wrote %s", path)
				}
			}
			// The source's rewrite kept its replaced content as a version;
			// every other key in the bucket is one a row still names.
			for _, key := range h.objects.Keys() {
				id, err := object.ParseKey("arca/", key)
				if err != nil {
					t.Fatal(err)
				}
				if referenced, _ := h.store.Referenced(t.Context(), nil, id); !referenced {
					t.Errorf("a refused copy left the key %s with no row", key)
				}
			}
		})
	}
}

// TestACopyAsksOnceWithBothEnds is criterion 19's first half: one
// file.write with the destination as the path, the source as from, copy
// set, the sum of the bytes as the size, recursive on a folder, and no id.
func TestACopyAsksOnceWithBothEnds(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)

	for _, c := range []struct {
		source, body, path string
		size               int64
		recursive          bool
	}{
		{"files/notes", `{"copy_to":"files/reports","recursive":true}`, "files/reports",
			int64(len("aaaa") + len("bbbbbb")), true},
		{"files/other.md", `{"copy_to":"files/reports/other.md"}`, "files/reports/other.md", 1, false},
	} {
		h.asked.asked = nil
		if w := h.post(t, c.source, c.body); w.Code != http.StatusOK {
			t.Fatalf("the copy of %s answered %d: %s", c.source, w.Code, w.Body)
		}
		if len(h.asked.asked) != 1 {
			t.Fatalf("the copy of %s asked %d questions: %v", c.source, len(h.asked.asked), h.asked.actions())
		}
		q := h.asked.asked[0]
		if q.action != authorizer.ActionFileWrite || q.res.ID != "" {
			t.Errorf("the question is %s about the id %q", q.action, q.res.ID)
		}
		got := q.res.Fields
		if got["owner"] != h.owner || got["path"] != c.path || got["from"] != c.source ||
			got["plane"] != "files" || got["copy"] != true || got["size"] != c.size {
			t.Errorf("the question of the copy of %s carries %v", c.source, got)
		}
		if _, present := got["recursive"]; present != c.recursive {
			t.Errorf("the question of the copy of %s carries recursive=%v", c.source, got["recursive"])
		}
	}

	// A refusal on another space is the answer a missing source gives.
	denied := newHarness(t)
	folderOf(t, denied)
	for source, body := range map[string]string{
		"files/notes":    `{"copy_to":"files/reports","recursive":true}`,
		"files/other.md": `{"copy_to":"files/reports/other.md"}`,
	} {
		denied.asked.refuse = &auth.Error{Code: auth.CodeForbidden, Detail: "no rule allows it"}
		if w := denied.post(t, source, body); w.Code != http.StatusForbidden {
			t.Errorf("a refused copy of %s answered %d: %s", source, w.Code, w.Body)
		}
		denied.asked.refuse = &auth.Error{Code: auth.CodeNotFound, Detail: "no rule allows it"}
		if got := code(t, denied.as(t, "carol", http.MethodPost, denied.object(source), body)); got != api.CodeNotFound {
			t.Errorf("a refused copy of %s in another space is %q", source, got)
		}
	}
	if denied.bucket.Calls(blob.MethodCopy) != 0 {
		t.Error("a refused copy reached the bucket")
	}
}

// TestACopyIsBounded is criterion 19's second half: a folder past
// MaxCopyObjects is too_many_objects before any bucket call, and one at the
// bound is not refused for its size.
func TestACopyIsBounded(t *testing.T) {
	h := newHarness(t)
	for i := range MaxCopyObjects + 1 {
		if err := h.store.Upsert(t.Context(), nil, store.File{
			Owner: h.owner, Path: fmt.Sprintf("files/big/%05d", i), ObjectID: object.NewID(), CreatedBy: h.owner,
			SizeBytes: 1, Checksum: digest("x"), ChecksumKind: object.ChecksumSHA256,
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := h.post(t, "files/big", `{"copy_to":"files/copy","recursive":true}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a folder past the bound answered %d: %s", w.Code, w.Body)
	}
	if got := code(t, w); got != api.CodeTooManyObjects {
		t.Errorf("a folder past the bound is %q", got)
	}
	if h.bucket.Total() != 0 {
		t.Errorf("a folder past the bound reached the bucket %d times", h.bucket.Total())
	}
	if len(h.store.files) != MaxCopyObjects+1 {
		t.Error("a refused copy wrote a row")
	}
	if MaxCopyObjects != 10_000 || CopyConcurrency != 8 {
		t.Errorf("the bound is %d objects, %d at a time; spec 028 says 10,000 and eight", MaxCopyObjects, CopyConcurrency)
	}
}

// TestACopyIsPrivateAndBare is criterion 20: a copy carries the live
// content and nothing that hangs off the source's path. No version, no
// star, no grant and no publicity follows, the copy of a public object is
// private, and the copy is created by the caller.
func TestACopyIsPrivateAndBare(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/notes/a.md", "first")
	h.seed(t, "files/notes/a.md", "second")
	if w := h.call(t, http.MethodPut, "/v1/stars", strings.NewReader(
		`{"owner":"me","path":"files/notes/a.md"}`), api.HeaderContentType, api.JSONMediaType); w.Code != http.StatusNoContent {
		t.Fatalf("the star answered %d", w.Code)
	}
	h.store.mu.Lock()
	public := h.store.files[key(h.owner, "files/notes/a.md")]
	public.IsPublic = true
	h.store.files[key(h.owner, "files/notes/a.md")] = public
	h.store.mu.Unlock()
	if err := h.objects.SetPublic(t.Context(), public.ObjectID.Key("arca/"), true); err != nil {
		t.Fatal(err)
	}

	if w := h.as(t, "carol", http.MethodPost, h.object("files/notes"), `{"copy_to":"files/copy","recursive":true}`); w.Code != http.StatusOK {
		t.Fatalf("the copy answered %d: %s", w.Code, w.Body)
	}
	copied, held := h.read(t, "files/copy/a.md")
	if !held || copied.Checksum != digest("second") {
		t.Fatalf("the copy is %+v", copied)
	}
	if copied.IsPublic || h.objects.Public(copied.ObjectID.Key("arca/")) {
		t.Error("the copy of a public object is public")
	}
	if copied.CreatedBy != h.subject("carol") {
		t.Errorf("the copy was created by %q, want the caller", copied.CreatedBy)
	}
	for _, v := range h.store.versions {
		if v.Path != "files/notes/a.md" {
			t.Errorf("a version followed the copy to %s", v.Path)
		}
	}
	for _, s := range h.store.stars {
		if s.Path != "files/notes/a.md" {
			t.Errorf("a star followed the copy to %s", s.Path)
		}
	}
	if len(h.grants.moved) != 0 || len(h.grants.removed) != 0 {
		t.Errorf("a copy touched the grants: moved %v, removed %v", h.grants.moved, h.grants.removed)
	}
}

// TestACopyNeedsAGrantOnBothEnds: a copy names two paths, so under the owner
// policy a grantee copies in another space only when the grant it holds
// admits a write on the source and on the destination (spec 006).
func TestACopyNeedsAGrantOnBothEnds(t *testing.T) {
	for _, c := range []struct {
		name   string
		held   map[string]string
		copies bool
	}{
		{"write on both", map[string]string{"files/notes": "write", "files/inbox": "write"}, true},
		{"write on the destination alone", map[string]string{"files/inbox": "write"}, false},
		{"write on the source alone", map[string]string{"files/notes": "write"}, false},
		{"read on the source", map[string]string{"files/notes": "read", "files/inbox": "write"}, false},
	} {
		for _, form := range []struct{ source, body, written string }{
			{"files/notes", `{"copy_to":"files/inbox/notes","recursive":true}`, "files/inbox/notes/a.md"},
			{"files/notes/a.md", `{"copy_to":"files/inbox/a.md"}`, "files/inbox/a.md"},
		} {
			t.Run(c.name+" "+form.source, func(t *testing.T) {
				table := &grantsOf{held: map[string]auth.Permission{}}
				for prefix, rung := range c.held {
					table.held[prefix] = auth.Permission(rung)
				}
				h := newHarness(t, underTheOwnerPolicy(table))
				table.grantee = h.subject("carol")
				h.seed(t, "files/notes/a.md", "aaaa")

				w := h.as(t, "carol", http.MethodPost, h.object(form.source), form.body)
				_, written := h.read(t, form.written)
				if c.copies {
					if w.Code != http.StatusOK || !written {
						t.Fatalf("the copy answered %d and wrote=%t: %s", w.Code, written, w.Body)
					}
					return
				}
				if got := code(t, w); got != api.CodeNotFound {
					t.Errorf("the copy is %q, want %q", got, api.CodeNotFound)
				}
				if written || h.bucket.Calls(blob.MethodCopy) != 0 {
					t.Errorf("a refused copy wrote=%t after %d bucket copies", written, h.bucket.Calls(blob.MethodCopy))
				}
			})
		}
	}
}
