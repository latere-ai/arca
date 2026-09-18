// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"strings"
)

// The alerts of spec 018, one row per row of that spec's table.
//
// They are Go values rather than YAML, for two reasons. The committed
// manifest is then a rendering that a test holds to this table, so an alert
// cannot be edited in the file and left out of the deck. And
// TestAlertsNameKnownMetrics reads every metric and every label an
// expression names and holds them to internal/metrics, so an alert over a
// series nothing publishes fails the build rather than sitting silent in a
// cluster.

// Group is the name of the one rule group and of the object that holds it.
const Group = "arcad"

// ReapInterval is the default of ARCA_REAP_INTERVAL (spec 002). Spec 018's
// reaper alert fires at three times it, and a rules file cannot read a
// deployment's variable, so this is the window an installation edits when it
// changes the interval.
const ReapInterval = "5m"

// Alert is one row of spec 018's alert table.
type Alert struct {
	// Name is the alert as a pager shows it.
	Name string
	// Expr is the expression, in PromQL.
	Expr string
	// For is how long the expression holds before it fires. Empty fires on
	// the first evaluation, which is right for a counter that must never
	// move at all.
	For string
	// Summary is the one line whoever is woken reads first.
	Summary string
}

// alerts is spec 018's table, in that table's order.
var alerts = []Alert{
	{
		Name: "ArcaReadinessFailing",
		Expr: `kube_pod_status_ready{condition="true",pod=~"arcad-.*"} == 0`,
		For:  "5m",
		Summary: "An arcad Pod has been out of rotation for five minutes. " +
			"Read /readyz on the Pod: it names the check that failed.",
	},
	{
		Name: "ArcaErrorRate",
		Expr: `sum(rate(arca_requests_total{status_class="5xx"}[5m])) / sum(rate(arca_requests_total[5m])) > 0.01`,
		For:  "5m",
		Summary: "More than one request in a hundred is failing with a server error. " +
			"The route label says which surface.",
	},
	{
		Name:    "ArcaSlowRequests",
		Expr:    `histogram_quantile(0.95, sum by (le, route) (rate(arca_request_duration_seconds_bucket[5m]))) > 2`,
		For:     "10m",
		Summary: "A route is above two seconds at the 95th percentile.",
	},
	{
		Name: "ArcaBucketErrors",
		Expr: `sum(rate(arca_bucket_ops_total{result="error"}[5m])) / sum(rate(arca_bucket_ops_total[5m])) > 0.05`,
		For:  "5m",
		Summary: "More than one bucket call in twenty is failing. " +
			"A missing object is not counted here; this is the store itself.",
	},
	{
		Name: "ArcaMissingBytes",
		Expr: `increase(arca_reaper_findings_total{kind="missing_bytes"}[10m]) > 0`,
		Summary: "A row names an object the bucket does not hold. " +
			"This is invariant 2 broken, and it is the one alert that always reaches a human.",
	},
	{
		Name: "ArcaReaperFailing",
		Expr: `increase(arca_reaper_runs_total{outcome="error"}[15m]) > 0 or increase(arca_reaper_runs_total[15m]) == 0`,
		Summary: "The reconciler is failing, or has not finished a run in three intervals. " +
			"The window is three times the default ARCA_REAP_INTERVAL of five minutes; widen it if yours is longer.",
	},
	{
		Name: "ArcaOrphanGrowth",
		Expr: `increase(arca_reaper_findings_total{kind="orphan_object",action="found"}[30m]) > 0 ` +
			`and increase(arca_reaper_findings_total{kind="orphan_object",action="repaired"}[30m]) == 0`,
		For: "30m",
		Summary: "Objects nothing references are being found and none are being removed. " +
			"Bytes are accumulating that no row will ever name.",
	},
	{
		Name:    "ArcaAuthorizerUnavailable",
		Expr:    `increase(arca_decisions_total{outcome="unavailable"}[5m]) > 0`,
		Summary: "A request was refused because the authorizer did not answer. No decision is ever an allow.",
	},
	{
		Name: "ArcaWritesRefusedByLimit",
		Expr: `increase(arca_limit_rejections_total[30m]) > 0`,
		Summary: "Writes are being refused against the limit an authorizer's answer carried. " +
			"The limit is the platform's and not Arca's; the administrative overview names the space.",
	},
	{
		Name: "ArcaLedgerDrifting",
		Expr: `increase(arca_reaper_findings_total{kind="usage_corrected"}[15m]) > 0`,
		For:  "15m",
		Summary: "The usage ledger keeps needing correction, which is a write path that forgot its delta. " +
			"It is a bug and not a capacity signal.",
	},
	{
		Name: "ArcaUploadsExpiring",
		Expr: `increase(arca_upload_sessions_total{outcome="expired"}[1h]) > ` +
			`increase(arca_upload_sessions_total{outcome="completed"}[1h])`,
		For:     "1h",
		Summary: "Clients are starting more uploads than they finish.",
	},
	{
		Name: "ArcaDatabaseSaturated",
		Expr: `arca_db_conns{state="idle"} == 0 and arca_db_conns{state="in_use"} > 0`,
		For:  "5m",
		Summary: "The connection pool has held no idle connection for five minutes. " +
			"Every further query waits for one to be given back.",
	},
	{
		Name:    "ArcaReplicasPinned",
		Expr:    `kube_horizontalpodautoscaler_status_current_replicas == kube_horizontalpodautoscaler_spec_max_replicas`,
		For:     "15m",
		Summary: "The autoscaler has been at its maximum replica count for fifteen minutes.",
	},
}

// Alerts answers spec 018's table, in that table's order.
func Alerts() []Alert { return append([]Alert(nil), alerts...) }

// Document is the plain Prometheus rules document, which is what
// `promtool check rules` parses: promtool reads a rules file and not a
// Kubernetes object, so the tool prints this and the manifest wraps it.
func Document() string {
	var b strings.Builder
	b.WriteString("groups:\n")
	fmt.Fprintf(&b, "  - name: %s\n", Group)
	b.WriteString("    rules:\n")
	for _, a := range alerts {
		fmt.Fprintf(&b, "      - alert: %s\n", a.Name)
		fmt.Fprintf(&b, "        expr: %s\n", quote(a.Expr))
		if a.For != "" {
			fmt.Fprintf(&b, "        for: %s\n", a.For)
		}
		b.WriteString("        annotations:\n")
		fmt.Fprintf(&b, "          summary: %s\n", quote(a.Summary))
	}
	return b.String()
}

// quote renders a scalar as a single-quoted YAML string, which is the one
// style that needs no reading of the value: an expression carries braces,
// quotes and comparisons, and a summary carries punctuation, and neither is
// interpreted inside single quotes.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
