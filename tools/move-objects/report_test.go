// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"latere.ai/x/arca/tools/internal/manifest"
)

// rendered answers the report's text over the outcomes.
func rendered(r *Report) string {
	var b strings.Builder
	r.Write(&b)
	return b.String()
}

func TestTheReportNamesEveryKeyAnOperatorHasToAnswerFor(t *testing.T) {
	r := NewReport("manifest.tsv", prefix, "arca-test", false)
	r.Outcomes = []Outcome{
		{Entry: manifest.Entry{Key: notesKey}, Destination: "drive/1f/a", State: Copied},
		{Entry: manifest.Entry{Key: logoKey}, Destination: "drive/20/b", State: Skipped},
		{Entry: manifest.Entry{Key: "drive/u-1/files/odd"}, Destination: "drive/21/c", State: Mismatched,
			Why: "the destination holds 3 bytes and the manifest says 10"},
		{Entry: manifest.Entry{Key: goneKey}, Destination: "drive/22/d", State: Failed,
			Why: "the source is not in the bucket"},
	}
	if r.OK() {
		t.Fatal("a report with a mismatch and a failure reads as clean")
	}
	out := rendered(r)
	for _, says := range []string{
		"copied", "skipped", "mismatched", "failed",
		"drive/u-1/files/odd", "the destination holds 3 bytes",
		goneKey, "the source is not in the bucket",
		"the move is not clean",
	} {
		if !strings.Contains(out, says) {
			t.Errorf("the report holds no %q:\n%s", says, out)
		}
	}
	// A key that worked is not named: a list of what worked is a list
	// nobody reads.
	if strings.Count(out, notesKey) != 0 {
		t.Errorf("the report names a key that worked:\n%s", out)
	}
	for state, want := range map[State]int{Copied: 1, Skipped: 1, Mismatched: 1, Failed: 1} {
		if got := r.Count(state); got != want {
			t.Errorf("%s counts %d, want %d", name(state), got, want)
		}
	}
}

func TestTheReportCountsWhatItCouldNotProveAndWhatTheStoreWouldNotDo(t *testing.T) {
	r := NewReport("manifest.tsv", prefix, "arca-test", false)
	r.Outcomes = []Outcome{
		{Entry: manifest.Entry{Key: notesKey}, State: Copied, SizeOnly: true},
		{Entry: manifest.Entry{Key: logoKey}, State: Skipped, SizeOnly: true, Unstamped: true},
	}
	if !r.OK() {
		t.Fatal("a report of a copy and a skip does not read as clean")
	}
	out := rendered(r)
	for _, says := range []string{
		"noted", "verified on size", "publicity not stamped",
		"bucket policy", "the move holds",
	} {
		if !strings.Contains(out, says) {
			t.Errorf("the report holds no %q:\n%s", says, out)
		}
	}
}

func TestADryRunsReportReadsInTheConditional(t *testing.T) {
	r := NewReport("manifest.tsv", prefix, "arca-test", true)
	r.Outcomes = []Outcome{{Entry: manifest.Entry{Key: notesKey}, State: Copied}}
	out := rendered(r)
	for _, says := range []string{
		"dry run, nothing is written", "would copy it", "the dry run holds", "without -dry-run",
	} {
		if !strings.Contains(out, says) {
			t.Errorf("the report holds no %q:\n%s", says, out)
		}
	}
}

func TestAManifestOfNoKeysIsACleanMove(t *testing.T) {
	// A copy with no bytes to move leaves an empty manifest, and a move over
	// it is not a failure.
	r := NewReport("manifest.tsv", prefix, "arca-test", false)
	if !r.OK() {
		t.Fatal("an empty manifest does not read as clean")
	}
	if !strings.Contains(rendered(r), "0 keys") {
		t.Errorf("the report does not say it moved nothing:\n%s", rendered(r))
	}
}
