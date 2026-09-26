// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestARequestIsNamedByTheRowThatServesItAndNeverByItsPath is the name the
// request span and the request histogram take, read before the surface has
// answered: the registered pattern of the row, whether or not the verifier
// will let the request through, and "" wherever no row of the surface
// matches, so the name never holds anything the caller sent.
func TestARequestIsNamedByTheRowThatServesItAndNeverByItsPath(t *testing.T) {
	for _, base := range []string{DefaultBasePath, prefixed} {
		t.Run(base, func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.BasePath = base })
			for _, c := range []struct {
				method, path, want string
			}{
				// A row behind the verifier, asked without a bearer.
				{http.MethodGet, base + "/files/9ab3/notes.md", "GET " + base + "/files/{owner}/{path...}"},
				{http.MethodHead, base + "/files/9ab3/notes.md", "GET " + base + "/files/{owner}/{path...}"},
				// The public rows and the document, on the listener's own mux.
				{http.MethodGet, base + "/shares/links/lnk7f3a", "GET " + base + "/shares/links/{token}"},
				{http.MethodGet, base + "/shares/links/lnk7f3a/meta", "GET " + base + "/shares/links/{token}/meta"},
				{http.MethodGet, "/openapi.json", "GET /openapi.json"},
				// The router redirects a missing trailing slash to the row it
				// leads to, and the name is that row's.
				{http.MethodGet, base + "/shares/links/lnk7f3a/files", "GET " + base + "/shares/links/{token}/files/{path...}"},
				// No row: a method no link route takes, a path nobody
				// registered under the base, and a path outside it.
				{http.MethodPost, base + "/shares/links/lnk7f3a", ""},
				{http.MethodGet, base + "/nothing/here", ""},
				{http.MethodGet, "/elsewhere", ""},
			} {
				if got := h.api.Route(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
					t.Errorf("%s %s is named %q, want %q", c.method, c.path, got, c.want)
				}
			}
		})
	}
}

// TestALinkTokenIsKeptOffTheRecordedPath: the request span records the path
// a request was sent, and a link route carries its bearer secret there, so
// the segment in the token's place is replaced wherever the path is under
// the links collection, cleaned first, and whatever row does or does not
// serve it. Every other path is recorded as sent.
func TestALinkTokenIsKeptOffTheRecordedPath(t *testing.T) {
	for _, base := range []string{DefaultBasePath, prefixed} {
		t.Run(base, func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.BasePath = base })
			links := base + "/shares/links"
			for _, c := range []struct{ path, want string }{
				{links + "/lnk7f3a", links + "/{token}"},
				{links + "/lnk7f3a/meta", links + "/{token}/meta"},
				{links + "/lnk7f3a/files/reports/q3.pdf", links + "/{token}/files/reports/q3.pdf"},
				{links + "//lnk7f3a/files", links + "/{token}/files"},
				{links, links},
				{links + "/", links + "/"},
				{base + "/files/9ab3/notes.md", base + "/files/9ab3/notes.md"},
				{"/elsewhere/lnk7f3a", "/elsewhere/lnk7f3a"},
			} {
				if got := h.api.SpanPath(c.path); got != c.want {
					t.Errorf("SpanPath(%q) = %q, want %q", c.path, got, c.want)
				}
			}
		})
	}
}

// TestASurfaceNotYetMountedNamesNothing: the table is the mount's, so a
// surface that was never mounted names no request and rewrites no path.
func TestASurfaceNotYetMountedNamesNothing(t *testing.T) {
	var a API
	if got := a.Route(httptest.NewRequest(http.MethodGet, "/v1/shares/links/lnk7f3a", nil)); got != "" {
		t.Errorf("an unmounted surface named a request %q", got)
	}
	if got := a.SpanPath("/v1/shares/links/lnk7f3a"); got != "/v1/shares/links/lnk7f3a" {
		t.Errorf("an unmounted surface rewrote a path to %q", got)
	}
}
