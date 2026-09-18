// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/metrics"
)

// TestMetricsListenerOnly is criterion 4 of spec 018: the scrape endpoint is
// on the internal listener, it carries every name of the table, and the
// public listener answers it as a route that does not exist.
func TestMetricsListenerOnly(t *testing.T) {
	publicURL, internalURL, _, stop := startServe(t)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()

	code, body := get(t, internalURL+"/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics on the internal listener = %d %q", code, body)
	}
	for _, name := range metrics.Names() {
		if !strings.Contains(body, name) {
			t.Errorf("the exposition does not carry %s", name)
		}
	}
	if code, _ := get(t, publicURL+"/metrics"); code != 404 {
		t.Errorf("GET /metrics on the public listener = %d, and the series are not a client's to read", code)
	}
	// The probes keep answering on the same listener: the scrape endpoint is
	// a more specific pattern under the catch-all the probes are mounted on.
	if code, body := get(t, internalURL+"/livez"); code != 200 || body != "ok\n" {
		t.Errorf("GET /livez on the internal listener = %d %q", code, body)
	}
}

// TestNoExporterStillServes is criterion 10: with
// ARCA_OTEL_EXPORTER_OTLP_ENDPOINT unset the process starts, serves, and
// exports nothing, and /metrics still carries the table. It is the shape
// every self-hoster without a collector runs.
func TestNoExporterStillServes(t *testing.T) {
	endpoint := setenv
	t.Cleanup(func() { setenv = endpoint })
	handed := false
	setenv = func(string, string) error { handed = true; return nil }

	_, internalURL, _, stop := startServe(t)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()
	if handed {
		t.Error("an endpoint was handed to the exporter while the variable was unset")
	}
	if code, body := get(t, internalURL+"/metrics"); code != 200 || !strings.Contains(body, "arca_requests_total") {
		t.Errorf("GET /metrics = %d, and the table is served whether or not anything is exported", code)
	}
}

// TestTheEndpointVariableReachesTheSharedPackage is the one translation spec
// 018 names: spec 002's table owns every variable the server reads under one
// prefix, and pkg/otel reads the standard name from the process environment,
// so the value is handed over at start-up and nowhere else.
func TestTheEndpointVariableReachesTheSharedPackage(t *testing.T) {
	endpoint := setenv
	t.Cleanup(func() { setenv = endpoint })
	var name, value string
	setenv = func(k, v string) error { name, value = k, v; return nil }

	_, flush := observe(t.Context(), config.Config{OTelEndpoint: "http://collector.invalid:4318"}, &bytes.Buffer{})
	defer func() { _ = flush(context.WithoutCancel(t.Context())) }()

	if name != "OTEL_EXPORTER_OTLP_ENDPOINT" {
		t.Errorf("the endpoint was handed over as %q", name)
	}
	if value != "http://collector.invalid:4318" {
		t.Errorf("the endpoint was handed over as %q", value)
	}
}

// TestAnEndpointThatCannotBeHandedOverStillServes: the exporter is the one
// part of the process whose failure is not a reason to refuse to serve. The
// replica keeps every local signal and the line says what was lost.
func TestAnEndpointThatCannotBeHandedOverStillServes(t *testing.T) {
	endpoint := setenv
	t.Cleanup(func() { setenv = endpoint })
	setenv = func(string, string) error { return errors.New("the environment is read only") }

	var stderr bytes.Buffer
	recorder, flush := observe(t.Context(), config.Config{OTelEndpoint: "http://collector.invalid:4318"}, &stderr)
	defer func() { _ = flush(context.WithoutCancel(t.Context())) }()
	if recorder == nil {
		t.Fatal("no recording surface was built")
	}
	if !strings.Contains(stderr.String(), "ARCA_OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Errorf("stderr is %q and does not name the variable that was lost", stderr.String())
	}
}

// TestTheReaperProcessBootstrapsToo: a reconciler running as a process of
// its own opens no listener, so its trace and its lines are the whole of
// what leaves it, and a process that logged through a different path would
// carry no trace id.
func TestTheReaperProcessBootstrapsToo(t *testing.T) {
	endpoint := setenv
	t.Cleanup(func() { setenv = endpoint })
	handed := ""
	setenv = func(_, v string) error { handed = v; return nil }

	var out, errOut bytes.Buffer
	code := run(t.Context(), []string{"reap", "-once"}, stores(t, map[string]string{
		"ARCA_OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.invalid:4318",
	}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d; the unit tier has no database behind it", code)
	}
	if handed != "http://collector.invalid:4318" {
		t.Errorf("the reap process handed over %q", handed)
	}
}

// TestAGaugeReadsTheStoreAndNeverHangs: the two gauges of spec 018 that no
// process keeps a number for are read at scrape time, with no context of
// their own. Each read gets a bounded one, and a store that will not answer
// reads as zero rather than as a scrape that waits for it.
func TestAGaugeReadsTheStoreAndNeverHangs(t *testing.T) {
	counted := sampling(t.Context(), "arca_leases_held", func(context.Context, time.Time) (int64, error) {
		return 7, nil
	})
	if got := counted(); got != 7 {
		t.Errorf("a gauge over a store holding seven read %v", got)
	}
	// The query is given a deadline of its own, so a store that never
	// answers ends the read rather than the scrape.
	var budget time.Duration
	deadlined := sampling(t.Context(), "arca_upload_sessions_open", func(ctx context.Context, _ time.Time) (int64, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("a gauge was read on a context with no deadline")
		}
		budget = time.Until(deadline)
		return 1, nil
	})
	if got := deadlined(); got != 1 {
		t.Errorf("a gauge read %v", got)
	}
	if budget <= 0 || budget > gaugeBudget {
		t.Errorf("the gauge's budget is %s, and the spec's is %s", budget, gaugeBudget)
	}

	unread := sampling(t.Context(), "arca_leases_held", func(context.Context, time.Time) (int64, error) {
		return 0, errors.New("the database is unreachable")
	})
	if got := unread(); got != 0 {
		t.Errorf("a gauge whose query failed read %v", got)
	}
}

// TestAReplicaWithNoDatabaseStillServesBothStoreGauges: the two gauges whose
// source is a query are on the endpoint whatever the store answers. This
// replica's database is unreachable, so each reads zero with a line naming
// it, and the series exists for a dashboard from the first scrape rather than
// from the first successful query.
func TestAReplicaWithNoDatabaseStillServesBothStoreGauges(t *testing.T) {
	_, internalURL, _, stop := startServe(t)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()
	code, body := get(t, internalURL+"/metrics")
	if code != 200 {
		t.Fatalf("GET /metrics = %d", code)
	}
	for _, want := range []string{"arca_leases_held 0", "arca_upload_sessions_open 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not carry %q", want)
		}
	}
}
