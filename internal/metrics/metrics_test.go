// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/reaper"
)

// expose registers the table on a fresh registry and answers the Prometheus
// text it writes, which is what a scrape reads.
func expose(t *testing.T, record func(*Set)) string {
	t.Helper()
	reg := pkgmetrics.NewRegistry()
	s := Register(reg)
	if record != nil {
		record(s)
	}
	var out strings.Builder
	reg.WritePrometheus(&out)
	return out.String()
}

// TestRegisterWithoutARegistryKeepsItsOwn: a recording package under test
// asserts on behaviour and not on exposition, so it builds the surface with
// no registry and records into one of this package's own.
func TestRegisterWithoutARegistryKeepsItsOwn(t *testing.T) {
	s := Register(nil)
	s.RequestStarted()
	s.RequestFinished("/v1/events", "2xx", "ok", 3*time.Millisecond)
	if got := s.Requests.Value(map[string]string{"route": "/v1/events", "status_class": "2xx", "code": "ok"}); got != 1 {
		t.Errorf("the request was counted %d times", got)
	}
}

// TestTheRequestGaugeRisesAndFalls holds arca_requests_in_flight to the
// requests that started and have not been answered.
func TestTheRequestGaugeRisesAndFalls(t *testing.T) {
	reg := pkgmetrics.NewRegistry()
	s := Register(reg)
	s.RequestStarted()
	s.RequestStarted()
	if got := s.inFlight.Load(); got != 2 {
		t.Fatalf("%d requests in flight", got)
	}
	var out strings.Builder
	reg.WritePrometheus(&out)
	if !strings.Contains(out.String(), "arca_requests_in_flight 2") {
		t.Error("the gauge does not report the two requests in flight")
	}
	s.RequestFinished("/v1/events", "2xx", "ok", time.Millisecond)
	s.RequestFinished("/v1/events", "5xx", "internal", time.Millisecond)
	if got := s.inFlight.Load(); got != 0 {
		t.Errorf("%d requests in flight after both were answered", got)
	}
}

// TestALabelOutsideItsVocabularyIsNotRecorded is what keeps a series from
// growing: every recording method checks what it is handed, and a value the
// table does not name opens nothing.
func TestALabelOutsideItsVocabularyIsNotRecorded(t *testing.T) {
	text := expose(t, func(s *Set) {
		s.RequestFinished("/v1/files/{path...}", "6xx", "ok", time.Millisecond)
		s.RequestFinished("/v1/files/{path...}", "2xx", "teapot", time.Millisecond)
		s.Decided("nobody", "allow")
		s.Decided("authorizer", "maybe")
		s.Presigned("patch", "download")
		s.Presigned("get", "thumbnail")
		s.In("telepathy", 10)
		s.Out("telepathy", 10)
		s.EventAppended("rewritten")
		s.Finding("teleported", reaper.Found, 1)
		s.Finding(reaper.KindMissingBytes, "ignored", 1)
	})
	if strings.Contains(text, "6xx") || strings.Contains(text, "teapot") ||
		strings.Contains(text, "nobody") || strings.Contains(text, "maybe") ||
		strings.Contains(text, "patch") || strings.Contains(text, "thumbnail") ||
		strings.Contains(text, "telepathy") || strings.Contains(text, "rewritten") ||
		strings.Contains(text, "teleported") || strings.Contains(text, "ignored") {
		t.Error("a value outside a closed vocabulary reached the exposition")
	}
	if strings.Contains(text, `route="/v1/files/{path...}"`) {
		t.Error("a refused recording still opened its route series")
	}
}

// TestZeroAndNegativeAreNotRecorded: a byte count of nothing and a finding
// of nothing are not events, and counting them would put a series in the
// exposition that says something happened.
func TestZeroAndNegativeAreNotRecorded(t *testing.T) {
	s := Register(nil)
	s.In("sync", 0)
	s.Out("materialize", -1)
	s.LeaseExpired(0)
	s.Finding(reaper.KindTrashPurged, reaper.Found, 0)
	if got := s.BytesIn.Value(map[string]string{"kind": "sync"}); got != 0 {
		t.Errorf("bytes in reads %d", got)
	}
	if got := s.BytesOut.Value(map[string]string{"kind": "materialize"}); got != 0 {
		t.Errorf("bytes out reads %d", got)
	}
	if got := s.LeaseExpiries.Value(nil); got != 0 {
		t.Errorf("lease expiries reads %d", got)
	}
}

// TestTheRecordingSurfaceWritesWhereTheTableSaysIt walks every method of the
// surface once and reads back the series its row names.
func TestTheRecordingSurfaceWritesWhereTheTableSaysIt(t *testing.T) {
	s := Register(nil)
	s.In("sync", 1024)
	s.Out("materialize", 2048)
	s.Presigned("get", "download")
	s.Presigned("put", "part")
	s.LeaseExpired(3)
	s.EventAppended("sync")
	s.LimitRejected()
	s.TokenRejected("expired")
	s.TokenRejected("a reason the shared table added later")
	s.Decided("owner_policy", "allow")
	s.AuthorizerCall("allow", 0.02)

	for _, c := range []struct {
		name   string
		read   uint64
		expect uint64
	}{
		{"bytes in", s.BytesIn.Value(map[string]string{"kind": "sync"}), 1024},
		{"bytes out", s.BytesOut.Value(map[string]string{"kind": "materialize"}), 2048},
		{"presigned download", s.PresignedURLs.Value(map[string]string{"method": "get", "kind": "download"}), 1},
		{"presigned part", s.PresignedURLs.Value(map[string]string{"method": "put", "kind": "part"}), 1},
		{"lease expiries", s.LeaseExpiries.Value(nil), 3},
		{"events appended", s.EventsAppended.Value(map[string]string{"kind": "sync"}), 1},
		{"limit rejections", s.LimitRejections.Value(nil), 1},
		{"expired token", s.TokensRejected.Value(map[string]string{"reason": "expired"}), 1},
		{"unknown reason as malformed", s.TokensRejected.Value(map[string]string{"reason": "malformed"}), 1},
		{"owner policy allow", s.Decisions.Value(map[string]string{"source": "owner_policy", "outcome": "allow"}), 1},
	} {
		if c.read != c.expect {
			t.Errorf("%s reads %d, and %d was recorded", c.name, c.read, c.expect)
		}
	}
	if got := s.AuthorizerSeconds.Count(nil); got != 1 {
		t.Errorf("the authorizer histogram holds %d observations", got)
	}
}

// TestUsageSamplingIsAggregate is criterion 3 of spec 018: a run over spaces
// of known sizes fills the distribution and the cumulative bands, the bands
// are replaced by the next run rather than added to, and no series carries a
// space.
func TestUsageSamplingIsAggregate(t *testing.T) {
	reg := pkgmetrics.NewRegistry()
	s := Register(reg)

	// Five spaces: two under a gigabyte, one at 5 GiB, one at 50 GiB, one at
	// 500 GiB. The last clears all three bands.
	run(s, 1<<20, 512<<20, 5<<30, 50<<30, 500<<30)
	for band, want := range map[string]float64{"1g": 3, "10g": 2, "100g": 1} {
		if got := s.current[band]; got != want {
			t.Errorf("band %s counts %g spaces and %g clear it", band, got, want)
		}
	}
	if got := s.SpaceUsage.Count(nil); got != 5 {
		t.Errorf("the distribution holds %d observations of five spaces", got)
	}

	// A second run over a smaller installation. The bands report what this
	// run saw and not the sum of the two.
	run(s, 2<<30)
	for band, want := range map[string]float64{"1g": 1, "10g": 0, "100g": 0} {
		if got := s.current[band]; got != want {
			t.Errorf("after the second run band %s counts %g and %g clear it", band, got, want)
		}
	}

	var out strings.Builder
	reg.WritePrometheus(&out)
	text := out.String()
	if strings.Contains(text, "space=") || strings.Contains(text, "owner=") || strings.Contains(text, "subject=") {
		t.Error("a usage series names a space")
	}
	// Six spaces over two runs and still three band series, which is what
	// "adds no series as the space count grows" means.
	if got := strings.Count(text, "arca_spaces_by_usage{"); got != 3 {
		t.Errorf("%d band series exist after two runs over six spaces", got)
	}
}

// run is one reconciliation as the reaper drives the seam: a sample per
// space, then the run itself.
func run(s *Set, sizes ...int64) {
	for _, b := range sizes {
		s.Usage(b)
	}
	s.Run(true, time.Second)
}

// TestARunRecordsItsOutcomeAndDuration holds the two rows of the table a
// reconciliation fills beside its findings.
func TestARunRecordsItsOutcomeAndDuration(t *testing.T) {
	s := Register(nil)
	s.Run(true, 2*time.Second)
	s.Run(false, 30*time.Second)
	s.Finding(reaper.KindMissingBytes, reaper.Found, 2)
	s.Finding(reaper.KindLeaseExpired, reaper.Repaired, 1)
	if got := s.ReaperRuns.Value(map[string]string{"outcome": "ok"}); got != 1 {
		t.Errorf("%d runs succeeded", got)
	}
	if got := s.ReaperRuns.Value(map[string]string{"outcome": "error"}); got != 1 {
		t.Errorf("%d runs failed", got)
	}
	if got := s.ReaperDuration.Count(nil); got != 2 {
		t.Errorf("the duration histogram holds %d observations", got)
	}
	if got := s.ReaperFindings.Value(map[string]string{"kind": "missing_bytes", "action": "found"}); got != 2 {
		t.Errorf("missing_bytes found reads %d", got)
	}
	if got := s.ReaperFindings.Value(map[string]string{"kind": "lease_expired", "action": "repaired"}); got != 1 {
		t.Errorf("lease_expired repaired reads %d", got)
	}
}

// TestSetIsEverySeamTheOtherPackagesDeclare: one object implements the
// reaper's seam, so the node binds the recording surface once.
func TestSetIsEverySeamTheOtherPackagesDeclare(t *testing.T) {
	var _ reaper.Metrics = Register(nil)
}

// TestTheBucketDecoratorCountsEveryCall walks every method of the store
// through the decorator and holds the ops, the timings, and the presigned
// URLs to spec 018's table.
func TestTheBucketDecoratorCountsEveryCall(t *testing.T) {
	s := Register(nil)
	inner := blob.NewMemory()
	store := s.Bucket(inner)
	ctx := context.Background()

	if _, err := store.Put(ctx, "k", strings.NewReader("hello"), 5, blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	body, _, err := store.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
	if _, err := store.Head(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, "", "", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PresignGet(ctx, "k", blob.PresignOptions{}); err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateMultipart(ctx, "m", blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PresignPart(ctx, "m", id, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortMultipart(ctx, "m", id); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteMany(ctx, []string{"k"}); err != nil {
		t.Fatal(err)
	}
	if err := store.HeadBucket(ctx); err != nil {
		t.Fatal(err)
	}

	for op, want := range map[string]uint64{
		"put": 1, "get": 1, "head": 1, "list": 1, "presign": 2,
		"multipart_create": 1, "multipart_abort": 1, "delete": 2,
	} {
		if got := s.BucketOps.Value(map[string]string{"op": op, "result": "ok"}); got != want {
			t.Errorf("%s reads %d calls and %d were made", op, got, want)
		}
		if got := s.BucketOpSeconds.Count(map[string]string{"op": op}); got == 0 {
			t.Errorf("%s was not timed", op)
		}
	}
	if got := s.PresignedURLs.Value(map[string]string{"method": "get", "kind": "download"}); got != 1 {
		t.Errorf("%d downloads were signed", got)
	}
	if got := s.PresignedURLs.Value(map[string]string{"method": "put", "kind": "part"}); got != 1 {
		t.Errorf("%d parts were signed", got)
	}
	// HeadBucket is the cluster's probe and not a request's call, so it is
	// timed by nothing and counted by nothing.
	if got := s.BucketOps.Value(map[string]string{"op": "head", "result": "ok"}); got != 1 {
		t.Errorf("the readiness probe was counted as a head: %d", got)
	}
}

// TestTheBucketDecoratorClassifiesEveryRefusal holds the result label to the
// three outcomes a caller branches on, plus the catch-all an alert reads.
func TestTheBucketDecoratorClassifiesEveryRefusal(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		err    error
		result string
	}{
		{"a key the store does not hold", blob.ErrNotFound, "not_found"},
		{"a put onto a key that exists", blob.ErrPreconditionFailed, "exists"},
		{"a store that is down", errors.New("connection refused"), "error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Register(nil)
			counting := blob.NewCounting(blob.NewMemory())
			counting.FailNth(blob.MethodGet, 1, c.err)
			store := s.Bucket(counting)
			if _, _, err := store.Get(ctx, "k"); err == nil {
				t.Fatal("the call succeeded")
			}
			if got := s.BucketOps.Value(map[string]string{"op": "get", "result": c.result}); got != 1 {
				t.Errorf("the call was not counted as %s", c.result)
			}
		})
	}
}

// TestTheDecoratorPassesTheStoreThroughUntouched: the object ACL has no row
// in spec 018's op vocabulary, and a call outside a closed vocabulary is not
// recorded rather than recorded under a name nobody can alert on.
func TestTheDecoratorPassesTheStoreThroughUntouched(t *testing.T) {
	s := Register(nil)
	counting := blob.NewCounting(blob.NewMemory())
	counting.FailNth(blob.MethodCompleteMultipart, 1, errors.New("the store lost the upload"))
	store := s.Bucket(counting)
	ctx := context.Background()
	if _, err := store.Put(ctx, "k", strings.NewReader("x"), 1, blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	err := store.SetPublic(ctx, "k", true)
	if err != nil && !errors.Is(err, blob.ErrNotSupported) {
		t.Fatal(err)
	}
	if got := counting.Calls(blob.MethodSetPublic); got != 1 {
		t.Errorf("the ACL call reached the store %d times", got)
	}
	if got := s.BucketOps.Value(map[string]string{"op": "set_public", "result": "ok"}); got != 0 {
		t.Errorf("the ACL opened an op series the table does not name: %d", got)
	}
	if _, err := store.CompleteMultipart(ctx, "m", "nope", nil); err == nil {
		t.Error("a completion the store refused was answered as a success")
	}
	if got := s.BucketOps.Value(map[string]string{"op": "multipart_complete", "result": "error"}); got != 1 {
		t.Errorf("the refused completion reads %d", got)
	}
}

// TestAGaugeValueOutsideItsVocabularyPanics: binding is wiring and runs at
// start-up, so a name that does not exist is a start-up panic rather than a
// series nobody notices is missing.
func TestAGaugeValueOutsideItsVocabularyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("binding a band outside the vocabulary was accepted")
		}
	}()
	Register(nil).SpacesByUsage.BindFor("1t", func() float64 { return 0 })
}

// TestAnUnboundGaugeReadsZero: a gauge whose source has not been built yet
// reports a series at zero, so a dashboard and an alert see the name from
// the first scrape.
func TestAnUnboundGaugeReadsZero(t *testing.T) {
	text := expose(t, nil)
	for _, want := range []string{
		`arca_leases_held 0`,
		`arca_upload_sessions_open 0`,
		`arca_db_conns{state="in_use"} 0`,
		`arca_stored_bytes{plane="files"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition does not carry %q", want)
		}
	}
	// An unlabelled gauge with a source reports it.
	s := Register(nil)
	s.LeasesHeld.Bind(func() float64 { return 7 })
	if got := s.LeasesHeld.collect(); len(got) != 1 || got[0].Value != 7 {
		t.Errorf("the bound gauge reports %v", got)
	}
}

// TestTheRegistrarRefusesAHandleTheTableDoesNotDefine: the fields of Set and
// the rows of the table are held together by a lookup that panics, so a
// renamed row is a start-up failure and never a handle nothing writes to.
func TestTheRegistrarRefusesAHandleTheTableDoesNotDefine(t *testing.T) {
	r := register(pkgmetrics.NewRegistry())
	for _, c := range []struct {
		name string
		call func()
	}{
		{"counter", func() { r.counter("arca_nowhere_total") }},
		{"histogram", func() { r.histogram("arca_nowhere_seconds") }},
		{"gauge", func() { r.gauge("arca_nowhere") }},
		{"a counter asked for as a gauge", func() { r.gauge("arca_requests_total") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("the lookup answered a name the table does not define")
				}
			}()
			c.call()
		})
	}
}

// TestCombinationsSeedsClosedVocabulariesAndNothingElse is the rule behind
// criterion 1: a metric with no label has one series, a metric with closed
// vocabularies has the product of them, and a metric with an open one has
// none until it is recorded.
func TestCombinationsSeedsClosedVocabulariesAndNothingElse(t *testing.T) {
	if got := combinations(nil); len(got) != 1 || got[0] != nil {
		t.Errorf("an unlabelled metric seeds %v", got)
	}
	got := combinations([]Label{
		{Name: "method", Values: []string{"get", "put"}},
		{Name: "kind", Values: []string{"download", "part"}},
	})
	if len(got) != 4 {
		t.Errorf("two vocabularies of two seed %d series", len(got))
	}
	if combinations([]Label{{Name: "route"}}) != nil {
		t.Error("an open vocabulary seeded a series")
	}
	if combinations([]Label{{Name: "op", Values: []string{"get"}}, {Name: "route"}}) != nil {
		t.Error("a metric with one open vocabulary seeded a series")
	}
}

// TestTheAccessorsAnswerCopies: a caller that sorted or trimmed what these
// answer must not be editing the table every scrape reads.
func TestTheAccessorsAnswerCopies(t *testing.T) {
	for _, c := range []struct {
		name string
		read func() []string
	}{
		{"the finding kinds", FindingKinds},
		{"the event kinds", EventKinds},
		{"the error codes", ErrorCodes},
		{"the names", Names},
	} {
		first := c.read()
		if len(first) == 0 {
			t.Fatalf("%s is empty", c.name)
		}
		slices.Reverse(first)
		if slices.Equal(first, c.read()) {
			t.Errorf("%s answered the table itself", c.name)
		}
	}
	rows := Table()
	rows[0].Name = "changed"
	if Table()[0].Name == "changed" {
		t.Error("Table answered the table itself")
	}
}
