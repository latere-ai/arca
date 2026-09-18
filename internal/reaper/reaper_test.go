// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package reaper

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

const (
	aSpace       = "https://issuer.example|0f5c1d2e"
	anotherSpace = "https://issuer.example|9ab3c4d5"
	aPrefix      = "arca/"
)

// aMoment is the clock every case starts at.
var aMoment = time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC)

// installation is a reconciler over a world and a bucket in memory, with the
// clock the case moves.
type installation struct {
	*Reconciler
	world  *world
	bucket *blob.Counting
	memory *blob.Memory
	now    time.Time
	lines  *bytes.Buffer
}

// an builds one, with the options the case overrides.
func an(t *testing.T, over func(*Options)) *installation {
	t.Helper()
	i := &installation{world: newWorld(), memory: blob.NewMemory(), now: aMoment, lines: &bytes.Buffer{}}
	i.bucket = blob.NewCounting(i.memory)
	o := Options{
		DB:             i.world,
		Bucket:         i.bucket,
		Prefix:         aPrefix,
		TrashRetention: 720 * time.Hour,
		Now:            func() time.Time { return i.now },
		Log:            slog.New(slog.NewTextHandler(i.lines, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	if over != nil {
		over(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	i.Reconciler = r
	return i
}

// put writes bytes under an object's key, which is what a put leaves in the
// bucket before the database hears about it.
func (i *installation) put(t *testing.T, id object.ID) string {
	t.Helper()
	key := id.Key(aPrefix)
	if _, err := i.memory.Put(t.Context(), key, strings.NewReader("the bytes"), 9, blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	return key
}

// run runs one sequence and fails the test when a pass did.
func (i *installation) run(t *testing.T) Findings {
	t.Helper()
	f, err := i.Run(t.Context())
	if err != nil {
		t.Fatalf("the run reported %v", err)
	}
	return f
}

func TestTheFindingKindsAreClosed(t *testing.T) {
	want := []Kind{
		"orphan_object", "orphan_candidate", "missing_bytes", "workspace_purged",
		"file_purged", "trash_purged", "share_expired", "event_pruned",
		"star_pruned", "version_pruned", "upload_aborted", "usage_corrected", "lease_expired",
	}
	if got := Kinds(); !slices.Equal(got, want) {
		t.Fatalf("the vocabulary is %v", got)
	}
	Kinds()[0] = "rewritten"
	if Kinds()[0] != KindOrphanObject {
		t.Fatal("a caller that rewrites the answer rewrites the table")
	}
}

func TestFindingsCountWhatAPassSawAndRenderInOrder(t *testing.T) {
	var f Findings
	f.Add(KindUsageCorrected, Repaired, 2)
	f.Add(KindOrphanObject, Found, 1)
	f.Add(KindOrphanObject, Repaired, 1)
	f.Add(KindMissingBytes, Found, 0)
	f.Add(KindMissingBytes, Found, -3)

	if f.Count(KindOrphanObject, Found) != 1 || f.Total(KindOrphanObject) != 2 {
		t.Fatalf("the counts read %v", f.Rows())
	}
	if f.Count(KindMissingBytes, Found) != 0 || f.Total(KindMissingBytes) != 0 {
		t.Fatal("a pass that found nothing left a row")
	}
	want := []Row{
		{KindOrphanObject, Found, 1},
		{KindOrphanObject, Repaired, 1},
		{KindUsageCorrected, Repaired, 2},
	}
	if got := f.Rows(); !slices.Equal(got, want) {
		t.Fatalf("the table renders as %v", got)
	}
	if attrs := f.attributes(); len(attrs) != 6 || attrs[0] != "orphan_object_found" {
		t.Fatalf("the log line carries %v", attrs)
	}
}

func TestNewRefusesAReconcilerItCannotRun(t *testing.T) {
	for _, c := range []struct {
		name string
		over func(*Options)
	}{
		{"with no database", func(o *Options) { o.DB = nil }},
		{"with no bucket", func(o *Options) { o.Bucket = nil }},
		{"with a retention of no time", func(o *Options) { o.TrashRetention = 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := Options{DB: newWorld(), Bucket: blob.NewMemory(), TrashRetention: time.Hour}
			c.over(&o)
			if _, err := New(o); err == nil {
				t.Fatal("the reconciler was built anyway")
			}
		})
	}
	// A reconciler that names no clock and no logger reads the process's.
	if _, err := New(Options{DB: newWorld(), Bucket: blob.NewMemory(), TrashRetention: time.Hour}); err != nil {
		t.Fatal(err)
	}
}

// Criterion 11 of spec 010 at the seam: a put that failed after the bucket
// write leaves bytes with no row, and pass 1 reaps them after the grace
// window and not before.
func TestPassOneLeavesAnOrphanAloneInsideTheGraceWindow(t *testing.T) {
	i := an(t, nil)
	key := i.put(t, object.NewID())

	first := i.run(t)
	if first.Count(KindOrphanCandidate, Found) != 1 || first.Total(KindOrphanObject) != 0 {
		t.Fatalf("the first run found %v", first.Rows())
	}
	if _, held := i.memory.Bytes(key); !held {
		t.Fatal("the bytes went on the run that first saw them")
	}

	// Inside the window the key is still only a candidate: its row may be
	// one statement away from existing.
	i.now = i.now.Add(OrphanGrace - time.Minute)
	if inside := i.run(t); inside.Count(KindOrphanCandidate, Found) != 1 {
		t.Fatalf("inside the window the run found %v", inside.Rows())
	}
	if _, held := i.memory.Bytes(key); !held {
		t.Fatal("the bytes went inside the grace window")
	}

	i.now = i.now.Add(2 * time.Minute)
	after := i.run(t)
	if after.Count(KindOrphanObject, Repaired) != 1 {
		t.Fatalf("past the window the run found %v", after.Rows())
	}
	if _, held := i.memory.Bytes(key); held {
		t.Fatal("the orphan survived the window")
	}
}

func TestPassOneKeepsWhatARowStillNames(t *testing.T) {
	i := an(t, nil)
	id := object.NewID()
	key := i.put(t, id)
	i.world.files = append(i.world.files, fileRow{owner: aSpace, path: "files/q3.pdf", id: id, size: 9})

	i.now = i.now.Add(2 * OrphanGrace)
	for range 2 {
		if f := i.run(t); f.Total(KindOrphanObject) != 0 || f.Total(KindOrphanCandidate) != 0 {
			t.Fatalf("a key a row names was reported as %v", f.Rows())
		}
	}
	if _, held := i.memory.Bytes(key); !held {
		t.Fatal("the bytes of a live row went")
	}
}

func TestPassOneLeavesAKeyItCannotReadAlone(t *testing.T) {
	i := an(t, nil)
	if _, err := i.memory.Put(t.Context(), aPrefix+"somebody/elses/key", strings.NewReader("x"), 1, blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	i.now = i.now.Add(2 * OrphanGrace)
	for range 2 {
		if f := i.run(t); f.Total(KindOrphanObject) != 0 {
			t.Fatalf("a key in another shape was reported as %v", f.Rows())
		}
	}
	if _, held := i.memory.Bytes(aPrefix + "somebody/elses/key"); !held {
		t.Fatal("a key this installation did not write was deleted")
	}
}

func TestPassOneForgetsACandidateThatLeftTheBucketOnItsOwn(t *testing.T) {
	i := an(t, nil)
	id := object.NewID()
	key := i.put(t, id)
	i.run(t)
	if err := i.memory.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	i.run(t)

	// The key comes back: the window is armed afresh rather than already
	// spent, so the bytes of a put in flight are not deleted at once.
	i.put(t, id)
	i.now = i.now.Add(2 * OrphanGrace)
	if f := i.run(t); f.Count(KindOrphanCandidate, Found) != 1 {
		t.Fatalf("a key that came back was reported as %v", f.Rows())
	}
}

func TestPassOnePagesOnTheTruncationFlagAndNotOnAShortPage(t *testing.T) {
	i := an(t, nil)
	first, second := object.NewID(), object.NewID()
	paged := &pagedBucket{Store: i.bucket, pages: []blob.Listing{
		{Keys: []string{first.Key(aPrefix)}, Truncated: true},
		{Keys: []string{second.Key(aPrefix)}},
	}}
	r, err := New(Options{DB: i.world, Bucket: paged, Prefix: aPrefix, TrashRetention: time.Hour,
		Now: func() time.Time { return i.now }, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f, err := r.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if f.Count(KindOrphanCandidate, Found) != 2 {
		t.Fatalf("the sweep read %v, and it should have read both pages", f.Rows())
	}
}

func TestPassOneReportsABucketThatWillNotAnswer(t *testing.T) {
	i := an(t, nil)
	i.bucket.FailNth(blob.MethodList, 1, errors.New("the bucket is down"))
	if _, err := i.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "orphan bytes") {
		t.Fatalf("a bucket that would not list answered %v", err)
	}
}

func TestPassOneDefersAnOrphanItCouldNotDelete(t *testing.T) {
	i := an(t, nil)
	i.put(t, object.NewID())
	i.run(t)
	i.now = i.now.Add(2 * OrphanGrace)
	i.bucket.FailNth(blob.MethodDelete, 1, errors.New("the bucket is down"))

	f := i.run(t)
	if f.Count(KindOrphanObject, Deferred) != 1 || f.Count(KindOrphanObject, Repaired) != 0 {
		t.Fatalf("an orphan that would not delete was reported as %v", f.Rows())
	}
	// The next run tries again, which is what makes a rising found beside a
	// flat repaired the alert of spec 018 rather than a lost key.
	if next := i.run(t); next.Count(KindOrphanObject, Repaired) != 1 {
		t.Fatalf("the next run reported %v", next.Rows())
	}
}

// Criterion 14 of spec 010: a row whose key the bucket does not hold is
// reported and not deleted. A row without bytes is a lie the database is
// telling, and the right answer is a finding a human reads.
func TestPassTwoReportsARowWithoutItsBytesAndDeletesNothing(t *testing.T) {
	i := an(t, nil)
	live, gone := object.NewID(), object.NewID()
	i.put(t, live)
	i.world.files = append(i.world.files,
		fileRow{owner: aSpace, path: "files/here.pdf", id: live, size: 9},
		fileRow{owner: aSpace, path: "files/gone.pdf", id: gone, size: 9})
	i.world.versions = append(i.world.versions, fileRow{owner: aSpace, path: "files/gone.pdf", id: gone, size: 9})

	f := i.run(t)
	if f.Count(KindMissingBytes, Found) != 2 {
		t.Fatalf("the pass found %v, and one row of each table names bytes that are gone", f.Rows())
	}
	if len(i.world.files) != 2 || len(i.world.versions) != 1 {
		t.Fatal("the pass deleted a row")
	}
	if !strings.Contains(i.lines.String(), "the bucket does not hold the object a row names") {
		t.Fatalf("the finding reached no log line: %q", i.lines.String())
	}
	if strings.Contains(i.lines.String(), "gone.pdf") {
		t.Fatal("the line names the path, which carries what a person called their file")
	}
}

func TestPassTwoReportsAStoreThatWillNotAnswer(t *testing.T) {
	i := an(t, nil)
	i.world.faults["SELECT owner, object_id FROM files"] = errors.New("the connection went away")
	if _, err := i.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "orphan rows") {
		t.Fatalf("a store that would not answer gave %v", err)
	}
}

// Criterion 16 of spec 010 at the seam: trash past the retention leaves both
// stores, lowers the ledger, and appears as a purge event.
func TestPassFiveTakesTheRowsTheKeysAndTheBytesOfTheLedger(t *testing.T) {
	i := an(t, nil)
	id := object.NewID()
	key := i.put(t, id)
	deleted := aMoment.Add(-800 * time.Hour)
	i.world.files = append(i.world.files,
		fileRow{owner: aSpace, path: "files/old.pdf", id: id, size: 9, deleted: &deleted})
	i.world.usage[aSpace] = 9

	f := i.run(t)
	if f.Count(KindTrashPurged, Repaired) != 1 {
		t.Fatalf("the pass reported %v", f.Rows())
	}
	if len(i.world.files) != 0 {
		t.Fatal("the row stayed")
	}
	if _, held := i.memory.Bytes(key); held {
		t.Fatal("the bytes stayed")
	}
	if i.world.usage[aSpace] != 0 {
		t.Fatalf("the ledger holds %d after the purge", i.world.usage[aSpace])
	}
	if i.world.txs == 0 {
		t.Fatal("the rows and the ledger did not move in one transaction")
	}
	if !slices.Contains(i.world.actions(), "purge") {
		t.Fatalf("the log holds %v, and a purge is the last thing said about an object", i.world.actions())
	}
	// Pass 10 runs after it and has nothing to correct, because the delta
	// went with the rows.
	if f.Total(KindUsageCorrected) != 0 {
		t.Fatalf("a healthy purge left the ledger to correct: %v", f.Rows())
	}
}

func TestPassFiveKeepsBytesAVersionStillNames(t *testing.T) {
	i := an(t, nil)
	id := object.NewID()
	key := i.put(t, id)
	deleted := aMoment.Add(-800 * time.Hour)
	i.world.files = append(i.world.files, fileRow{owner: aSpace, path: "files/old.pdf", id: id, size: 9, deleted: &deleted})
	i.world.versions = append(i.world.versions, fileRow{owner: aSpace, path: "files/old.pdf", id: id, size: 9})

	i.run(t)
	if _, held := i.memory.Bytes(key); !held {
		t.Fatal("the bytes a surviving version names were deleted with the row")
	}
}

func TestPassFiveLeavesTheBytesToPassOneWhenTheBucketRefuses(t *testing.T) {
	i := an(t, nil)
	id := object.NewID()
	i.put(t, id)
	deleted := aMoment.Add(-800 * time.Hour)
	i.world.files = append(i.world.files, fileRow{owner: aSpace, path: "files/old.pdf", id: id, size: 9, deleted: &deleted})
	i.bucket.FailNth(blob.MethodDeleteMany, 1, errors.New("the bucket is down"))

	f := i.run(t)
	if f.Count(KindTrashPurged, Repaired) != 1 || f.Count(KindOrphanObject, Deferred) != 1 {
		t.Fatalf("the pass reported %v", f.Rows())
	}
}

func TestPassFiveReportsAStoreThatWillNotAnswer(t *testing.T) {
	i := an(t, nil)
	i.world.faults["DELETE FROM files WHERE deleted_at"] = errors.New("the connection went away")
	if _, err := i.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "trash") {
		t.Fatalf("a store that would not answer gave %v", err)
	}
}

func TestPassEightDropsAStarWhoseTargetIsGoneAndKeepsOneOnATrashedTarget(t *testing.T) {
	i := an(t, nil)
	trashed := aMoment.Add(-time.Hour)
	i.world.files = append(i.world.files,
		fileRow{owner: aSpace, path: "files/trashed.pdf", id: object.NewID(), deleted: &trashed})
	i.world.stars = append(i.world.stars,
		starRow{owner: aSpace, path: "files/trashed.pdf"},
		starRow{owner: anotherSpace, path: "files/purged.pdf"})

	f := i.run(t)
	if f.Count(KindStarPruned, Repaired) != 1 {
		t.Fatalf("the pass reported %v", f.Rows())
	}
	if len(i.world.stars) != 1 || i.world.stars[0].owner != aSpace {
		t.Fatalf("the stars left are %v, and a trashed target is restorable", i.world.stars)
	}
}

func TestPassNinePrunesTheLogAndAppendsNothingOfItsOwn(t *testing.T) {
	i := an(t, nil)
	i.world.events = append(i.world.events,
		eventRow{id: 1, owner: aSpace, action: "put", created: aMoment.Add(-LogRetention - time.Hour)},
		eventRow{id: 2, owner: aSpace, action: "put", created: aMoment.Add(-time.Hour)})

	f := i.run(t)
	if f.Count(KindEventPruned, Repaired) != 1 {
		t.Fatalf("the pass reported %v", f.Rows())
	}
	if got := i.world.actions(); !slices.Equal(got, []string{"put"}) {
		t.Fatalf("the log holds %v, and pruning it appends nothing", got)
	}
}

// Criterion 17 of spec 010 at the seam: a ledger row that disagrees with the
// rows holding its bytes is corrected and reported, and a healthy run
// corrects nothing.
func TestLedgerReconciles(t *testing.T) {
	i := an(t, nil)
	i.world.files = append(i.world.files, fileRow{owner: aSpace, path: "files/q3.pdf", id: object.NewID(), size: 100})
	i.world.usage[aSpace] = 100
	i.world.usage[anotherSpace] = 4096

	f := i.run(t)
	if f.Count(KindUsageCorrected, Repaired) != 1 || f.Count(KindUsageCorrected, Found) != 1 {
		t.Fatalf("the pass reported %v, and one of the two spaces had drifted", f.Rows())
	}
	if i.world.usage[anotherSpace] != 0 || i.world.usage[aSpace] != 100 {
		t.Fatalf("the ledger reads %v", i.world.usage)
	}
	if !strings.Contains(i.lines.String(), "the ledger disagrees") {
		t.Fatalf("the correction reached no log line: %q", i.lines.String())
	}
	if !slices.Contains(i.world.actions(), "reap") {
		t.Fatalf("the log holds %v, and a run that changed a space says so", i.world.actions())
	}
	// A healthy run corrects nothing, which is what makes a correction a
	// finding rather than routine housekeeping.
	if next := i.run(t); next.Total(KindUsageCorrected) != 0 {
		t.Fatalf("the second run corrected %v", next.Rows())
	}
}

// A correction racing a live charge loses and is recomputed on the next run
// instead of erasing the charge, which is the one thing pass 10 has to be
// told about two replicas running it at once.
func TestLedgerCorrectionLosesToALiveCharge(t *testing.T) {
	i := an(t, nil)
	i.world.usage[aSpace] = 4096
	landed := false
	i.world.beforeCorrect = func() {
		if !landed {
			landed = true
			i.world.usage[aSpace] = 8192
		}
	}

	f := i.run(t)
	if f.Count(KindUsageCorrected, Deferred) != 1 || f.Count(KindUsageCorrected, Repaired) != 0 {
		t.Fatalf("a correction that lost a race reported %v", f.Rows())
	}
	if i.world.usage[aSpace] != 8192 {
		t.Fatalf("the ledger reads %d, and the charge was erased", i.world.usage[aSpace])
	}
	// The next run recomputes and corrects, because nothing raced it.
	if next := i.run(t); next.Count(KindUsageCorrected, Repaired) != 1 {
		t.Fatalf("the next run reported %v", next.Rows())
	}
}

func TestPassTenReportsAStoreThatWillNotAnswer(t *testing.T) {
	i := an(t, nil)
	i.world.usage[aSpace] = 4096
	i.world.faults["SUM(size_bytes)"] = errors.New("the connection went away")
	if _, err := i.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "ledger reconciliation") {
		t.Fatalf("a store that would not answer gave %v", err)
	}
}

func TestTheSeamsRunWhenTheyAreGivenAndNotOtherwise(t *testing.T) {
	counts := map[string]*countingPass{}
	i := an(t, func(o *Options) {
		for _, name := range []string{"leases", "sessions", "tombstones", "grants"} {
			counts[name] = &countingPass{found: 2}
		}
		o.Leases, o.Sessions = counts["leases"], counts["sessions"]
		o.Tombstones, o.Grants = counts["tombstones"], counts["grants"]
	})
	f := i.run(t)
	for _, kind := range []Kind{KindLeaseExpired, KindUploadAborted, KindWorkspacePurged, KindShareExpired} {
		if f.Count(kind, Repaired) != 2 {
			t.Fatalf("%s reported %v", kind, f.Rows())
		}
	}
	for name, p := range counts {
		if p.swept != 1 {
			t.Fatalf("the %s pass ran %d times", name, p.swept)
		}
	}
	// A pass the reconciler was not given does not run and reports nothing.
	bare := an(t, nil).run(t)
	for _, kind := range []Kind{KindLeaseExpired, KindUploadAborted, KindWorkspacePurged, KindShareExpired} {
		if bare.Total(kind) != 0 {
			t.Fatalf("a pass that was not given reported %v", bare.Rows())
		}
	}
}

func TestAPassThatFailsDoesNotStopTheRest(t *testing.T) {
	failing := errors.New("the table is not there")
	i := an(t, func(o *Options) {
		o.Leases = &countingPass{err: failing}
		o.Grants = &countingPass{err: failing}
	})
	i.world.usage[aSpace] = 4096

	f, err := i.Run(t.Context())
	if err == nil {
		t.Fatal("a run with two failed passes reported success")
	}
	for _, name := range []string{"expired leases", "grant hygiene"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the run reported %v, which does not name %s", err, name)
		}
	}
	// The predecessor returned on the first failure, so a bucket that was
	// down kept the ledger from ever reconciling. Every pass runs.
	if f.Count(KindUsageCorrected, Repaired) != 1 {
		t.Fatalf("the ledger pass reported %v after an earlier failure", f.Rows())
	}
}

// Criterion 19 of spec 010, and the second bug the predecessor's reconciler
// carried: its dry run returned before counting in three of its passes and
// skipped two counters in a fourth, so what it printed was not what a run
// would do. A dry run reports the same findings and changes nothing.
func TestDryRunReportsWhatARunWouldChange(t *testing.T) {
	dry, wet := an(t, func(o *Options) { o.DryRun = true }), an(t, nil)
	for _, i := range []*installation{dry, wet} {
		fixture(t, i)
	}
	dryFindings, wetFindings := dry.run(t), wet.run(t)

	for _, kind := range Kinds() {
		if dryFindings.Count(kind, Found) != wetFindings.Count(kind, Found) {
			t.Errorf("the dry run found %d of %s and a run found %d",
				dryFindings.Count(kind, Found), kind, wetFindings.Count(kind, Found))
		}
	}
	if dryFindings.Total(KindTrashPurged) == 0 || dryFindings.Total(KindStarPruned) == 0 ||
		dryFindings.Total(KindEventPruned) == 0 || dryFindings.Total(KindUsageCorrected) == 0 ||
		dryFindings.Total(KindOrphanObject) == 0 {
		t.Fatalf("the dry run reported %v, and the fixture gives every pass something", dryFindings.Rows())
	}

	// And it changed nothing in either store.
	if len(dry.world.files) != 2 || len(dry.world.stars) != 1 || len(dry.world.events) != 1 {
		t.Fatalf("the dry run changed the database: %d files, %d stars, %d events",
			len(dry.world.files), len(dry.world.stars), len(dry.world.events))
	}
	if dry.world.usage[aSpace] != 4096 {
		t.Fatalf("the dry run moved the ledger to %d", dry.world.usage[aSpace])
	}
	if len(dry.memory.Keys()) != 3 {
		t.Fatalf("the dry run left %d keys in the bucket", len(dry.memory.Keys()))
	}
	if dry.world.txs != 0 {
		t.Fatal("the dry run opened a transaction")
	}
}

// fixture gives every pass of a run something to find: a live row, a trashed
// row past its retention, an orphan past its grace window, a star whose
// target is gone, a log row past thirty days, and a ledger that drifted.
func fixture(t *testing.T, i *installation) {
	t.Helper()
	live, trashed, orphan := object.NewID(), object.NewID(), object.NewID()
	i.put(t, live)
	i.put(t, trashed)
	key := i.put(t, orphan)
	// The orphan was first seen a window ago, which is the state a second
	// run of a process reaches and the one a dry run has to report the same.
	i.candidates[key] = aMoment.Add(-2 * OrphanGrace)
	deleted := aMoment.Add(-800 * time.Hour)
	i.world.files = append(i.world.files,
		fileRow{owner: aSpace, path: "files/live.pdf", id: live, size: 100},
		fileRow{owner: aSpace, path: "files/old.pdf", id: trashed, size: 9, deleted: &deleted})
	i.world.stars = append(i.world.stars, starRow{owner: aSpace, path: "files/purged.pdf"})
	i.world.events = append(i.world.events,
		eventRow{id: 1, owner: aSpace, action: "put", created: aMoment.Add(-LogRetention - time.Hour)})
	i.world.usage[aSpace] = 4096
}

// Criterion 18 of spec 010 at the seam: every pass is idempotent, so a
// second run over what the first left changes nothing.
func TestARunTwiceLeavesWhatOneRunLeft(t *testing.T) {
	i := an(t, nil)
	fixture(t, i)

	i.run(t)
	i.run(t)
	keys, files, usage := len(i.memory.Keys()), len(i.world.files), i.world.usage[aSpace]

	third := i.run(t)
	if len(i.memory.Keys()) != keys || len(i.world.files) != files || i.world.usage[aSpace] != usage {
		t.Fatal("a further run changed what the runs before it left")
	}
	for _, kind := range []Kind{KindTrashPurged, KindOrphanObject, KindUsageCorrected, KindStarPruned} {
		if third.Total(kind) != 0 {
			t.Fatalf("a settled installation reported %v", third.Rows())
		}
	}
}

func TestTheMetricsSeamSeesEveryFindingAndTheRun(t *testing.T) {
	m := &recordingMetrics{}
	i := an(t, func(o *Options) { o.Metrics = m })
	i.world.usage[aSpace] = 4096

	i.run(t)
	if m.runs != 1 || !m.ok {
		t.Fatalf("the seam saw %d runs, ok=%v", m.runs, m.ok)
	}
	if len(m.usage) != 1 || m.usage[0] != 4096 {
		t.Fatalf("the seam saw the usage as %v, once per space per run", m.usage)
	}
	if m.findings[finding{KindUsageCorrected, Repaired}] != 1 {
		t.Fatalf("the seam saw %v", m.findings)
	}
	// A reconciler with no seam records nothing and still runs.
	an(t, nil).run(t)
}

func TestLoopRunsOnATickAndNotAtAllWhenTheIntervalIsZero(t *testing.T) {
	i := an(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	i.Loop(ctx, 0) // an interval of zero returns at once
	if len(i.world.sent) != 0 {
		t.Fatal("an interval of zero ran a pass")
	}

	j := an(t, nil)
	j.world.faults["SELECT owner, object_id FROM files"] = errors.New("the connection went away")
	ticking, stop := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer stop()
	j.Loop(ticking, 20*time.Millisecond)
	if len(j.world.sent) == 0 {
		t.Fatal("the loop ran no pass")
	}
	if !strings.Contains(j.lines.String(), "the reconciliation") {
		t.Fatalf("a failed run reached no log line: %q", j.lines.String())
	}
}

// pagedBucket answers the listing pages the case scripted, so the sweep is
// held to the store's truncation flag rather than to a short page.
type pagedBucket struct {
	blob.Store
	pages []blob.Listing
	at    int
}

func (b *pagedBucket) List(context.Context, string, string, int32) (blob.Listing, error) {
	if b.at >= len(b.pages) {
		return blob.Listing{}, nil
	}
	b.at++
	return b.pages[b.at-1], nil
}

// countingPass is one of the four seams, as the spec that owns its table
// will implement it.
type countingPass struct {
	found int
	err   error
	swept int
	dry   bool
}

func (p *countingPass) Sweep(_ context.Context, _ store.Querier, _ time.Time, dry bool) (int, error) {
	p.swept++
	p.dry = dry
	return p.found, p.err
}

// recordingMetrics is spec 018's seam, as a test reads it.
type recordingMetrics struct {
	findings map[finding]int
	usage    []int64
	runs     int
	ok       bool
}

func (m *recordingMetrics) Finding(kind Kind, outcome Outcome, n int) {
	if m.findings == nil {
		m.findings = map[finding]int{}
	}
	m.findings[finding{kind, outcome}] += n
}

func (m *recordingMetrics) Usage(bytes int64) { m.usage = append(m.usage, bytes) }

func (m *recordingMetrics) Run(ok bool, _ time.Duration) {
	m.runs++
	m.ok = ok
}
