// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

func TestTheReportHoldsALineForEveryTableOfThePlan(t *testing.T) {
	r := NewReport(TableNames())
	var out strings.Builder
	r.Write(&out)
	for _, name := range TableNames() {
		if !strings.Contains(out.String(), name) {
			t.Errorf("the report names no %s", name)
		}
	}
	if !strings.Contains(out.String(), "not verified") {
		t.Error("a table nobody verified reads as verified")
	}
}

func TestTheReportCountsWhatArrivedAndWhatDidNot(t *testing.T) {
	r := NewReport(TableNames())
	r.Source, r.Target = "postgres://drive", "postgres://arca"
	r.Issuer, r.Prefix = "https://issuer.example", "drive/"
	for range 3 {
		r.Copy("files")
	}
	r.Drop("shares", DropRole)
	r.Drop("shares", DropRole)
	r.Drop("shares", DropEmail)
	r.Note("shares", "tokens minted for a grant that carried none")
	r.Verify("files", "counts hold", true)

	if got := r.Table("files").Copied; got != 3 {
		t.Errorf("files copied %d, want 3", got)
	}
	if got := r.Table("shares").DroppedTotal(); got != 3 {
		t.Errorf("shares dropped %d, want 3", got)
	}
	var out strings.Builder
	r.Write(&out)
	for _, text := range []string{
		"postgres://drive", "postgres://arca", "https://issuer.example", "drive/",
		"rows dropped", DropRole, DropEmail, "noted", "tokens minted", "counts hold",
	} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("the report holds no %q:\n%s", text, out.String())
		}
	}
}

func TestADryRunSaysItWroteNothing(t *testing.T) {
	r := NewReport(TableNames())
	r.DryRun = true
	var out strings.Builder
	r.Write(&out)
	if !strings.Contains(out.String(), "dry run, nothing is written") {
		t.Errorf("the report does not say it wrote nothing:\n%s", out.String())
	}
}

func TestEveryReportCarriesTheBytesFinding(t *testing.T) {
	// The counts can all hold and the cutover still not be done, so the
	// finding is on every report and not only on a failed one.
	r := NewReport(TableNames())
	for _, name := range TableNames() {
		r.Verify(name, "counts hold", true)
	}
	var out strings.Builder
	r.Write(&out)
	if !strings.Contains(out.String(), "the bytes") {
		t.Error("the report carries no bytes block")
	}
	if !strings.Contains(out.String(), "Do not switch the routes on this report alone.") {
		t.Errorf("the finding is not in the report:\n%s", out.String())
	}
	if !r.OK() {
		t.Error("a report whose every table verified is not OK")
	}
}

func TestAReportIsNotOKUntilEveryTableIsVerified(t *testing.T) {
	r := NewReport(TableNames())
	if r.OK() {
		t.Fatal("a copy nobody checked reads as OK")
	}
	for _, name := range TableNames() {
		r.Verify(name, "counts hold", true)
	}
	r.Verify("files", "the target holds 2 rows and the copy wrote 3", false)
	if r.OK() {
		t.Fatal("a report with a failed table reads as OK")
	}
}

func TestATableOutsideThePlanStillGetsALine(t *testing.T) {
	r := NewReport(nil)
	r.Copy("late")
	if r.Table("late").Copied != 1 {
		t.Error("a table the plan did not name is not counted")
	}
	var out strings.Builder
	r.Write(&out)
	if !strings.Contains(out.String(), "late") {
		t.Error("a table the plan did not name is not printed")
	}
}

func TestABlockWithNothingInItIsNotPrinted(t *testing.T) {
	r := NewReport(TableNames())
	var out strings.Builder
	r.Write(&out)
	if strings.Contains(out.String(), "rows dropped") {
		t.Error("a copy that dropped nothing prints a dropped block")
	}
	if strings.Contains(out.String(), "\nnoted\n") {
		t.Error("a copy that noted nothing prints a noted block")
	}
}

func TestIndentPutsEveryLineUnderItsHeading(t *testing.T) {
	got := indent("one\n\ntwo")
	if got != "  one\n\n  two" {
		t.Errorf("indent = %q", got)
	}
}
