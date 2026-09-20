# Operating Arca

What to do after [`install.md`](install.md): checking an installation,
upgrades, rollbacks, what a version number promises, and what happens when
something a replica depends on goes away.

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
ok    database    PostgreSQL 18.0, schema at 0005_usage_events, clean
ok    issuer      https://issuer.example: discovery ok, 3 keys, RS256 ES256
fail  authorizer  https://authz.example/decide: allowed the probe resource
ok    public-url  https://arca.example: answers the version endpoint
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
| `public-url` | `ARCA_PUBLIC_URL` answers this server's `/version`. A URL that cannot be reached from where the check runs is not a failure, because an ingress often does not answer from inside its own cluster | a URL that answers something else: it names another installation, and every URL this server writes points there |

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
traces and log records over OTLP. Leave it unset and nothing leaves the
process: `/metrics` still serves everything, so an installation without a
collector loses no local signal.

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
from a user's complaint to a trace and back.

The route on a line and on a metric is the pattern a route is registered
under, such as `GET /v1/workspaces/{id}/sync`, never the path a caller sent.
A path carries what a person called their file, and it belongs in neither.

`arcad reap`, run as a job of its own, opens no listener. Its traces and its
lines still reach the collector; its counters are scraped from a replica that
serves.

## Running the reconciler on its own

The reconciler sweeps for objects the database no longer names, for trash
past its retention, for workspaces deleted longer ago than that, and for
grants that expired that long ago. It runs inside `arcad serve` by default, which is
what a small installation wants.

To move it off the API replicas, patch `arcad-reaper` to one replica in your
overlay and set `ARCA_REAP_INTERVAL=0` on `arcad`. Exactly one replica: the
sweep is over the whole installation, and two would do the same work twice.

## Migrating from Drive

Arca replaces a service called Drive. If you run that service, two commands
move it during the cutover, in this order: `migrate-drive` copies its metadata
into an Arca database, and `move-objects` copies its objects to the keys the
copied rows name. The first reads the old database and writes the new one; the
second copies inside your bucket. Neither changes anything in the old
database, and neither deletes an object during the cutover. Deleting the old
keys is a later step, run with a flag of its own once the new service holds.

Read this whole section before you start. Both commands have to succeed before
you switch a route.

### What to prepare

- **A fresh Arca database**, with the migrations applied and no rows in it.
  Run `arcad migrate` against it. The copy refuses a database that already
  holds rows, so a second attempt begins by dropping this database and
  migrating it again.
- **The issuer URL** your identity provider signs tokens with, for example
  `https://issuer.example`. Every personal space becomes the subject
  `<issuer>|<id>`, so this value has to be the one your installation verifies
  against. It is what `ARCA_OIDC_ISSUERS` names.
- **A mapping of organizations to subjects**, exported from your identity
  provider. Drive addressed an organization by its own id; Arca addresses it
  by the subject the provider assigns it, and nothing but the provider knows
  which is which. JSON or CSV, read by what the file begins with and not by
  its name:

  ```json
  {
    "33333333-3333-4333-8333-cccccccccccc": "https://issuer.example|org-acme"
  }
  ```

  ```csv
  organization,subject
  33333333-3333-4333-8333-cccccccccccc,https://issuer.example|org-acme
  ```

  The header row is optional. An organization in the old database that the
  file does not name stops the run before it writes anything, and names the
  id you have to add.

  **Or no file at all.** If your identity provider gives an organization the
  subject `<issuer>|<its id>` rather than a subject of its own, that is a rule
  and not a table: pass `-org-issuer https://orgs.example` instead of
  `-org-subjects`, and every `o-<id>` becomes `https://orgs.example|<id>`. The
  issuer is often not the one your people carry, which is why it is its own
  flag. Give one or the other; both is a command line to correct.
- **Drive in read-only mode.** The copy is a snapshot. A write that lands
  after a table has been read is a row the new database does not have and
  nothing will tell you about. Turn the read-only flag on and confirm writes
  are refused before you start, and leave it on until you have switched your
  routes.

### Copying the rows

It runs from a checkout of this repository, with the Go toolchain and reach to
both databases. It is not in the server image: it runs once, from wherever you
can reach the two databases, and never from inside the service.

```sh
go run ./tools/migrate-drive \
  -source 'postgres://user:password@old-db/drive?sslmode=require' \
  -target 'postgres://user:password@new-db/arca?sslmode=require' \
  -issuer https://issuer.example \
  -org-subjects orgs.json \
  -manifest manifest.tsv
```

Add `-dry-run` to read, rewrite and report without writing. A dry run prints
the report a real run prints, so run it first: it tells you which rows will be
dropped and whether the mapping is complete, and it leaves the target alone.

`-manifest` writes the file the second command reads. Keep it: it is the only
record of which old key became which object id, and without it nothing can
move your objects. It is tab separated, one line per distinct old key, and the
last line says the copy verified:

```
#arca-manifest	1	drive/
drive/u-1/files/notes.md	0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f	21	3b1f…	false
#complete	1
```

A dry run writes no manifest; it writes no rows, so there are no ids for it to
be the record of.

`-prefix` defaults to `drive/` and is the bucket prefix the old installation
wrote its keys under. The run checks it twice: every key in the source has to
begin with it, and it has to match `ARCA_BUCKET_PREFIX` where that variable is
set, so a copy cannot be made under one prefix for a server deployed with
another.

The run exits 0 when every table verified, 1 when anything went wrong or was
refused, and 2 on a bad flag.

### What the copy's report means

```
table                  copied  dropped  verified
subjects               2       0        counts hold
files                  4       0        counts hold, and 4 checksums were sampled
shares                 5       4        counts hold
space_usage            2       0        recomputed over 2 spaces
```

- **copied** is the rows now in the new database.
- **dropped** is the rows that did not come across, listed under `rows
  dropped` with the reason for each. Four reasons, all of them deliberate:
  grants to a role, to a team, or to an email address, which Arca does not
  have; share requests, which are the grants that were pending or denied and
  are your platform's to hold now; and link or public grants that allowed
  writing, which Arca's links do not. Read these counts. They are access
  somebody had yesterday and will not have tomorrow.
- **verified** is what was checked. `counts hold` means the old database held
  as many rows as the run accounted for, copied plus dropped, and the new one
  holds as many as it wrote. For files a sample of rows is read back and
  compared on checksum and size. For `space_usage`, which is recomputed
  rather than copied, the ledger is compared against the sum the run made
  while reading.
- **noted** lists what changed inside a row without dropping it: tokens
  minted for a link or public grant that had none, invite tokens cleared
  where the grantee became a subject, repositories that became workspaces,
  `agents_folded` for each row that left the retired agent zone, actions in
  the log that Arca's vocabulary does not have, and upload sessions still
  open.
- **manifest**, in the header block, is where the file was written, how many
  old keys it lists, and whether it is complete. Only a run that verified
  completes it, and the second command refuses one that is not complete.

Any table that does not verify makes the run exit 1 and names what it found.
Do not switch your routes on a run that exited 1, and do not move the objects
on one either.

### What changes on the way

- Owners become subjects. `u-<id>` becomes `<issuer>|<id>`, and `o-<id>`
  becomes the subject your mapping gives it.
- Paths move plane. Arca has two, `files/` and `workspaces/`. `memory/`
  becomes `files/memory/`, `agents/` becomes `files/agents/`, and
  `repos/<name>/` becomes `workspaces/<name>/`. The workspace keeps its name;
  it stops being a separate kind of thing.
- The `agents/` rows land in the same space they were in, counted in the
  report as `agents_folded`. Arca has no rule that keeps a machine out of
  where a person curates, so those files are files: read the count, because
  the person who owns the space can now see them in their own plane. The
  objects do not move for the fold; a bucket key derives from an object id and
  carries no path.
- A path in any other plane stops the run. If your installation holds one,
  decide where those rows belong before you migrate.
- `quotas`, `webhooks`, `agent_visibility` and `admin_audit` are not copied.
  Arca counts what a space holds and stores no limit; the event log is the
  integration point; audit is the event log.
- The event log keeps every id, so a consumer holding a cursor keeps its
  place.

### Moving the objects

**The copy moves rows and not objects, and setting `ARCA_BUCKET_PREFIX` to
`drive/` does not make the old objects readable.**

Drive built a bucket key out of the owner and the path,
`drive/<owner>/<path>`. Arca builds one out of an object id,
`<prefix><shard>/<id>`. There is no id inside an old key to carry over, so
the copy mints a new object id for every distinct key it reads, and those ids
name keys that no bytes lie at yet. The second command puts them there, one
copy per key, inside the bucket. No byte leaves your store.

```sh
go run ./tools/move-objects \
  -manifest manifest.tsv \
  -bucket arca-prod \
  -endpoint https://s3.example \
  -region us-east-1 \
  -path-style \
  -prefix drive/
```

Run it right after the copy, before you switch any route. The credentials come
from `ARCA_BUCKET_ACCESS_KEY` and `ARCA_BUCKET_SECRET_KEY`, or from your
cloud's credential chain when both are unset, so no secret goes on the command
line. `-prefix` has to be the manifest's prefix and `ARCA_BUCKET_PREFIX`, and
the run refuses the command if the three disagree. `-concurrency` defaults to
16 keys at once. `-path-style` is for a store without virtual hosted buckets,
such as MinIO.

Add `-dry-run` first. It reads both ends of every line and writes nothing, so
the counts it prints are the counts the real run will print.

**The byte check is on, and you have to turn it off rather than on.** After
each copy, and for each key a resumed run skips, the object is read back out
of your bucket, hashed, and compared to the checksum the row carried. That is
the only check that holds at a store which reports no checksum of its own,
which is every S3 store we know of, DigitalOcean Spaces and MinIO among them:
without it a copy is proved by its length. It costs one read of every byte you
move, so the run takes about as long as reading your bucket once.

| Flag | Default | What it does |
|---|---|---|
| `-verify-bytes` | on | read each destination back and hash it |
| `-verify-bytes-max` | 268435456 (256 MiB) | read back in full every object at or under this size |
| `-verify-sample` | 10 | the percentage of the larger objects to read back, chosen by hashing the key, so a rerun reads the same ones |

Turn it off only when you are rehearsing against a copy of your data.

```
outcome     keys  means
copied      812   copied to the object id's key and read back
skipped     0     the destination already held the bytes, so this run left it alone
mismatched  0     the destination holds other bytes, and nothing was overwritten
failed      0     the store could not answer for the key

noted
  verified on bytes  807  read back from the store and digested to the checksum the row
                          carries
  verified on size   5    the size and the store's own copy are what hold: the row's
                          checksum is a digest no store reports, and this run did not
                          read the object back
```

- **copied** is the objects now readable at their object id's key.
- **skipped** is the destinations that were already right. A killed run
  resumes and a finished run repeats: rerun the same command as often as you
  like.
- **mismatched** is a destination holding something else, a destination whose
  bytes hash to something else included. Nothing is overwritten and every key
  is named. Look at each one before you continue.
- **failed** is a key the store could not answer for, a missing source among
  them. Every key is named with the reason.
- **verified on bytes** is the objects read back and hashed. This is the count
  that makes the move provable; aim for all of them.
- **verified on label** appears where the row's checksum is a label your store
  reports for a whole object, which is compared without reading the bytes.
- **verified on size** counts what neither check could reach: an object above
  the threshold that the sample did not pick, a row whose checksum is the
  composite label of a multipart upload, or every row if you turned the byte
  check off. Read this number. It is how many objects you are taking on
  trust.
- **publicity not stamped** appears when your store has no object ACLs and
  serves public objects through a bucket policy instead. Those objects are
  copied; only the stamp is the policy's job.

The run exits 0 when nothing mismatched and nothing failed, 1 when anything
did or the command was refused, and 2 on a bad flag. Without
`-delete-sources` it deletes nothing: the old keys stay where they are, and
the manifest is what tells you which they were.

Both halves have to hold before the routes switch: the copy's report and this
one. Spec 019 in this repository records why.

### Deleting the old keys

After the move, every object is in your bucket twice: under the old key and
under the key its object id derives. When you retire the old service, and not
before, the same command deletes the old ones.

```sh
go run ./tools/move-objects \
  -manifest manifest.tsv \
  -bucket arca-prod \
  -endpoint https://s3.example \
  -region us-east-1 \
  -path-style \
  -prefix drive/ \
  -delete-sources
```

The flag adds a pass of its own after the move. The move runs first and
verifies every destination exactly as it did before; only then is each source
key deleted, one key per request, and only where its destination held. A
source whose destination mismatched, failed, or is not in the bucket is kept
and named: the bytes under it are your only copy of that object, and the run
exits 1 so that you look at it.

```
sources
  deleted drive/u-1/files/notes.md
  kept drive/u-1/files/logo.png: drive/20/b did not verify: the destination holds 3 bytes and the manifest says 10

1 source key deleted, 1 kept, 0 the store would not delete
```

- Add `-dry-run` first. It names every key it would delete and calls nothing.
- The manifest is the only list. No key outside its first column is touched,
  which is what makes this safe to run against a bucket Arca is already
  serving from.
- `-verify-bytes=false` is refused with this flag. A delete leaves one copy of
  the bytes, and a length is not a proof to leave it on.
- Rerunning is safe: a destination that is already right is skipped, and a key
  that is already gone deletes cleanly.

Run it only after a person has read an object through the new service. Until
then, a byte the move got wrong is still under its old key.
