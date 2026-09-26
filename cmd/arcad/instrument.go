// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/arca/internal/api"
)

// The request span of spec 025, on both listeners.
//
// Each listener's whole handler is wrapped in latere.ai/x/pkg/otel's
// Handler, outside the surface's own frame and outside the verifier, so a
// request the verifier refuses is one span and one measurement like any
// other. The wrapper opens a SERVER span per request, records
// http.server.request.duration for every request whether or not its span is
// sampled, and continues a trace the caller propagated. Everything a handler
// starts from the request's context, the bucket calls of internal/metrics
// and the authorizer's calls among them, is a child of that span.

// polled are the paths the cluster polls. They are served and never
// observed: a probe every few seconds on every replica would be most of the
// spans and most of the measurements while saying nothing about the traffic.
var polled = []string{"/livez", "/readyz"}

// scraped is the path the metrics scraper polls on the internal listener,
// left unobserved for the same reason. The scraper reports its own duration.
const scraped = "/metrics"

// listener describes one listener to the wrapper: its mux, the patterns this
// file registered on it, the surface mounted on it if any, and the paths it
// serves without observing.
type listener struct {
	mux     *http.ServeMux
	own     []string
	surface *api.API
	skip    []string
}

// instrument wraps a listener's mux in the request span and the request
// metrics.
//
// The span records the path a request was sent as url.path, and on the
// public listener that path may hold a link's token, so the surface's
// SpanPath is written over it before the mux runs, whatever the mux then
// does with the request.
//
// A ServeMux writes the pattern it matched onto the request it is handed,
// and otelhttp labels the request metrics with that pattern in place of the
// route template. On the public listener the match for every route behind
// the verifier is the subtree the surface is mounted at, so the mux is
// handed a copy of the request and the route template is the one route the
// metrics carry.
func instrument(l listener) http.Handler {
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.surface != nil {
			if p := l.surface.SpanPath(r.URL.Path); p != r.URL.Path {
				otel.SetAttributes(r.Context(), attribute.String("url.path", p))
			}
		}
		l.mux.ServeHTTP(w, r.WithContext(r.Context()))
	})
	return otel.Handler(served, ServiceName,
		otel.WithRouteTemplate(l.route),
		otel.WithSkip(func(r *http.Request) bool { return slices.Contains(l.skip, r.URL.Path) }),
	)
}

// route is the route template of the listener's request span and request
// metrics: the path of the pattern a request matches, such as
// "/v1/files/{owner}/{path...}", with the method left to its own attribute,
// or api.Unmatched. It is read from the router's tables before the request
// is served, which is when the span is named.
//
// The surface names its own rows. A pattern this file registered is taken
// from the mux, and only when it is one of the patterns registered here: the
// mux also reports the subtree the surface is mounted at, and a CONNECT
// request it redirects by the path the client is sent to, which is built
// from the request.
func (l listener) route(r *http.Request) string {
	var pattern string
	if l.surface != nil {
		pattern = l.surface.Route(r)
	}
	if pattern == "" {
		if _, p := l.mux.Handler(r); slices.Contains(l.own, p) {
			pattern = p
		}
	}
	if pattern == "" {
		return api.Unmatched
	}
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}
