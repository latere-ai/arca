# Configuration

Every environment variable `arcad` reads, whether it is required, its
default, and what it does.

`arcad` reads its whole configuration once at start-up. A variable that is
missing or malformed stops the process with one message that names every
problem at once, sorted by variable, so a deployment is fixed in one round.
`arcad check` reads the same configuration and then reaches the bucket, the
database, the issuers, and the authorizer; see
[operations](operations.md#checking-an-installation).

A blank value is the same as an unset one. Durations are written the way Go
parses them: `5m`, `2h30m`, `720h`. Sizes are whole numbers of bytes. Lists
are comma separated, and blank entries are dropped.

## Listeners and addresses

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_PUBLIC_URL` | yes | none | The origin clients reach the public listener at, such as `https://arca.example.com`, with its scheme and no path. Every URL the server writes is built on it, and it is the server the served OpenAPI document names. |
| `ARCA_BASE_PATH` | no | `/v1` | The base every API route is registered under. Leave it unset when Arca owns its origin. Set it when Arca shares an origin with other services and has been given a prefix, such as `/v1/storage`: the value begins with `/`, has no trailing slash, and its first segment is `v1`. The probes, `GET /`, and `GET /openapi.json` stay at the origin root whatever it is. |
| `ARCA_PUBLIC_ADDR` | no | `:8080` | The address the public listener binds: the API, `GET /`, `GET /openapi.json`, and the probes. |
| `ARCA_INTERNAL_ADDR` | no | `:8081` | The address the internal listener binds: the probes and `/metrics`. It must differ from `ARCA_PUBLIC_ADDR`. Do not route it. |

## The bucket

Object bytes live in one bucket at any S3 compatible endpoint. The key of an
object derives from its id, never from its path, so a move or a rename
changes a database row and no key.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_BUCKET` | yes | none | The bucket. It must exist; Arca never creates one. |
| `ARCA_BUCKET_REGION` | yes | none | The region requests are signed for, as the provider names it. |
| `ARCA_BUCKET_ENDPOINT` | no | derived from the region | The S3 endpoint, an absolute `http` or `https` URL. Unset, the SDK derives the AWS endpoint from the region. Set it for every other provider. |
| `ARCA_BUCKET_PREFIX` | no | `arca/` | The prefix every key carries. A missing trailing slash is added; a leading slash is refused; the value holds letters, digits, and `. _ - /`. Several installations share one bucket by taking a prefix each. |
| `ARCA_BUCKET_PATH_STYLE` | no | `false` | `true` puts the bucket in the path rather than in the host name. MinIO and any endpoint reached by IP need it. |
| `ARCA_BUCKET_ACCESS_KEY` | no | unset | The access key id. Set together with `ARCA_BUCKET_SECRET_KEY` or not at all. Both unset, the SDK's credential chain applies: environment, shared files, instance or workload roles. |
| `ARCA_BUCKET_SECRET_KEY` | no | unset | The secret access key. Keep it in a Secret. |
| `ARCA_PUBLIC_CDN_URL` | no | unset | A base URL that serves the bucket's public objects, such as a CDN in front of it. A read of an object a public link marked redirects to `<this URL>/<key>`, which carries no expiry. Unset, such a read redirects to an ordinary presigned URL. |

## The database

Metadata lives in one Postgres database. It is the authority on whether an
object exists.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_DB_URL` | yes | none | The Postgres connection string, beginning `postgres://` or `postgresql://`. `arcad migrate` reads this variable and nothing else, so a migration job needs no other configuration. |

The deployment manifests read it from the key `ARCA_DATABASE_URL` of the
`arcad-database` Secret and map it onto `ARCA_DB_URL`, so a Secret written
from `deploy/bootstrap/secrets.example.yaml` keeps working.

## Identity and authorization

Every API request carries a bearer token from an issuer you list. Whether
the caller may act is decided by an authorization endpoint you run, or by the
built-in owner policy when you run none. The [API guide](api.md#authorization)
describes both.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_OIDC_ISSUERS` | yes | none | The issuer URLs whose tokens are accepted, comma separated. Each must serve OpenID discovery and a key set. An issuer is `https`, or `http` on a loopback address. |
| `ARCA_OIDC_AUDIENCE` | no | `arca` | The audiences an accepted token may carry, comma separated. A token is accepted when its `aud` names any entry. The first entry is the primary, the name this installation answers to. List a second entry when a gateway in front of Arca mints credentials addressed to its own origin. |
| `ARCA_OIDC_INSECURE_ISSUERS` | no | `false` | `true` admits an `http` issuer that is not on loopback. For a test stack only. |
| `ARCA_AUTHORIZER_URL` | no | unset | Your authorization endpoint, an absolute `http` or `https` URL. Arca asks it before every action. Unset, the built-in owner policy decides. |
| `ARCA_AUTHORIZER_TOKEN` | with `ARCA_AUTHORIZER_URL` | none | The bearer Arca presents to that endpoint. Setting the URL without it is a start-up failure. |
| `ARCA_ADMIN_SUBJECTS` | no | unset | Subjects the owner policy treats as administrators of every space, comma separated, each written `<issuer>\|<sub>`. Read only when no authorizer endpoint is set; with one, an administrator is whoever that endpoint says. |

## Limits

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_REQUESTS_PER_MINUTE` | no | `600` | Requests one authenticated subject may send one replica in a minute. `0` turns the limit off. |
| `ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE` | no | `60` | Requests one client address may send one replica in a minute on the routes that carry no bearer (public links) and on requests whose token was refused. `0` turns the limit off. The address is the connection's peer, so behind a proxy that does not preserve the client's address every such caller shares one budget. |
| `ARCA_MAX_UPLOAD_BYTES` | no | `5368709120` (5 GiB) | The largest object the server accepts by any route. |
| `ARCA_INLINE_BYTES` | no | `16777216` (16 MiB) | The largest object streamed through the server. A `PUT` above it is refused; an upload session sends the parts straight to the bucket instead. A read above it is a redirect to a presigned URL. It cannot exceed `ARCA_MAX_UPLOAD_BYTES`. |

## Housekeeping

A reconciler removes what a failure between the bucket and the database left
behind, and expires what has a deadline. See
[operations](operations.md#running-the-reconciler-on-its-own).

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_REAP_INTERVAL` | no | `5m` | How often `arcad serve` runs the reconciler. `0` turns the in-process loop off, for an installation that runs `arcad reap` on its own. |
| `ARCA_TRASH_RETENTION` | no | `720h` (30 days) | How long a trashed object stays restorable before it is purged. The same window applies to a soft deleted workspace and to how long an expired grant is kept. Must be above zero. |

## Telemetry

Metrics are always served on the internal listener at `/metrics`. Traces and
log records leave the process only when an endpoint is set.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` | no | unset | The OpenTelemetry collector traces and log records are exported to over OTLP. A value that is not an absolute `http` or `https` URL is a start-up failure. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | unset | The standard variable, read when `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` is unset, so a platform that injects it into every workload needs nothing set for Arca. Its shape is left to the exporter. |

## Testing only

| Variable | Required | Default | What it is |
|---|---|---|---|
| `ARCA_TEST_DRIFT` | no | unset | Makes the build answer one part of the API wrong, so the conformance suite can prove it notices. Empty in every deployment; the server logs it when set, and an unknown value is a start-up failure. |

## Subcommands

`arcad` with no subcommand, or `arcad serve`, runs the server.

| Subcommand | What it does | Reads |
|---|---|---|
| `serve` | the two listeners, the API, and the reconciler loop; `-version` prints the build and exits | everything above |
| `migrate` | applies the pending database migrations and exits | `ARCA_DB_URL` |
| `check` | reaches every dependency once, prints one line per requirement, and exits 1 on any failure | everything above |
| `reap` | the reconciler as a process of its own; `-once` runs one pass and exits, `-dry-run` reports and changes nothing | everything above |
