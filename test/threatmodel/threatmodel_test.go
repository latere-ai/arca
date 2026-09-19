// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package threatmodel holds the two harnesses spec 015 asks for over itself:
// the controls table against the test functions this tree has, and
// SECURITY.md's commitments against the rows of that table.
//
// A threat model whose proofs cannot be looked up is a claim. The first
// draft of spec 015 named thirty-eight test functions, of which three
// existed; the controls were real and tested throughout, and the names had
// been invented at drafting time and never reconciled with the tree. Nothing
// in a document catches that, which is why it is a test and why it sits
// here, beside the other suites that read what the repository ships rather
// than what a package does.
package threatmodel

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// control is one row of spec 015's controls table.
type control struct {
	threat  string
	control string
	// specs are the three digit spec numbers the Spec cell names; "this
	// spec" names 015 and is dropped, because a spec does not depend on
	// itself.
	specs []string
	// tests are the function names the Test cell names, and proof is that
	// cell whole: two rows are proved by a pipeline job rather than by a
	// function, and they say so in words.
	tests []string
	proof string
}

var (
	// named matches a Test cell's function names, which the document writes
	// in backticks. A cell may also carry a sentence, so the names are read
	// out of it rather than the cell being split.
	named = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	// specNumber matches a Spec cell's three digit references.
	specNumber = regexp.MustCompile(`\b(\d{3})\b`)
	// separator matches the |---|---| line under a table header.
	separator = regexp.MustCompile(`^\|[\s:|-]+\|?$`)
)

// root answers the repository root from this file's own location.
//
// It is read through runtime.Caller and not through a relative path, because
// the tempdir gate runs the suite from an empty directory and a test that
// opened "../../specs" would pass there by not finding the file.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test binary carries no source position")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// read answers one file of the repository.
func read(t *testing.T, name ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{root(t)}, name...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// section answers the lines of one `###` section of a document, without its
// heading. A heading the document does not carry fails the test: the
// harnesses below read the document's shape, so a renamed section is a
// harness that silently checks nothing.
func section(t *testing.T, doc, heading string) []string {
	t.Helper()
	var out []string
	inside := false
	for line := range strings.SplitSeq(doc, "\n") {
		switch {
		case strings.HasPrefix(line, "### ") && strings.TrimSpace(line) == heading:
			inside = true
		case inside && strings.HasPrefix(line, "### "):
			return out
		case inside:
			out = append(out, line)
		}
	}
	if !inside {
		t.Fatalf("the document carries no %q section", heading)
	}
	return out
}

// cells splits one Markdown table row, without the empty cells the leading
// and trailing pipes make.
func cells(line string) []string {
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, 0, len(parts)-2)
	for _, c := range parts[1 : len(parts)-1] {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}

// controls parses the controls table of spec 015.
func controls(t *testing.T) []control {
	t.Helper()
	spec := read(t, "specs", "015-security-and-threat-model.md")
	var out []control
	for _, line := range section(t, spec, "### Controls") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") || separator.MatchString(strings.TrimSpace(line)) {
			continue
		}
		c := cells(line)
		if len(c) != 4 || c[0] == "Threat" {
			continue
		}
		row := control{threat: c[0], control: c[1], proof: c[3]}
		for _, m := range specNumber.FindAllStringSubmatch(c[2], -1) {
			row.specs = append(row.specs, m[1])
		}
		for _, m := range named.FindAllStringSubmatch(c[3], -1) {
			row.tests = append(row.tests, m[1])
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		t.Fatal("the controls table of spec 015 parsed to no rows; has its shape changed?")
	}
	return out
}

// TestEveryControlNamesATestTheTreeHas is criterion 23 of spec 015: every
// `Test` cell of the controls table names a function `go test -list` finds.
//
// The listing carries the tiers tag of spec 014, because the store, e2e and
// conformance tiers are where a control over a real bucket, a real database
// or a running installation is proved, and eleven of the cells name one of
// those. A tagged listing is a superset of the untagged one, so one run
// answers for both, at the cost of the unit run compiling the tier files
// too: a tier that stops compiling now reds this gate rather than the next
// `make test-e2e`.
func TestEveryControlNamesATestTheTreeHas(t *testing.T) {
	list := exec.CommandContext(t.Context(), "go", "test", "-tags=tiers", "-list", ".*", "./...")
	list.Dir = root(t)
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("listing the tests of the tree: %v\n%s", err, out)
	}
	found := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if name := strings.TrimSpace(line); strings.HasPrefix(name, "Test") {
			found[name] = true
		}
	}
	if len(found) == 0 {
		t.Fatalf("the listing names no test at all:\n%s", out)
	}

	cells := 0
	for _, row := range controls(t) {
		if row.proof == "" {
			t.Errorf("the control %q names nothing that proves it", row.threat)
		}
		for _, name := range row.tests {
			cells++
			if !found[name] {
				t.Errorf("the control %q names %s, which no package in this tree has",
					row.threat, name)
			}
		}
	}
	if cells == 0 {
		t.Fatal("no control names a test; the Test column is not being read")
	}
}

// TestEveryCommitmentOfTheRootFileIsAControl is criterion 24 of spec 015.
//
// SECURITY.md is what a reviewer arriving at the repository reads, and it is
// prose rather than a table, so the mapping lives in the spec: one row per
// commitment and the control that answers it. This holds the three sides
// together. Every commitment the root file makes is a row of that mapping,
// and no row invents one; every control the mapping names is a row of the
// controls table; and every spec a control names is in this spec's
// depends_on, so a control cannot cite a spec the threat model does not
// declare it reads.
func TestEveryCommitmentOfTheRootFileIsAControl(t *testing.T) {
	said := commitments(t)
	rows := mapping(t)

	known := map[string]bool{}
	for _, row := range controls(t) {
		known[row.threat] = true
	}

	claimed := map[string]bool{}
	for commitment, threats := range rows {
		if !slices.Contains(said, commitment) {
			t.Errorf("the mapping carries the commitment %q, which SECURITY.md does not make", commitment)
		}
		claimed[commitment] = true
		for _, threat := range threats {
			if !known[threat] {
				t.Errorf("the commitment %q names the control %q, which is not a row of the controls table",
					commitment, threat)
			}
		}
	}
	for _, commitment := range said {
		if !claimed[commitment] {
			t.Errorf("SECURITY.md commits to %q, and no row of the mapping answers it", commitment)
		}
	}
}

// TestEverySpecAControlNamesIsADependency is the second half of criterion
// 24: a control that cites a spec is a control read against that spec, and a
// spec this one reads is an edge of the graph.
func TestEverySpecAControlNamesIsADependency(t *testing.T) {
	declared := dependencies(t)
	for _, row := range controls(t) {
		for _, number := range row.specs {
			if !slices.Contains(declared, number) {
				t.Errorf("the control %q names spec %s, which is not in this spec's depends_on",
					row.threat, number)
			}
		}
	}
}

// lead is the sentence SECURITY.md opens its commitments with. Everything
// after it and before the paragraph's end is the list, clause by clause.
const lead = "The commitments the design makes:"

// commitments reads the commitments out of SECURITY.md, in the words that
// file uses.
//
// They are a semicolon-separated list inside one paragraph rather than a
// list of bullets, because the file is read by a person arriving at the
// repository and prose is what that reader wants. The split is therefore on
// the punctuation the prose already uses, and the mapping's rows are held
// equal to what it yields: an editor who adds a commitment gets a failure
// naming it, rather than a document that quietly commits to more than the
// controls table answers.
func commitments(t *testing.T) []string {
	t.Helper()
	// The file is hard wrapped, so each paragraph is flattened before it is
	// read: the sentence the list opens with spans two lines.
	for paragraph := range strings.SplitSeq(read(t, "SECURITY.md"), "\n\n") {
		flat := strings.Join(strings.Fields(paragraph), " ")
		_, after, found := strings.Cut(flat, lead)
		if !found {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimSpace(after), ".")
		var out []string
		for clause := range strings.SplitSeq(rest, ";") {
			if clause = strings.TrimSpace(clause); clause != "" {
				out = append(out, clause)
			}
		}
		if len(out) < 2 {
			t.Fatalf("SECURITY.md's commitments parsed to %v; has the paragraph's shape changed?", out)
		}
		return out
	}
	t.Fatalf("SECURITY.md carries no paragraph opening %q", lead)
	return nil
}

// mapping reads the commitment table of spec 015's root file section: the
// commitment as SECURITY.md words it, and the control that answers it. A
// commitment answered by more than one control has one row per control.
func mapping(t *testing.T) map[string][]string {
	t.Helper()
	spec := read(t, "specs", "015-security-and-threat-model.md")
	out := map[string][]string{}
	for _, line := range section(t, spec, "### The root file") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") || separator.MatchString(strings.TrimSpace(line)) {
			continue
		}
		c := cells(line)
		if len(c) != 2 || strings.HasPrefix(c[0], "Commitment") {
			continue
		}
		out[c[0]] = append(out[c[0]], c[1])
	}
	if len(out) == 0 {
		t.Fatal("the root file section of spec 015 carries no commitment table")
	}
	return out
}

// dependencies reads the spec numbers of spec 015's depends_on.
func dependencies(t *testing.T) []string {
	t.Helper()
	spec := read(t, "specs", "015-security-and-threat-model.md")
	var out []string
	inside := false
	for line := range strings.SplitSeq(spec, "\n") {
		switch {
		case strings.HasPrefix(line, "depends_on:"):
			inside = true
		case inside && strings.HasPrefix(line, "  - "):
			if m := specNumber.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		case inside:
			inside = false
		}
	}
	if len(out) == 0 {
		t.Fatal("spec 015 declares no depends_on; has the frontmatter's shape changed?")
	}
	return out
}
