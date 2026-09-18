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

Trying it out before there is anything to install takes one command,
`make run`: the server on loopback, serving its probes. Once the
[object store](../specs/003-object-store.md) and the
[metadata store](../specs/004-metadata-store.md) land, `make run` also
starts MinIO and Postgres beside it, so a clean clone stores its first
object in one command.

## Building against it

| Page | |
|---|---|
| API | the endpoints and error codes in the [API spec](../specs/013-api.md), and the authorizer an operator writes in the [identity spec](../specs/006-identity.md) |
| The packages | the [architecture spec](../specs/001-architecture.md) names the exported packages and what each promises |

## Changing it

The design, and the reasoning behind each decision, is in
[`specs/`](../specs/README.md). [`CONTRIBUTING.md`](../CONTRIBUTING.md) is
how to build and test a change.
