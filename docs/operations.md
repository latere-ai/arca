# Operating Arca

What to do after [`install.md`](install.md): upgrades, rollbacks, what a
version number promises, and what happens when something a replica depends
on goes away.

## What a release is

A `vX.Y.Z` tag publishes, under the repository that built it:

| Artifact | What it is |
|---|---|
| `arcad` for linux and darwin, amd64 and arm64 | four `.tar.gz` archives |
| `checksums.txt` | SHA-256 over every archive |
| `checksums.txt.cosign.bundle` | the signature over that file |
| `arcad` and `arca-stubs` images | one multi-arch image each, both architectures |
| three SPDX documents | the module graph, and the contents of each image |

Every image is signed keyless, with the workflow's own identity as the
signer, so you can check one without holding a key of ours:

```sh
cosign verify ghcr.io/latere-ai/arcad:vX.Y.Z \
  --certificate-identity-regexp '^https://github\.com/.+/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

gh attestation verify oci://ghcr.io/latere-ai/arcad:vX.Y.Z --repo <owner>/arca

cosign verify-blob --bundle checksums.txt.cosign.bundle checksums.txt \
  --certificate-identity-regexp '^https://github\.com/.+/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum -c checksums.txt
```

Check the archives before you run them, and check the image before you
deploy it. That is what the signatures are for.

## What a version number promises

| Change | Bump |
|---|---|
| a route, a field, an error code, a configuration variable or an event payload removed, or given a different meaning | major |
| a new route, field, variable, metric, event kind or capability | minor |
| a fix with no visible change | patch |

Before `v1.0.0` a minor may remove a field, with a changelog entry that
names the break. From `v1.0.0` the API contract binds, and a client written
against release N works against release N+1 inside one major.

A field is deprecated in one minor, written down here, and removed no
earlier than the next major.

The two most recent minor series receive patches.

## Upgrading

```sh
# 1. Read the changelog section for the version you are going to.
# 2. Run the migration, with the Job's image set to that version.
kubectl -n arca delete job arcad-migrate --ignore-not-found
kubectl -n arca apply -f deploy/bootstrap/migrate-job.yaml
kubectl -n arca wait --for=condition=complete job/arcad-migrate --timeout=300s

# 3. Roll the replicas.
kubectl -n arca set image deployment/arcad arcad=ghcr.io/latere-ai/arcad:vX.Y.Z
kubectl -n arca rollout status deployment/arcad --timeout=600s

# 4. Prove it.
kubectl -n arca exec deploy/arcad -- arcad check
BASE_URL=https://arca.example.com TAG=vX.Y.Z tools/smoke/release.sh
```

The migration runs before the rollout, and there is no window in which it
must not. Each migration is additive, or is preceded by one release that
writes both shapes, so a replica of release N and a replica of release N+1
serve the same database while the rollout is half done. That is what makes
a rolling update safe rather than a maintenance window.

The smoke's `TAG` is the step that catches a rollout that returned while
replicas of the previous release were still in the endpoint list: the
served version would be the old one, and the smoke fails.

## Rolling back

Inside a minor series, a rollback is a rollback of the image:

```sh
kubectl -n arca rollout undo deployment/arcad
```

Across a migration it is not. A binary started against a schema recorded
above its own refuses to start and names both versions, so the rollback
stops before it corrupts anything rather than after. To go back across a
migration you restore the database from a `pg_dump` taken before it, and
leave the bucket alone: the bucket holds no schema, and a key the restored
database no longer names is something the reconciler finds and reports, not
a loss.

Take that dump before every migration. It is the only rollback there is.

## When a store goes away

| What | What happens | What to do |
|---|---|---|
| the bucket is unreachable | readiness fails on every replica and they leave the endpoint list; the pods keep running | fix the bucket; the replicas return by themselves |
| the database is unreachable | the same | the same |
| a replica is killed | nothing; it holds no state a new one cannot read back | nothing |
| a node is drained | the budget keeps one ready replica, and an unready one is always evictable, so a drain during an outage proceeds rather than blocking | nothing |

There is no state on a pod to recover. Back up the database and the bucket;
that is the whole of it.

## Network policy

The base admits the two listeners and nothing else, and lets a replica
reach DNS, HTTPS, Postgres and an OTLP collector and nothing else. A
cluster whose CNI does not enforce NetworkPolicy applies those objects and
gets nothing from them; Cilium and Calico enforce them, and the default CNI
of a kind cluster does not.

## What to watch

### Metrics

The internal listener serves `/metrics` in the Prometheus text format. It is
reached where the pod runs rather than through a Service: it is not routed,
and nothing should route it. The public listener answers `/metrics` with a
404, and that is deliberate.

Every metric name is on the endpoint from the first scrape, at zero, so a
dashboard and an alert work before the first request. The names begin
`arca_`. The ones you will reach for first:

| Metric | What it answers |
|---|---|
| `arca_requests_total` | is the API serving, by route and status class |
| `arca_request_duration_seconds` | how long a route takes |
| `arca_bucket_ops_total` | are the calls to the bucket succeeding |
| `arca_reaper_findings_total` | what the reconciler found, by kind |
| `arca_spaces_by_usage` | how many spaces are at least 1, 10 and 100 GiB |
| `arca_space_usage_bytes` | how the installation's bytes are spread |
| `arca_decisions_total` | are authorization decisions being made, and by whom |

Usage is published in aggregate and never per space. A space is addressed by
the subject its owner's token carries, so a label naming one would put a
person's identity on an endpoint anyone who can scrape the namespace reads,
and would add a series for every principal that ever used the installation.
To find out which space is the large one, read the administrative overview
under an administrator's token.

To scrape with the Prometheus operator, point a `PodMonitor` at the internal
port. Without the operator, add the pod IP and port 8081 to your scrape
configuration.

### Alerts

`deploy/base/prometheusrule.yaml` holds thirteen alert rules. Apply it beside
the base:

```sh
kubectl apply -n arca -f deploy/base/prometheusrule.yaml
```

It needs the Prometheus operator's CustomResourceDefinition, which is why it
is not part of the base. Without the operator, copy the rules out of its
`spec` into your own Prometheus configuration; `go run ./tools/rules` prints
exactly that document.

Two of them matter more than the rest. `ArcaMissingBytes` means a row names
an object the bucket does not hold, which is data loss and always reaches a
human. `ArcaReaperFailing` means the reconciler has stopped, and everything
it cleans up will accumulate until it runs again. Its window is three times
the default `ARCA_REAP_INTERVAL` of five minutes; if you lengthened the
interval, lengthen the window.

Two alerts read metrics Arca does not publish, `ArcaReadinessFailing` and
`ArcaReplicasPinned`. They come from kube-state-metrics, and an installation
without it simply never fires them.

### Traces and logs

Set `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` to your collector and `arcad` exports
traces and log records over OTLP. Leave it unset and nothing leaves the
process: `/metrics` still serves everything, so an installation without a
collector loses no local signal.

Logs are JSON on standard error. Each request ends on one line carrying the
route, the method, the status, the error code, the duration, the subject, the
request id and the trace id. The request id is not the trace id: the first is
what a client sees in `X-Request-Id` and in every error body, the second is
what your tracing backend indexes, and both are on the line, so you can go
from a user's complaint to a trace and back.

The route on a line and on a metric is the pattern a route is registered
under, such as `GET /v1/workspaces/{id}/sync`, never the path a caller sent.
A path carries what a person called their file, and it belongs in neither.

`arcad reap`, run as a job of its own, opens no listener. Its traces and its
lines still reach the collector; its counters are scraped from a replica that
serves.

## Running the reconciler on its own

The reconciler sweeps for objects the database no longer names and for
trash past its retention. It runs inside `arcad serve` by default, which is
what a small installation wants.

To move it off the API replicas, patch `arcad-reaper` to one replica in your
overlay and set `ARCA_REAP_INTERVAL=0` on `arcad`. Exactly one replica: the
sweep is over the whole installation, and two would do the same work twice.
