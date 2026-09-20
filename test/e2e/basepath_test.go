// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// prefixed is the base a hosted installation serves under: api.latere.ai
// partitions /v1 by capability and storage is Arca's segment (spec 027).
const prefixed = "/v1/storage"

// TestE2EEveryDocumentedPathAnswersUnderTheBasePathAndNoneAtTheRoot is
// criterion 1 of spec 027 through the binary: an installation started with
// ARCA_BASE_PATH mounts its whole surface under that base, and the root of
// the version answers no route of it.
//
// Every path is driven, from the document the server itself serves, so a row
// registered and not described, or described and not registered, fails here
// rather than at a caller. What each request is held to is whether it reached
// the surface at all, read off the request id the frame writes on every
// answer it makes: under the base path each one does, at the root none does.
// The status is not the assertion, because a guarded row answers 401 without
// a bearer while the three public link rows answer their handler, and both
// are the surface answering.
func TestE2EEveryDocumentedPathAnswersUnderTheBasePathAndNoneAtTheRoot(t *testing.T) {
	i := startWith(t, "ARCA_BASE_PATH="+prefixed)

	code, body := get(t, i.publicURL+"/openapi.json")
	if code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", code, body)
	}
	var document struct {
		Paths   map[string]map[string]any `json:"paths"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if len(document.Paths) < 20 {
		t.Fatalf("the document names %d paths, want the several dozen spec 013 registers", len(document.Paths))
	}
	// The server is the origin root and carries no base: the address of a
	// route is the server and the path the document writes, so a base on
	// both would name the prefix twice.
	if len(document.Servers) != 1 || document.Servers[0].URL != "http://127.0.0.1" {
		t.Errorf("the document names the servers %+v, want ARCA_PUBLIC_URL unchanged", document.Servers)
	}

	for path := range document.Paths {
		if !strings.HasPrefix(path, prefixed+"/") {
			t.Errorf("the document names %s, which is outside the base path this installation serves under", path)
			continue
		}
		t.Run(path, func(t *testing.T) {
			under := fill(path)
			if id := requestID(t, i.publicURL+under); id == "" {
				t.Errorf("GET %s reached no surface: the answer carries no request id", under)
			}
			flat := "/v1" + strings.TrimPrefix(under, prefixed)
			status, answer, id := probe(t, i.publicURL+flat)
			if id != "" {
				t.Errorf("GET %s reached the surface and answered the request id %q", flat, id)
			}
			if status != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404: the root of the version serves no route of this installation", flat, status)
			}
			if strings.Contains(answer, `"error"`) {
				t.Errorf("GET %s answered the envelope of a surface that is not mounted there: %s", flat, answer)
			}
		})
	}

	// The probes and the document keep the origin root. They sit outside the
	// surface on the listener, and no base path moves them.
	for _, root := range []string{"/livez", "/readyz", "/version", "/openapi.json"} {
		if status, _ := get(t, i.publicURL+root); status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 at the root", root, status)
		}
	}
}

// fill renders a path template as a request: every wildcard takes a value
// that names nothing, because what is driven is where a path answers and not
// what it answers about.
func fill(path string) string {
	out := strings.ReplaceAll(path, "{owner}", "me")
	out = strings.ReplaceAll(out, "{token}", "tkn")
	out = strings.ReplaceAll(out, "{id}", "01J8E2E")
	out = strings.ReplaceAll(out, "{aid}", "01J8E2E")
	out = strings.ReplaceAll(out, "{path}", "files/e2e.txt")
	return out
}

// requestID answers the request id one unauthenticated GET comes back with,
// which is the frame's mark on every answer the surface makes.
func requestID(t *testing.T, url string) string {
	t.Helper()
	_, _, id := probe(t, url)
	return id
}

// probe sends one unauthenticated GET and answers the status, the body and
// the request id header.
func probe(t *testing.T, url string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(read), resp.Header.Get("X-Request-Id")
}
