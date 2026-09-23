// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"
)

// The two rules every test of the request path sets on the stub endpoint.
var (
	allowEverything = stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true}
	denyEverything  = stub.Rule{Subject: "*", Action: "*", Resource: "*"}
)

// counting is the recording surface of spec 018 as a test reads it: one row
// per answered request, and the reasons the verifier gave.
type counting struct {
	mu       sync.Mutex
	started  int
	answered []answered
	reasons  []string
}

type answered struct {
	route, statusClass, code string
	took                     time.Duration
}

func (c *counting) RequestStarted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started++
}

func (c *counting) RequestFinished(route, statusClass, code string, took time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answered = append(c.answered, answered{route, statusClass, code, took})
}

func (c *counting) TokenRejected(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, reason)
}

// last answers the row of the request just served.
func (c *counting) last(t *testing.T) answered {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.answered) == 0 {
		t.Fatal("no request was recorded")
	}
	return c.answered[len(c.answered)-1]
}

// observing mounts a surface whose request path records into c and whose
// lines land in lines.
func observing(t *testing.T, c *counting, lines *bytes.Buffer) *harness {
	t.Helper()
	return newHarness(t, func(o *Options) {
		o.Metrics = c
		o.Logger = slog.New(slog.NewJSONHandler(lines, nil))
	})
}

// TestARequestIsCountedByItsRouteAndItsStatus is spec 018's
// arca_requests_total: the label is the mux pattern of the row that matched,
// never the path, and the code is the row of the error table a refusal
// answered or ok.
func TestARequestIsCountedByItsRouteAndItsStatus(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)
	h.endpoint.Allow(allowEverything)

	h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", h.bearer())
	got := c.last(t)
	if got.route != "GET /v1/files/{owner}/{path...}" {
		t.Errorf("the request was counted under the route %q", got.route)
	}
	if got.statusClass != "2xx" || got.code != OK {
		t.Errorf("the request was counted as %s %s", got.statusClass, got.code)
	}
	if strings.Contains(got.route, "notes.md") {
		t.Error("the route label carries the path a caller sent")
	}
	if c.started != 1 {
		t.Errorf("%d requests were started for one served", c.started)
	}
}

// TestARefusalIsCountedByTheRowOfTheErrorTable: the code label is settled
// where the envelope is written, which is the one place a refusal of this
// surface goes out.
func TestARefusalIsCountedByTheRowOfTheErrorTable(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)
	h.endpoint.Deny(denyEverything, "no rule allows it")

	h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", h.bearer())
	got := c.last(t)
	if got.statusClass != "4xx" || got.code != CodeForbidden {
		t.Errorf("a denied request was counted as %s %s", got.statusClass, got.code)
	}
}

// TestAPathNoRouteRegistersIsCountedUnderOneBoundedLabel: an unmatched path
// is exactly what a route label must never carry, so it is counted under one
// word and its 404 still reaches the series.
func TestAPathNoRouteRegistersIsCountedUnderOneBoundedLabel(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)

	h.do(t, http.MethodGet, "/v1/nothing/here", h.bearer())
	got := c.last(t)
	if got.route != Unmatched {
		t.Errorf("an unregistered path was counted under the route %q", got.route)
	}
	if got.statusClass != "4xx" || got.code != CodeNotFound {
		t.Errorf("the 404 was counted as %s %s", got.statusClass, got.code)
	}
}

// TestARefusedBearerIsStillCounted is why the middleware is outermost: a 401
// is a request this replica served, and an error rate computed without it is
// the wrong number. The reason table row reaches the token counter at the
// same time.
func TestARefusedBearerIsStillCounted(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)

	h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", "")
	got := c.last(t)
	if got.statusClass != "4xx" || got.code != CodeUnauthenticated {
		t.Errorf("a request with no bearer was counted as %s %s", got.statusClass, got.code)
	}
	if len(c.reasons) != 1 || c.reasons[0] == "" {
		t.Errorf("the refusal reached the token counter as %v", c.reasons)
	}
}

// TestTheRequestLineCarriesTheIdsAndNothingSecret is criterion 6 of spec
// 018, as far as this phase reaches it: one line per request at INFO with
// the request id and the trace id on it, and no attribute carrying a token,
// a credential or the path.
func TestTheRequestLineCarriesTheIdsAndNothingSecret(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)
	h.endpoint.Allow(allowEverything)

	token := h.bearer()
	w := h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", token)

	records := decodeLines(t, &lines)
	if len(records) != 1 {
		t.Fatalf("%d lines were written for one request", len(records))
	}
	line := records[0]
	if line["subject"] == "" {
		t.Error("the line names no subject for a request the verifier settled")
	}
	for _, key := range []string{"route", "method", "status", "code", "duration_ms", "subject", "request_id", "trace_id"} {
		if _, carried := line[key]; !carried {
			t.Errorf("the line carries no %s", key)
		}
	}
	if line["request_id"] != w.Header().Get(Header) {
		t.Errorf("the line says %v and the response says %q", line["request_id"], w.Header().Get(Header))
	}
	if line["route"] != "GET /v1/files/{owner}/{path...}" {
		t.Errorf("the line names the route %v", line["route"])
	}
	body := lines.String()
	if strings.Contains(body, token) || strings.Contains(body, "Bearer") {
		t.Error("the bearer reached a log line")
	}
	if strings.Contains(body, "notes.md") {
		t.Error("the path reached a log line, and a path carries what a person called their file")
	}
}

// TestOneLineAndOneObservationPerRequest is criterion 8's first half: one
// line per request, and not one per middleware that touched it.
func TestOneLineAndOneObservationPerRequest(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)
	h.endpoint.Allow(allowEverything)

	for range 3 {
		h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", h.bearer())
	}
	if got := len(decodeLines(t, &lines)); got != 3 {
		t.Errorf("%d lines for three requests", got)
	}
	if got := len(c.answered); got != 3 {
		t.Errorf("%d observations for three requests", got)
	}
	if c.started != 3 {
		t.Errorf("%d starts for three requests", c.started)
	}
}

// TestTheDocumentIsObservedToo: GET /openapi.json is a request this replica
// served and is counted under its own pattern.
func TestTheDocumentIsObservedToo(t *testing.T) {
	var lines bytes.Buffer
	c := &counting{}
	h := observing(t, c, &lines)

	h.do(t, http.MethodGet, "/openapi.json", "")
	if got := c.last(t); got.route != "GET /openapi.json" || got.statusClass != "2xx" {
		t.Errorf("the document was counted as %s %s", got.route, got.statusClass)
	}
}

// TestASurfaceWithNoSeamRecordsNothingAndStillServes: the node that exports
// nothing and the test that asserts on behavior both build the frame with
// no recording surface, and the request path costs no branch.
func TestASurfaceWithNoSeamRecordsNothingAndStillServes(t *testing.T) {
	h := newHarness(t)
	h.endpoint.Allow(allowEverything)
	if _, ok := h.api.metrics.(nothing); !ok {
		t.Fatalf("the seam defaulted to %T", h.api.metrics)
	}
	if w := h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", h.bearer()); w.Code != http.StatusOK {
		t.Errorf("the request was answered %d", w.Code)
	}
	h.api.metrics.RequestStarted()
	h.api.metrics.RequestFinished("r", "2xx", OK, time.Second)
	h.api.metrics.TokenRejected("missing")
}

// TestTheStatusClassIsTheHundredsDigit holds the label to the four classes
// spec 018 names, and answers nothing for a status outside them so the
// recording surface drops it rather than opening a series.
func TestTheStatusClassIsTheHundredsDigit(t *testing.T) {
	for status, want := range map[int]string{
		200: "2xx", 204: "2xx", 302: "3xx", 404: "4xx", 429: "4xx", 500: "5xx", 503: "5xx",
		100: "", 600: "",
	} {
		if got := class(status); got != want {
			t.Errorf("%d is class %q, want %q", status, got, want)
		}
	}
}

// TestTheRecordingWriterKeepsTheWriterBeneathIt: a handler that streams
// reaches the real writer through http.ResponseController, which follows
// Unwrap, and the first status written is the one recorded.
func TestTheRecordingWriterKeepsTheWriterBeneathIt(t *testing.T) {
	under := httptest.NewRecorder()
	w := &recording{ResponseWriter: under, status: http.StatusOK}
	if w.Unwrap() != http.ResponseWriter(under) {
		t.Error("the writer beneath is not reachable")
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("a flush through the recorder: %v", err)
	}
	w.WriteHeader(http.StatusTeapot)
	w.WriteHeader(http.StatusInternalServerError)
	if w.status != http.StatusTeapot {
		t.Errorf("the status recorded is %d, and the first one written is what the client got", w.status)
	}

	// A handler that writes a body without a status answered 200, which is
	// what net/http sends.
	plain := &recording{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if _, err := plain.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if plain.status != http.StatusOK {
		t.Errorf("a body with no status was recorded as %d", plain.status)
	}
}

// TestTheObservationIsAbsentOutsideARequest: the two writers into it are
// called from the frame and from a handler, and neither may panic where no
// middleware put one on the context.
func TestTheObservationIsAbsentOutsideARequest(t *testing.T) {
	if observationFrom(t.Context()) != nil {
		t.Error("an observation exists outside a request")
	}
	noting(t.Context(), CodeInternal)
	naming("GET /v1/files/{owner}/{path...}", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/files/9ab3/notes.md", nil))
}

// TestTheDefaultLoggerIsTheOneTheNodeBootstrapped: a surface built with no
// logger writes through slog's default, which is what cmd/arcad set.
func TestTheDefaultLoggerIsTheOneTheNodeBootstrapped(t *testing.T) {
	var lines bytes.Buffer
	kept := slog.Default()
	t.Cleanup(func() { slog.SetDefault(kept) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lines, nil)))

	h := newHarness(t, func(o *Options) { o.Logger = nil })
	h.endpoint.Allow(allowEverything)
	h.do(t, http.MethodGet, "/v1/files/9ab3/notes.md", h.bearer())
	if !strings.Contains(lines.String(), `"route":"GET /v1/files/{owner}/{path...}"`) {
		t.Errorf("the default logger carries %q", lines.String())
	}
}

// decodeLines reads the JSON lines a run wrote.
func decodeLines(t *testing.T, lines *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(lines.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a line is not JSON: %v", err)
		}
		out = append(out, record)
	}
	return slices.Clip(out)
}
