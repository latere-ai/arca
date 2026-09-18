// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestE2EBinaryServesItsProbesAgainstBothStores is criterion 10 of spec
// 014 as far as this phase reaches: arcad as a process, its subcommand
// dispatch, its two listeners, and its probes, with a real bucket and a
// real database behind readiness. The check subcommand joins it with spec
// 012 and the /v1 routes with spec 013.
func TestE2EBinaryServesItsProbesAgainstBothStores(t *testing.T) {
	i := start(t)

	for _, base := range []string{i.publicURL, i.internalURL} {
		for _, probe := range []string{"/livez", "/readyz"} {
			if code, body := get(t, base+probe); code != http.StatusOK || body != "ok\n" {
				t.Errorf("GET %s%s = %d %q", base, probe, code, body)
			}
		}
		if code, body := get(t, base+"/version"); code != http.StatusOK || !strings.Contains(body, `"version"`) {
			t.Errorf("GET %s/version = %d %q", base, code, body)
		}
	}
	if code, body := get(t, i.publicURL+"/"); code != http.StatusOK || !strings.HasPrefix(body, "arcad ") {
		t.Errorf("GET / = %d %q", code, body)
	}
	if code, _ := get(t, i.publicURL+"/metrics"); code != http.StatusNotFound {
		t.Errorf("GET /metrics on the public listener = %d, want 404 until spec 018 mounts it", code)
	}
}

// TestE2EMigrateIsIdempotent is criterion 1 of spec 004 through the
// binary: the second run of a migration job changes nothing and exits 0,
// which is what a rolling deploy runs.
func TestE2EMigrateIsIdempotent(t *testing.T) {
	i := start(t)
	out, err := i.command(t, "migrate")
	if err != nil {
		t.Fatalf("the second migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "every migration this binary carries") {
		t.Errorf("stdout = %q", out)
	}
}

// TestE2EAnUnknownSubcommandIsAUsageError holds the exit codes of spec 002
// where an operator meets them, in the process rather than in a function.
func TestE2EAnUnknownSubcommandIsAUsageError(t *testing.T) {
	i := start(t)
	out, err := i.command(t, "frobnicate")
	if err == nil {
		t.Fatalf("an unknown subcommand exited 0:\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("an unknown subcommand answered %v:\n%s", err, out)
	}
	if !strings.Contains(out, "unknown subcommand") {
		t.Errorf("stderr = %q", out)
	}
}

// TestE2ETheTierSkipsWithoutTheStack is criterion 5 of spec 014: a tier
// whose variables are unset skips with the remediation in its message, so
// a clean clone with no services is green and a reader knows what to run.
func TestE2ETheTierSkipsWithoutTheStack(t *testing.T) {
	t.Setenv("E2E_DATABASE_URL", "")
	t.Setenv("E2E_S3_ENDPOINT", "")
	if _, reason := readStack(); reason != skipWithoutTheStack {
		t.Fatalf("the skip reason is %q", reason)
	}
	t.Setenv("E2E_DATABASE_URL", "postgres://arca@db/arca")
	if _, reason := readStack(); reason == "" {
		t.Fatal("a tier ran with half the stack configured")
	}
	t.Setenv("E2E_S3_ENDPOINT", "http://127.0.0.1:9000")
	s, reason := readStack()
	if reason != "" {
		t.Fatalf("the tier skipped with the stack configured: %q", reason)
	}
	if s.bucket != "arca-test" || s.key != "minioadmin" {
		t.Fatalf("the defaults of the stack are %+v", s)
	}
	if got := envOr("E2E_S3_BUCKET", "arca-test"); got != "arca-test" {
		t.Errorf("envOr = %q", got)
	}
}
