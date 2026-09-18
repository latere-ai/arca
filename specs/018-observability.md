---
title: "Observability: the metric table, traces across the two stores, logs with trace ids, the alert rules"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/003-object-store.md
  - specs/004-metadata-store.md
  - specs/006-identity.md
  - specs/009-workspaces.md
  - specs/010-events-and-reaper.md
  - specs/013-api.md
affects: [internal/metrics/, internal/api/, internal/blob/, internal/store/, internal/auth/, internal/events/, internal/reaper/, cmd/arcad/, tools/rules/, deploy/base/prometheusrule.yaml, .github/workflows/verify.yml]
effort: medium
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Observability

## Overview

An operator answers four questions from outside the process: is the API
serving, are bytes moving, how much is stored and by how many spaces,
and are the two stores still agreeing. Metrics on the internal listener answer
them in aggregate, traces answer them per request across the bucket and
the database, logs carry the developer detail with a trace id on every
line, and a rules file turns the metrics into the alerts an
installation starts with.

Every name in the deck is in this spec's table. A package that records
a signal takes its handle from `internal/metrics`, the one package that
registers, so a metric of a spec not yet built still exists at zero and
nothing is added ad hoc. The SDK is the family's, through
`latere.ai/x/pkg/otel` and `latere.ai/x/pkg/metrics`, and nothing
leaves the process until an operator sets an endpoint.

## Current state

Not built. `cmd/arcad` serves the probes and `/metrics` is the empty
registry of [[002-repository-scaffold]].

## Design

### The scrape surface and the exporter

`GET /metrics` is on the internal listener only
([[002-repository-scaffold]]), in the Prometheus text format, from one
registry. `arcad reap` as a process of its own opens no listener, so
its counters leave over OTLP and nothing else; an installation that
runs the reaper on the API replicas scrapes the same counters from
`/metrics`.

`cmd/arcad` calls `otel.Bootstrap` with the service name `arcad`, the
version of `internal/version`, and the endpoint from
`ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` ([[002-repository-scaffold]]'s
table). The variable carries the `ARCA_` prefix rather than the
standard `OTEL_` name because that table owns every variable the server
reads and every one is read through one lookup function, so an operator
configures one prefix and a test passes one map; `pkg/otel` receives
the value as its endpoint. Unset exports nothing: spans are still
created and discarded, and `/metrics` still serves, so a self-hoster
with no collector loses no local signal. Sampling is `pkg/otel`'s
default of 0.2, which `arcad` does not override and the conformance run
sets to always-on.

### Metrics

Labels come from closed vocabularies, with no exception. No label
carries a path, an object name, a principal's subject, a workspace
slug, or a token. `route` is the
`http.ServeMux` pattern, taken from a context value a middleware behind
the mux fills, because the route template is not known before the mux
matches. Every histogram names its bounds. A labelled counter is
registered with one zero observation per combination of its
vocabularies, so a series exists before the first event.

| Metric | Type | Labels | Owner |
|---|---|---|---|
| `arca_requests_total` | counter | `route`, `status_class` (`2xx` to `5xx`), `code` (the error code of [[013-api]], `ok` when there is none) | 013 |
| `arca_request_duration_seconds` | histogram, 5 ms to 10 s, 12 bounds | `route` | 013 |
| `arca_requests_in_flight` | gauge | | 013 |
| `arca_bytes_in_total` | counter | `kind` (`inline`, `part`, `sync`) | [[005-files]], [[007-uploads]], [[009-workspaces]] |
| `arca_bytes_out_total` | counter | `kind` (`inline`, `materialize`) | 005, 009 |
| `arca_presigned_urls_total` | counter | `method` (`get`, `put`), `kind` (`download`, `part`) | [[003-object-store]], 007 |
| `arca_upload_sessions_total` | counter | `outcome` (`created`, `completed`, `aborted`, `expired`) | 007 |
| `arca_upload_sessions_open` | gauge | | 007 |
| `arca_upload_parts_total` | counter | `outcome` (`presigned`, `completed`, `missing`) | 007 |
| `arca_space_usage_bytes` | histogram, 1 MiB to 1 TiB, 12 bounds | | [[010-events-and-reaper]] |
| `arca_spaces_by_usage` | gauge | `band` (`1g`, `10g`, `100g`) | 010 |
| `arca_stored_bytes` | gauge | `plane` (`files`, `workspaces`) | 010 |
| `arca_limit_rejections_total` | counter | | 010 |
| `arca_reaper_runs_total` | counter | `outcome` (`ok`, `error`) | 010 |
| `arca_reaper_duration_seconds` | histogram, 1 s to 30 min, 12 bounds | | 010 |
| `arca_reaper_findings_total` | counter | `kind`, `action` (`found`, `repaired`, `deferred`) | 010 |
| `arca_lease_expiries_total` | counter | | 009 |
| `arca_leases_held` | gauge | | 009 |
| `arca_events_appended_total` | counter | `kind` (the event kinds of [[010-events-and-reaper]]) | 010 |
| `arca_bucket_ops_total` | counter | `op` (`get`, `put`, `head`, `delete`, `list`, `presign`, `multipart_create`, `multipart_complete`, `multipart_abort`), `result` (`ok`, `not_found`, `exists`, `error`) | 003 |
| `arca_bucket_op_seconds` | histogram, 5 ms to 10 s, 12 bounds | `op` | 003 |
| `arca_db_query_seconds` | histogram, 1 ms to 5 s, 12 bounds | `op` | [[004-metadata-store]] |
| `arca_db_conns` | gauge | `state` (`in_use`, `idle`) | 004 |
| `arca_decisions_total` | counter | `source` (`authorizer`, `owner_policy`), `outcome` (`allow`, `deny`, `unavailable`) | [[006-identity]] |
| `arca_authorizer_seconds` | histogram, 5 ms to 10 s, 12 bounds | | 006 |
| `arca_tokens_rejected_total` | counter | `reason` (`missing`, `malformed`, `expired`, `audience`, `issuer`, `signature`) | 006 |

`arca_reaper_findings_total`'s `kind` is a closed vocabulary, one
member per thing the reconciler of
[[010-events-and-reaper]] can find: `orphan_object`,
`orphan_candidate`, `missing_bytes`, `workspace_purged`,
`file_purged`, `trash_purged`, `share_expired`, `event_pruned`,
`star_pruned`, `version_pruned`, `upload_aborted`, `usage_corrected`,
`lease_expired`.
`usage_corrected` is pass 10's finding, a space whose ledger row
disagreed with the rows holding its bytes, and a healthy installation
reports none. `missing_bytes` is
the finding of invariant 2, a row whose object the bucket does not
hold, and it is the one a human always reads.

`lease_expired` is the thirteenth member and pass 3's finding: an
attachment past its expiry whose writer lease the pass cleared. The
first draft of this table left it out, because
`arca_lease_expiries_total` counts the same event, and
[[010-events-and-reaper]] asked for it while building that pass. A pass
whose findings no run reports is a pass an operator cannot see run at
all, and a run whose table names twelve kinds while the reconciler
sweeps thirteen reads as a pass that found nothing. The two are not a
duplicate: the counter is the rate a platform alerts on, and the finding
is the row of the run's own table, beside the twelve others, that says
the pass ran.

**Usage is sampled per space and published in aggregate.** The reaper
reads the ledger once per run and, for every space, observes its bytes
into `arca_space_usage_bytes` and increments every band its size
clears, so the bands are cumulative and a space of 500 GiB counts in all
three.
There is no series per space, and the reason is [[004-metadata-store]]:
a space is addressed by the subject `<issuer>|<sub>` and there is no
second identifier, so a `space` label would be a subject on an endpoint
anyone who can scrape the namespace reads, and it would grow a series
for every principal that ever touched the installation. Hashing the
subject would hide the name and keep the growth, which buys nothing. The
three signals an operator acts on survive the aggregation: the
distribution says how the installation's bytes are spread,
`arca_spaces_by_usage{band}` says how many spaces are large and how
large, and `arca_limit_rejections_total` says whether anybody is being
refused against a limit their platform set. Which space is the large one
is a question with a name in the answer, so it is answered by
[[012-administration]]'s overview, which reads the ledger under an
administrator's token and needs no time series.

No metric here names a limit, because Arca stores none. A limit lives in
the authorizer's answer and differs per space and per answer, so the
count of refusals is the only thing about it Arca can honestly publish.

`arca_stored_bytes` is the same reaper sample summed over the
installation, split by plane, which is the number an operator bills and
capacity-plans from.

`TestMetricsTable` holds the registry to exactly this table, reading
this file through `runtime.Caller` because the `tempdir` gate runs the
suite from an empty directory.

### Traces

One span per request, named by the route pattern, with the request id,
the trace id, the subject, the space id, the plane, and the object id
as attributes. Never the path and never the object's name: a path
carries what a person called their file.

Children, in the order a write reaches them, which is invariant 1 seen
from the tracing side:

| Span | Opened by | Covers |
|---|---|---|
| `auth.verify` | `internal/auth` | the token's verification; no issuer call on the request path, so the span is the key set's cache lookup and closes in microseconds |
| `auth.ask` | `internal/auth` | the authorizer call; absent under the owner policy, and the span test configures a stub authorizer to get it |
| `bucket.<op>` | `internal/blob` | one per bucket call, beside the HTTP transport span `pkg/otel` adds, because the transport span names the method and this one names the operation |
| `db.<op>` | `internal/store` | one per query or transaction |

A put's spans therefore read `auth.verify`, `auth.ask`, `bucket.put`,
`db.insert` in that order, and a delete's read `db.delete` before
`bucket.delete`. A trace that shows the two in the other order is the
invariant broken, and the test below asserts the order rather than the
presence.

A presigned download's span ends when the redirect is written; the
transfer is between the client and the bucket and produces no span,
which is invariant 4. The reaper's run is one trace per run with a
child per finding kind, not per finding.

### Logs

`slog` through the OTel bridge, JSON on stderr, one line per request at
`INFO` carrying `route`, `method`, `status`, `code`, `duration_ms`,
`subject`, `space`, `object`, `bytes_in`, `bytes_out`, `request_id`,
and `trace_id`. The request id is not the trace id and both are on
every line of the request and on its spans, so a log line and a trace
find each other in either direction. One line at the open of a stream
and one at its close, none per part.

`WARN` for a ledger row pass 10 had to correct, a write refused against
the limit an answer carried, and a reaper finding that could not be
repaired. `ERROR` for a
row whose bytes the bucket does not hold, for a bucket or database call
that failed after its retries, and for a completion the multipart
refused.

Redaction is one handler wrapping both paths of `pkg/otel.Bootstrap`'s
tee, so the local JSON handler and the OTLP bridge see the same
attributes. It replaces the value of any key whose name is or contains
`token`, `secret`, `credential`, `signature`, `password`,
`Authorization`, or `Proxy-Authorization`, and it rewrites any value
that looks like a presigned URL, replacing everything from
`X-Amz-Signature=` to the next separator, because a presigned URL is a
credential for one object and a log is not where it belongs. The
structural rule at the call site is that no handler passes a token, a
credential, or a presigned URL as an attribute at all; the handler is
the second line and the test below plants one canary of each kind.

### Alerts

`deploy/base/prometheusrule.yaml` is a `PrometheusRule` object.
[[016-release-and-installation]] says why it sits beside the base's
kustomization rather than in it; this spec owns what is in it.
`tools/rules` prints the rules document out of the object, because
`promtool check rules` reads a rules file and not a Kubernetes object,
and a `rules` job in `verify.yml` runs `promtool check rules` over that
output. The same job runs `TestAlertsNameKnownMetrics`, which fails on
an alert naming an `arca_` metric or label absent from the table above;
a metric another exporter publishes does not carry the prefix and is
not checked.

| Alert | Expression, in words |
|---|---|
| `ArcaReadinessFailing` | `kube_pod_status_ready` false for the `arcad` Pods for 5 minutes, from kube-state-metrics |
| `ArcaErrorRate` | `arca_requests_total{status_class="5xx"}` over `arca_requests_total` above 1 percent for 5 minutes |
| `ArcaSlowRequests` | `arca_request_duration_seconds` p95 above 2 seconds for 10 minutes |
| `ArcaBucketErrors` | `arca_bucket_ops_total{result="error"}` over `arca_bucket_ops_total` above 5 percent for 5 minutes |
| `ArcaMissingBytes` | `arca_reaper_findings_total{kind="missing_bytes"}` increasing over 10 minutes; a row without its bytes is invariant 2 broken and is the one alert that always reaches a human |
| `ArcaReaperFailing` | `arca_reaper_runs_total{outcome="error"}` increasing, or no increase in `arca_reaper_runs_total` over three times `ARCA_REAP_INTERVAL` |
| `ArcaOrphanGrowth` | `arca_reaper_findings_total{kind="orphan_object",action="found"}` rising while `action="repaired"` does not, for 30 minutes |
| `ArcaAuthorizerUnavailable` | `arca_decisions_total{outcome="unavailable"}` increasing over 5 minutes |
| `ArcaWritesRefusedByLimit` | `arca_limit_rejections_total` increasing over 30 minutes; the limit is the platform's and not Arca's, so this alerts the operator that somebody's platform is refusing writes, and which space is named by [[012-administration]]'s overview and not by a label |
| `ArcaLedgerDrifting` | `arca_reaper_findings_total{kind="usage_corrected"}` increasing over three runs; a write path that forgot its delta, which is a bug and not a capacity signal |
| `ArcaUploadsExpiring` | `arca_upload_sessions_total{outcome="expired"}` above `outcome="completed"` over 1 hour, which says clients are starting uploads they cannot finish |
| `ArcaDatabaseSaturated` | `arca_db_conns{state="in_use"}` at the pool's size for 5 minutes |
| `ArcaReplicasPinned` | the autoscaler at its maximum replica count for 15 minutes, from kube-state-metrics |

### What arrives from Drive

Almost nothing, which is the point of this spec. The service exported
no metrics, registered no Prometheus registry, imported no
OpenTelemetry package, served no `PrometheusRule`, and read no `OTEL_`
variable. Its whole observable surface was `log/slog` at the call site
with no trace id and no request line.

| From the service | What it becomes |
|---|---|
| the `slog` call sites of `drive/internal/handler` (`files.go`, `uploads.go`, `quota.go`, `trash.go`, `attach.go`, `events.go`, `links.go`, `visibility.go`, `directory.go`) | the `WARN` and `ERROR` rows above. Each call already carries the context, so each gains the trace id for free once `cmd/arcad` wraps the handler; each also becomes the recording site of the counter in the table that covers it, so a failure is both a line and a number. The `orphan cleanup failed` line of `files.go` is `arca_reaper_findings_total{kind="orphan_object",action="deferred"}`, and the `blob stream aborted` lines are the `ERROR` row for a bucket call |
| `drive/internal/gc`'s `Result` and its one summary line per run | `arca_reaper_findings_total`'s `kind` vocabulary, one member per field of that struct, plus `arca_reaper_runs_total` and `arca_reaper_duration_seconds`. The service logged ten counts in one line per run and kept no series, so nothing could alert on an orphan count that stopped falling; the vocabulary is the same ten and the shape is a counter |
| `drive/internal/handler/attach.go`'s `attachment reaper released expired attachments` | `arca_lease_expiries_total` and `arca_leases_held`, the metrics behind invariant 7 |
| `drive/internal/handler/quota.go`'s `quota usage query failed; allowing write` | nothing, and that is the point. The ledger is read and written inside the write's own transaction, so a failure refuses the write ([[015-security-and-threat-model]]) and shows up as a 5xx in `arca_requests_total` rather than as a warning nobody reads |
| nothing | every metric, every span, the request log line, the redaction handler, `tools/rules`, and `deploy/base/prometheusrule.yaml` are new in Arca |

## Not in this spec

Dashboards; a platform builds those from the same metrics. Profiling
endpoints. A sampling policy beyond `pkg/otel`'s default. The event log
itself, which is a product surface and not telemetry
([[010-events-and-reaper]]). That log is also where an administrator's
actions are recorded, which makes it doubly a product surface and not
telemetry ([[012-administration]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Right after start-up, with nothing recorded, `/metrics` carries every name in the table and a zero series for every closed vocabulary | `TestMetricsTable` in `internal/metrics`, reading this file through `runtime.Caller` |
| 2 | Over a full conformance run, no label value equals a path, an object name, a subject, or a workspace slug the run used | `TestLabelsAreBounded` against the run's fixture |
| 3 | A reaper run over a fixture of spaces at known sizes yields the right histogram, and each band counts every space at or above it, and adds no series as the space count grows | `TestUsageSamplingIsAggregate` over two fixtures of different sizes |
| 4 | `/metrics` is on the internal listener and not on the public one, and `arcad reap` opens no listener while its counters still arrive over OTLP | `TestMetricsListenerOnly`, `TestReapExportsOverOTLP` with an in-memory collector |
| 5 | A put produces one parent span named by the route with `auth.verify`, `auth.ask`, `bucket.put`, and `db.insert` in that order; a delete produces `db.delete` before `bucket.delete`; a presigned download produces no span for the transfer | `TestWriteSpanOrder`, `TestDeleteSpanOrder`, `TestRedirectHasNoTransferSpan` |
| 6 | Every request log line carries the request id and the trace id, and the same ids are on the request's spans | `TestLogLineCarriesTraceIDs` |
| 7 | A canary of each kind (a bearer, a link token, a bucket secret key, a presigned URL, an `Authorization` header) appears on neither path of the tee | `TestLogsRedact` |
| 8 | One log line per request, one per stream open and close, none per part and none per presigned URL | `TestLogVolume` |
| 9 | The rules document `tools/rules` prints passes `promtool check rules`, and every `arca_` metric and label an alert names is in the table | the `rules` job of `verify.yml`, `TestAlertsNameKnownMetrics` |
| 10 | With `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` unset the process starts, serves, and exports nothing, and `/metrics` still carries the table | `TestNoExporterStillServes` |
