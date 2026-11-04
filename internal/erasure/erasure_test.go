// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package erasure

import (
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/admin"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The unit tier of spec 032: the order the rows and the bytes go in, driven
// with a fake query set and the in-memory bucket, so each failure the order
// exists for is one a test makes happen.

const owner = "https://issuer.example|9ab3"

// fakeRows is the query set a case scripts.
type fakeRows struct {
	uploads    []store.OpenUpload
	uploadsErr error
	erased     store.SpaceErased
	eraseErr   error
	erasedKeep string
	calls      []string
}

func (f *fakeRows) OpenUploads(context.Context, store.Querier, string) ([]store.OpenUpload, error) {
	f.calls = append(f.calls, "uploads")
	return f.uploads, f.uploadsErr
}

func (f *fakeRows) Erase(_ context.Context, _ store.Querier, _, keep string) (store.SpaceErased, error) {
	f.calls = append(f.calls, "erase")
	f.erasedKeep = keep
	return f.erased, f.eraseErr
}

// fakeDB runs the transaction body against no database and answers the
// commit a case scripts, noting whether it ran.
type fakeDB struct {
	commitErr error
	ran       bool
}

func (*fakeDB) Querier() store.Querier { return nil }

func (d *fakeDB) Tx(_ context.Context, fn func(store.Querier) error) error {
	d.ran = true
	if err := fn(nil); err != nil {
		return err
	}
	return d.commitErr
}

// fakeLog records the appends, and fails them when told to.
type fakeLog struct {
	events.Log
	appended []events.Event
	err      error
}

func (l *fakeLog) Append(_ context.Context, _ store.Querier, e events.Event) (int64, error) {
	if l.err != nil {
		return 0, l.err
	}
	l.appended = append(l.appended, e)
	return int64(len(l.appended)), nil
}

// world is one erasure over the fakes, with a bucket that holds the named
// objects.
type world struct {
	eraser *Eraser
	rows   *fakeRows
	db     *fakeDB
	log    *fakeLog
	bucket *blob.Counting
	memory *blob.Memory
	// referenced is the objects a row still names.
	referenced map[object.ID]bool
}

func newWorld(t *testing.T, erased store.SpaceErased) *world {
	t.Helper()
	w := &world{
		rows: &fakeRows{erased: erased}, db: &fakeDB{}, log: &fakeLog{},
		memory: blob.NewMemory(), referenced: map[object.ID]bool{},
	}
	w.bucket = blob.NewCounting(w.memory)
	for _, id := range erased.Objects {
		if _, err := w.memory.Put(t.Context(), id.Key("arca/"), strings.NewReader("bytes"), 5, blob.PutOptions{}); err != nil {
			t.Fatalf("seed the bucket: %v", err)
		}
	}
	e, err := New(Options{
		DB: w.db, Bucket: w.bucket, Prefix: "arca/", Log: w.log, Rows: w.rows,
		Referenced: func(_ context.Context, _ store.Querier, id object.ID) (bool, error) {
			return w.referenced[id], nil
		},
	})
	if err != nil {
		t.Fatalf("build the eraser: %v", err)
	}
	w.eraser = e
	return w
}

func (w *world) held(t *testing.T, id object.ID) bool {
	t.Helper()
	_, err := w.memory.Head(t.Context(), id.Key("arca/"))
	switch {
	case err == nil:
		return true
	case errors.Is(err, blob.ErrNotFound):
		return false
	}
	t.Fatalf("head the bucket: %v", err)
	return false
}

func TestAnErasureDropsTheBytesNoRemainingRowNames(t *testing.T) {
	gone, shared := object.NewID(), object.NewID()
	w := newWorld(t, store.SpaceErased{Files: 2, Objects: []object.ID{gone, shared}})
	w.referenced[shared] = true

	got, err := w.eraser.Erase(t.Context(), owner)
	if err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if got.Files != 2 || got.Objects != 1 {
		t.Errorf("the erasure counted %+v, want two files and one object dropped", got)
	}
	if w.held(t, gone) {
		t.Error("the bytes of an object no row names were kept")
	}
	if !w.held(t, shared) {
		t.Error("the bytes of an object another row names were deleted")
	}
}

func TestTheRecordOfTheDeletionIsAppendedInTheTransactionAndNamesNoPath(t *testing.T) {
	w := newWorld(t, store.SpaceErased{Files: 3, Trashed: 1, Versions: 2, Events: 7, Objects: []object.ID{object.NewID()}})
	if _, err := w.eraser.Erase(t.Context(), owner); err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if len(w.log.appended) != 1 {
		t.Fatalf("the erasure appended %d events, want one", len(w.log.appended))
	}
	e := w.log.appended[0]
	if e.Owner != owner || e.Action != events.ActionSpaceDelete || e.Path != "" {
		t.Errorf("the record is %+v, want the space's, the action space_delete and no path", e)
	}
	if e.Detail["files"] != int64(3) || e.Detail["versions"] != int64(2) {
		t.Errorf("the record's counts are %v", e.Detail)
	}
	if w.rows.erasedKeep != string(events.ActionSpaceDelete) {
		t.Errorf("the erasure keeps %q in the log, want the record of a deletion", w.rows.erasedKeep)
	}
}

func TestAnAppendThatFailsFailsTheTransactionAndKeepsTheBytes(t *testing.T) {
	id := object.NewID()
	w := newWorld(t, store.SpaceErased{Files: 1, Objects: []object.ID{id}})
	w.log.err = errors.New("the insert failed")
	if _, err := w.eraser.Erase(t.Context(), owner); err == nil {
		t.Fatal("an erasure whose record could not be written answered success")
	}
	if !w.held(t, id) {
		t.Error("the bytes went although the transaction that deleted their rows did not commit")
	}
}

func TestACommitThatFailsKeepsTheBytes(t *testing.T) {
	id := object.NewID()
	w := newWorld(t, store.SpaceErased{Files: 1, Objects: []object.ID{id}})
	w.db.commitErr = errors.New("the commit failed")
	if _, err := w.eraser.Erase(t.Context(), owner); err == nil {
		t.Fatal("an erasure that did not commit answered success")
	}
	if !w.held(t, id) {
		t.Error("the bytes went before the rows were gone")
	}
}

func TestABucketThatWillNotDeleteIsLoggedAndTheErasureStillAnswers(t *testing.T) {
	id := object.NewID()
	w := newWorld(t, store.SpaceErased{Files: 1, Objects: []object.ID{id}})
	w.bucket.FailNth(blob.MethodDeleteMany, 1, errors.New("the bucket is down"))
	got, err := w.eraser.Erase(t.Context(), owner)
	if err != nil {
		t.Fatalf("a failed byte deletion failed the call, and the rows were already gone: %v", err)
	}
	if got.Objects != 0 {
		t.Errorf("the erasure counted %d objects dropped, and none were", got.Objects)
	}
	if !w.held(t, id) {
		t.Error("the bytes went through a bucket that refused the delete")
	}
}

func TestAnOpenUploadIsAbortedBeforeAnyRowGoes(t *testing.T) {
	w := newWorld(t, store.SpaceErased{Uploads: 1})
	w.rows.uploads = []store.OpenUpload{{ObjectID: object.NewID(), UploadID: "u-1"}}
	w.bucket.FailNth(blob.MethodAbortMultipart, 1, errors.New("the bucket is down"))
	if _, err := w.eraser.Erase(t.Context(), owner); err == nil {
		t.Fatal("an erasure whose upload could not be aborted answered success")
	}
	if w.db.ran {
		t.Error("the rows were deleted although the parts of an upload could not be discarded, which strands them")
	}

	ok := newWorld(t, store.SpaceErased{Uploads: 1})
	ok.rows.uploads = []store.OpenUpload{{ObjectID: object.NewID(), UploadID: "u-1"}}
	if _, err := ok.eraser.Erase(t.Context(), owner); err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if n := ok.bucket.Calls(blob.MethodAbortMultipart); n != 1 {
		t.Errorf("the erasure aborted %d uploads, want 1", n)
	}
}

func TestASpaceThatHeldNothingRecordsNothingAndTouchesNoBytes(t *testing.T) {
	w := newWorld(t, store.SpaceErased{})
	got, err := w.eraser.Erase(t.Context(), owner)
	if err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if got != (admin.Erased{}) || len(w.log.appended) != 0 || w.bucket.Total() != 0 {
		t.Errorf("an empty space left %d events and %d bucket calls", len(w.log.appended), w.bucket.Total())
	}
}

func TestANewEraserRefusesWhatItCannotDeleteThrough(t *testing.T) {
	for name, o := range map[string]Options{
		"no database": {Bucket: blob.NewMemory(), Log: &fakeLog{}},
		"no bucket":   {DB: &fakeDB{}, Log: &fakeLog{}},
		"no log":      {DB: &fakeDB{}, Bucket: blob.NewMemory()},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: the eraser built", name)
		}
	}
}

func TestAFailedReadOrDeleteOfTheRowsFailsTheErasureAndKeepsTheBytes(t *testing.T) {
	id := object.NewID()
	read := newWorld(t, store.SpaceErased{Files: 1, Objects: []object.ID{id}})
	read.rows.uploadsErr = errors.New("the read failed")
	del := newWorld(t, store.SpaceErased{Files: 1, Objects: []object.ID{id}})
	del.rows.eraseErr = errors.New("the delete failed")
	for name, w := range map[string]*world{"read": read, "delete": del} {
		if _, err := w.eraser.Erase(t.Context(), owner); err == nil {
			t.Errorf("%s: the erasure answered success", name)
		}
		if !w.held(t, id) {
			t.Errorf("%s: the bytes went with rows that stayed", name)
		}
	}
}

func TestAnObjectWhoseReferencesCannotBeReadIsKeptForTheReconciler(t *testing.T) {
	id := object.NewID()
	w := newWorld(t, store.SpaceErased{Files: 1, Objects: []object.ID{id}})
	w.eraser.o.Referenced = func(context.Context, store.Querier, object.ID) (bool, error) {
		return false, errors.New("the read failed")
	}
	got, err := w.eraser.Erase(t.Context(), owner)
	if err != nil {
		t.Fatalf("the erasure failed, and its rows were already gone: %v", err)
	}
	if got.Objects != 0 || !w.held(t, id) {
		t.Errorf("an object whose references were unknown was dropped: %+v", got)
	}
}
