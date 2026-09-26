// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	gotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/health"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/events"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/metrics"
	"latere.ai/x/arca/internal/store"
)

// requestDuration is the request histogram the OpenTelemetry HTTP semantic
// conventions name, which otelhttp records for every request it observes,
// sampled or not.
const requestDuration = "http.server.request.duration"

// recording installs an in-memory tracer provider and meter provider as the
// process's own, the two otel.Bootstrap installs when an endpoint is set, and
// puts the previous two back when the case ends. It runs before the handler
// is built, because otelhttp takes its meter when the handler is built.
func recording(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spans),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previousTracer, previousMeter := gotel.GetTracerProvider(), gotel.GetMeterProvider()
	gotel.SetTracerProvider(tracer)
	gotel.SetMeterProvider(meter)
	t.Cleanup(func() {
		gotel.SetTracerProvider(previousTracer)
		gotel.SetMeterProvider(previousMeter)
		ctx := context.WithoutCancel(t.Context())
		if err := tracer.Shutdown(ctx); err != nil {
			t.Errorf("the tracer provider did not shut down: %v", err)
		}
		if err := meter.Shutdown(ctx); err != nil {
			t.Errorf("the meter provider did not shut down: %v", err)
		}
	})
	return spans, reader
}

// serverSpans is every ended span of kind SERVER. The recorder also holds the
// CLIENT spans of the instrumented transport and the INTERNAL spans of the
// bucket decorator, and a request is counted by its SERVER span alone.
func serverSpans(spans *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			out = append(out, s)
		}
	}
	return out
}

// attributeOf reads one attribute of a span, and "" where it has none.
func attributeOf(s sdktrace.ReadOnlySpan, key attribute.Key) string {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value.String()
		}
	}
	return ""
}

// measuredRoutes is how many requests the request histogram counted under
// each http.route, with "" for a data point that carries none.
func measuredRoutes(t *testing.T, reader *sdkmetric.ManualReader) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("the meter reader did not collect: %v", err)
	}
	out := map[string]uint64{}
	found := false
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != requestDuration {
				continue
			}
			found = true
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is a %T, want a float64 histogram", requestDuration, m.Data)
			}
			for _, p := range histogram.DataPoints {
				route, _ := p.Attributes.Value("http.route")
				out[route.String()] += p.Count
			}
		}
	}
	if !found {
		t.Fatalf("no %s was recorded", requestDuration)
	}
	return out
}

// send drives one request against a running listener and answers the
// response with its body read and closed. A redirect is answered rather than
// followed, so one request is one request on the server.
func send(t *testing.T, method, target, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestEveryRequestIsOneServerSpanAndOneMeasurementNamedByItsRoute is the
// request span of spec 025 at the node: every request either listener
// serves, apart from the probes and the scrape, is one SERVER span and one
// measurement of the request histogram, both named by the pattern of the row
// it matched and never by the path it was sent. A request the verifier
// refused is named by the route it asked for, so a 401 is counted where it
// happened.
func TestEveryRequestIsOneServerSpanAndOneMeasurementNamedByItsRoute(t *testing.T) {
	spans, reader := recording(t)
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	publicURL, internalURL, _, stop := startServe(t, map[string]string{"ARCA_OIDC_ISSUERS": iss.URL()})
	bearer := iss.Mint(issuertest.Claims{Sub: "9ab3"})

	// The link token is a bearer secret: the path carries it, and neither
	// the route nor the span may.
	const linkToken = "lnk-canary-7f3a"
	links := publicURL + "/v1/shares/links/" + linkToken
	observed := []struct {
		method, target, bearer, route string
	}{
		{http.MethodGet, publicURL + "/", "", "/{$}"},
		{http.MethodGet, publicURL + "/version", "", "/version"},
		{http.MethodGet, publicURL + "/openapi.json", "", "/openapi.json"},
		{http.MethodGet, publicURL + "/v1/trash", "", "/v1/trash"},
		{http.MethodGet, publicURL + "/v1/trash", bearer, "/v1/trash"},
		{http.MethodGet, links, "", "/v1/shares/links/{token}"},
		{http.MethodGet, links + "/files/notes.md", "", "/v1/shares/links/{token}/files/{path...}"},
		// Two requests that carry a token and reach no link handler: a
		// method no link route takes, which the verifier refuses, and a
		// path the router redirects before any handler runs, which is named
		// by the row the redirect leads to.
		{http.MethodPost, links, "", api.Unmatched},
		{http.MethodGet, links + "/files", "", "/v1/shares/links/{token}/files/{path...}"},
		{http.MethodGet, publicURL + "/v1/nothing/here", bearer, api.Unmatched},
		{http.MethodGet, publicURL + "/nothing", "", api.Unmatched},
		{http.MethodGet, internalURL + "/version", "", "/version"},
	}
	probed := []string{
		publicURL + "/livez", publicURL + "/readyz",
		internalURL + "/livez", internalURL + "/readyz", internalURL + "/metrics",
	}

	var requestIDs []string
	for _, o := range observed {
		resp := send(t, o.method, o.target, o.bearer)
		if id := resp.Header.Get(api.Header); id != "" {
			requestIDs = append(requestIDs, id)
		}
	}
	for _, p := range probed {
		send(t, http.MethodGet, p, "")
	}
	// The stop waits for every handler to return, and a span ends and a
	// measurement is recorded when its handler returns.
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}

	server := serverSpans(spans)
	if len(server) != len(observed) {
		names := make([]string, len(server))
		for i, s := range server {
			names[i] = s.Name()
		}
		t.Errorf("%d SERVER spans for %d observed requests: %q", len(server), len(observed), names)
	}
	var want, got []string
	for _, o := range observed {
		want = append(want, o.method+" "+o.route)
	}
	for _, s := range server {
		got = append(got, s.Name())
		method := attributeOf(s, "http.request.method")
		if route := attributeOf(s, "http.route"); method+" "+route != s.Name() {
			t.Errorf("the span %q carries http.route %q", s.Name(), route)
		}
		if path := attributeOf(s, "url.path"); strings.Contains(path, linkToken) {
			t.Errorf("the span %q carries the link token in url.path %q", s.Name(), path)
		}
		if strings.Contains(s.Name(), linkToken) || strings.Contains(s.Name(), "notes.md") {
			t.Errorf("the span name %q carries what the path carried", s.Name())
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the SERVER spans are named\n%q\nwant\n%q", got, want)
	}
	// The request id the surface answered is on the request's own span, so
	// a trace is found from the id a caller quotes.
	for _, id := range requestIDs {
		if !slices.ContainsFunc(server, func(s sdktrace.ReadOnlySpan) bool {
			return attributeOf(s, "arca.request_id") == id
		}) {
			t.Errorf("no SERVER span carries the request id %q", id)
		}
	}

	measured := measuredRoutes(t, reader)
	wantMeasured := map[string]uint64{}
	for _, o := range observed {
		wantMeasured[o.route]++
	}
	for route, n := range wantMeasured {
		if measured[route] != n {
			t.Errorf("%s counted %d requests under http.route %q, want %d", requestDuration, measured[route], route, n)
		}
	}
	for route, n := range measured {
		if _, ok := wantMeasured[route]; !ok {
			t.Errorf("%s counted %d requests under http.route %q, which no observed request matched", requestDuration, n, route)
		}
	}
}

// TestABucketCallIsAChildOfItsRequestSpan: a put through the public
// listener's handler opens its bucket call inside the request's span, so a
// trace reads one request with its storage operations under it rather than
// a request and a bucket call that never meet. The unit tier has no
// database, so the metadata store answers every read as no row and refuses
// the commit: the put reaches the bucket, fails after it, and removes what
// it wrote, which is two bucket calls under one request.
func TestABucketCallIsAChildOfItsRequestSpan(t *testing.T) {
	spans, _ := recording(t)
	iss := issuertest.New(t, issuertest.WithDefaultAudience("arca"))
	endpoint := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	endpoint.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	identity, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audiences: []string{"arca"},
		AuthorizerURL: endpoint.URL(), AuthorizerToken: endpoint.Token(),
	})
	if err != nil {
		t.Fatalf("the identity of spec 006 would not start: %v", err)
	}
	recorder := metrics.Register(nil)
	object := files.New(files.Options{
		DB: failingStore{err: pgx.ErrNoRows}, Bucket: recorder.Bucket(blob.NewMemory()),
		Decide: identity.Authorizer,
		Config: config.Config{BucketPrefix: "arca/", InlineBytes: 1 << 10, MaxUploadBytes: 1 << 20},
		Ledger: fileLedger{log: events.NewLog(), usage: events.NewLedger()},
		// The liveness rule of spec 009 is read for a workspace path only,
		// and this put is not one.
		Workspaces: store.NewWorkspaces(),
	})
	surface, err := api.New(api.Options{
		Verifier: identity.Verifier, Authorizer: identity.Authorizer,
		PublicURL: publicBase, Events: events.NewLog(), Routes: files.Bind(object),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("the surface would not build: %v", err)
	}
	handler := publicHandler(health.Handler(health.Options{}), surface)

	owner := url.PathEscape(iss.URL() + "|9ab3")
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/v1/files/"+owner+"/files/notes.md", strings.NewReader("# notes\n"))
	r.Header.Set("Authorization", "Bearer "+iss.Mint(issuertest.Claims{Sub: "9ab3"}))
	r.Header.Set(api.HeaderContentType, "text/markdown")
	handler.ServeHTTP(httptest.NewRecorder(), r)

	server := serverSpans(spans)
	if len(server) != 1 {
		t.Fatalf("%d SERVER spans for one request", len(server))
	}
	request := server[0].SpanContext()
	if name := server[0].Name(); name != "PUT /v1/files/{owner}/{path...}" {
		t.Errorf("the request span is named %q", name)
	}
	var calls []string
	for _, s := range spans.Ended() {
		if !strings.HasPrefix(s.Name(), "bucket.") {
			continue
		}
		calls = append(calls, s.Name())
		if s.SpanContext().TraceID() != request.TraceID() {
			t.Errorf("%s is in trace %s, and the request is in %s", s.Name(), s.SpanContext().TraceID(), request.TraceID())
		}
		if s.Parent().SpanID() != request.SpanID() {
			t.Errorf("%s has parent %s, want the request span %s", s.Name(), s.Parent().SpanID(), request.SpanID())
		}
	}
	// The write, then the removal of what a refused commit left behind.
	if want := []string{"bucket.put", "bucket.delete"}; !slices.Equal(calls, want) {
		t.Errorf("the put reached the bucket as %q, want %q", calls, want)
	}
}
