// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"
)

// The three bands of arca_spaces_by_usage, in bytes. They are cumulative: a
// space of 500 GiB clears all three, so every band counts every space at or
// above it and an operator reads "how many spaces are at least this large"
// off one series rather than by summing a set of disjoint ones.
var bands = map[string]int64{
	"1g":   1 << 30,
	"10g":  10 << 30,
	"100g": 100 << 30,
}

// Set is every handle of spec 018's table and the surface the recording
// packages write through. A field is never nil: a metric whose spec is not
// built yet has a handle nothing writes to, and its series still reads zero
// from the first scrape.
//
// The methods on it are the seams the other packages declare. One object
// implements all of them, so a node binds the recording surface once and no
// package reaches a registry of its own.
type Set struct {
	// The request path (spec 013).
	Requests         *pkgmetrics.Counter
	RequestDuration  *pkgmetrics.Histogram
	RequestsInFlight *GaugeFamily

	// Bytes through the two planes (specs 005, 007, 009).
	BytesIn  *pkgmetrics.Counter
	BytesOut *pkgmetrics.Counter

	// The bucket (spec 003).
	PresignedURLs   *pkgmetrics.Counter
	BucketOps       *pkgmetrics.Counter
	BucketOpSeconds *pkgmetrics.Histogram

	// Uploads (spec 007).
	UploadSessions     *pkgmetrics.Counter
	UploadSessionsOpen *GaugeFamily
	UploadParts        *pkgmetrics.Counter

	// Usage and the reconciler (spec 010).
	SpaceUsage      *pkgmetrics.Histogram
	SpacesByUsage   *GaugeFamily
	StoredBytes     *GaugeFamily
	LimitRejections *pkgmetrics.Counter
	ReaperRuns      *pkgmetrics.Counter
	ReaperDuration  *pkgmetrics.Histogram
	ReaperFindings  *pkgmetrics.Counter
	EventsAppended  *pkgmetrics.Counter

	// The writer lease (spec 009).
	LeaseExpiries *pkgmetrics.Counter
	LeasesHeld    *GaugeFamily

	// The database (spec 004).
	DBQuerySeconds *pkgmetrics.Histogram
	DBConns        *GaugeFamily

	// Identity (spec 006).
	Decisions         *pkgmetrics.Counter
	AuthorizerSeconds *pkgmetrics.Histogram
	TokensRejected    *pkgmetrics.Counter

	// reg is the registry the table was walked onto, which [Set.Handler]
	// writes and nothing else reads.
	reg *pkgmetrics.Registry

	// inFlight backs arca_requests_in_flight, a gauge over a number this
	// package keeps rather than one a package reports.
	inFlight atomic.Int64

	// mu guards the usage sample. A run fills pending space by space and
	// publishes it whole when the run ends, so a scrape never reads half of
	// one walk over the ledger and two runs never accumulate into one count.
	mu      sync.Mutex
	pending map[string]float64
	current map[string]float64
}

// Register registers every metric of the table on reg and answers the
// recording surface. A nil registry gets one of its own, which is what a
// test of a recording package that asserts on no exposition wants.
func Register(reg *pkgmetrics.Registry) *Set {
	if reg == nil {
		reg = pkgmetrics.NewRegistry()
	}
	r := register(reg)
	s := &Set{
		Requests:         r.counter("arca_requests_total"),
		RequestDuration:  r.histogram("arca_request_duration_seconds"),
		RequestsInFlight: r.gauge("arca_requests_in_flight"),

		BytesIn:  r.counter("arca_bytes_in_total"),
		BytesOut: r.counter("arca_bytes_out_total"),

		PresignedURLs:   r.counter("arca_presigned_urls_total"),
		BucketOps:       r.counter("arca_bucket_ops_total"),
		BucketOpSeconds: r.histogram("arca_bucket_op_seconds"),

		UploadSessions:     r.counter("arca_upload_sessions_total"),
		UploadSessionsOpen: r.gauge("arca_upload_sessions_open"),
		UploadParts:        r.counter("arca_upload_parts_total"),

		SpaceUsage:      r.histogram("arca_space_usage_bytes"),
		SpacesByUsage:   r.gauge("arca_spaces_by_usage"),
		StoredBytes:     r.gauge("arca_stored_bytes"),
		LimitRejections: r.counter("arca_limit_rejections_total"),
		ReaperRuns:      r.counter("arca_reaper_runs_total"),
		ReaperDuration:  r.histogram("arca_reaper_duration_seconds"),
		ReaperFindings:  r.counter("arca_reaper_findings_total"),
		EventsAppended:  r.counter("arca_events_appended_total"),

		LeaseExpiries: r.counter("arca_lease_expiries_total"),
		LeasesHeld:    r.gauge("arca_leases_held"),

		DBQuerySeconds: r.histogram("arca_db_query_seconds"),
		DBConns:        r.gauge("arca_db_conns"),

		Decisions:         r.counter("arca_decisions_total"),
		AuthorizerSeconds: r.histogram("arca_authorizer_seconds"),
		TokensRejected:    r.counter("arca_tokens_rejected_total"),

		reg:     reg,
		pending: map[string]float64{},
		current: map[string]float64{},
	}
	s.RequestsInFlight.Bind(func() float64 { return float64(s.inFlight.Load()) })
	for band := range bands {
		s.SpacesByUsage.BindFor(band, s.spacesIn(band))
	}
	return s
}

// spacesIn is the source of one band, read at every scrape from the last run
// that finished. A band with no run behind it reads zero rather than nothing,
// so the series exists before the first reconciliation.
func (s *Set) spacesIn(band string) func() float64 {
	return func() float64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.current[band]
	}
}

// RequestStarted records that one request has begun.
func (s *Set) RequestStarted() { s.inFlight.Add(1) }

// RequestFinished records one answered request: the route it matched, the
// class of its status, the error code it carried or ok, and how long it
// took. The route is the mux pattern and never the path.
func (s *Set) RequestFinished(route, statusClass, code string, took time.Duration) {
	s.inFlight.Add(-1)
	// The duration is labeled by the route alone, so there is no
	// vocabulary that could refuse it and a request whose status or code
	// this table does not name was still served in some amount of time.
	s.RequestDuration.Observe(map[string]string{"route": route}, took.Seconds())
	if !slices.Contains(statusClasses, statusClass) || !slices.Contains(errorCodes, code) {
		return
	}
	s.Requests.Inc(map[string]string{"route": route, "status_class": statusClass, "code": code})
}

// TokenRejected records one bearer the verifier refused. A reason outside
// spec 018's table is recorded as malformed: the vocabulary is closed, and a
// row a shared package adds later must not open a series per refusal.
func (s *Set) TokenRejected(reason string) {
	if !slices.Contains(rejectionReasons, reason) {
		reason = "malformed"
	}
	s.TokensRejected.Inc(map[string]string{"reason": reason})
}

// Decided records one authorization decision: which of the two answered and
// what it answered.
func (s *Set) Decided(source, outcome string) {
	if !slices.Contains(decisionSources, source) || !slices.Contains(decisionOutcomes, outcome) {
		return
	}
	s.Decisions.Inc(map[string]string{"source": source, "outcome": outcome})
}

// AuthorizerCall records one call to the operator's endpoint and how long it
// took. The result is already counted as a decision, so only the duration is
// read here; the owner policy decides in process, makes no call, and records
// nothing.
func (s *Set) AuthorizerCall(_ string, seconds float64) {
	s.AuthorizerSeconds.Observe(nil, seconds)
}

// In records bytes accepted into a space, by spec 018's vocabulary: inline,
// part, or sync.
func (s *Set) In(kind string, n int64) {
	if n <= 0 || !slices.Contains(bytesInKinds, kind) {
		return
	}
	s.BytesIn.Add(map[string]string{"kind": kind}, uint64(n))
}

// Out records bytes served out of a space: streamed inline, or handed out as
// the presigned URLs of a materialize.
func (s *Set) Out(kind string, n int64) {
	if n <= 0 || !slices.Contains(bytesOutKinds, kind) {
		return
	}
	s.BytesOut.Add(map[string]string{"kind": kind}, uint64(n))
}

// Presigned records one URL signed for a client to use against the bucket.
func (s *Set) Presigned(method, kind string) {
	if !slices.Contains(presignMethods, method) || !slices.Contains(presignKinds, kind) {
		return
	}
	s.PresignedURLs.Inc(map[string]string{"method": method, "kind": kind})
}

// LeaseExpired records n writer leases ended because their deadline passed.
func (s *Set) LeaseExpired(n int) {
	if n > 0 {
		s.LeaseExpiries.Add(nil, uint64(n))
	}
}

// EventAppended records one row appended to the log of spec 010.
func (s *Set) EventAppended(kind string) {
	if !slices.Contains(eventKinds, kind) {
		return
	}
	s.EventsAppended.Inc(map[string]string{"kind": kind})
}

// LimitRejected records one write refused against the limit an authorizer's
// answer carried. Arca stores no limit, so the count of refusals is the only
// thing about one it can publish.
func (s *Set) LimitRejected() { s.LimitRejections.Inc(nil) }

// The three seams spec 007 records through when it lands. They are methods
// rather than the bare handles above for the reason every other method here
// is one: a caller reaching a counter directly would write a label this
// table does not name, and a closed vocabulary that any caller can add to is
// not closed.

// UploadSession records one session by what became of it: created,
// completed, aborted or expired.
func (s *Set) UploadSession(outcome string) {
	if !slices.Contains(sessionOutcomes, outcome) {
		return
	}
	s.UploadSessions.Inc(map[string]string{"outcome": outcome})
}

// UploadPart records one part by what became of it: presigned, completed or
// missing.
func (s *Set) UploadPart(outcome string) {
	if !slices.Contains(partOutcomes, outcome) {
		return
	}
	s.UploadParts.Inc(map[string]string{"outcome": outcome})
}

// SessionsOpen binds the gauge of sessions started and not yet finished to
// the count spec 007 keeps. Until that spec lands the gauge reads zero,
// which is what an installation with no upload route holds.
func (s *Set) SessionsOpen(open func() float64) { s.UploadSessionsOpen.Bind(open) }

// Handler answers this set's exposition in the Prometheus text format. It is
// GET /metrics of spec 002, mounted on the internal listener and on no other:
// the series say what an installation holds and how much of it is used, and a
// scrape endpoint is for the cluster that runs the replica rather than for
// the replica's clients.
func (s *Set) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.reg.WritePrometheus(w)
	})
}
