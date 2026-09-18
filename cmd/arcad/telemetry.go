// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/metrics"
	"latere.ai/x/arca/internal/version"
)

// The exporter of spec 018.
//
// Every role of the binary bootstraps: serve and reap both, because a
// reconciler running as a process of its own is where an operator most needs
// the trace and the line, and because a process that logged through a
// different path would carry no trace id.

// ServiceName labels every span, metric and log record this binary emits.
const ServiceName = "arcad"

// otelEndpointVariable is the name pkg/otel reads the endpoint from.
// ARCA_OTEL_EXPORTER_OTLP_ENDPOINT is the name spec 002's table owns, and
// this is where the one becomes the other: the table stays one prefix an
// operator sets and one map a test passes, and the shared package keeps
// reading the standard name a collector's operator injects.
const otelEndpointVariable = "OTEL_EXPORTER_OTLP_ENDPOINT"

// setenv is how the value reaches the shared package, which reads the
// process environment rather than a field. It is a variable so a test drives
// the translation without writing to the environment of the test binary.
var setenv = os.Setenv

// observe wires the logger, the tracer and the exporter, registers the metric
// table, and answers the recording surface with the flush the process runs at
// the end.
//
// With no endpoint configured nothing is exported: spans are still created
// and discarded, the logger is the JSON handler the process had before, and
// /metrics still serves, so a self-hoster with no collector loses no local
// signal (spec 018).
//
// The local handler writes to standard error, which is where spec 018 puts
// the JSON lines: the start-up lines an operator reads are on standard
// output and stay a plain sentence each.
func observe(ctx context.Context, cfg config.Config, stderr io.Writer) (*metrics.Set, func(context.Context) error) {
	// The handover happens before the bootstrap, because the shared package
	// reads the variable as it builds the exporters. A failure is reported
	// after it, through the logger the bootstrap returns, so the line lands
	// on the same handler as every other line of the process.
	var handover error
	if cfg.OTelEndpoint != "" {
		handover = setenv(otelEndpointVariable, cfg.OTelEndpoint)
	}
	logger, flush, err := otel.Bootstrap(ctx, otel.Config{
		ServiceName: ServiceName,
		Version:     version.Version,
		Stdout:      slog.NewJSONHandler(stderr, nil),
	})
	if handover != nil {
		// The endpoint could not be handed over, so nothing will be
		// exported. It is not a reason to refuse to serve: the process keeps
		// every local signal, and the line says what was lost.
		logger.WarnContext(ctx, "telemetry: the endpoint was not handed to the exporter",
			"variable", "ARCA_OTEL_EXPORTER_OTLP_ENDPOINT", "error", handover)
	}
	if err != nil {
		// The local handler is always usable; only the OTLP log bridge
		// failed, and the process serves without it.
		logger.WarnContext(ctx, "telemetry: the log bridge did not start", "error", err)
	}
	// The one registry of spec 018, walked over that spec's table so every
	// name is on the endpoint from the first scrape. Every recording package
	// takes its handle from here, and no package registers a metric of its
	// own.
	return metrics.Register(nil), flush
}

// gaugeBudget is how long a gauge's query may take. It is the readiness
// probe's budget: a store that cannot count a row in two seconds is one this
// replica is already failing readiness on, and a scrape must not be the call
// that waits for it.
const gaugeBudget = 2 * time.Second

// sampling turns a counting query into the source of a gauge.
//
// A gauge is read at scrape time and carries no context of its own, so each
// read derives one from the process's, bounded by the budget above. A store
// that will not answer reads as zero with a line saying which gauge went
// unread: the series says what the installation holds, and a scrape is not
// the place a store outage is reported from. That is readiness', and the
// reader of this endpoint sees it there.
func sampling(parent context.Context, gauge string,
	count func(context.Context, time.Time) (int64, error),
) func() float64 {
	return func() float64 {
		ctx, cancel := context.WithTimeout(parent, gaugeBudget)
		defer cancel()
		n, err := count(ctx, time.Now())
		if err != nil {
			slog.WarnContext(ctx, "telemetry: a gauge went unread", "gauge", gauge, "error", err)
			return 0
		}
		return float64(n)
	}
}
