// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// prefixed is the base path a hosted installation serves under: the origin
// is partitioned by capability and storage is Arca's segment (spec 027).
const prefixed = "/v1/storage"

// TestTheDocumentNamesThePathsTheMuxAnswersAt is criterion 2 of spec 027:
// the served document's paths are the patterns the mux registered, under the
// base path, and its servers names ARCA_PUBLIC_URL unchanged. The two are
// built from one list read twice, and this reads the document back from the
// bytes GET /openapi.json answers and then asks the mux for every path it
// names.
//
// The server is asserted unchanged because it is the other half of the
// address: servers plus paths is where a route answers, so a base added to
// both would name the prefix twice.
func TestTheDocumentNamesThePathsTheMuxAnswersAt(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.BasePath = prefixed })
	d := h.document(t)

	if len(d.Servers) != 1 || d.Servers[0].URL != "https://storage.example" {
		t.Errorf("the document names the servers %+v, want ARCA_PUBLIC_URL alone and unchanged", d.Servers)
	}
	if len(d.Paths) != len(pathsOf(h.api.rows)) {
		t.Fatalf("the document names %d paths and the table registers %d", len(d.Paths), len(pathsOf(h.api.rows)))
	}
	for _, declared := range pathsOf(h.api.rows) {
		want := prefixed + strings.TrimPrefix(declared, DefaultBasePath)
		if _, named := d.Paths[template(want)]; !named {
			t.Errorf("the document names no %s; it names %v", want, keysOf(d.Paths))
		}
	}
	for path := range d.Paths {
		if !strings.HasPrefix(path, prefixed+"/") {
			t.Errorf("the document names %s, which is outside the base path %s", path, prefixed)
		}
	}
	// Every path the document names is answered where it names it: a
	// guarded row meets the verifier and a public link row its handler.
	// What it is not, is a 404 from the router, which is what a document
	// naming a path nothing registered would produce.
	for path := range d.Paths {
		t.Run(path, func(t *testing.T) {
			w := h.do(t, http.MethodGet, fill(untemplate(path)), "")
			if w.Code == http.StatusNotFound {
				t.Fatalf("%s is named by the document and answered 404: %s", path, w.Body)
			}
		})
	}
}

// TestAPathUnderV1OutsideTheBasePathIsAPlainNotFound is the first half of
// criterion 9: an installation mounted under a prefix registers no pattern
// at the flat path, so a stale caller takes the router's own 404 with no
// envelope. It is the inverse of spec 013's subtree rule: 401 before 404
// holds inside the surface, and outside it there is no surface to protect.
func TestAPathUnderV1OutsideTheBasePathIsAPlainNotFound(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.BasePath = prefixed })
	for _, path := range []string{
		fill(probePath),
		"/v1/shares/links/tkn",
		"/v1/no-such-route",
		"/v1/",
	} {
		t.Run(path, func(t *testing.T) {
			w := h.do(t, http.MethodGet, path, h.bearer())
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s answered %d, want 404: %s", path, w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), `"error"`) {
				t.Errorf("%s answered the envelope of a surface that is not mounted there: %s", path, w.Body)
			}
			if got := w.Header().Get(Header); got != "" {
				t.Errorf("%s carries the request id %q, so it reached the surface", path, got)
			}
		})
	}
}

// TestAPathUnderTheBasePathIsRefusedBeforeItIsNotFound is the second half:
// inside the base path the subtree rule holds as it does at the root, so a
// path no row registers is a 401 without a bearer and a 404 in the envelope
// with one. Whether a route exists is not something an unauthenticated
// caller learns, wherever the surface is mounted.
func TestAPathUnderTheBasePathIsRefusedBeforeItIsNotFound(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.BasePath = prefixed })
	const path = prefixed + "/no-such-route"

	w := h.do(t, http.MethodGet, path, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("%s answered %d without a bearer, want 401: %s", path, w.Code, w.Body)
	}
	if got := decode(t, w).Error.Code; got != CodeUnauthenticated {
		t.Errorf("the code is %q", got)
	}

	verified := h.do(t, http.MethodGet, path, h.bearer())
	if verified.Code != http.StatusNotFound {
		t.Fatalf("%s answered %d with a bearer, want 404: %s", path, verified.Code, verified.Body)
	}
	if got := decode(t, verified).Error.Code; got != CodeNotFound {
		t.Errorf("the code is %q", got)
	}
}

// TestTheBasePathDefaultsToTheRootOfTheVersion: a build that configures no
// base serves the table as it is declared, which is the self-hosted
// installation spec 027 leaves unchanged.
func TestTheBasePathDefaultsToTheRootOfTheVersion(t *testing.T) {
	h := newHarness(t)
	if got := h.api.BasePath(); got != DefaultBasePath {
		t.Errorf("the base path is %q, want %q", got, DefaultBasePath)
	}
	w := h.do(t, http.MethodGet, "/v1/no-such-route", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("the root surface answered %d without a bearer, want 401: %s", w.Code, w.Body)
	}
}

// pathsOf is every distinct path the rows declare, in the table's order.
func pathsOf(rows []route) []string {
	var out []string
	for _, r := range rows {
		if !slices.Contains(out, r.path) {
			out = append(out, r.path)
		}
	}
	return out
}

// template renders a registration the way the document writes it, and
// untemplate reads one back, so a test compares the two spellings of the
// same path without a second copy of either rule.
func template(path string) string   { return strings.ReplaceAll(path, "...}", "}") }
func untemplate(path string) string { return strings.ReplaceAll(path, "{path}", "{path...}") }

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
