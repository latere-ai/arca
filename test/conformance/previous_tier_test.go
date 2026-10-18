// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPreviousSuitePasses is spec 024: the suite of the release before the
// one being cut, with the driver it shipped with, fetched as a module and run
// against this release's binary. A case it fails is a break, unless the case
// is a refusal this release turned into an operation and is listed in
// supersededCases, in which case it is skipped and named in the evidence.
//
// It runs where the release pipeline names the tag being cut, in
// ARCA_RELEASE_TAG, and points the previous driver at the target the way this
// release's driver is pointed: through ARCA_TEST_URL and the variables beside
// it, which the driver reads as its defaults. With no ARCA_TEST_URL the
// previous driver starts an installation of ARCA_BINARY on the stack, which
// is how it runs outside a release. ARCA_PREVIOUS_SUITE_MD, when set, is
// where the evidence is written.
func TestPreviousSuitePasses(t *testing.T) {
	tag := os.Getenv("ARCA_RELEASE_TAG")
	if tag == "" {
		t.Skip("ARCA_RELEASE_TAG names no tag being cut; the release pipeline's conformance job sets it")
	}
	previous, why, err := previousTag(tag, tagsOf(t))
	if err != nil {
		t.Fatal(err)
	}
	if previous == "" {
		writeEvidence(t, skippedEvidence(tag, why))
		t.Skip(why)
	}
	apply, inert, err := applying(supersededCases, previous, tag)
	if err != nil {
		t.Fatal(err)
	}

	// The previous suite is a module the proxy serves, which is what a
	// consumer pinning that tag gets. -t brings the modules its driver's own
	// imports need, the stubs among them, at that tag.
	module := t.TempDir()
	goCommand(t, module, "mod", "init", "arca-previous-suite")
	goCommand(t, module, "get", "-t", "latere.ai/x/arca/test/conformance@"+previous)
	source := strings.TrimSpace(goCommand(t, module, "list", "-m", "-f", "{{.Dir}}", "latere.ai/x/arca"))
	suite, err := suiteCases(filepath.Join(source, "test", "conformance"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSuperseded(apply, suite); err != nil {
		t.Fatalf("the superseded list does not hold against the suite of %s: %v", previous, err)
	}

	args := []string{"test", "-v", "-count=1", "-timeout", "30m", "-tags=tiers", "-run", "^TestContract$"}
	if pattern := skipPattern(apply); pattern != "" {
		args = append(args, "-skip", pattern)
	}
	args = append(args, "latere.ai/x/arca/test/conformance")
	run := exec.CommandContext(t.Context(), "go", args...)
	run.Dir, run.Env = module, goEnv()
	raw, runErr := run.CombinedOutput()
	out := string(raw)
	t.Logf("the suite of %s against this binary:\n%s", previous, out)

	var ranAnyway []string
	for _, e := range apply {
		if slices.Contains(ranCases(out), e.Name) {
			ranAnyway = append(ranAnyway, e.Name)
		}
	}
	count := countLine(out)
	writeEvidence(t, evidence(previous, tag, count, apply, inert))
	switch {
	case runErr != nil:
		t.Fatalf("the suite of %s fails against this binary: %v", previous, runErr)
	case len(ranAnyway) > 0:
		t.Fatalf("the superseded cases %v ran, and the skip was meant to keep them from running", ranAnyway)
	case count == "":
		t.Fatalf("the suite of %s ran and logged no count", previous)
	}
}

// tagsOf answers the tags of the repository this test is in, which is where
// spec 024 reads the previous tag from.
func tagsOf(t *testing.T) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "tag", "--list", "v*")
	cmd.Dir = dir(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list the tags: %v", err)
	}
	return strings.Fields(string(out))
}

// goCommand runs one go command in the temporary module and answers its
// output, failing the test with the output when it fails.
func goCommand(t *testing.T, module string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir, cmd.Env = module, goEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// goEnv is the environment of a go command in the temporary module: this
// one, with GOWORK off so no go.work file above the module pulls this
// tree's packages in, and the module free to record what the previous
// suite needs.
func goEnv() []string {
	return append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
}

// writeEvidence writes the section where ARCA_PREVIOUS_SUITE_MD names, and
// logs it either way.
func writeEvidence(t *testing.T, md string) {
	t.Helper()
	t.Logf("evidence:\n%s", md)
	path := os.Getenv("ARCA_PREVIOUS_SUITE_MD")
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
		t.Fatalf("write the evidence: %v", err)
	}
}
