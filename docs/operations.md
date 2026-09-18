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

The internal listener serves `/metrics` in the Prometheus text format, and
is reached where the pod runs rather than through a Service: it is not
routed, and nothing should route it.

`deploy/base/prometheusrule.yaml` holds the alert rules, applied beside the
base by an installation that runs the Prometheus operator.

## Running the reconciler on its own

The reconciler sweeps for objects the database no longer names, for trash
past its retention, for workspaces deleted longer ago than that, and for
grants that expired that long ago. It runs inside `arcad serve` by default, which is
what a small installation wants.

To move it off the API replicas, patch `arcad-reaper` to one replica in your
overlay and set `ARCA_REAP_INTERVAL=0` on `arcad`. Exactly one replica: the
sweep is over the whole installation, and two would do the same work twice.
