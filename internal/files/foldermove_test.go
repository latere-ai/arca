// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// Spec 030 at the unit tier: a folder moves in one call and one
// transaction, as a rename of rows. The tree is folderOf's: a folder with a
// file, a nested file and a trashed file in it, a file at the folder's own
// path, and a sibling whose name begins with the folder's.

// moveAnswer is what a folder move answers.
type moveAnswer struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// snapshot is everything a refused move must leave as it was.
type snapshot struct {
	files    map[string]store.File
	versions map[string]store.Version
	stars    map[string]store.Star
	events   int
	grants   int
}

// snap reads the state of the harness's store.
func (h *harness) snap() snapshot {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return snapshot{
		files: maps.Clone(h.store.files), versions: maps.Clone(h.store.versions),
		stars: maps.Clone(h.store.stars), events: len(h.store.events),
		grants: len(h.grants.moved) + len(h.grants.movedUnder),
	}
}

// unchanged fails a case whose refused move changed anything.
func (h *harness) unchanged(t *testing.T, before snapshot, what string) {
	t.Helper()
	after := h.snap()
	if !reflect.DeepEqual(after.files, before.files) || !reflect.DeepEqual(after.versions, before.versions) ||
		!reflect.DeepEqual(after.stars, before.stars) {
		t.Errorf("%s changed the rows: the space holds %v", what, h.live())
	}
	if after.events != before.events || after.grants != before.grants {
		t.Errorf("%s appended %d events and carried %d grants", what,
			after.events-before.events, after.grants-before.grants)
	}
}

// TestAFolderMoveRenamesTheRowsAndTouchesNoBytes is criteria 1, 2, 3 and 10
// of spec 030: every live object under the folder takes the same path below
// move_to with its id and its object, its history and its stars follow, the
// grants under the folder are carried, nothing beside the folder moves, the
// bucket is never called, and one move event carries the counts.
func TestAFolderMoveRenamesTheRowsAndTouchesNoBytes(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	h.seed(t, "files/notes/a.md", "a second content")
	if w := h.call(t, http.MethodPut, "/v1/stars", strings.NewReader(
		`{"owner":"me","path":"files/notes/deep/b.md"}`), api.HeaderContentType, api.JSONMediaType); w.Code != http.StatusNoContent {
		t.Fatalf("the star answered %d: %s", w.Code, w.Body)
	}
	before := map[string]store.File{}
	for _, path := range []string{"files/notes/a.md", "files/notes/deep/b.md"} {
		before[path], _ = h.read(t, path)
	}
	usage, calls, events := h.store.usage[h.owner], h.bucket.Total(), len(h.store.events)

	w := h.post(t, "files/notes", `{"move_to":"files/archive/2026","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the folder move answered %d: %s", w.Code, w.Body)
	}
	var out moveAnswer
	decode(t, w, &out)
	size := int64(len("a second content") + len("bbbbbb"))
	if out.Path != "files/archive/2026" || out.Files != 2 || out.Bytes != size {
		t.Fatalf("the folder move answered %+v", out)
	}
	if got := h.bucket.Total() - calls; got != 0 {
		t.Errorf("the folder move made %d bucket calls, and a key derives from an id", got)
	}
	if got := h.store.usage[h.owner]; got != usage {
		t.Errorf("the folder move moved the usage from %d to %d", usage, got)
	}
	if got, want := h.live(), []string{
		"files/archive/2026/a.md", "files/archive/2026/deep/b.md",
		"files/notes", "files/notes-archive/c.md", "files/other.md",
	}; !slices.Equal(got, want) {
		t.Errorf("the space holds %v live, want %v", got, want)
	}
	if got := h.trashed(); !slices.Equal(got, []string{"files/notes/old.md"}) {
		t.Errorf("the trash holds %v; a trashed object stays where it was", got)
	}
	for from, row := range before {
		to := "files/archive/2026" + strings.TrimPrefix(from, "files/notes")
		moved, held := h.read(t, to)
		if !held || moved.ID != row.ID || moved.ObjectID != row.ObjectID || moved.Checksum != row.Checksum {
			t.Errorf("%s moved as %+v from %+v", from, moved, row)
		}
	}
	for _, v := range h.store.versions {
		if v.Path != "files/archive/2026/a.md" && v.Path != "files/notes/old.md" {
			t.Errorf("a version stayed at %q", v.Path)
		}
	}
	for _, s := range h.store.stars {
		if s.Path != "files/archive/2026/deep/b.md" {
			t.Errorf("a bookmark stayed at %q", s.Path)
		}
	}
	if want := []string{h.owner + " files/notes -> files/archive/2026  files/archive/2026/a.md,files/archive/2026/deep/b.md"}; !slices.Equal(h.grants.movedUnder, want) {
		t.Errorf("the folder move carried the grants as %v, want %v", h.grants.movedUnder, want)
	}
	if len(h.grants.moved) != 0 {
		t.Errorf("the folder move ran the move of one object's grants: %v", h.grants.moved)
	}
	if len(h.store.events) != events+1 {
		t.Fatalf("the folder move appended %d events, want one", len(h.store.events)-events)
	}
	e := h.last(t)
	if e.Action != EventMove || e.Path != "files/archive/2026" || e.Actor != h.owner {
		t.Errorf("the event is %+v", e)
	}
	if e.Detail["from"] != "files/notes" || e.Detail["recursive"] != true || e.Detail["files"] != 2 || e.Detail["bytes"] != size {
		t.Errorf("the event's detail is %v", e.Detail)
	}
	// The moved objects read at their new paths.
	if row, held := h.read(t, "files/archive/2026/a.md"); !held || row.Checksum != digest("a second content") {
		t.Errorf("the moved object reads %+v", row)
	}
}

// TestAFolderMoveOntoATakenPathMovesNothing is criterion 4: a destination a
// live row holds, a trashed row holds, or an object of this very move holds
// refuses the whole move as path_taken, and every row, version, star, grant
// and event is as it was.
func TestAFolderMoveOntoATakenPathMovesNothing(t *testing.T) {
	for _, c := range []struct {
		name, source, body, taken string
		setup                     func(t *testing.T, h *harness)
	}{
		{"a live object at a destination", "files/notes", `{"move_to":"files/archive","recursive":true}`, "files/archive/deep/b.md",
			func(t *testing.T, h *harness) { h.seed(t, "files/archive/deep/b.md", "taken") }},
		{"a trashed object at a destination", "files/notes", `{"move_to":"files/archive","recursive":true}`, "files/archive/a.md",
			func(t *testing.T, h *harness) {
				h.seed(t, "files/archive/a.md", "in the trash")
				if w := h.call(t, http.MethodDelete, h.object("files/archive/a.md"), nil); w.Code != http.StatusNoContent {
					t.Fatalf("the trash answered %d", w.Code)
				}
			}},
		{"an object of the move at a destination", "files/notes/deep", `{"move_to":"files/notes","recursive":true}`, "files/notes/deep/b.md",
			func(t *testing.T, h *harness) { h.seed(t, "files/notes/deep/deep/b.md", "two folders down") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			folderOf(t, h)
			c.setup(t, h)
			before := h.snap()
			w := h.post(t, c.source, c.body)
			if got := code(t, w); got != api.CodePathTaken {
				t.Fatalf("the move is %q, want %q", got, api.CodePathTaken)
			}
			if !strings.Contains(w.Body.String(), c.taken) {
				t.Errorf("the refusal does not name %s: %s", c.taken, w.Body)
			}
			h.unchanged(t, before, "a move onto a taken path")
		})
	}
}

// TestAFolderMoveRefusesWhatIsNotAMoveOfAFolder is criterion 5: the folder
// itself and a path inside it are not destinations, the plane is not a
// folder, a destination that is not a path is refused on move_to, and a
// folder with nothing live under it is not there to move. None of them is
// asked about or changes anything.
func TestAFolderMoveRefusesWhatIsNotAMoveOfAFolder(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	before := h.snap()
	h.asked.asked = nil
	for _, c := range []struct {
		source, to, code string
		fields           []string
	}{
		{"files/notes", "files/notes", api.CodeInvalidField, []string{"move_to"}},
		{"files/notes", "files/notes/deep", api.CodeInvalidField, []string{"move_to"}},
		{"files/notes", "files/notes/new/place", api.CodeInvalidField, []string{"move_to"}},
		{"files/notes", "files/notes/", api.CodeInvalidPath, []string{"move_to"}},
		{"files/notes", "files/x/../y", api.CodeInvalidPath, []string{"move_to"}},
		{"files/notes", "nowhere/notes", api.CodeUnknownPlane, []string{"move_to"}},
		{"files/notes", "files", api.CodeUnknownPlane, []string{"move_to"}},
		{"files", "files/everything", api.CodeInvalidPath, []string{"path"}},
		{"files/never/was", "files/somewhere", api.CodeNotFound, nil},
	} {
		w := h.post(t, c.source, `{"move_to":"`+c.to+`","recursive":true}`)
		if got := code(t, w); got != c.code {
			t.Errorf("a move of %s to %s is %q, want %q", c.source, c.to, got, c.code)
		}
		if got := fieldsOf(t, w); !slices.Equal(got, c.fields) {
			t.Errorf("a move of %s to %s names the fields %v, want %v", c.source, c.to, got, c.fields)
		}
	}
	// A trashed object is not live, so a folder holding only that holds
	// nothing to move.
	if got := code(t, h.post(t, "files/notes/old.md", `{"move_to":"files/x","recursive":true}`)); got != api.CodeNotFound {
		t.Errorf("a move of a folder with nothing live under it is %q", got)
	}
	if len(h.asked.asked) != 0 {
		t.Errorf("a refused move asked %v", h.asked.actions())
	}
	h.unchanged(t, before, "a refused move")
}

// TestAFolderMoveAsksOnceWithBothEnds is criterion 6: one file.write with
// the destination as the path, the source as from, recursive set, the sum
// of the bytes under the folder as the size, and no id and no copy. A
// refusal is the caller's own answer on its space and a missing folder on
// another's, and nothing moves either way.
func TestAFolderMoveAsksOnceWithBothEnds(t *testing.T) {
	h := newHarness(t)
	folderOf(t, h)
	h.asked.asked = nil
	if w := h.post(t, "files/notes", `{"move_to":"files/archive","recursive":true}`); w.Code != http.StatusOK {
		t.Fatalf("the folder move answered %d: %s", w.Code, w.Body)
	}
	if len(h.asked.asked) != 1 {
		t.Fatalf("the folder move asked %d questions: %v", len(h.asked.asked), h.asked.actions())
	}
	q := h.asked.asked[0]
	if q.action != authorizer.ActionFileWrite || q.res.ID != "" || q.lookup {
		t.Errorf("the question is %s about the id %q, lookup %t", q.action, q.res.ID, q.lookup)
	}
	want := map[string]any{
		"owner": h.owner, "path": "files/archive", "plane": "files", "from": "files/notes",
		"recursive": true, "size": int64(len("aaaa") + len("bbbbbb")),
	}
	if !reflect.DeepEqual(q.res.Fields, want) {
		t.Errorf("the question carries %v, want %v", q.res.Fields, want)
	}

	denied := newHarness(t)
	folderOf(t, denied)
	before := denied.snap()
	denied.asked.refuse = &auth.Error{Code: auth.CodeForbidden, Detail: "no rule allows it"}
	if w := denied.post(t, "files/notes", `{"move_to":"files/archive","recursive":true}`); w.Code != http.StatusForbidden {
		t.Errorf("a refused folder move answered %d: %s", w.Code, w.Body)
	}
	denied.asked.refuse = &auth.Error{Code: auth.CodeNotFound, Detail: "no rule allows it"}
	if got := code(t, denied.as(t, "carol", http.MethodPost, denied.object("files/notes"),
		`{"move_to":"files/archive","recursive":true}`)); got != api.CodeNotFound {
		t.Errorf("a refused folder move in another space is %q", got)
	}
	denied.unchanged(t, before, "a refused folder move")
}

// TestAFolderMoveNeedsAGrantOnBothEnds: under the owner policy a grantee
// moves a folder of another space only when the grant it holds admits a
// write on the source and on the destination (spec 006).
func TestAFolderMoveNeedsAGrantOnBothEnds(t *testing.T) {
	for _, c := range []struct {
		name  string
		held  map[string]auth.Permission
		moves bool
	}{
		{"write on both", map[string]auth.Permission{"files/notes": auth.PermissionWrite, "files/inbox": auth.PermissionWrite}, true},
		{"write on the destination alone", map[string]auth.Permission{"files/inbox": auth.PermissionWrite}, false},
		{"write on the source alone", map[string]auth.Permission{"files/notes": auth.PermissionWrite}, false},
		{"read on the source", map[string]auth.Permission{"files/notes": auth.PermissionRead, "files/inbox": auth.PermissionWrite}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			table := &grantsOf{held: c.held}
			h := newHarness(t, underTheOwnerPolicy(table))
			table.grantee = h.subject("carol")
			h.seed(t, "files/notes/a.md", "aaaa")

			w := h.as(t, "carol", http.MethodPost, h.object("files/notes"), `{"move_to":"files/inbox/notes","recursive":true}`)
			_, moved := h.read(t, "files/inbox/notes/a.md")
			if c.moves {
				if w.Code != http.StatusOK || !moved {
					t.Fatalf("the folder move answered %d and moved=%t: %s", w.Code, moved, w.Body)
				}
				return
			}
			if got := code(t, w); got != api.CodeNotFound {
				t.Errorf("the folder move is %q, want %q", got, api.CodeNotFound)
			}
			if _, stayed := h.read(t, "files/notes/a.md"); moved || !stayed {
				t.Errorf("a refused folder move left moved=%t stayed=%t", moved, stayed)
			}
		})
	}
}

// TestAFolderMoveLeavesOutWhatTheAnswerOmitsAtBothEnds is criterion 7: an
// object whose source path or destination path the answer omits is neither
// moved nor written into, the grants are carried under the same omission,
// the counts leave it out, and a folder the answer omits whole is the answer
// a folder with nothing in it gives.
func TestAFolderMoveLeavesOutWhatTheAnswerOmitsAtBothEnds(t *testing.T) {
	h := shared(t)
	w := h.as(t, "carol", http.MethodPost, h.object("files/team"), `{"move_to":"files/moved","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the folder move answered %d: %s", w.Code, w.Body)
	}
	var out moveAnswer
	decode(t, w, &out)
	if out.Files != 2 || out.Bytes != int64(len("plan")+len("notes")) {
		t.Errorf("the folder move with the prefix omitted answered %+v", out)
	}
	if got, want := h.live(), []string{
		"files/moved/payroll-notes.md", "files/moved/plan.md", "files/other.md",
		"files/team/payroll/2026/june.pdf", "files/team/payroll/may.pdf",
	}; !slices.Equal(got, want) {
		t.Errorf("the space holds %v, want %v", got, want)
	}
	if e := h.last(t); e.Action != EventMove || e.Detail["files"] != 2 {
		t.Errorf("the event is %+v", e)
	}
	if want := []string{h.owner + " files/team -> files/moved " + omittedPrefix + " files/moved/payroll-notes.md,files/moved/plan.md"}; !slices.Equal(h.grants.movedUnder, want) {
		t.Errorf("the grants were carried as %v, want %v", h.grants.movedUnder, want)
	}
	// The size was counted before the answer, so it is the whole folder's.
	q := h.asked.asked[len(h.asked.asked)-1]
	if q.res.Fields["size"] != int64(len("plan")+len("may pay")+len("june pay!")+len("notes")) {
		t.Errorf("the question carried the size %v", q.res.Fields["size"])
	}

	// The destination end: what would land under an omitted prefix stays,
	// and a destination that holds something there is not checked.
	h.asked.limitsFor[h.subject("carol")] = `{"omit":["files/mirror/payroll-notes.md"]}`
	h.seed(t, "files/mirror/payroll-notes.md", "something the caller cannot see")
	w = h.as(t, "carol", http.MethodPost, h.object("files/moved"), `{"move_to":"files/mirror","recursive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the folder move onto an omitted destination answered %d: %s", w.Code, w.Body)
	}
	decode(t, w, &out)
	if out.Files != 1 {
		t.Errorf("the folder move onto an omitted destination answered %+v", out)
	}
	if row, _ := h.read(t, "files/mirror/payroll-notes.md"); row.Checksum != digest("something the caller cannot see") {
		t.Error("the folder move wrote under a destination the answer omits")
	}
	if _, stayed := h.read(t, "files/moved/payroll-notes.md"); !stayed {
		t.Error("an object whose destination the answer omits moved")
	}

	// Everything left out: nothing to move.
	h.asked.limitsFor[h.subject("carol")] = `{"omit":["` + omittedPrefix + `"]}`
	before := h.snap()
	if got := code(t, h.as(t, "carol", http.MethodPost, h.object(omittedPrefix), `{"move_to":"files/elsewhere","recursive":true}`)); got != api.CodeNotFound {
		t.Errorf("a folder move of a folder the answer omits whole is %q", got)
	}
	h.unchanged(t, before, "a folder move with nothing left in it")
	// The second caller moves the whole folder.
	w = h.as(t, "dave", http.MethodPost, h.object("files/team"), `{"move_to":"files/whole","recursive":true}`)
	decode(t, w, &out)
	if w.Code != http.StatusOK || out.Files != 2 {
		t.Errorf("the folder move with no omission answered %d %+v", w.Code, out)
	}
}

// bulk writes count rows under a folder without bytes, which is what a case
// about the bound needs and all it needs.
func (h *harness) bulk(t *testing.T, folder string, count int) {
	t.Helper()
	for i := range count {
		if err := h.store.Upsert(t.Context(), nil, store.File{
			Owner: h.owner, Path: fmt.Sprintf("%s/%05d", folder, i), ObjectID: object.NewID(), CreatedBy: h.owner,
			SizeBytes: 1, Checksum: digest("x"), ChecksumKind: object.ChecksumSHA256,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAFolderMoveIsBounded is criterion 8: a folder past MaxCopyObjects is
// too_many_objects before any row changes, and one at the bound moves.
func TestAFolderMoveIsBounded(t *testing.T) {
	h := newHarness(t)
	h.bulk(t, "files/big", MaxCopyObjects+1)
	before := h.snap()
	w := h.post(t, "files/big", `{"move_to":"files/moved","recursive":true}`)
	if w.Code != http.StatusUnprocessableEntity || code(t, w) != api.CodeTooManyObjects {
		t.Fatalf("a folder past the bound answered %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "move its subfolders") {
		t.Errorf("the refusal does not tell the caller what to do: %s", w.Body)
	}
	h.unchanged(t, before, "a folder move past the bound")

	at := newHarness(t)
	at.bulk(t, "files/big", MaxCopyObjects)
	w = at.post(t, "files/big", `{"move_to":"files/moved","recursive":true}`)
	var out moveAnswer
	decode(t, w, &out)
	if w.Code != http.StatusOK || out.Files != MaxCopyObjects {
		t.Errorf("a folder at the bound answered %d %+v", w.Code, out)
	}
}

// TestAThinnedPageIsNotTakenForTheWholeFolder is criterion 8's second half:
// the rows are read one past the bound, and a page past it is refused before
// the destination end of the omission leaves rows out of it. Thinned first,
// a page of a larger folder would fit under the bound and be copied or
// moved as if it were the whole folder, and the rows past it would be left
// behind with an answer that said nothing of them.
func TestAThinnedPageIsNotTakenForTheWholeFolder(t *testing.T) {
	for _, c := range []struct{ verb, body string }{
		{"copy", `{"copy_to":"files/out","recursive":true}`},
		{"move", `{"move_to":"files/out","recursive":true}`},
	} {
		t.Run(c.verb, func(t *testing.T) {
			h := newHarness(t)
			h.bulk(t, "files/big", MaxCopyObjects+3)
			// Three of the first page land under an omitted destination.
			h.asked.limits = `{"omit":["files/out/00000","files/out/00001","files/out/00002"]}`
			before := h.snap()
			w := h.post(t, "files/big", c.body)
			if got := code(t, w); got != api.CodeTooManyObjects {
				t.Fatalf("a %s of a folder past the bound with its first page thinned is %q", c.verb, got)
			}
			if !strings.Contains(w.Body.String(), c.verb+" its subfolders") {
				t.Errorf("the refusal does not tell the caller to %s the subfolders: %s", c.verb, w.Body)
			}
			if h.bucket.Total() != 0 {
				t.Errorf("a refused %s reached the bucket %d times", c.verb, h.bucket.Total())
			}
			h.unchanged(t, before, "a refused "+c.verb)
		})
	}
}

// racingFiles is the files query set with a hook that runs when the
// destination check is asked, which is how a case lets another request
// change the folder between the read and the rename, and with a check that
// can be made to miss a row written in that window.
type racingFiles struct {
	store.Files
	during func()
	blind  bool
}

func (r *racingFiles) Taken(ctx context.Context, q store.Querier, owner string, paths []string) (string, bool, error) {
	if r.during != nil {
		r.during()
	}
	if r.blind {
		return "", false, nil
	}
	return r.Files.Taken(ctx, q, owner, paths)
}

// racing mounts the harness over racingFiles.
func racing(t *testing.T, r *racingFiles) *harness {
	t.Helper()
	return newHarness(t, func(o *Options) {
		r.Files = o.Files
		o.Files = r
	})
}

// TestAFolderMoveThatCannotBeRecordedMovesNothing is criterion 9: an object
// deleted between the read and the rename is precondition_failed, a row
// written at a destination in that window or a history already at one is
// path_taken, and a store that fails at any statement is
// storage_unavailable. Each leaves every row where it was and records
// nothing.
func TestAFolderMoveThatCannotBeRecordedMovesNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(t *testing.T, h *harness, r *racingFiles)
		want string
	}{
		{"an object deleted while the folder moved", func(t *testing.T, h *harness, r *racingFiles) {
			r.during = func() {
				if _, _, err := h.store.HardDelete(t.Context(), nil, h.owner, "files/notes/a.md"); err != nil {
					t.Error(err)
				}
			}
		}, api.CodePreconditionFailed},
		{"a destination written while the folder moved", func(t *testing.T, h *harness, r *racingFiles) {
			r.blind = true
			r.during = func() {
				if err := h.store.Upsert(t.Context(), nil, store.File{
					Owner: h.owner, Path: "files/archive/a.md", ObjectID: object.NewID(), CreatedBy: h.owner,
					Checksum: digest("written in between"), ChecksumKind: object.ChecksumSHA256,
				}); err != nil {
					t.Error(err)
				}
			}
		}, api.CodePathTaken},
		{"a history already at a destination", func(t *testing.T, h *harness, _ *racingFiles) {
			h.store.mu.Lock()
			defer h.store.mu.Unlock()
			for _, path := range []string{"files/notes/a.md", "files/archive/a.md"} {
				h.store.versions[key(h.owner, path, "1")] = store.Version{
					Owner: h.owner, Path: path, VersionNo: 1, ObjectID: object.NewID(),
				}
			}
		}, api.CodePathTaken},
		{"a database that answers nothing", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.store.failAll = errOutage
		}, api.CodeStorageUnavailable},
		{"a database that will not open the transaction", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.store.failTx = errOutage
		}, api.CodeStorageUnavailable},
		{"a destination check that fails", func(_ *testing.T, h *harness, r *racingFiles) {
			r.during = func() { h.store.failWrite = errOutage }
		}, api.CodeStorageUnavailable},
		{"a rename that fails", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.store.failRename = map[string]error{"files": errOutage}
		}, api.CodeStorageUnavailable},
		{"a history that will not move", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.store.failRename = map[string]error{"versions": errOutage}
		}, api.CodeStorageUnavailable},
		{"bookmarks that will not move", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.store.failRename = map[string]error{"stars": errOutage}
		}, api.CodeStorageUnavailable},
		{"grants that will not move", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.grants.failMoveUnder = errOutage
		}, api.CodeStorageUnavailable},
		{"limits the server cannot read", func(_ *testing.T, h *harness, _ *racingFiles) {
			h.asked.limits = `{"omit":"files/notes"}`
		}, api.CodeStorageUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &racingFiles{}
			h := racing(t, r)
			folderOf(t, h)
			c.arm(t, h, r)
			before := h.snap()
			if got := code(t, h.post(t, "files/notes", `{"move_to":"files/archive","recursive":true}`)); got != c.want {
				t.Fatalf("the folder move is %q, want %q", got, c.want)
			}
			h.unchanged(t, before, "a folder move that could not be recorded")
		})
	}
}
