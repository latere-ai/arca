# Installing Arca

From a Kubernetes cluster to a serving installation. Every step says what
fails when you skip it, so a mistake is found by the step that follows it
rather than by a user.

You need a Kubernetes cluster at 1.30 or newer, `kubectl`, a bucket, a
Postgres database, and an OIDC issuer. Arca keeps nothing on local disk, so there is no volume
to size and no pod to back up: everything an installation holds is in the
bucket and the database.

## Try it first

A throwaway installation on a [kind](https://kind.sigs.k8s.io) cluster,
with MinIO, Postgres, a stub issuer, and a stub authorizer beside it, is one
script in a checkout. It needs `kind`, `kubectl`, and `docker`, and pulls
the images of the release you name:

```sh
git clone https://github.com/latere-ai/arca.git && cd arca
ARCA_IMAGE_TAG=vX.Y.Z deploy/examples/kind/up.sh
curl -s http://localhost:30180/readyz
deploy/examples/kind/down.sh
```

`up.sh` prints every address it published: `arcad` on port 30180, the stub
issuer on 30081, Postgres on 30432, and MinIO on 30900. Nothing there is
meant for production, and every credential in it is published.

## Get the manifests

The manifests are in the repository, at the tag of the release you install.
The [releases page](https://github.com/latere-ai/arca/releases) lists them;
take the newest unless you have a reason not to.

```sh
git clone --depth 1 --branch vX.Y.Z https://github.com/latere-ai/arca.git
cd arca
```

| Directory | What it holds |
|---|---|
| `deploy/base/` | the service: the API Deployment, a reconciler Deployment scaled to zero, the Service, network policies, the disruption budget, and the autoscaler |
| `deploy/bootstrap/` | what is applied once by hand: the namespace, the Secrets template, and the migration Job |
| `deploy/examples/` | overlays to copy: `aws`, `digitalocean`, and `kind` |

You never apply `deploy/base` directly. You copy an example overlay and
apply your copy.

## 1. A bucket

Any store that speaks the S3 API: AWS S3, MinIO, DigitalOcean Spaces,
Cloudflare R2, Tigris, and others. Create one bucket and a credential that
may put, get, head, delete, and list under one prefix, and nothing else.
Arca writes every key under `ARCA_BUCKET_PREFIX` (`arca/` by default), so
several installations can share one bucket with a prefix each.

Arca writes every object with `If-None-Match: *`, so a key is written once
and never overwritten. A store that refuses that header still works: Arca
logs `the store refuses a conditional create; writes run unguarded` once
and writes without it.

Do not add a lifecycle rule that expires or rewrites objects under the
prefix. Arca manages every object's life itself, and a key deleted behind
its back is a file whose bytes are gone.

Skip this and `arcad check` fails on its `bucket` line.

## 2. A database

Postgres 16 or newer, one database per installation, and a role that owns
its schema: the migrations create tables and indexes.

Skip this and `arcad check` fails on its `database` line.

## 3. The issuers

Decide whose tokens this installation accepts, and list them in
`ARCA_OIDC_ISSUERS`, comma separated. Any OIDC provider will do: Arca
fetches each issuer's discovery document and key set and verifies every
request against it.

Every token must carry the audience `arca`. That is the default, and the
base Deployment names it explicitly, so a token addressed to another
service is refused by decision rather than by accident. An installation
that needs a different audience sets `ARCA_OIDC_AUDIENCE` in its overlay
and asks its issuer to mint for that value.

`ARCA_OIDC_AUDIENCE` is a comma separated list, and a token is accepted when
its `aud` names any entry. The first entry is the primary. List a second
entry when a gateway in front of Arca issues credentials addressed to its
own origin.

Skip this and `arcad check` fails on its `issuer` line, and every request
is answered 401.

## 4. An authorizer, or not

Arca verifies who is asking and asks something else whether they may. That
something else is an HTTP endpoint you write:

| Variable | Where it goes | What it is |
|---|---|---|
| `ARCA_AUTHORIZER_URL` | the overlay, as plain environment | where to ask |
| `ARCA_AUTHORIZER_TOKEN` | the `arcad-auth` Secret | the bearer Arca presents to it |

With no URL set, the owner policy applies: a subject may act on its own
space, the subjects listed in `ARCA_ADMIN_SUBJECTS` may act on every space,
and anyone else gets what a grant or a link gives them. That is the right
choice for a single team.

Set the URL when who may see what is a decision your product makes rather
than a property of storage. The endpoint answers one question per action,
and a question it cannot answer is a refusal, never an allow. What it
receives and what it must answer is in the
[API guide](api.md#the-authorization-endpoint).

`arcad check` asks your endpoint one probe question, about a reserved
resource that belongs to nobody, and passes only when the answer is a deny.
An endpoint that allows everything fails that line, which is the failure
worth catching: until somebody reads somebody else's objects, it is
indistinguishable from a working one.

## 5. The namespace and the Secrets

```sh
kubectl apply -f deploy/bootstrap/namespace.yaml

cp deploy/bootstrap/secrets.example.yaml /tmp/arcad-secrets.yaml
# fill in the bucket, the database, and the issuers from steps 1 to 4
kubectl apply -f /tmp/arcad-secrets.yaml
rm /tmp/arcad-secrets.yaml
```

Three Secrets, one per endpoint, so a credential that leaks opens one thing:

| Secret | Keys |
|---|---|
| `arcad-bucket` | `ARCA_BUCKET`, `ARCA_BUCKET_REGION`, `ARCA_BUCKET_ENDPOINT`, `ARCA_BUCKET_ACCESS_KEY`, `ARCA_BUCKET_SECRET_KEY` |
| `arcad-database` | `ARCA_DATABASE_URL`, the connection string, which the Deployment passes to `arcad` as `ARCA_DB_URL` |
| `arcad-auth` | `ARCA_OIDC_ISSUERS`, `ARCA_AUTHORIZER_TOKEN` |

Leave the two bucket keys out when the cluster gives the pods a role; the
SDK's credential chain applies then.

Skip this and the overlay applies and never becomes ready, because every
pod starts without its configuration.

## 6. The schema

The migration runs as a Job, with the image of the release you install:

```sh
kubectl -n arca delete job arcad-migrate --ignore-not-found
sed 's|ghcr.io/latere-ai/arcad:unreleased|ghcr.io/latere-ai/arcad:vX.Y.Z|' \
  deploy/bootstrap/migrate-job.yaml | kubectl -n arca apply -f -
kubectl -n arca wait --for=condition=complete job/arcad-migrate --timeout=300s
```

A Job and never an init container: two replicas rolling at once would each
run an init container, and two processes applying the same migrations at
the same time is the one thing the schema cannot survive.

Skip this and the server refuses to start, naming the migration it needs
and telling you to run `arcad migrate`.

## 7. The overlay

Copy an example overlay and make it yours:

```sh
cp -r deploy/examples/aws deploy/mine     # or deploy/examples/digitalocean
```

Change, in your copy:

| File | What |
|---|---|
| `ingress.yaml` | your hostname, your ingress class, and your certificate |
| `public-url.yaml` | `ARCA_PUBLIC_URL`, the same hostname with its scheme; `ARCA_AUTHORIZER_URL` beside it if you run one |
| `replicas.yaml` | the autoscaler's bounds for your cluster |
| `kustomization.yaml` | add an `images:` entry naming `ghcr.io/latere-ai/arcad` and the release you install |

The image entry is required. The base names the placeholder tag
`unreleased`, which never resolves, so an overlay that pins nothing fails
to pull rather than running an unknown build:

```yaml
images:
  - name: ghcr.io/latere-ai/arcad
    newTag: vX.Y.Z
```

`ARCA_PUBLIC_URL` has no default. Every URL the server writes is built from
it, so a value that does not match your ingress sends clients to an address
that answers nothing.

Leave `ARCA_BASE_PATH` unset. Its default is `/v1`, so
`https://arca.example.com/v1/files/...` is where your routes answer. Set it
only when Arca shares an origin with other services and has been given a
prefix of its own, such as `/v1/storage`: your ingress then forwards that
prefix without rewriting it, and the document at `/openapi.json` names every
path under it.

Your ingress must accept request bodies of at least `ARCA_INLINE_BYTES`
(16 MiB by default), because objects up to that size are written through
the server; larger ones go straight to the bucket. The `digitalocean`
example sets ingress-nginx's body limit off and its timeouts to ten
minutes, and the `aws` example sets the load balancer's idle timeout to
the same.

Then:

```sh
kubectl apply -k deploy/mine
kubectl -n arca rollout status deployment/arcad --timeout=600s
```

## 8. Check it

```sh
kubectl -n arca exec deploy/arcad -- arcad check
```

Five lines, one per requirement, and exit 1 on any failure: `bucket` (a
probe object written, read back, and deleted), `database` (the connection
and the schema version), `issuer` (each issuer's key set), `authorizer`
(the answer to the probe question, or which policy applies), and
`public-url`. It runs with the pod's own environment, because a check
against another environment checks nothing. [Operations](operations.md#checking-an-installation)
explains each line and how to fix it.

## 9. Prove it

From outside the cluster, through your hostname:

```sh
ARCA_URL=https://arca.example.com
for p in /livez /readyz /openapi.json /version; do
  curl -s -o /dev/null -w "$p %{http_code}\n" "$ARCA_URL$p"
done
curl -s -o /dev/null -w '/v1/files/me/ %{http_code}\n' "$ARCA_URL/v1/files/me/"
```

The four probes and the document answer 200, and `/version` names the
release you installed. The last request carries no token and must answer
401: a refusal from the API is the proof that your ingress routes the base
path to Arca and not only the probes. A 404 there means the ingress does
not.

Then write and read one object with a token from your issuer:

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/plain' \
  --data-binary 'hello' "$ARCA_URL/v1/files/me/files/hello.txt"
curl -H "Authorization: Bearer $TOKEN" "$ARCA_URL/v1/files/me/files/hello.txt"
```

The conformance suite is the fuller answer to whether an installation
behaves like Arca. It runs from a checkout against any installation with
tokens from your issuer; [testing](internals/testing.md#the-conformance-suite)
has the command.

## Alerts

`deploy/base/prometheusrule.yaml` is applied beside the base, not by it: a
`PrometheusRule` needs the Prometheus operator, which Arca does not require.
If you run the operator:

```sh
kubectl -n arca apply -f deploy/base/prometheusrule.yaml
```

## Next

[`operations.md`](operations.md) is what to do after this: checking an
installation, upgrades, rollbacks, verifying a release, and what happens
when a store goes away. [`configuration.md`](configuration.md) lists every
variable.
