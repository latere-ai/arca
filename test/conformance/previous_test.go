// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The unit half of spec 024: what TestPreviousSuitePasses decides before it
// runs anything, held without a network, a stack or a previous release.

// TestThePreviousTagIsTheHighestBelowInTheSameMajor is criterion 2 of spec
// 024: the previous tag is the highest plain v* tag below the one being cut,
// and a release with none, or one that moves the major, says why instead of
// running nothing quietly.
func TestThePreviousTagIsTheHighestBelowInTheSameMajor(t *testing.T) {
	tags := []string{"v0.1.0", "v0.6.0", "v0.7.0", "v0.10.0", "v0.8.0-rc1", "latest", "v1.0.0"}
	for _, c := range []struct {
		tag, want string
		why       bool
	}{
		{"v0.8.0", "v0.7.0", false},
		{"v0.7.1", "v0.7.0", false},
		{"v0.7.0", "v0.6.0", false},
		{"v0.11.0", "v0.10.0", false},
		{"v0.1.0", "", true},
		{"v1.0.0", "", true},
		{"v1.0.1", "v1.0.0", false},
		{"v2.0.0", "", true},
	} {
		got, why, err := previousTag(c.tag, tags)
		if err != nil || got != c.want || (why != "") != c.why {
			t.Errorf("previousTag(%s) = %q, %q, %v; want %q with a reason %t", c.tag, got, why, err, c.want, c.why)
		}
	}
	for _, bad := range []string{"", "main", "v0.8", "v0.8.0-rc1", "0.8.0"} {
		if _, _, err := previousTag(bad, tags); err == nil {
			t.Errorf("previousTag(%q) took a tag that is not one a release cuts", bad)
		}
	}
}

// folderCopy is the entry the folder move of spec 030 needs, and the shape
// every case below varies.
var folderCopy = superseded{
	Name: "028/FolderCopy", Refusal: CodeInvalidField, Since: "v0.8.0",
	Reason: "a move with recursive moves a folder (spec 030)",
}

// TestASupersededEntryAppliesBetweenItsTwoTags is criterion 5's first half:
// an entry applies to a run whose previous tag is below the tag that
// superseded the case and whose tag being cut is at or above it, and is
// inert on either side of that window. An entry that is not one is refused.
func TestASupersededEntryAppliesBetweenItsTwoTags(t *testing.T) {
	for _, c := range []struct {
		previous, tag string
		applies       bool
	}{
		{"v0.7.0", "v0.8.0", true},
		{"v0.7.0", "v0.9.0", true},
		{"v0.6.0", "v0.8.1", true},
		{"v0.7.0", "v0.7.1", false},
		{"v0.8.0", "v0.9.0", false},
		{"v0.8.0", "v0.8.1", false},
	} {
		apply, inert, err := applying([]superseded{folderCopy}, c.previous, c.tag)
		if err != nil {
			t.Fatalf("applying(%s, %s): %v", c.previous, c.tag, err)
		}
		if got := len(apply) == 1; got != c.applies || len(apply)+len(inert) != 1 {
			t.Errorf("between %s and %s the entry applies=%t inert=%d, want applies=%t",
				c.previous, c.tag, got, len(inert), c.applies)
		}
	}
	for name, broken := range map[string]superseded{
		"a name with no spec":   {Name: "FolderCopy", Refusal: CodeInvalidField, Since: "v0.8.0", Reason: "r"},
		"a name of a group":     {Name: "028", Refusal: CodeInvalidField, Since: "v0.8.0", Reason: "r"},
		"a tag that is not one": {Name: "028/FolderCopy", Refusal: CodeInvalidField, Since: "next", Reason: "r"},
		"no reason":             {Name: "028/FolderCopy", Refusal: CodeInvalidField, Since: "v0.8.0"},
		"a reason of two lines": {Name: "028/FolderCopy", Refusal: CodeInvalidField, Since: "v0.8.0", Reason: "one\ntwo"},
		"no refusal":            {Name: "028/FolderCopy", Since: "v0.8.0", Reason: "r"},
	} {
		if _, _, err := applying([]superseded{broken}, "v0.7.0", "v0.8.0"); err == nil {
			t.Errorf("%s was taken as an entry", name)
		}
	}
	if _, _, err := applying([]superseded{folderCopy, folderCopy}, "v0.7.0", "v0.8.0"); err == nil {
		t.Error("one case listed twice was taken")
	}
}

// TestASupersededEntryIsARefusalThePreviousCaseDeclared is criterion 5's
// second half: an applying entry names a case of the previous suite, a code
// that case declares it provokes, and a 4xx row of spec 013's table. Every
// other entry fails the check, so the list can neither rot nor excuse a
// success or a server error that stopped answering.
func TestASupersededEntryIsARefusalThePreviousCaseDeclared(t *testing.T) {
	suite := map[string][]string{
		"028/FolderCopy": {CodePathTaken, CodeInvalidField, CodeExclusiveFields, CodeNotFound},
		"028/Limits":     {CodeStorageUnavailable, CodeNotFound},
	}
	if err := checkSuperseded([]superseded{folderCopy}, suite); err != nil {
		t.Fatalf("the folder copy's refusal was not admitted: %v", err)
	}
	for name, c := range map[string]struct {
		entry superseded
		says  string
	}{
		"a case the previous suite does not hold": {superseded{Name: "028/FolderMove", Refusal: CodeInvalidField, Since: "v0.8.0", Reason: "r"}, "028/FolderMove"},
		"a code the case does not declare":        {superseded{Name: "028/FolderCopy", Refusal: CodeForbidden, Since: "v0.8.0", Reason: "r"}, CodeForbidden},
		"a server error":                          {superseded{Name: "028/Limits", Refusal: CodeStorageUnavailable, Since: "v0.8.0", Reason: "r"}, "503"},
		"a code outside the table":                {superseded{Name: "028/FolderCopy", Refusal: "moved", Since: "v0.8.0", Reason: "r"}, "moved"},
	} {
		err := checkSuperseded([]superseded{c.entry}, suite)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: the check answered %v, want a refusal naming %q", name, err, c.says)
		}
	}
}

// TestTheSupersededListIsRefusals holds the list this release ships to the
// rules the check reads, so an entry that could never apply is refused on
// the commit that adds it rather than on the release that reads it.
func TestTheSupersededListIsRefusals(t *testing.T) {
	if _, _, err := applying(supersededCases, "v0.0.0", "v99.0.0"); err != nil {
		t.Fatalf("the list is not one the check takes: %v", err)
	}
	for _, e := range supersededCases {
		if r, ok := codeTable[e.Refusal]; !ok || r.status < 400 || r.status > 499 {
			t.Errorf("%s supersedes %q, which is not a 4xx row of spec 013's table", e.Name, e.Refusal)
		}
	}
	if !slices.Contains(supersededCases, folderCopy) {
		t.Errorf("the list does not carry the folder copy's refusal of a move with recursive: %v", supersededCases)
	}
}

// TestTheSuiteIsReadFromItsSource: the previous suite is read from its
// source, because a case is not a value one release can ask another for.
// Read from this release's own source, it is exactly what cases() runs, with
// the codes each case declares.
func TestTheSuiteIsReadFromItsSource(t *testing.T) {
	got, err := suiteCases(dir(t))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{}
	for _, sp := range cases() {
		for _, c := range sp.cases {
			want[sp.number+"/"+c.name] = c.codes
		}
	}
	if len(got) != len(want) {
		t.Errorf("the source holds %d cases and the suite runs %d", len(got), len(want))
	}
	for name, codes := range want {
		read, held := got[name]
		if !held || !slices.Equal(read, codes) {
			t.Errorf("%s reads from the source as %v, %t; the suite declares %v", name, read, held, codes)
		}
	}

	empty := t.TempDir()
	if _, err := suiteCases(empty); err == nil {
		t.Error("a directory with no suite in it was read as one")
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "conformance.go"), []byte("package conformance\nfunc cases( {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := suiteCases(broken); err == nil {
		t.Error("a source that does not parse was read as a suite")
	}
}

// TestTheSkipPatternNamesEachCaseAndNoOther: go test's -skip takes one
// pattern per level of a subtest's name, and a top-level alternation of
// whole names, so the pattern names each case at its three levels and a
// second case is never skipped by the product of two.
func TestTheSkipPatternNamesEachCaseAndNoOther(t *testing.T) {
	move := superseded{Name: "005/Move", Refusal: CodeInvalidField, Since: "v0.8.0", Reason: "r"}
	if got, want := skipPattern([]superseded{folderCopy, move}),
		`^TestContract$/^028$/^FolderCopy$|^TestContract$/^005$/^Move$`; got != want {
		t.Errorf("the pattern is %q, want %q", got, want)
	}
	if got := skipPattern(nil); got != "" {
		t.Errorf("no entry skips %q", got)
	}
}

// TestTheRunIsReadOffItsVerboseOutput: which cases ran and the suite's own
// count are read off go test -v, which is all a run of another release's
// driver leaves.
func TestTheRunIsReadOffItsVerboseOutput(t *testing.T) {
	out := strings.Join([]string{
		"=== RUN   TestContract",
		"=== RUN   TestContract/028",
		"=== RUN   TestContract/028/FolderDelete",
		"=== RUN   TestContract/028/FolderCopy/inner",
		"=== RUN   TestContract/005/Move",
		"    contract_test.go:105: 52 passed, 0 failed, 1 skipped, 96 objects created and deleted",
		"--- PASS: TestContract (0.99s)",
	}, "\n")
	if got, want := ranCases(out), []string{"005/Move", "028/FolderDelete"}; !slices.Equal(got, want) {
		t.Errorf("the cases that ran are %v, want %v", got, want)
	}
	if got := countLine(out); got != "52 passed, 0 failed, 1 skipped, 96 objects created and deleted" {
		t.Errorf("the count is %q", got)
	}
	if got := countLine("FAIL\tlatere.ai/x/arca/test/conformance [build failed]"); got != "" {
		t.Errorf("a run that never counted answered %q", got)
	}
}

// TestTheEvidenceListsEachSkippedCaseUnderItsReason is criterion 6's
// evidence half: the release body names the previous tag, the suite's own
// count, each skipped case under its tag and reason, and each entry that no
// longer applies, so a reader sees which compatibility claim was checked and
// which was excused.
func TestTheEvidenceListsEachSkippedCaseUnderItsReason(t *testing.T) {
	older := superseded{Name: "005/Move", Refusal: CodeInvalidField, Since: "v0.6.0", Reason: "an older change"}
	md := evidence("v0.7.0", "v0.8.0", "52 passed, 0 failed, 0 skipped", []superseded{folderCopy}, []superseded{older})
	for _, want := range []string{
		"## The previous release's suite",
		"`v0.7.0`", "`v0.8.0`", "52 passed, 0 failed, 0 skipped",
		"- `028/FolderCopy`, which expected `invalid_field`: v0.8.0: a move with recursive moves a folder (spec 030)",
		"- `005/Move`: v0.6.0: an older change",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the evidence does not hold %q:\n%s", want, md)
		}
	}
	plain := evidence("v0.7.0", "v0.7.1", "53 passed", nil, nil)
	if strings.Contains(plain, "Skipped") || strings.Contains(plain, "no longer") {
		t.Errorf("a run that skipped nothing says it did:\n%s", plain)
	}
	none := skippedEvidence("v0.1.0", "no v0 tag is below v0.1.0")
	if !strings.Contains(none, "no v0 tag is below v0.1.0") || !strings.Contains(none, "`v0.1.0`") {
		t.Errorf("a release with no previous suite does not say why:\n%s", none)
	}
}
