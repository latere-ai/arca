// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// This file carries no build tag on purpose: what it reads is a file in
// the tree, so it belongs to the unit tier and runs on every push. It is
// the tiers' contract with the pipeline, criterion 12 of spec 014.
package e2e

import (
	"os"
	"strings"
	"testing"
)

// TestWorkflowJobsMatchTheTable reads verify.yml and holds it to spec
// 014's table: one job per service tier, on hosted runners, running the
// command the spec names. A job that drifts from the table is a tier that
// stops running without anyone noticing.
func TestWorkflowJobsMatchTheTable(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/verify.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)

	for _, job := range []struct {
		name     string
		selector string
		packages string
	}{
		{"store", "-run '^TestStore'", "./internal/blob/... ./internal/store/..."},
		{"e2e", "-run '^TestE2E'", "./test/e2e/..."},
	} {
		if !strings.Contains(workflow, "\n  "+job.name+":\n") {
			t.Errorf("verify.yml has no %s job", job.name)
			continue
		}
		for _, want := range []string{
			job.selector,
			job.packages,
			"-tags=tiers",
			"-race",
			"-covermode=atomic",
			"-coverprofile=" + job.name + ".out",
		} {
			if !strings.Contains(workflow, want) {
				t.Errorf("the %s job does not run %q", job.name, want)
			}
		}
	}
	if strings.Count(workflow, "runs-on: ubuntu-latest") < 4 {
		t.Error("a job left GitHub's hosted runners; the repository is public and they cost nothing")
	}
	if !strings.Contains(workflow, "run: make up") {
		t.Error("a tier job starts the stack from something other than compose.yaml")
	}
}
