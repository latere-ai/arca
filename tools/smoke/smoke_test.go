// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package smoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stub serves what a healthy installation serves through the platform
// origin: the two probes, the API description, /version naming the given
// version, and the surface under the capability prefix, which answers an
// unauthenticated request with a 401 (spec 027). An empty version answers
// /version with a page, the way a build whose probes were not mounted would.
func stub(t *testing.T, version string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /v1/storage/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
	}))
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openapi":"3.1.0"}`))
	})
	if version == "" {
		mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><title>not json</title>\n"))
		})
	} else {
		mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"` + version + `","commit":"abc1234","build_time":"2026-09-18T00:00:00Z"}` + "\n"))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// script is the path to release.sh, found from this file rather than from
// the working directory, which `go test` sets to the package.
func script(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(filepath.Dir(file), "release.sh")
}

// run executes the script with the given environment and returns its
// combined output and whether it exited zero.
//
// It skips when the tools the script needs are not on PATH. The gate's
// hermetic run puts only the Go toolchain there, on purpose: arcad forks no
// binary, and this script is the one thing in the tree that does. The
// ordinary run, and every CI job, has curl and grep, so the assertions
// below are made on every run that can make them.
func run(t *testing.T, env ...string) (string, bool) {
	t.Helper()
	for _, cmd := range []string{"bash", "curl", "grep", "mktemp"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("%s is not on PATH, so release.sh cannot run here", cmd)
		}
	}
	cmd := exec.CommandContext(context.Background(), "bash", script(t))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// TestSmokePassesWhenTheServedVersionMatchesTheTag is the ordinary run: a
// healthy installation serving the release that was just deployed.
func TestSmokePassesWhenTheServedVersionMatchesTheTag(t *testing.T) {
	srv := stub(t, "v1.2.3")
	evidence := filepath.Join(t.TempDir(), "smoke.md")
	out, ok := run(t, "BASE_URL="+srv.URL, "TAG=v1.2.3", "COMMIT=abc1234", "OUTPUT_MD="+evidence)
	if !ok {
		t.Fatalf("the smoke failed against a matching tag:\n%s", out)
	}
	if !strings.Contains(out, "served version matches the tag") {
		t.Fatalf("the version check did not run:\n%s", out)
	}
	md, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Release evidence",
		"- Tag: `v1.2.3`",
		"- Commit: `abc1234`",
		"Served version: `v1.2.3`",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("the evidence lacks %q:\n%s", want, md)
		}
	}
}

// TestSmokeFailsOnAVersionThatIsNotTheTag is the failure the smoke exists
// for. A rollout returns as soon as the new replicas are ready, and a
// replica of the previous release still in the endpoint list answers with
// the previous version; without this check the release would be published
// over a half-finished deployment.
func TestSmokeFailsOnAVersionThatIsNotTheTag(t *testing.T) {
	srv := stub(t, "v1.2.2")
	out, ok := run(t, "BASE_URL="+srv.URL, "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against a served version of v1.2.2:\n%s", out)
	}
	if !strings.Contains(out, "version mismatch") {
		t.Errorf("the failure does not name the mismatch:\n%s", out)
	}
}

// TestSmokeRecordsTheVersionWithNoTag is what an operator gets running it
// by hand: no tag to compare against, and the served version written down
// either way.
func TestSmokeRecordsTheVersionWithNoTag(t *testing.T) {
	srv := stub(t, "v1.2.3")
	evidence := filepath.Join(t.TempDir(), "smoke.md")
	out, ok := run(t, "BASE_URL="+srv.URL, "OUTPUT_MD="+evidence)
	if !ok {
		t.Fatalf("the smoke failed with no tag set:\n%s", out)
	}
	if !strings.Contains(out, "served version recorded (v1.2.3)") {
		t.Errorf("the served version was not recorded:\n%s", out)
	}
	md, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "Served version: `v1.2.3`") {
		t.Errorf("the evidence does not record the served version:\n%s", md)
	}
}

// TestSmokeRefusesWithNoTarget keeps the script from having a default
// origin. One would be an origin somebody smokes by accident.
func TestSmokeRefusesWithNoTarget(t *testing.T) {
	out, ok := run(t, "BASE_URL=")
	if ok {
		t.Fatalf("the smoke passed with no BASE_URL:\n%s", out)
	}
	if !strings.Contains(out, "BASE_URL is required") {
		t.Errorf("the failure does not say what is missing:\n%s", out)
	}
}

// TestSmokeFailsOnABrokenEmbed holds the /openapi.json check. The
// description is served from an embed, and a build whose embed broke
// answers every probe and serves no contract.
func TestSmokeFailsOnABrokenEmbed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v1.2.3"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	out, ok := run(t, "BASE_URL="+srv.URL, "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed with no /openapi.json:\n%s", out)
	}
	if !strings.Contains(out, "/openapi.json") {
		t.Errorf("the failure does not name the missing description:\n%s", out)
	}
}

// TestSmokeFailsWhenTheOriginRoutesNoSurface is the failure the prefixed
// check exists for: a rollout that lands the image without the Ingress rule
// that claims the prefix. Every probe at the origin root answers, the served
// version is the tag, and no route of the API is reachable at all, which is
// a release that would be published over a console taking 404s.
func TestSmokeFailsWhenTheOriginRoutesNoSurface(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openapi":"3.1.0"}`))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v1.2.3"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	out, ok := run(t, "BASE_URL="+srv.URL, "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against an origin that routes no surface:\n%s", out)
	}
	if !strings.Contains(out, "/v1/storage/files/me/") {
		t.Errorf("the failure does not name the path that answered nothing:\n%s", out)
	}
}

// TestSmokeFailsOnAVersionThatIsNotJSON catches the build whose /version
// answers a page. The served version would be empty, and an empty value
// compared with a tag would pass on a string comparison that was not
// guarded.
func TestSmokeFailsOnAVersionThatIsNotJSON(t *testing.T) {
	srv := stub(t, "")
	out, ok := run(t, "BASE_URL="+srv.URL, "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against a /version that is not JSON:\n%s", out)
	}
	if !strings.Contains(out, "no version in") {
		t.Errorf("the failure does not name the unreadable answer:\n%s", out)
	}
}
