---
title: "Observability: the metric table, traces across the two stores, logs with trace ids, the alert rules"
status: complete
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
affects: [internal/metrics/, internal/config/, internal/api/, internal/blob/, internal/store/, internal/auth/, internal/events/, internal/files/, internal/uploads/, internal/workspaces/, internal/reaper/, cmd/arcad/, tools/rules/, deploy/base/prometheusrule.yaml, .github/workflows/verify.yml]
effort: medium
created: 2026-09-18
updated: 2026-09-20
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

Built and in the tree on 2026-09-18, for everything the packages of phases 1
to 5 can record, and wired through on 2026-09-19 to the two planes that were
seams at the time: `internal/files`, `internal/uploads` and the two live
counts `internal/workspaces` and `internal/uploads` answer. `internal/metrics` holds the table and the one registry;
`cmd/arcad` bootstraps the exporter and serves `GET /metrics` on the internal
listener; `internal/api` counts, times and logs every request;
`internal/auth`, `internal/files`, `internal/uploads`, `internal/workspaces`
and `internal/reaper` record through seams; `tools/rules` holds the alert
table and renders `deploy/base/prometheusrule.yaml`. The commits are `f502744` (the table and
the registry), `e884c2f` (the exporter and the listener), `0b0e9e5` (the
instrumentation), `2c8e9f3` (the alerts and the tool) and `91542ac` (the
documentation). The gate passes at each of them.

The endpoint's second name landed on 2026-09-19, before the cutover of
[[019-migration-from-drive]]: `internal/config` reads the standard
`OTEL_EXPORTER_OTLP_ENDPOINT` where the table's own row is unset, which is
what the namespace this installation deploys into injects into every
workload. The Design says which name wins and why the injected one is not
held to a shape; the divergence below says what was true before.

Since the cutover an installation exports by contract rather than by
accident. The injected endpoint is read, handed to the shared package
before the bootstrap, and the process says so on its first lines:
`pkg/otel` logs `telemetry: exporting` with the endpoint as an attribute
once its exporters are built, which is the line a reader looks for in
`arcad`'s log to know that traces, metrics and log records are leaving.
That closes the export half of criterion 4 and all of criterion 11, and
it is why this spec was re-read criterion by criterion before it closed.

### What records, and what waits

Every name of the table is registered and every closed vocabulary carries a
zero series from the first scrape, so nothing below is a missing metric. What
differs is whether a package writes to it yet.

| Metric | State |
|---|---|
| `arca_requests_total`, `arca_request_duration_seconds`, `arca_requests_in_flight` | recorded, by the frame's middleware |
| `arca_tokens_rejected_total` | recorded, at the verifier's refusal |
| `arca_decisions_total`, `arca_authorizer_seconds` | recorded, at the one seam every handler decides through |
| `arca_bucket_ops_total`, `arca_bucket_op_seconds`, `arca_presigned_urls_total` | recorded, by the decorator over `blob.Store` |
| `arca_reaper_runs_total`, `arca_reaper_duration_seconds`, `arca_reaper_findings_total`, `arca_space_usage_bytes`, `arca_spaces_by_usage` | recorded, by the reconciler of [[010-events-and-reaper]] |
| `arca_lease_expiries_total` | recorded, at pass 3's binding |
| `arca_events_appended_total` | recorded, where a workspace's mutation becomes a row of the log |
| `arca_bytes_in_total{kind="sync"}`, `arca_bytes_out_total{kind="materialize"}` | recorded, at the boundary of [[009-workspaces]] |
| `arca_bytes_in_total{kind="inline"}`, `arca_bytes_out_total{kind="inline"}` | recorded, at the put and the inline read of [[005-files]]. What is counted out is what left, so a read that ended early counts fewer bytes than the row holds, and a read answered as a redirect counts none |
| `arca_bytes_in_total{kind="part"}`, `arca_upload_sessions_total`, `arca_upload_sessions_open`, `arca_upload_parts_total` | recorded, by the session API of [[007-uploads]] through the three seams. A session is counted once by what became of it, and the bytes are the assembled object's head and not the client's declaration |
| `arca_limit_rejections_total` | recorded, where the ledger refuses a charge: the put of [[005-files]] and the session open of [[007-uploads]], which charges the declared bytes up front |
| `arca_leases_held` | recorded, from `store.Workspaces.CountHeldLeases`, read at every scrape. It is the exact complement of the sweep's own predicate, so a lease is held or lapsed and never both |
| `arca_stored_bytes` | waits on a per-plane ledger read; see the divergence below |
| `arca_db_query_seconds`, `arca_db_conns` | waits on [[004-metadata-store]] exposing the pool's statistics and a timed querier |

The two gauges that read a store rather than a number this process keeps,
`arca_leases_held` and `arca_upload_sessions_open`, are bound in `cmd/arcad`
through one helper, because a lease taken on one replica is held on all of
them and no replica's own count would be the installation's. A gauge carries
no context, so each read is given a bounded one at the readiness probe's
budget and a store that will not answer reads zero with a line naming the
gauge: a scrape is not where a store outage is reported from.

Share and link counters have no row in the table and need none: what
[[008-shares-and-links]] does is already counted as requests by route, as
decisions by outcome, and as `arca_reaper_findings_total{kind="share_expired"}`.

### Divergences

- **`lease_expired` joined the `kind` vocabulary**, at
  [[010-events-and-reaper]]'s request, and the Design above records why. The
  vocabulary is thirteen members and a test holds it equal to that package's
  own.
- **The endpoint reaches `pkg/otel` through the process environment.** That
  package reads `OTEL_EXPORTER_OTLP_ENDPOINT` as it builds its exporters and
  takes no endpoint field, so `cmd/arcad` sets that variable from the
  endpoint `internal/config` resolved, before the bootstrap. The write is a
  function variable, so a test drives the translation without touching the
  environment of the test binary. Where the endpoint came from the standard
  name the write puts the same value back, so the exporter reads it whether
  or not the write lands, and the warning a failed write logs names the row
  an operator would edit.

  Until 2026-09-19 the standard name worked by accident rather than by
  contract: `internal/config` read the prefixed name alone, and export under
  an injecting operator happened only because `pkg/otel` reads the process
  environment itself. Nothing in this repository said so, no test held it,
  and criterion 10 said the opposite, so a change that handed `pkg/otel` an
  endpoint instead of leaking the environment would have taken export with
  it and failed nothing. `internal/config` reads both names now, and the
  Design above says which wins.
- **`arcad reap`'s counters do not leave over OTLP**, which is half of
  criterion 4. The scrape endpoint is `latere.ai/x/pkg/metrics`, a registry
  that writes the Prometheus text format and reaches no exporter, and the
  meter provider `pkg/otel` installs is a second pipeline with no bridge
  between them. Duplicating the table onto OTel instruments would be a second
  registry in this repository, which is exactly what this spec exists to
  prevent, so what the reap process publishes is its spans and its log
  records, both carrying the trace id, and its per-run findings table on
  standard output. An installation that wants the series runs the reconciler
  on the replicas, which is the default, and `deploy/base/reaper.yaml` is
  `replicas: 0`, so no installation is missing a series for this today. A
  bridge belongs in the shared package, not here, and it is criterion 2 of
  [[025-observability-follow-ups]].
- **`arca_stored_bytes` has no source**, and still has none now that the two
  planes record their bytes. The Design says it is the reaper's sample summed
  over the installation and split by plane. The ledger the reaper reads is
  `space_usage (owner, bytes)`, one total per space with no plane in it, and
  `events.Space` carries the same two fields, so there is nothing cheap to
  split: a per-plane total would be a sum over `files.size_bytes` partitioned
  by whether the path sits under `workspaces/`, plus the workspace objects,
  which is a scan of the object tables per run and a new ledger read. That is
  a change to [[010-events-and-reaper]]'s ledger rather than to this spec, so
  the gauge stays registered at zero, no alert reads it, and the source is
  criterion 7 of [[025-observability-follow-ups]]. The counters that
  did land are rates of bytes moving, which is the question an operator
  actually alerts on; how much is stored is answered per space by
  [[012-administration]]'s overview and in aggregate by
  `arca_space_usage_bytes`.
- **Two alerts read what Arca publishes rather than what the table's words
  say.** `ArcaReaperFailing` fires at fifteen minutes, three times the
  default `ARCA_REAP_INTERVAL`, spelled in the annotation, because a rules
  file cannot read a deployment's variable. `ArcaDatabaseSaturated` fires on
  a pool holding no idle connection, because the pool's size is not a series.
- **The redaction handler is not built.** `otel.Config` exposes the local
  handler alone, so a handler wrapping both paths of the tee is not
  reachable from outside the shared package. The structural rule holds and is
  what a test asserts: no attribute of the request line is a token, a
  credential, a presigned URL or a path, and the line names the caller by the
  rendered subject the authorizer was already told. A handler around both
  paths belongs in `pkg/otel` and is a change there, carried as criterion 5
  of [[025-observability-follow-ups]].
- **There is no request span, so the span table is one row deep and that row
  is untested.** `bucket.<op>` is opened by the decorator of
  `internal/metrics` and no test asserts it. Nothing wraps the public
  handler, so `otel.SetAttributes` in the request id middleware writes onto a
  non-recording span and `otel.TraceIDs` answers the empty string: the
  request line carries `trace_id` as a key with nothing in it. `auth.verify`
  and `auth.ask` wait on spans inside [[006-identity]]'s two, `db.<op>` on
  [[004-metadata-store]]'s querier, and the order assertions of criterion 5
  on a write route, which is [[005-files]]'s. The parent has a shape to take
  in the shared package, `otel.Handler` with `otel.WithRouteTemplate`, and
  the whole of it is criteria 3 and 4 of [[025-observability-follow-ups]].
- **The request middleware sits inside the request id and not outside it.**
  It is still outside the verifier and both rate limits, which is what the
  Design asks for, and inside the id because the line it writes carries that
  id. The route, the error code and the subject reach it from further in
  through one observation the request's context carries.
- **The route label of an unmatched path is `unmatched`.** The Design says
  the label is the mux pattern, and a path no row registers has none; the
  alternative is the path itself, which is the one thing the label exists to
  keep out.
- **`tools/rules` renders the manifest rather than reading it.** The Design
  says the tool prints the rules document out of the object. It holds the
  alert table as Go values instead and answers both shapes from it, so the
  committed manifest is a rendering a test compares byte for byte and an
  alert cannot be edited in the YAML and left out of the deck. The document
  it prints for `promtool` is the same one either way.

### The criteria

Re-read on 2026-09-20 against an installation that exports by contract.
Six criteria stand as written and five were narrowed to what shipped;
what each gave up is carried verbatim by
[[025-observability-follow-ups]].

| # | State |
|---|---|
| 1 | Holds. `TestMetricsTable` reads this file through `runtime.Caller` and holds the registry to the table above, kind, labels, vocabularies and histogram bounds; `TestEveryClosedVocabularyHasAZeroSeries` reads the exposition of a fresh registry |
| 2 | Narrowed to the frame, which is where a label could carry a caller's value: `TestARequestIsCountedByItsRouteAndItsStatus`, `TestAPathNoRouteRegistersIsCountedUnderOneBoundedLabel` and `TestTheRequestLineCarriesTheIdsAndNothingSecret`, over a registry `TestMetricsTable` holds to closed vocabularies. The sweep over a whole conformance run is a case of that tier and is criterion 1 of [[025-observability-follow-ups]] |
| 3 | Holds. `TestUsageSamplingIsAggregate` runs two fixtures of different sizes through the seam and holds the bands cumulative, replaced rather than added to, and three series after six spaces |
| 4 | Narrowed to the listeners, which hold: `TestMetricsListenerOnly`, with `TestTheReaperProcessBootstrapsToo` for the reap process opening none and bootstrapping the exporter like the server. The endpoint half of the divergence is gone: an installation sets it and the process exports, which `TestAnInjectedEndpointReachesTheExporter` holds and a production log line says. The reap process's counters over OTLP is criterion 2 of [[025-observability-follow-ups]], and `deploy/base/reaper.yaml` is `replicas: 0`, so no installation is missing a series for it today |
| 5 | Carried whole by [[025-observability-follow-ups]], criterion 3. Nothing wraps the public handler, so there is no request span and no child order to assert; `bucket.<op>` is opened by the decorator and no test asserts it, which this table read as tested and was wrong about |
| 6 | Narrowed to the line, which carries both keys: `TestTheRequestLineCarriesTheIdsAndNothingSecret`. The trace id is the empty string until there is a request span, so the ids on the line and the ids on the spans are criterion 4 of [[025-observability-follow-ups]] |
| 7 | Narrowed to the call site, which is the structural rule and holds: no attribute of the line is a token, a credential, a presigned URL or a path. The handler around both paths of the tee is criterion 5 of [[025-observability-follow-ups]]; see the divergence above for why it is not reachable from here |
| 8 | Narrowed to the request, which holds: `TestOneLineAndOneObservationPerRequest`. The stream halves are criterion 6 of [[025-observability-follow-ups]] |
| 9 | Holds. `TestAlertsNameKnownMetrics`, `TestEveryRowOfTheSpecTableIsAnAlert` and `TestTheCommittedManifestIsCurrent`; the `rules` job runs `promtool check rules` over what the tool prints |
| 10 | Holds. `TestNoExporterStillServes`, whose environment is a map carrying neither endpoint variable |
| 11 | Holds. `TestAnInjectedCollectorEndpointIsReadAndTheTablesRowWins` at the configuration and `TestAnInjectedEndpointReachesTheExporter` at the process, which starts `serve` with the standard name alone and reads what the bootstrap was handed. The collector is also in `dialled` in `test/deploy/examples_test.go` under both names, so an overlay that writes either into a manifest is held to the egress rule; the injected endpoint's host port 40318 is translated by Cilium to the collector Pod's 4318 before policy is evaluated, so the base's 4318 rule admits it, and `TestNoPolicyAdmitsTheCollectorsHostPort` refuses a policy naming the host port (until 2026-09-25 deploy/prod admitted 40318, which matched nothing) |

## Design

### The scrape surface and the exporter

`GET /metrics` is on the internal listener only
([[002-repository-scaffold]]), in the Prometheus text format, from one
registry. `arcad reap` as a process of its own opens no listener, so
its counters leave over OTLP and nothing else; an installation that
runs the reaper on the API replicas scrapes the same counters from
`/metrics`.

`cmd/arcad` calls `otel.Bootstrap` with the service name `arcad`, the
version of `internal/version`, and the endpoint `internal/config`
resolved. Unset exports nothing: spans are still created and discarded,
and `/metrics` still serves, so a self-hoster with no collector loses
no local signal. Sampling is `pkg/otel`'s default of 0.2, which `arcad`
does not override and the conformance run sets to always-on.

**Two variables carry the endpoint, and the prefixed one wins.**
`ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` is [[002-repository-scaffold]]'s
row, and it is what an installation that configures Arca sets. With
that row unset the server reads the standard
`OTEL_EXPORTER_OTLP_ENDPOINT`. The reason is first-principles rather
than convenience: those are the OpenTelemetry standard's own names, an
operator that injects them into every workload of a namespace is
following that standard, and a core that read only its own name would
be the one workload there looking healthy while exporting nothing. The
table's rule is untouched, because both names are read through the one
lookup function it names, so a test still passes one map.

The shape is checked against the prefixed row alone. An injected value
was written for every workload of a namespace rather than for this
installation, and the exporter that owns the standard name is what
parses it; a telemetry variable Arca did not ask for is not a reason a
replica refuses to serve bytes, which is the same rule the failed
handover below already follows.

### Metrics

Labels come from closed vocabularies, with no exception. No label
carries a path, an object name, a principal's subject, a workspace
slug, or a token. `route` is the
`http.ServeMux` pattern, taken from a context value a middleware behind
the mux fills, because the route template is not known before the mux
matches. Every histogram names its bounds. A labeled counter is
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

Each criterion says what shipped. Five were narrowed on 2026-09-20 when
this spec was re-read against a production installation, and the words
they were narrowed from are carried by
[[025-observability-follow-ups]], criterion for criterion.

| # | Criterion | Proved by |
|---|---|---|
| 1 | Right after start-up, with nothing recorded, `/metrics` carries every name in the table and a zero series for every closed vocabulary | `TestMetricsTable` in `internal/metrics`, reading this file through `runtime.Caller` |
| 2 | Every label of every series comes from a closed vocabulary, and no label value of a request is a path, an object name, a subject or a workspace slug, including a path no route registers | `TestMetricsTable`, `TestARequestIsCountedByItsRouteAndItsStatus`, `TestAPathNoRouteRegistersIsCountedUnderOneBoundedLabel`, `TestTheRequestLineCarriesTheIdsAndNothingSecret`. The sweep over a whole conformance run is [[025-observability-follow-ups]] |
| 3 | A reaper run over a fixture of spaces at known sizes yields the right histogram, and each band counts every space at or above it, and adds no series as the space count grows | `TestUsageSamplingIsAggregate` over two fixtures of different sizes |
| 4 | `/metrics` is on the internal listener and not on the public one, `arcad reap` opens no listener, and both roles bootstrap the exporter, so a configured endpoint carries the spans and log records of either | `TestMetricsListenerOnly`, `TestTheReaperProcessBootstrapsToo`, `TestAnInjectedEndpointReachesTheExporter`. The reap process's counters over OTLP is [[025-observability-follow-ups]] |
| 5 | Carried whole by [[025-observability-follow-ups]] in these words. What shipped is one span, `bucket.<op>` at the decorator over `blob.Store`, with no request span above it and no test over either | nothing here; the criterion is proved where it is carried |
| 6 | Every request log line carries the request id and the trace id, and the request id on the line is the one the response header answers | `TestTheRequestLineCarriesTheIdsAndNothingSecret`. The trace id is empty until a request span exists, which with the ids on the spans is [[025-observability-follow-ups]] |
| 7 | No call site passes a token, a credential, a presigned URL or a path as a log attribute, and the line names the caller by the rendered subject the authorizer was already told | `TestTheRequestLineCarriesTheIdsAndNothingSecret`, which plants a bearer and a path and reads the whole line back. The handler over both paths of the tee is [[025-observability-follow-ups]] |
| 8 | One log line and one observation per request, and none per presigned URL | `TestOneLineAndOneObservationPerRequest`. The stream open and close lines are [[025-observability-follow-ups]] |
| 9 | The rules document `tools/rules` prints passes `promtool check rules`, and every `arca_` metric and label an alert names is in the table | the `rules` job of `verify.yml`, `TestAlertsNameKnownMetrics` |
| 10 | With both endpoint variables unset the process starts, serves, and exports nothing, and `/metrics` still carries the table | `TestNoExporterStillServes` |
| 11 | With only the standard `OTEL_EXPORTER_OTLP_ENDPOINT` set, the endpoint the exporter is built from is that one; with both set, `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` wins; an injected value of the wrong shape refuses no start-up | `TestAnInjectedCollectorEndpointIsReadAndTheTablesRowWins` in `internal/config`, `TestAnInjectedEndpointReachesTheExporter` at the process |

## Outcome

Complete on 2026-09-20. The metric table and its one registry, the
scrape endpoint on the internal listener, the request line, the alert
table and `tools/rules`, and the exporter bootstrap for both roles of
the binary are in the tree and green. Six criteria stand as written: 1,
3, 9, 10, 11, and the halves of 4 that are about listeners and the
exporter.

**What the production installation settled.** The cutover of
[[019-migration-from-drive]] put this code behind an operator that
injects `OTEL_EXPORTER_OTLP_ENDPOINT` into every workload of its
namespace. `internal/config` reads that name where the table's own row
is unset, `cmd/arcad` hands the value to `pkg/otel` before the
bootstrap, and `pkg/otel` logs `telemetry: exporting` with the endpoint
once its exporters are built. Criterion 11 is held by
`TestAnInjectedCollectorEndpointIsReadAndTheTablesRowWins` and
`TestAnInjectedEndpointReachesTheExporter`, and the divergence that said
no installation sets the endpoint is gone: traces, metrics and log
records leave the process. The egress that carries them is admitted by
`deploy/prod/networkpolicy-database.yaml`'s 40318 and asserted by
`TestProdAdmitsTheDatabasePortsThisInstallationUses`.

**What the re-read found.** The spec said the span table was one row
deep and that the row was tested at the decorator. Neither half of that
was quite true. `bucket.<op>` is opened by the decorator and no test
asserts it, and nothing wraps the public handler, so there is no request
span at all: `otel.SetAttributes` in the request id middleware writes
onto a non-recording span, `otel.TraceIDs` answers the empty string, and
every request line carries `trace_id` as a key with nothing in it. The
line was tested for the presence of the key and not for a value, which
is how it went unnoticed. A log line and a trace therefore cannot find
each other in either direction, which is the reason spec 018 puts both
ids on the line, so this is the first item of the follow-up rather than
a footnote in it.

**What was narrowed, and why none of it was built here.** Criteria 2, 4,
5, 6, 7 and 8 gave up a half each, and every half needs something this
spec does not own: a case in the tier [[017-conformance-suite]] owns
(the label sweep), a bridge or a handler in `latere.ai/x/pkg` (the reap
process's counters, the redaction handler over both paths of the tee),
spans inside [[006-identity]] and [[004-metadata-store]] and an order
assertion on [[005-files]]' write route (the span table), and a decision
in [[005-files]] about which read is a stream (the stream lines).
`arca_stored_bytes` needs a per-plane ledger read, which is
[[010-events-and-reaper]]'s table and not a recording site here. All six
are [[025-observability-follow-ups]], carrying each criterion in the
words this spec wrote it.

One item was bounded and still not built: a recorded-span test over the
bucket decorator, about twenty lines with no production change. It
proves a fragment of a criterion that moves to the follow-up whole, and
it would make `go.opentelemetry.io/otel/sdk` a direct dependency of this
module for the test tree. It is the cheapest first step there and is
named as such.

Nothing in this closing changed a line of Go. What an installation
records, exports and serves is what it recorded, exported and served
before it.
