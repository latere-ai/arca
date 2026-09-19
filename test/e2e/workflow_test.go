// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// This file carries no build tag on purpose: what it reads is a file in
// the tree, so it belongs to the unit tier and runs on every push. It is
// the tiers' contract with the pipeline, criterion 12 of spec 014.
package e2e

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestWorkflowJobsMatchTheTable reads verify.yml and holds it to spec
// 014's table: one job per service tier, on hosted runners, running the
// command the spec names. A job that drifts from the table is a tier that
// stops running without anyone noticing.
//
// The package list of each job is asserted here and nowhere else. Spec 014
// describes the store tier as the packages that reach a store rather than
// printing them, because a list written in a document nothing fails on is a
// list that goes stale; this is the copy a change to the job has to come
// past.
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
		{"store", "-run '^TestStore'", "./internal/blob/... ./internal/store/... ./internal/events/... ./internal/reaper/..."},
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

// TestMakeRun reads the run target and holds it to the seven steps of spec
// 014, each by the command it runs. `make run` is what a clean clone
// becomes an installation with, and a step dropped from it is an
// installation a contributor has to finish by hand; the tier that starts
// the same stack cannot see that, because it starts it itself.
//
// What the two steps this test cannot reach actually do is proved where a
// stack is running: `TestE2ECheckPassesAgainstTheStack` for the check's
// five ok lines, and
// `TestE2EAPutRoundTripsAndAReadAboveTheBoundaryRedirects` for the put and
// the read the token line prints.
func TestMakeRun(t *testing.T) {
	program := makeTarget(t, makefile(t), "run")

	for _, step := range []struct {
		number int
		name   string
		runs   []string
	}{
		{1, "build arcad and arca-stubs", []string{
			"-o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)",
			"-o $(OUT_DIR)/$(STUBS) ./test/stubs/cmd/$(STUBS)",
		}},
		{2, "the stack: Postgres, MinIO, and the bucket", []string{
			"$(COMPOSE) up -d",
			"$(DEV_S3_ENDPOINT)/minio/health/live",
		}},
		{3, "the stubs, waited for at the issuer's key set", []string{
			"$(OUT_DIR)/$(STUBS) -issuer-listen",
			"$(DEV_ISSUER_URL)/jwks",
		}},
		{4, "the migration", []string{
			"$(OUT_DIR)/$(SERVICE) migrate",
		}},
		{5, "the server, waited for at its readiness probe", []string{
			"$(DEV_SERVICE_ENV) $(OUT_DIR)/$(SERVICE)\n",
			"$(DEV_INTERNAL_PORT)/readyz",
		}},
		{6, "arcad check against the running installation", []string{
			"$(OUT_DIR)/$(SERVICE) check",
		}},
		{7, "the token, and a put and a read of one object", []string{
			"$(DEV_ISSUER_URL)/mint",
			"export ARCA_URL=",
			"-X PUT",
			"$(DEV_OBJECT_PATH)",
		}},
	} {
		for _, runs := range step.runs {
			if !strings.Contains(program, runs) {
				t.Errorf("step %d of make run, %s, runs no %q", step.number, step.name, runs)
			}
		}
	}
}

// TestMakeRunSideBySide is criterion 9 of spec 014: two checkouts run the
// stack and the server at once without colliding. Nothing here starts a
// second stack, because what makes the second one safe is the derivation
// rather than any one pair of numbers: the compose project is the
// checkout's directory, every published port is that project's port base
// plus an offset below the stride the bases are spaced by, and compose.yaml
// publishes nothing at a number of its own. Two checkouts therefore take
// two windows of ports that cannot overlap, and two projects whose
// containers, network and volumes are named apart.
func TestMakeRunSideBySide(t *testing.T) {
	mk := makefile(t)
	vars := makeVars(mk)

	for _, derived := range []struct{ name, from string }{
		// The project is the checkout, and it names the compose project,
		// so the containers, the network and the volumes are this
		// checkout's and no other's.
		{"DEV_PROJECT", "$(notdir $(CURDIR))"},
		{"COMPOSE", "-p $(DEV_PROJECT)"},
		// The stubs' pid and log file are per checkout too: two runs
		// sharing one pid file would each stop the other's stubs.
		{"DEV_DIR", "$(DEV_PROJECT)"},
		// And the port base is the project's.
		{"DEV_PORT_BASE", "$(DEV_PROJECT)"},
	} {
		if !strings.Contains(vars[derived.name], derived.from) {
			t.Errorf("%s = %q, which does not derive from %s", derived.name, vars[derived.name], derived.from)
		}
	}

	stride := 0
	if m := regexp.MustCompile(`\*\s*(\d+)`).FindStringSubmatch(vars["DEV_PORT_BASE"]); m != nil {
		stride, _ = strconv.Atoi(m[1])
	}
	if stride == 0 {
		t.Fatalf("DEV_PORT_BASE = %q, which spaces two checkouts by no stride at all", vars["DEV_PORT_BASE"])
	}

	// Every port this checkout publishes is the base plus an offset. Two
	// offsets below the stride cannot reach the next base, and no two
	// services share one, so one checkout's window holds every listener it
	// starts and nothing of another's.
	taken := map[int]string{}
	for _, port := range []string{
		"DEV_PUBLIC_PORT", "DEV_INTERNAL_PORT", "DEV_S3_PORT",
		"DEV_S3_CONSOLE_PORT", "DEV_DB_PORT", "DEV_ISSUER_PORT", "DEV_AUTHORIZER_PORT",
	} {
		offset, ok := portOffset(vars[port])
		if !ok {
			t.Errorf("%s = %q, which is not the port base plus an offset", port, vars[port])
			continue
		}
		if offset >= stride {
			t.Errorf("%s is the base plus %d, which reaches the next checkout's base %d away", port, offset, stride)
		}
		if other, dup := taken[offset]; dup {
			t.Errorf("%s and %s are both the base plus %d", other, port, offset)
		}
		taken[offset] = port
	}

	raw, err := os.ReadFile("../../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(raw)
	if !strings.Contains(compose, "name: ${DEV_PROJECT") {
		t.Error("compose.yaml names a project of its own, so two checkouts share the containers, the network and the volumes")
	}
	for line := range strings.SplitSeq(compose, "\n") {
		published := strings.TrimSpace(line)
		if !strings.HasPrefix(published, `- "127.0.0.1:`) {
			continue
		}
		if !strings.Contains(published, "${DEV_") {
			t.Errorf("compose.yaml publishes %s at a number of its own", published)
		}
	}
}

// makefile reads the tree's Makefile.
func makefile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// makeTarget answers everything `make <target>` runs: the target's own
// recipe, the recipe of every target it names as a prerequisite, and the
// body of every define those recipes reach. A step is a command in that
// text, wherever the Makefile keeps it, so moving one into a define or a
// prerequisite does not hide it from a test.
func makeTarget(t *testing.T, mk, target string) string {
	t.Helper()
	defines := makeDefines(mk)
	seen := map[string]bool{}
	var walk func(string) string
	walk = func(name string) string {
		if seen[name] {
			return ""
		}
		seen[name] = true
		prerequisites, recipe, ok := makeRule(mk, name)
		if !ok {
			return ""
		}
		for _, prerequisite := range prerequisites {
			recipe += walk(prerequisite)
		}
		return recipe
	}
	program := walk(target)
	if program == "" {
		t.Fatalf("the Makefile has no %s target, or it runs nothing", target)
	}
	for grew := true; grew; {
		grew = false
		for name, body := range defines {
			if seen["define "+name] || !strings.Contains(program, "$("+name+")") {
				continue
			}
			seen["define "+name] = true
			program += body
			grew = true
		}
	}
	return program
}

// makeRule answers one target's prerequisites and its recipe lines.
func makeRule(mk, target string) (prerequisites []string, recipe string, ok bool) {
	lines := strings.Split(mk, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, target+":") || strings.HasPrefix(line, target+":=") {
			continue
		}
		prerequisites = strings.Fields(strings.TrimPrefix(line, target+":"))
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "\t") {
				break
			}
			recipe += strings.TrimPrefix(next, "\t") + "\n"
		}
		return prerequisites, recipe, true
	}
	return nil, "", false
}

// makeDefines answers the body of every multi-line variable, by name.
func makeDefines(mk string) map[string]string {
	defines := map[string]string{}
	var name string
	var body strings.Builder
	for line := range strings.SplitSeq(mk, "\n") {
		switch {
		case strings.HasPrefix(line, "define "):
			name = strings.TrimSpace(strings.TrimPrefix(line, "define "))
			body.Reset()
		case line == "endef" && name != "":
			defines[name] = body.String()
			name = ""
		case name != "":
			body.WriteString(strings.TrimPrefix(line, "\t") + "\n")
		}
	}
	return defines
}

// makeVars answers every single-line variable of the Makefile by name,
// with the continuation lines of one assignment joined.
var makeAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*[:?]?=\s*(.*)$`)

func makeVars(mk string) map[string]string {
	vars := map[string]string{}
	lines := strings.Split(mk, "\n")
	for i := 0; i < len(lines); i++ {
		m := makeAssignment.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		value := m[2]
		for strings.HasSuffix(value, `\`) && i+1 < len(lines) {
			i++
			value = strings.TrimSuffix(value, `\`) + strings.TrimSpace(lines[i])
		}
		vars[m[1]] = value
	}
	return vars
}

// portOffset answers what a port variable adds to the port base.
var makePortOffset = regexp.MustCompile(`^\$\(shell expr \$\(DEV_PORT_BASE\) \+ (\d+)\)$`)

func portOffset(value string) (int, bool) {
	if value == "$(DEV_PORT_BASE)" {
		return 0, true
	}
	m := makePortOffset.FindStringSubmatch(value)
	if m == nil {
		return 0, false
	}
	offset, err := strconv.Atoi(m[1])
	return offset, err == nil
}
