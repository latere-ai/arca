// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package metrics is every metric Arca publishes and the one place they are
// registered. It is spec 018's.
//
// The table below is that spec's table in code: one row per metric with its
// type, its label vocabularies, and its bounds. [Register] walks it once at
// start-up, so GET /metrics carries every name from the first scrape,
// including the names of a spec that is not built yet, and answers the
// handle each recording package writes through. Nothing outside this package
// registers a metric, and no package reaches a registry of its own.
//
// A label value comes from a closed vocabulary or from nowhere. The one
// exception is a vocabulary no set of values can be seeded at start-up: the
// route of a request is the mux pattern and the op of a query is the
// statement's name, and those series appear as they are recorded. No label
// carries a path, an object name, a subject, a workspace slug, or a token.
//
// The registry itself is latere.ai/x/pkg/metrics; this is the list of names
// over it and the recording surface above it.
package metrics

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	pkgmetrics "latere.ai/x/pkg/metrics"
)

// Kind is what a row of the table becomes on the registry.
type Kind int

// The three shapes spec 018's table uses.
const (
	Counter Kind = iota
	Histogram
	Gauge
)

// Label is one label of a metric with the values it may take. An empty
// Values is an open vocabulary: no series is seeded for it, because the
// values are not known before the first recording.
type Label struct {
	Name   string
	Values []string
}

// Metric is one row of spec 018's table.
type Metric struct {
	Name    string
	Kind    Kind
	Help    string
	Labels  []Label
	Buckets []float64
}

// The bucket sets. Each is spec 018's range for that row, with the number of
// bounds that row names, so a dashboard reads the bounds the spec published
// rather than the ones a package happened to pick.
var (
	// requestBuckets and bucketOpBuckets and authorizerBuckets are the same
	// range, 5 ms to 10 s in twelve bounds. They are three variables because
	// they are three rows of the table and a later change to one is not a
	// change to the others.
	requestBuckets    = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 7.5, 10}
	bucketOpBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 7.5, 10}
	authorizerBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 7.5, 10}
	// dbQueryBuckets is 1 ms to 5 s in twelve bounds.
	dbQueryBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
	// reaperBuckets is 1 s to 30 min in twelve bounds. A run is a sweep over
	// the installation, so the range is minutes and not milliseconds.
	reaperBuckets = []float64{1, 2.5, 5, 10, 30, 60, 120, 300, 600, 900, 1200, 1800}
	// spaceUsageBuckets is 1 MiB to 1 TiB in twelve bounds, a quarter of a
	// decade apart, which is the shape a distribution of space sizes has.
	spaceUsageBuckets = []float64{
		1 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20,
		1 << 30, 4 << 30, 16 << 30, 64 << 30, 256 << 30, 512 << 30, 1 << 40,
	}
)

// The label vocabularies. A value outside one is never recorded, so neither a
// hostile client nor a new caller can grow the cardinality of a series.
var (
	statusClasses   = []string{"2xx", "3xx", "4xx", "5xx"}
	bytesInKinds    = []string{"inline", "part", "sync"}
	bytesOutKinds   = []string{"inline", "materialize"}
	presignMethods  = []string{"get", "put"}
	presignKinds    = []string{"download", "part"}
	sessionOutcomes = []string{"created", "completed", "aborted", "expired"}
	partOutcomes    = []string{"presigned", "completed", "missing"}
	usageBands      = []string{"1g", "10g", "100g"}
	planes          = []string{"files", "workspaces"}
	runOutcomes     = []string{"ok", "error"}
	findingActions  = []string{"found", "repaired", "deferred"}
	bucketOps       = []string{
		"get", "put", "head", "delete", "list", "presign",
		"multipart_create", "multipart_complete", "multipart_abort",
	}
	bucketResults    = []string{"ok", "not_found", "exists", "error"}
	connStates       = []string{"in_use", "idle"}
	decisionSources  = []string{"authorizer", "owner_policy"}
	decisionOutcomes = []string{"allow", "deny", "unavailable"}
	rejectionReasons = []string{"missing", "malformed", "expired", "audience", "issuer", "signature"}
)

// findingKinds is the closed vocabulary of arca_reaper_findings_total, one
// member per thing the reconciler of spec 010 can find. The thirteenth,
// lease_expired, joined the vocabulary when that spec's pass 3 reported a
// finding for the lease it cleared: a pass whose findings no run reports is
// a pass an operator cannot see run at all.
//
// It is spelled here rather than imported from internal/reaper, so this
// package registers the whole table without loading the packages that record
// it; a test holds the two equal.
var findingKinds = []string{
	"orphan_object", "orphan_candidate", "missing_bytes", "workspace_purged",
	"file_purged", "trash_purged", "share_expired", "event_pruned",
	"star_pruned", "version_pruned", "upload_aborted", "usage_corrected",
	"lease_expired",
}

// eventKinds is the closed vocabulary of arca_events_appended_total, the
// action of one row of spec 010's log. Spelled here for the same reason, and
// held equal to that package's table by a test.
var eventKinds = []string{
	"put", "move", "delete", "restore", "purge",
	"attach", "release", "sync", "reap", "share_created", "share_revoked",
}

// errorCodes is the closed vocabulary of arca_requests_total's code: every
// row of spec 013's error table, plus ok for a response that refused
// nothing. A test holds it equal to that package's table.
var errorCodes = append([]string{"ok"},
	"attachment_gone", "authorizer_unavailable", "bad_request", "body_too_large",
	"exclusive_fields", "forbidden", "internal", "invalid_field", "invalid_path",
	"length_required", "link_read_only", "manifest_incomplete", "missing_field",
	"not_acceptable", "not_found", "not_implemented", "object_too_large",
	"path_taken", "precondition_failed", "quota_exceeded", "rate_limited",
	"slug_taken", "storage_unavailable", "too_many_parts", "unauthenticated",
	"unknown_field", "unknown_plane", "unsupported_media_type", "writer_held",
	"lease_not_held",
)

// table is spec 018's metric table, in that table's order.
var table = []Metric{
	{Name: "arca_requests_total", Kind: Counter, Help: "requests served on the public listener",
		Labels: []Label{{Name: "route"}, {Name: "status_class", Values: statusClasses}, {Name: "code", Values: errorCodes}}},
	{Name: "arca_request_duration_seconds", Kind: Histogram, Help: "time to serve one request",
		Buckets: requestBuckets, Labels: []Label{{Name: "route"}}},
	{Name: "arca_requests_in_flight", Kind: Gauge, Help: "requests started and not yet answered"},
	{Name: "arca_bytes_in_total", Kind: Counter, Help: "bytes accepted into a space",
		Labels: []Label{{Name: "kind", Values: bytesInKinds}}},
	{Name: "arca_bytes_out_total", Kind: Counter, Help: "bytes served out of a space",
		Labels: []Label{{Name: "kind", Values: bytesOutKinds}}},
	{Name: "arca_presigned_urls_total", Kind: Counter, Help: "URLs signed for a client to use against the bucket",
		Labels: []Label{{Name: "method", Values: presignMethods}, {Name: "kind", Values: presignKinds}}},
	{Name: "arca_upload_sessions_total", Kind: Counter, Help: "upload sessions by what became of them",
		Labels: []Label{{Name: "outcome", Values: sessionOutcomes}}},
	{Name: "arca_upload_sessions_open", Kind: Gauge, Help: "upload sessions started and not yet finished"},
	{Name: "arca_upload_parts_total", Kind: Counter, Help: "upload parts by what became of them",
		Labels: []Label{{Name: "outcome", Values: partOutcomes}}},
	{Name: "arca_space_usage_bytes", Kind: Histogram, Help: "bytes one space holds, sampled once per reaper run",
		Buckets: spaceUsageBuckets},
	{Name: "arca_spaces_by_usage", Kind: Gauge, Help: "spaces at or above each band, which are cumulative",
		Labels: []Label{{Name: "band", Values: usageBands}}},
	{Name: "arca_stored_bytes", Kind: Gauge, Help: "bytes the installation holds, by plane",
		Labels: []Label{{Name: "plane", Values: planes}}},
	{Name: "arca_limit_rejections_total", Kind: Counter, Help: "writes refused against the limit an authorizer's answer carried"},
	{Name: "arca_reaper_runs_total", Kind: Counter, Help: "reconciliation runs by result",
		Labels: []Label{{Name: "outcome", Values: runOutcomes}}},
	{Name: "arca_reaper_duration_seconds", Kind: Histogram, Help: "time one reconciliation run took",
		Buckets: reaperBuckets},
	{Name: "arca_reaper_findings_total", Kind: Counter, Help: "what the reconciler found, by kind and what it did about it",
		Labels: []Label{{Name: "kind", Values: findingKinds}, {Name: "action", Values: findingActions}}},
	{Name: "arca_lease_expiries_total", Kind: Counter, Help: "writer leases ended because their deadline passed"},
	{Name: "arca_leases_held", Kind: Gauge, Help: "writer leases held right now"},
	{Name: "arca_events_appended_total", Kind: Counter, Help: "rows appended to the event log, by action",
		Labels: []Label{{Name: "kind", Values: eventKinds}}},
	{Name: "arca_bucket_ops_total", Kind: Counter, Help: "bucket calls by operation and result",
		Labels: []Label{{Name: "op", Values: bucketOps}, {Name: "result", Values: bucketResults}}},
	{Name: "arca_bucket_op_seconds", Kind: Histogram, Help: "time one bucket call took",
		Buckets: bucketOpBuckets, Labels: []Label{{Name: "op", Values: bucketOps}}},
	{Name: "arca_db_query_seconds", Kind: Histogram, Help: "time one query or transaction took",
		Buckets: dbQueryBuckets, Labels: []Label{{Name: "op"}}},
	{Name: "arca_db_conns", Kind: Gauge, Help: "connections of the pool, by state",
		Labels: []Label{{Name: "state", Values: connStates}}},
	{Name: "arca_decisions_total", Kind: Counter, Help: "authorization decisions by who answered and what they answered",
		Labels: []Label{{Name: "source", Values: decisionSources}, {Name: "outcome", Values: decisionOutcomes}}},
	{Name: "arca_authorizer_seconds", Kind: Histogram, Help: "time one call to the authorizer's endpoint took",
		Buckets: authorizerBuckets},
	{Name: "arca_tokens_rejected_total", Kind: Counter, Help: "bearers the verifier refused, by the row of the reason table",
		Labels: []Label{{Name: "reason", Values: rejectionReasons}}},
}

// Table answers spec 018's table as this package registers it, in the
// spec's order. It is what the table test reads and what tools/rules checks
// an alert's metric and label names against.
func Table() []Metric { return slices.Clone(table) }

// Names reports every metric name of the table, in the table's order.
func Names() []string {
	out := make([]string, 0, len(table))
	for _, m := range table {
		out = append(out, m.Name)
	}
	return out
}

// FindingKinds answers the closed vocabulary of arca_reaper_findings_total.
func FindingKinds() []string { return slices.Clone(findingKinds) }

// EventKinds answers the closed vocabulary of arca_events_appended_total.
func EventKinds() []string { return slices.Clone(eventKinds) }

// ErrorCodes answers the closed vocabulary of arca_requests_total's code.
func ErrorCodes() []string { return slices.Clone(errorCodes) }

// GaugeFamily is one gauge of the table. Every value of its vocabulary reads
// 0 until a recording package binds a source, so the series exists before
// the package that fills it is built: the registry drops a gauge collector
// that returns nothing at all.
type GaugeFamily struct {
	labels []Label

	mu   sync.Mutex
	srcs map[string]func() float64
}

// Bind gives an unlabelled gauge its source, read at every scrape.
func (g *GaugeFamily) Bind(fn func() float64) { g.BindFor("", fn) }

// BindFor gives one label value of a gauge its source. The value must be in
// the vocabulary of the table, and a gauge with more than one label has
// none: no metric of the table needs that.
func (g *GaugeFamily) BindFor(value string, fn func() float64) {
	if !slices.Contains(g.values(), value) {
		panic(fmt.Sprintf("metrics: %q is not a value of this gauge", value))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.srcs[value] = fn
}

// values reports the label values this gauge answers one series for. An
// unlabelled gauge has the single empty value.
func (g *GaugeFamily) values() []string {
	if len(g.labels) == 0 {
		return []string{""}
	}
	return g.labels[0].Values
}

// collect is the registry's scrape-time callback.
func (g *GaugeFamily) collect() []pkgmetrics.LabeledValue {
	g.mu.Lock()
	srcs := maps.Clone(g.srcs)
	g.mu.Unlock()
	out := make([]pkgmetrics.LabeledValue, 0, len(srcs)+1)
	for _, v := range g.values() {
		var labels map[string]string
		if v != "" {
			labels = map[string]string{g.labels[0].Name: v}
		}
		var value float64
		if fn := srcs[v]; fn != nil {
			value = fn()
		}
		out = append(out, pkgmetrics.LabeledValue{Labels: labels, Value: value})
	}
	return out
}

// combinations reports every label set a metric's vocabularies allow: one nil
// set for a metric with no label, and nothing at all for a metric with an
// open vocabulary, whose series appear as they are recorded.
func combinations(labels []Label) []map[string]string {
	out := []map[string]string{nil}
	for _, l := range labels {
		if len(l.Values) == 0 {
			return nil
		}
		next := make([]map[string]string, 0, len(out)*len(l.Values))
		for _, base := range out {
			for _, v := range l.Values {
				m := map[string]string{l.Name: v}
				maps.Copy(m, base)
				next = append(next, m)
			}
		}
		out = next
	}
	return out
}

// registrar holds what the walk over the table built, keyed by name, so a
// field of [Set] and its row cannot drift: a name the table does not define
// with that type panics at start-up, before a scrape can miss it.
type registrar struct {
	counters   map[string]*pkgmetrics.Counter
	histograms map[string]*pkgmetrics.Histogram
	gauges     map[string]*GaugeFamily
}

func (r registrar) counter(name string) *pkgmetrics.Counter {
	c, ok := r.counters[name]
	if !ok {
		panic("metrics: no counter " + name + " in the table")
	}
	return c
}

func (r registrar) histogram(name string) *pkgmetrics.Histogram {
	h, ok := r.histograms[name]
	if !ok {
		panic("metrics: no histogram " + name + " in the table")
	}
	return h
}

func (r registrar) gauge(name string) *GaugeFamily {
	g, ok := r.gauges[name]
	if !ok {
		panic("metrics: no gauge " + name + " in the table")
	}
	return g
}

// register walks the table onto reg and answers the handles by name.
func register(reg *pkgmetrics.Registry) registrar {
	r := registrar{
		counters:   map[string]*pkgmetrics.Counter{},
		histograms: map[string]*pkgmetrics.Histogram{},
		gauges:     map[string]*GaugeFamily{},
	}
	for _, m := range table {
		switch m.Kind {
		case Counter:
			c := reg.Counter(m.Name, m.Help)
			// The series reads 0 before the first event, so a dashboard and
			// an alert see it from the first scrape rather than from the
			// first incident.
			for _, labels := range combinations(m.Labels) {
				c.Add(labels, 0)
			}
			r.counters[m.Name] = c
		case Histogram:
			h := reg.Histogram(m.Name, m.Help, m.Buckets)
			for _, labels := range combinations(m.Labels) {
				h.Init(labels)
			}
			r.histograms[m.Name] = h
		case Gauge:
			g := &GaugeFamily{labels: m.Labels, srcs: map[string]func() float64{}}
			reg.Gauge(m.Name, m.Help, g.collect)
			r.gauges[m.Name] = g
		}
	}
	return r
}
