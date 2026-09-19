// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"strings"
	"testing"
)

// verifiedSource is the fixture of plan_test.go with the counts Drive's
// tables hold and the sample the verification reads out of them.
func verifiedSource() *fake {
	f := source()
	// The counts go in front of the copy's own queries, because a count over
	// one table holds that table's name and would otherwise read its rows.
	f.answers = append([]answer{
		{"count(*) FROM principal_directory", [][]any{{int64(2)}}},
		{"count(*) FROM files", [][]any{{int64(4)}}},
		{"count(*) FROM file_versions", [][]any{{int64(1)}}},
		{"count(*) FROM stars", [][]any{{int64(1)}}},
		{"count(*) FROM upload_sessions", [][]any{{int64(1)}}},
		{"count(*) FROM shares", [][]any{{int64(9)}}},
		{"count(*) FROM workspaces", [][]any{{int64(2)}}},
		{"count(*) FROM workspace_attachments", [][]any{{int64(1)}}},
		{"count(*) FROM events", [][]any{{int64(3)}}},
		{"ORDER BY md5(id::text)", [][]any{{"f1", sha, int64(10)}}},
	}, f.answers...)
	return f
}

// verifiedTarget is Arca's side: empty before the copy, and afterwards holding
// the rows the copy wrote. The preflight reads the first set of counts and the
// verification the second, which is what after says.
func verifiedTarget() *fake {
	return newFake(answer{"count(*) FROM", [][]any{{int64(0)}}})
}

// after gives the target the counts it holds once the copy has run.
func after(f *fake) *fake {
	f.answers = append([]answer{
		{"count(*) FROM subjects", [][]any{{int64(2)}}},
		{"count(*) FROM files", [][]any{{int64(4)}}},
		{"count(*) FROM file_versions", [][]any{{int64(1)}}},
		{"count(*) FROM stars", [][]any{{int64(1)}}},
		{"count(*) FROM upload_sessions", [][]any{{int64(1)}}},
		{"count(*) FROM shares", [][]any{{int64(5)}}},
		{"count(*) FROM workspaces", [][]any{{int64(2)}}},
		{"count(*) FROM workspace_attachments", [][]any{{int64(1)}}},
		{"count(*) FROM events", [][]any{{int64(3)}}},
		{"id = ANY($1::uuid[])", [][]any{{"f1", sha, int64(10)}}},
		{"SELECT owner, bytes FROM space_usage", [][]any{
			{selfOwner, int64(75)}, {orgOwner, int64(30)},
		}},
	}, f.answers...)
	return f
}

// verified runs the plan and hands back the run, so a case starts from the
// counters a copy leaves behind.
func verified(t *testing.T, src, dst *fake, dryRun bool) *Run {
	t.Helper()
	r := testRunPair(src, dst, dryRun)
	if err := r.Copy(t.Context()); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	return r
}

func TestACopyThatAddsUpVerifies(t *testing.T) {
	r := verified(t, verifiedSource(), after(verifiedTarget()), false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.Report.OK() {
		for _, name := range TableNames() {
			t.Logf("%s: %s", name, r.Report.Table(name).Verified)
		}
		t.Fatal("a copy that adds up does not verify")
	}
	if got := r.Report.Table("files").Verified; !strings.Contains(got, "1 checksums were sampled") {
		t.Errorf("the files verdict is %q", got)
	}
	if got := r.Report.Table("space_usage").Verified; !strings.Contains(got, "2 spaces") {
		t.Errorf("the ledger verdict is %q", got)
	}
}

func TestARowThatWentMissingBetweenTheReadAndTheDecisionFails(t *testing.T) {
	src := verifiedSource()
	replace(src, "count(*) FROM stars", [][]any{{int64(5)}})
	r := verified(t, src, after(verifiedTarget()), false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Report.OK() {
		t.Fatal("a source holding more rows than the copy accounted for verifies")
	}
	if got := r.Report.Table("stars").Verified; !strings.Contains(got, "the source holds 5 rows") {
		t.Errorf("the verdict is %q", got)
	}
}

func TestARowThatWentMissingBetweenTheDecisionAndTheCommitFails(t *testing.T) {
	dst := after(verifiedTarget())
	replace(dst, "count(*) FROM subjects", [][]any{{int64(1)}})
	r := verified(t, verifiedSource(), dst, false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := r.Report.Table("subjects").Verified; !strings.Contains(got, "the target holds 1 rows") {
		t.Errorf("the verdict is %q", got)
	}
	if r.Report.OK() {
		t.Fatal("a target holding fewer rows than the copy wrote verifies")
	}
}

func TestAChecksumThatDiffersFails(t *testing.T) {
	dst := after(verifiedTarget())
	replace(dst, "id = ANY($1::uuid[])", [][]any{{"f1", strings.Repeat("f", 64), int64(10)}})
	r := verified(t, verifiedSource(), dst, false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := r.Report.Table("files").Verified; !strings.Contains(got, "carry another checksum") {
		t.Errorf("the verdict is %q", got)
	}
	if r.Report.OK() {
		t.Fatal("a sampled row carrying another checksum verifies")
	}
}

func TestASampledRowThatIsNotInTheTargetFails(t *testing.T) {
	dst := after(verifiedTarget())
	replace(dst, "id = ANY($1::uuid[])", [][]any{})
	r := verified(t, verifiedSource(), dst, false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := r.Report.Table("files").Verified; !strings.Contains(got, "0 of 1 sampled rows") {
		t.Errorf("the verdict is %q", got)
	}
}

func TestATableWithNoRowsHasNothingToSample(t *testing.T) {
	src := verifiedSource()
	replace(src, "FROM files ORDER BY id", [][]any{})
	replace(src, "count(*) FROM files", [][]any{{int64(0)}})
	replace(src, "ORDER BY md5(id::text)", [][]any{})
	dst := after(verifiedTarget())
	replace(dst, "count(*) FROM files", [][]any{{int64(0)}})
	replace(dst, "SELECT owner, bytes FROM space_usage", [][]any{{orgOwner, int64(30)}})
	r := verified(t, src, dst, false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := r.Report.Table("files").Verified; !strings.Contains(got, "no row to sample") {
		t.Errorf("the verdict is %q", got)
	}
}

func TestALedgerThatDisagreesWithTheSumFails(t *testing.T) {
	dst := after(verifiedTarget())
	replace(dst, "SELECT owner, bytes FROM space_usage", [][]any{
		{selfOwner, int64(1)}, {orgOwner, int64(30)},
	})
	r := verified(t, verifiedSource(), dst, false)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := r.Report.Table("space_usage").Verified; !strings.Contains(got, "1 differ") {
		t.Errorf("the verdict is %q", got)
	}
	if r.Report.OK() {
		t.Fatal("a ledger that disagrees with the sum verifies")
	}
}

func TestADryRunVerifiesTheSourceAndNotTheTarget(t *testing.T) {
	dst := verifiedTarget()
	r := verified(t, verifiedSource(), dst, true)
	if err := Verify(t.Context(), r); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.Report.OK() {
		t.Fatal("a dry run over a source that adds up does not verify")
	}
	if got := r.Report.Table("shares").Verified; !strings.Contains(got, "nothing was written") {
		t.Errorf("the verdict is %q", got)
	}
	if got := r.Report.Table("space_usage").Verified; !strings.Contains(got, "2 spaces") {
		t.Errorf("the ledger verdict is %q", got)
	}
	for _, s := range dst.sent {
		t.Errorf("a dry run wrote %q", s.sql)
	}
}

func TestAFailedVerificationReadStopsTheRun(t *testing.T) {
	boom := errors.New("the database went away")
	for _, c := range []struct {
		text     string
		onSource bool
	}{
		{"count(*) FROM principal_directory", true},
		{"ORDER BY md5(id::text)", true},
		{"count(*) FROM subjects", false},
		{"id = ANY($1::uuid[])", false},
		{"SELECT owner, bytes FROM space_usage", false},
	} {
		src, dst := verifiedSource(), after(verifiedTarget())
		r := verified(t, src, dst, false)
		if c.onSource {
			src.queryErr[c.text] = boom
		} else {
			dst.queryErr[c.text] = boom
		}
		if err := Verify(t.Context(), r); !errors.Is(err, boom) {
			t.Errorf("a failed read of %q answered %v", c.text, err)
		}
	}
}

func TestATargetThatHoldsRowsIsRefused(t *testing.T) {
	dst := newFake(answer{"count(*) FROM", [][]any{{int64(3)}}})
	err := Preflight(t.Context(), testRunPair(clean(), dst, false))
	if err == nil {
		t.Fatal("a target that already holds rows is a refusal")
	}
	if !strings.Contains(err.Error(), "already holds 3 rows") {
		t.Errorf("the refusal is %v", err)
	}
	if len(dst.sent) != 0 {
		t.Errorf("the refusal wrote %d statements", len(dst.sent))
	}
}

func TestADryRunReadsAFullTargetWithoutComplaint(t *testing.T) {
	dst := newFake(answer{"count(*) FROM", [][]any{{int64(3)}}})
	if err := Preflight(t.Context(), testRunPair(clean(), dst, true)); err != nil {
		t.Fatalf("a dry run over a full target is refused: %v", err)
	}
}

func TestATargetThatCannotBeCountedStopsTheRun(t *testing.T) {
	boom := errors.New("the database went away")
	dst := emptyTarget()
	dst.queryErr["count(*) FROM subjects"] = boom
	if err := Preflight(t.Context(), testRunPair(clean(), dst, false)); !errors.Is(err, boom) {
		t.Errorf("Preflight answered %v", err)
	}
}
