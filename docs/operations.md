# Operating Arca

What to do after [`install.md`](install.md): checking an installation,
verifying a release, upgrades and rollbacks, what a version number
promises, what happens when something a replica depends on goes away, and
what to watch.

## Checking an installation

`arcad check` reads the same configuration as the server, reaches everything
the server depends on once, and prints one line per requirement. It exits 0
when every line passed and 1 when any failed. It opens no listener, runs no
migration, and writes nothing it does not delete, so it is safe to run
against production at any time.

```sh
kubectl -n arca exec deploy/arcad -- arcad check
```

```
ok    bucket      arca-prod at https://s3.example, prefix arca/: wrote, read, deleted
ok    database    PostgreSQL 18.0, schema at 0006_one_tree, clean
ok    issuer      https://issuer.example: discovery ok, 3 keys, RS256 ES256
fail  authorizer  https://authz.example/decide: allowed the probe resource
ok    public-url  https://arca.example, serving under /v1: answers the version endpoint
arcad: 1 of 5 checks failed
```

The table goes to stdout and the summary to stderr, so a script reads the
table and a person reads both. The lines are always these five in this
order, and a requirement that does not apply says so on its own line, so two
runs against a healthy installation print exactly the same thing.

| Line | Passes when | Fix a failure by |
|---|---|---|
| `bucket` | the bucket answers, and a small object written under `ARCA_BUCKET_PREFIX`, read back, and deleted all succeed | checking `ARCA_BUCKET`, `ARCA_BUCKET_ENDPOINT`, `ARCA_BUCKET_REGION` and the credentials; a listing-only credential passes nothing here |
| `database` | the connection opens, the server answers, and the schema holds every migration this binary carries | running the migration job for this version: `arcad migrate` |
| `issuer` | every issuer in `ARCA_OIDC_ISSUERS` serves a discovery document and a key set holding at least one RS256 or ES256 key | checking the issuer list and that the issuer is reachable from the cluster |
| `authorizer` | `ARCA_AUTHORIZER_URL` answers the reserved probe resource with a deny. Unset is not a failure: the line says the built-in owner policy applies and how many subjects `ARCA_ADMIN_SUBJECTS` lists | an endpoint that **allowed** the probe: it is not reading the request, and it will allow every action Arca ever adds. Fix the endpoint before anything else |
| `public-url` | `ARCA_PUBLIC_URL` answers this server's `/version`. The line also names `ARCA_BASE_PATH`, the base this server serves its routes under, which it reports rather than dials: this check passes on an address nothing answers, so a dial there could not fail where the base is wrong. A URL that cannot be reached from where the check runs is not a failure, because an ingress often does not answer from inside its own cluster | a URL that answers something else: it names another installation, and every URL this server writes points there |

Run it after every configuration change, after every upgrade, and first when
something is wrong. A failure here is a fact about the installation, not
about the load it is under.

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

gh attestation verify oci://ghcr.io/latere-ai/arcad:vX.Y.Z --repo latere-ai/arca

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

1. Read the [changelog](../CHANGELOG.md) section of every release between
   the one you run and the one you are going to. A break is named there.
2. Take a `pg_dump` of the database. It is the only way back across a
   migration; see [rolling back](#rolling-back).
3. Run the migration with the new release's image:

   ```sh
   kubectl -n arca delete job arcad-migrate --ignore-not-found
   sed 's|ghcr.io/latere-ai/arcad:unreleased|ghcr.io/latere-ai/arcad:vX.Y.Z|' \
     deploy/bootstrap/migrate-job.yaml | kubectl -n arca apply -f -
   kubectl -n arca wait --for=condition=complete job/arcad-migrate --timeout=300s
   ```

4. Set `newTag` in your overlay's `images:` entry to the new release and
   apply it. The one entry covers the API replicas and the reconciler,
   which run the same image.

   ```sh
   kubectl apply -k deploy/mine
   kubectl -n arca rollout status deployment/arcad --timeout=600s
   ```

5. Check it, and confirm the served version is the new one:

   ```sh
   kubectl -n arca exec deploy/arcad -- arcad check
   curl -s https://arca.example.com/version
   ```

The migration runs before the rollout, and there is no window in which it
must not. Each migration is additive, or is preceded by one release that
writes both shapes, so a replica of release N and a replica of release N+1
serve the same database while the rollout is half done. That is what makes
a rolling update safe rather than a maintenance window. One migration,
`0006_one_tree`, is neither: it removes two tables, and
[its section](#the-one-tree-migration) says what a replica of the previous
release answers until the rollout replaces it.

A replica the rollout stops keeps serving for 5 seconds after its pod is
deleted, while an ingress controller learns the address is gone and stops
sending it requests. On the stop signal that follows, it fails readiness,
serves 3 seconds more, then closes its listeners and gives the requests
in flight up to 60 seconds to finish. The base's 90 second
`terminationGracePeriodSeconds` covers all three; an overlay that lowers
it below 68 seconds cuts a long request short.

Reading `/version` through your hostname catches a rollout that reported
done while replicas of the previous release were still behind the
Service.

### The one-tree migration

The release that carries migration `0006_one_tree` removes workspaces. From
then on a space is one tree, every path begins with `files/`, and what a
workspace held is an ordinary folder. The migration moves it there. Its
statements commit together or not at all, and it makes no call to the
bucket: every object keeps its id, its key, and its size, and a space's
usage is unchanged.

| Before | After |
|---|---|
| the objects of a workspace, under `workspaces/<slug>/` | the same objects under the folder `files/workspaces/<slug>/`. Where the space already held something at that path, the folder is `files/workspaces/<slug>-2`, then `-3`, and so on, so nothing is overwritten |
| a star, a grant, a link, or an open upload session on such a path | the same, on the new path |
| a grant or a link on all of `workspaces` | the same on `files/workspaces`, unless the space already held something there; then it is revoked, so it covers nothing it did not cover before |
| a deleted workspace that was still restorable | its objects are in the trash, trashed at the moment the workspace was deleted. They are restored one object at a time, and purged when the workspace would have been |
| an object under `workspaces/` that no workspace named | moved the same way, and trashed at the time of the migration |
| the space's event log | one `move` event per workspace, at the folder it moved to, with the detail `{"from": "workspaces/<slug>", "files": <n>, "trashed": <bool>}`. Earlier events stay as written |
| the tables `workspaces` and `workspace_attachments` | dropped. An attachment open at that moment ends with them, and nothing it had synced is lost |

What to do:

1. Take the dump and run the migration as steps 2 and 3 above describe. It
   needs nothing else from you.
2. Move every client of `/v1/workspaces`, and every client that reads or
   writes a `workspaces/...` path through the file routes, to
   `files/workspaces/<slug>/...`. The first now answers `404 not_found`
   and the second `400 unknown_plane`. Where a folder took a `-2` name,
   the `move` event says so.
3. Take the retired series out of your dashboards and alerts:
   `arca_lease_expiries_total`, `arca_leases_held`, the `sync` kind of
   `arca_bytes_in_total`, the `materialize` kind of `arca_bytes_out_total`,
   the `workspaces` value of the `plane` label, and the kinds
   `workspace_purged`, `file_purged`, and `lease_expired` of
   `arca_reaper_findings_total`.

An authorization endpoint needs no change: it is never asked a
`workspace.*` action again, and `plane` is `files` on every question. A
consumer of the event log no longer receives `attach`, `release`, or
`sync`, and a row of the administrative overview no longer carries
`workspaces` and `leases`.

While the rollout is half done, a replica of the previous release answers
`503 storage_unavailable` on its `/v1/workspaces` routes, and its
reconciler reports the two passes over the dropped tables as failed, so
`ArcaReaperFailing` can fire until the last such replica is replaced. Its
routes under `files/` serve as before. The same holds for an image rolled
back across this migration: the objects stay in `files/workspaces/`, where
the previous release reads them as ordinary files, and the only way back
to the earlier layout is the dump.

## Rolling back

Inside a minor series, a rollback is a rollback of the image:

```sh
kubectl -n arca rollout undo deployment/arcad
```

or set the previous tag in your overlay and apply it.

An image rollback across a migration is not refused either: `arcad` starts
against a database whose schema is ahead of its own. That is safe only
because every migration is additive, or is preceded by a release that
writes both shapes, so the older binary reads what the newer one wrote. A
binary refuses to start only against a schema behind its own. The one
migration that is neither is `0006_one_tree`, and
[its section](#the-one-tree-migration) says what the older binary can and
cannot serve after it.

When a migration itself has to be undone, restore the database from a
`pg_dump` taken before it, and leave the bucket alone. The bucket holds no
schema. An object written after the dump has a key the restored database
no longer names, and the reconciler removes such keys after its 24 hour
grace, so copy out anything written since the dump before you restore.

Take that dump before every migration. It is the only way back across one.

## When a store goes away

| What | What happens | What to do |
|---|---|---|
| the bucket is unreachable | readiness fails on every replica and they leave the endpoint list; the pods keep running | fix the bucket; the replicas return by themselves |
| the database is unreachable | the same | the same |
| an issuer is unreachable at start | the replica starts, writes one line naming `ARCA_OIDC_ISSUERS`, and fails the `issuers` check until the issuer answers; it retries in the background | fix the issuer; the replicas return by themselves |
| a replica is killed | nothing; it holds no state a new one cannot read back | nothing |
| a node is drained | the budget keeps one ready replica, and an unready one is always evictable, so a drain during an outage proceeds rather than blocking | nothing |

There is no state on a pod to recover. Back up the database and the bucket;
that is the whole of it.

## Network policy

The base admits the two listeners and nothing else, and lets a replica
reach DNS, HTTPS, Postgres and an OTLP collector and nothing else. A
cluster whose CNI does not enforce NetworkPolicy applies those objects and
gets nothing from them; Cilium, Calico and the default CNI of a kind
cluster all enforce them.

Egress is an allow-list of ports, so a dependency on a port the base does
not name is unreachable, and unreachable here means the packet is dropped:
the replica waits out its own timeout rather than being refused. Point Arca
at a bucket, an issuer or an authorizer on a port other than 443 or 80 and
you must admit that port yourself, in a second NetworkPolicy that selects
`app.kubernetes.io/name: arcad`. Policies are additive, so the second one
widens the egress and leaves the base alone;
`deploy/examples/kind/networkpolicy-stack.yaml` is that file for the kind
stack, whose issuer, authorizer and bucket sit on 8081, 8082 and 9000.

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
traces, log records and its request metrics over OTLP. Leave it unset and
nothing leaves the process: `/metrics` still serves everything, so an
installation without a collector loses no local signal.

Every request either listener serves is one span, named by its method and
the route it matched, such as `PUT /v1/files/{owner}/{path...}`, and one
measurement of `http.server.request.duration` carrying the same route as
`http.route`. A request refused for want of a valid bearer is recorded under
the route it asked for. A request no route serves carries no `http.route`,
and its span is named by its method alone. The probes and the scrape of
`/metrics` are served but not recorded. The calls a request makes to the
bucket are spans under its own, `bucket.put`, `bucket.get` and the rest, so a
trace shows one request with its storage operations.

Traces are sampled when they start: `OTEL_TRACES_SAMPLER_ARG` is the share
of new traces kept, 0.2 unless you set it, and a trace your ingress or a
caller started keeps the decision it arrived with. The request histogram is
recorded for every request, sampled or not, so request rates and latencies
read from it are complete.

A span records the path the request was sent, which names the space and the
file. A public link's token is the one credential a path carries, and it is
replaced by `{token}` before the span leaves the process.

If your cluster runs an operator that instruments a whole namespace, you
have nothing to set. Such an operator injects the OpenTelemetry standard
variables into every workload, `OTEL_EXPORTER_OTLP_ENDPOINT` among them, and
`arcad` reads that name wherever `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` is
unset. Set the prefixed one to send Arca's telemetry somewhere else: it wins
wherever it is set. `arcad` checks the address you set for shape and refuses
to start on one it cannot dial; an injected address it does not check, since
that one is your platform's and the exporter is what reads it.

Whichever name carries it, the collector's port has to be open. `arcad`
confines its own egress (`deploy/base/networkpolicy.yaml`), and a port no
policy names is a connection dropped rather than refused, so a collector on
a port outside the base's 4317 and 4318 needs a policy of its own in your
overlay. A dropped export fails no probe and logs nothing on the replica:
the service serves while exporting nothing.

Logs are JSON on standard error. Each request ends on one line carrying the
route, the method, the status, the error code, the duration, the subject, the
request id and the trace id. The request id is not the trace id: the first is
what a client sees in `X-Request-Id` and in every error body, the second is
what your tracing backend indexes, and both are on the line, so you can go
from a user's complaint to a trace and back. While traces are exported, a
response carries the trace id too, in `X-Trace-Id`.

The route on a line and on `arca_requests_total` and
`arca_request_duration_seconds` is the pattern a route is registered under,
such as `POST /v1/uploads/{id}/complete`, never the path a caller sent. A path
carries what a person called their file, and it belongs in neither. It is
the route the request's span is named by, with the method in front. A
request refused before it reaches its route, a 401 or a 429 of either rate
limit, carries the route it asked for, and a request no route serves carries
`unmatched`, where its span carries no route.

`arcad reap`, run as a job of its own, opens no listener. Its traces and its
lines still reach the collector; its counters are scraped from a replica that
serves.

## Running the reconciler on its own

The reconciler removes bytes the database no longer names, reports rows
whose bytes are missing, aborts expired upload sessions, purges trash past
its retention, removes grants that expired that long ago, prunes stars
whose file is gone and events older than 30 days, and corrects the usage
counters. Those are its eight passes. It runs inside `arcad serve` every
`ARCA_REAP_INTERVAL`, which is what a small installation wants. Every pass
is safe to run on several replicas at once.

To move it off the API replicas, patch the `arcad-reaper` Deployment to one
replica in your overlay and set `ARCA_REAP_INTERVAL=0` on `arcad`. Exactly
one replica: the sweep is over the whole installation, and two would do the
same work twice. `arcad reap` runs every pass a serving replica runs, so
nothing is left undone with the loop off everywhere else.

To see what a pass would do without changing anything:

```sh
kubectl -n arca exec deploy/arcad -- arcad reap -once -dry-run
```
