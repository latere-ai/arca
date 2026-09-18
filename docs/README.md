# Documentation

For people who run `arcad`, store and share objects through it, or build
a platform on the packages.

## Running it

| Page | |
|---|---|
| [Install](install.md) | from an empty cluster to a serving installation: the bucket, the database, the issuers, the authorizer, the schema, the overlay, and the two commands that prove it |
| [Operations](operations.md) | after the install: upgrades and rollbacks, what a version number promises, how to check a release's signatures, and what happens when a store goes away |
| Configuration | the table in the [repository scaffold spec](../specs/002-repository-scaffold.md) until `docs/configuration.md` is generated from the code |
| Identity | the [section in the README](../README.md#identity) is what an operator needs: the issuers, the authorizer, and the owner policy that applies without one |

`arcad` needs a bucket and a Postgres database, so trying it out starts
both. One command does all of it:

```sh
make run
```

It builds `arcad` and the test stubs, starts Postgres and MinIO from
`compose.yaml` with the bucket created, applies the migrations, starts the
server, and prints the address and a token minted at the stub issuer. The
ports derive from the directory name, so two clones run side by side, and
everything is published on loopback.

| Command | |
|---|---|
| `make run` | the stack, the stubs, the migrations, and the server, in that order |
| `make run-down` | stops the server and the stubs and leaves the stack up, so a failed run is debuggable |
| `make up`, `make down` | the stack alone |
| `make clean` | removes the stack with its volumes and the build output |

What answers today is the server's own surface: `GET /` with the build
identity, and `/livez`, `/readyz`, `/version` on both listeners.
Readiness reaches both stores, so a 200 there means the bucket answered
its probe and the database holds every migration the binary carries. The
`/v1` routes a token is for arrive with the [API spec](../specs/013-api.md).

Without Docker or Podman the server still runs: point `ARCA_BUCKET_*` and
`ARCA_DATABASE_URL` at a bucket and a database of your own, apply the
migrations with `arcad migrate`, and start `arcad`.

## Building against it

| Page | |
|---|---|
| API | the endpoints and error codes in the [API spec](../specs/013-api.md), and the authorizer an operator writes in the [identity spec](../specs/006-identity.md) |
| The packages | the [architecture spec](../specs/001-architecture.md) names the exported packages and what each promises |

## Changing it

The design, and the reasoning behind each decision, is in
[`specs/`](../specs/README.md). [`CONTRIBUTING.md`](../CONTRIBUTING.md) is
how to build and test a change.
