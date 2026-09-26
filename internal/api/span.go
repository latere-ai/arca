// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"path"
	"slices"
	"strings"
)

// The request span's half of the surface: the name a layer wrapping the
// whole listener gives a request, and the path that layer may record.
//
// That layer is latere.ai/x/pkg/otel's Handler, which opens the request's
// SERVER span and records the request histogram. It runs outside this
// package's frame and outside the verifier, so a refused request is spanned
// and measured like any other, and it names both before the surface has
// answered: the span takes its name when it starts. The name is therefore
// read from the router's table and not from the observation of observe.go,
// which a middleware behind the verifier fills and which a request the
// verifier refused never reaches.

// tokenWildcard is the segment of a pattern that matches a link's token.
const tokenWildcard = "{token}"

// routes answers which row a request would be served by, without serving
// it. It mirrors the two levels of the mount: the patterns registered on the
// listener's own mux, which are the document and the public rows, beside
// the subtree everything else is mounted under, and behind that subtree the
// mux of the rows the verifier guards.
type routes struct {
	// top holds the patterns the mount registers on the listener's mux,
	// each with a handler that is never served, and the subtree.
	top     *http.ServeMux
	subtree string
	// guarded is the mux of the rows behind the verifier, the same one the
	// mount serves them from.
	guarded *http.ServeMux
	// rows is every pattern a row registered. A mux reports a CONNECT
	// request it redirects by the path the client is sent to, which is
	// built from the request, so a pattern the mux reports is a name only
	// when it is one of these.
	rows map[string]bool
	// tokens are the registered paths up to a token wildcard, such as
	// "/v1/shares/links/", each ending in the slash before it.
	tokens []string
}

// newRoutes starts the table for a surface mounted under subtree.
func newRoutes(subtree string) *routes {
	top := http.NewServeMux()
	top.Handle(subtree, http.NotFoundHandler())
	return &routes{top: top, subtree: subtree, rows: map[string]bool{}}
}

// front records a pattern the mount registered on the listener's own mux.
func (n *routes) front(pattern string) {
	n.top.Handle(pattern, http.NotFoundHandler())
	n.add(pattern)
}

// behind records a pattern the mount registered behind the verifier.
func (n *routes) behind(pattern string) { n.add(pattern) }

func (n *routes) add(pattern string) {
	n.rows[pattern] = true
	_, p, _ := strings.Cut(pattern, " ")
	if before, _, found := strings.Cut(p, "/"+tokenWildcard); found {
		if prefix := before + "/"; !slices.Contains(n.tokens, prefix) {
			n.tokens = append(n.tokens, prefix)
		}
	}
}

// Route names a request by the row of the route table that serves it: the
// pattern it was registered under, such as "GET /v1/files/{owner}/{path...}",
// or "" where no row of this surface matches. It reads the method and the
// path and serves nothing, so the request span is named before the surface
// answers, and a request the verifier refuses is named by the route it asked
// for. A request the router redirects is named by the row the redirect leads
// to. A path nobody registered and a wrong method answer "", and so does
// anything the router reports by a path rather than a pattern, so the name
// never carries anything a caller sent.
//
// It answers "" until [API.Mount] has run.
func (a *API) Route(r *http.Request) string {
	n := a.routes
	if n == nil {
		return ""
	}
	_, pattern := n.top.Handler(r)
	if pattern == n.subtree {
		_, pattern = n.guarded.Handler(r)
	}
	if !n.rows[pattern] {
		return ""
	}
	return pattern
}

// SpanPath is the path a request span may record for a request sent to p.
// It is p, except where p is under a path a link route declares its token
// at: there the segment in the token's place is replaced by "{token}". The
// token is a bearer secret (spec 015), and the request span records the path
// it was sent as url.path, so the rule reads the path alone and holds for a
// request no row serves too: a wrong method, or a redirect the router
// answers before any handler runs, still carried the token.
//
// It answers p until [API.Mount] has run.
func (a *API) SpanPath(p string) string {
	n := a.routes
	if n == nil {
		return p
	}
	clean := path.Clean(p)
	for _, prefix := range n.tokens {
		rest, found := strings.CutPrefix(clean, prefix)
		if !found || rest == "" {
			continue
		}
		if _, tail, more := strings.Cut(rest, "/"); more {
			return prefix + tokenWildcard + "/" + tail
		}
		return prefix + tokenWildcard
	}
	return p
}
