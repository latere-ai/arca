// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/arca/tools/internal/manifest"
)

// The keys the manifest cases read, and the digest the rows carry for them.
const (
	objectNotes = "drive/u-1/files/notes.md"
	objectLogo  = "drive/u-1/files/logo.png"
	objectBig   = "drive/u-1/files/big.bin@ab12ef34cd56"
	objectSHA   = "1111111111111111111111111111111111111111111111111111111111111111"
	logoSHA     = "2222222222222222222222222222222222222222222222222222222222222222"
	staleSHA    = "3333333333333333333333333333333333333333333333333333333333333333"
)

// objectSource answers a source whose object pass reads the given rows. The
// columns are the key, the size, the checksum, the publicity and whether the
// row is a live file, which is the order the query reads them in.
func objectSource(rows ...[]any) *fake {
	return newFake(answer{"true AS live", rows})
}

// runOver answers a run whose object pass has already read the source.
func runOver(t *testing.T, source *fake, dryRun bool) *Run {
	t.Helper()
	r := testRunPair(source, emptyTarget(), dryRun)
	if err := Objects(t.Context(), r); err != nil {
		t.Fatalf("Objects: %v", err)
	}
	return r
}

func TestOneKeyBecomesOneObjectIDAndOneManifestLine(t *testing.T) {
	r := runOver(t, objectSource(
		[]any{objectNotes, int64(10), objectSHA, false, true},
		[]any{objectNotes, int64(5), staleSHA, false, false},
		[]any{objectLogo, int64(84), logoSHA, true, true},
	), false)

	entries := r.ManifestEntries()
	if len(entries) != 2 {
		t.Fatalf("two keys became %d lines", len(entries))
	}
	// A file and the version it superseded name one key, so they keep one
	// object id: what the reference union of spec 004 counts, and what the
	// move has one destination for.
	if r.objectID(objectNotes) != entries[0].ID {
		t.Error("one key was minted two ids")
	}
	if entries[0].ID == entries[1].ID {
		t.Error("two keys were minted one id")
	}
	// The live row describes the bytes the key holds now; the version's
	// size is what the key held before it was overwritten in place.
	if entries[0].Size != 10 || entries[0].Checksum != objectSHA {
		t.Errorf("the live row did not win: %+v", entries[0])
	}
	if entries[0].Public || !entries[1].Public {
		t.Errorf("publicity is %v and %v", entries[0].Public, entries[1].Public)
	}
	if n := r.Report.Table("files").Noted[NoteTwoDescriptions]; n != 1 {
		t.Errorf("%d keys were counted as described twice, want 1", n)
	}
}

func TestALiveRowWinsWhateverOrderItArrivesIn(t *testing.T) {
	// The query orders the live rows first, and the rule does not rest on
	// it: a version read first is replaced by the live row behind it.
	r := runOver(t, objectSource(
		[]any{objectNotes, int64(5), staleSHA, false, false},
		[]any{objectNotes, int64(10), objectSHA, true, true},
	), false)
	entries := r.ManifestEntries()
	if entries[0].Size != 10 || entries[0].Checksum != objectSHA || !entries[0].Public {
		t.Fatalf("the line is %+v", entries[0])
	}
}

func TestTwoVersionsOfOneKeyAreCountedAndTheFirstStands(t *testing.T) {
	r := runOver(t, objectSource(
		[]any{objectNotes, int64(5), staleSHA, false, false},
		[]any{objectNotes, int64(7), objectSHA, false, false},
	), false)
	if got := r.ManifestEntries()[0].Size; got != 5 {
		t.Errorf("the line carries %d bytes, want the first version's 5", got)
	}
	if n := r.Report.Table("files").Noted[NoteTwoDescriptions]; n != 1 {
		t.Errorf("the disagreement was counted %d times", n)
	}
}

func TestAKeyWithNoObjectYetIsLeftOffTheManifest(t *testing.T) {
	r := runOver(t, objectSource([]any{objectNotes, int64(10), objectSHA, false, true}), false)
	// An open upload's destination carries an object id like any other key
	// and no bytes: its parts are invisible to a listing until it completes.
	id := r.objectID(objectBig)
	if id == "" {
		t.Fatal("an upload session got no object id")
	}
	entries := r.ManifestEntries()
	if len(entries) != 1 || entries[0].Key != objectNotes {
		t.Fatalf("the manifest holds %+v", entries)
	}

	r.Manifest = filepath.Join(t.TempDir(), "manifest.tsv")
	if err := WriteManifest(r); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if r.Report.ManifestKeys != 1 {
		t.Errorf("the report counts %d keys", r.Report.ManifestKeys)
	}
	written, err := manifest.ReadFile(r.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Entries) != 1 || written.Entries[0].Key != objectNotes {
		t.Errorf("the file lists %+v", written.Entries)
	}
}

func TestTheManifestIsWrittenBeforeTheCopyAndCompletedAfterIt(t *testing.T) {
	r := runOver(t, objectSource(
		[]any{objectNotes, int64(10), objectSHA, false, true},
		[]any{objectLogo, int64(84), logoSHA, true, true},
	), false)
	r.Manifest = filepath.Join(t.TempDir(), "manifest.tsv")

	if err := WriteManifest(r); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	// Before the verification the file is a body with no trailer, which is
	// what the move refuses.
	body, err := manifest.ReadFile(r.Manifest)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if body.Complete || len(body.Entries) != 2 || body.Prefix != "drive/" {
		t.Fatalf("the body is %+v", body)
	}
	if body.Entries[0].Key != objectNotes || body.Entries[1].Key != objectLogo {
		t.Errorf("the lines are out of the source's key order: %+v", body.Entries)
	}

	// A copy that did not verify leaves the body as it is.
	if err := CompleteManifest(r); err != nil {
		t.Fatal(err)
	}
	if done, _ := manifest.ReadFile(r.Manifest); done.Complete {
		t.Fatal("an unverified copy completed its manifest")
	}

	for _, name := range TableNames() {
		r.Report.Verify(name, "counts hold", true)
	}
	if err := CompleteManifest(r); err != nil {
		t.Fatalf("CompleteManifest: %v", err)
	}
	done, err := manifest.ReadFile(r.Manifest)
	if err != nil || !done.Complete {
		t.Fatalf("the verified copy left %+v, %v", done, err)
	}
	if !r.Report.ManifestComplete {
		t.Error("the report does not say the manifest is complete")
	}
}

func TestADryRunCountsTheKeysAndWritesNoManifest(t *testing.T) {
	r := runOver(t, objectSource([]any{objectNotes, int64(10), objectSHA, false, true}), true)
	r.Manifest = filepath.Join(t.TempDir(), "manifest.tsv")

	if err := WriteManifest(r); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if _, err := os.Stat(r.Manifest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run wrote a manifest: %v", err)
	}
	if r.Report.ManifestKeys != 1 {
		t.Errorf("the dry run counts %d keys", r.Report.ManifestKeys)
	}
	for _, name := range TableNames() {
		r.Report.Verify(name, "the source adds up", true)
	}
	if err := CompleteManifest(r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Manifest); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run completed a manifest it never wrote")
	}
}

func TestARunWithNoManifestFlagWritesNone(t *testing.T) {
	r := runOver(t, objectSource([]any{objectNotes, int64(10), objectSHA, false, true}), false)
	if err := WriteManifest(r); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if err := CompleteManifest(r); err != nil {
		t.Fatalf("CompleteManifest: %v", err)
	}
	if r.Report.ManifestPath != "" || r.Report.ManifestKeys != 0 {
		t.Errorf("the report names a manifest: %q, %d", r.Report.ManifestPath, r.Report.ManifestKeys)
	}
	if got := reportOf(r); !strings.Contains(got, "-manifest <path>") {
		t.Errorf("the report does not name the flag:\n%s", got)
	}
}

func TestAManifestThatCannotBeWrittenStopsTheRun(t *testing.T) {
	r := runOver(t, objectSource([]any{objectNotes, int64(10), objectSHA, false, true}), false)
	r.Manifest = filepath.Join(t.TempDir(), "no-such-directory", "manifest.tsv")
	if err := WriteManifest(r); err == nil {
		t.Fatal("a manifest under a directory that is not there was written")
	}
}

func TestAnObjectPassThatCannotReadStopsTheRun(t *testing.T) {
	boom := errors.New("the database went away")
	source := objectSource()
	source.queryErr["true AS live"] = boom
	if err := Objects(t.Context(), testRunPair(source, emptyTarget(), false)); !errors.Is(err, boom) {
		t.Errorf("Objects answered %v", err)
	}
}

// reportOf renders a report the way a run prints it.
func reportOf(r *Run) string {
	var b strings.Builder
	r.Report.Write(&b)
	return b.String()
}
