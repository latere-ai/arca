# Installing Arca

From an empty cluster to a serving installation. Every step says what fails
when you skip it, so a mistake is found by the step that follows it rather
than by a user.

You need a Kubernetes cluster, `kubectl`, a bucket, and a Postgres
database. Arca keeps nothing on local disk, so there is no volume to size
and no backup of the pods to take: everything an installation holds is in
those two stores.

## Try it first

One command brings up a whole installation on a throwaway cluster, with
MinIO, Postgres and a stub issuer beside it:

```sh
deploy/examples/kind/up.sh
curl -s http://localhost:30180/readyz
deploy/examples/kind/down.sh
```

Nothing there is meant for production, and every credential in it is
published. It exists so you can see the shape before you commit a bucket to
it.

## 1. A bucket

Any store with the S3 API that honours `If-None-Match: *` on put. Arca uses
that header to make a write conditional, which is what keeps two concurrent
writes from losing one of them; a store that ignores it silently drops
writes under load. AWS S3, MinIO, Tigris and Cloudflare R2 honour it.

Create one bucket and a credential that may put, get, head, delete, list
and presign under a single prefix, and nothing else.

Skip this and `arcad check` fails on the first line.

## 2. A database

Postgres 16 or newer, and a role that owns its schema: the migration
creates tables and indexes.

One database per installation. Arca does not share a schema with anything
else.

Skip this and `arcad check` fails on the second line.

## 3. The issuers

Decide whose tokens this installation accepts, and list them in
`ARCA_OIDC_ISSUERS`, comma separated. Any OIDC provider will do: Arca
fetches each issuer's key set at start-up and verifies every `/v1` request
against it.

Every token must carry the audience `arca`. That is the default and the
deploy base names it explicitly, so a token addressed to another service is
refused by decision rather than by accident. An installation that needs a
different audience sets `ARCA_OIDC_AUDIENCE` and tells its issuer to mint
for that value.

Skip this and `arcad check` cannot reach a key set, and every request is
answered 401.

## 4. An authorizer, or not

Arca verifies who is asking and asks something else whether they may. That
something else is an HTTP endpoint you write:

```
ARCA_AUTHORIZER_URL    where to ask
ARCA_AUTHORIZER_TOKEN  the bearer Arca presents to it
```

With neither set, the owner policy applies: a subject may read and write its
own space, and the subjects listed in `ARCA_ADMIN_SUBJECTS` may act on any
space. That is the right choice for a single team.

Set them when who may see what is a decision your product makes rather than
a property of storage. The endpoint answers one question per request and its
answer is final; a question it cannot answer is a refusal, never an allow.

`arcad check` asks your endpoint one probe question, about a reserved space
that belongs to nobody, and passes only when the answer is a denial. An
endpoint that allows everything fails that line, which is the failure worth
catching: it is indistinguishable from a working one until somebody reads
somebody else's objects.

## 5. The namespace and the secrets

```sh
kubectl apply -f deploy/bootstrap/namespace.yaml

cp deploy/bootstrap/secrets.example.yaml /tmp/arcad-secrets.yaml
# fill in the bucket, the database and the issuers from steps 1 to 4
kubectl apply -f /tmp/arcad-secrets.yaml
rm /tmp/arcad-secrets.yaml
```

Three Secrets, one per endpoint: `arcad-bucket`, `arcad-database` and
`arcad-auth`. They are separate so that a credential that leaks opens one
thing.

Skip this and the overlay in step 7 applies and never becomes ready,
because every pod starts without its configuration.

## 6. The schema

```sh
kubectl -n arca delete job arcad-migrate --ignore-not-found
kubectl -n arca apply -f deploy/bootstrap/migrate-job.yaml
kubectl -n arca wait --for=condition=complete job/arcad-migrate --timeout=300s
```

Set the Job's image to the release you are installing before you apply it.

A Job and never an init container: two replicas rolling at once would each
run an init container, and two processes applying the same forward-only
migrations at the same time is the one thing the schema cannot survive.

Skip this and the server refuses to serve, naming the schema version it
needs and the one it found.

## 7. The overlay

Copy an example overlay and make it yours:

```sh
cp -r deploy/examples/aws deploy/mine     # or examples/digitalocean
```

Change, in your copy:

| File | What |
|---|---|
| `ingress.yaml` | your hostname, and your certificate or issuer |
| `public-url.yaml` | `ARCA_PUBLIC_URL`, the same hostname, with its scheme |
| `replicas.yaml` | the bounds your cluster has room for |
| `kustomization.yaml` | the image tag: the release you are installing |

`ARCA_PUBLIC_URL` has no default. Every URL the server writes is built from
it, a presigned redirect included, so a value that does not match your
ingress sends clients to an address that answers nothing.

Then:

```sh
kubectl apply -k deploy/mine
kubectl -n arca rollout status deployment/arcad --timeout=600s
```

## 8. Check it

```sh
kubectl -n arca exec deploy/arcad -- arcad check
```

One line per requirement, and exit 1 on any failure: the bucket, the
conditional write, the database and its schema version, each issuer's key
set, and the authorizer's answer to the probe question. It runs against the
pod's own environment, because a check against another environment checks
nothing.

## 9. Prove it

```sh
BASE_URL=https://arca.example.com tools/smoke/release.sh
```

`/livez`, `/readyz`, `/openapi.json` and `/version` each answer 200, and the
served version is written down. Add `TAG=vX.Y.Z` to make the served version
having to equal it a condition of success, which is what the release
pipeline does after every rollout.

The conformance suite is the fuller answer to "does this installation behave
like Arca": it runs against any installation with a token from your issuer,
and is what an operator runs after an upgrade and after a store is replaced.

## Alerts

`deploy/base/prometheusrule.yaml` is applied beside the base, not by it: a
`PrometheusRule` needs the Prometheus operator, which Arca does not require.
If you run the operator:

```sh
kubectl -n arca apply -f deploy/base/prometheusrule.yaml
```

## Next

[`operations.md`](operations.md) is what to do after this: upgrades,
rollbacks, what a version number promises, and what to do when a store goes
away.
