# Documentation

For people who run `arcad`, build against its API, or change it.

## Running it

Read these in order the first time.

| Page | |
|---|---|
| [Install](install.md) | from a Kubernetes cluster to a serving installation: the bucket, the database, the issuers, the authorizer, the schema, the overlay, and how to check it |
| [Configuration](configuration.md) | every environment variable `arcad` reads, its default, and the subcommands |
| [Operations](operations.md) | checking an installation, what a release is and how to verify one, upgrades and rollbacks, what happens when a store goes away, metrics, alerts, traces, and logs |

Trying Arca on your own machine first takes one command, `make run`; the
[README](../README.md#try-it-in-a-few-minutes) says what it needs and what
it prints.

## Building against it

| Page | |
|---|---|
| [API](api.md) | spaces and planes, authentication, every route with the action it asks, lists, conditional requests, errors, uploads, shares and links, workspaces, events, administration, and the authorization endpoint you write |
| [OpenAPI](../api/openapi.yaml) | the same surface as an OpenAPI 3.1 document; every installation serves it at `GET /openapi.json` |
| [Conformance](internals/testing.md#the-conformance-suite) | the suite that checks an installation, or another implementation, against the API |

## Changing it

| Page | |
|---|---|
| [Internals](internals/README.md) | how Arca is built: the packages, the two stores and the order they are written in, uploads, workspaces, the event log, and the reconciler |
| [Testing](internals/testing.md) | the test tiers, the stubs, and how to run each |
| [Releasing](internals/releasing.md) | how a release is cut and what the pipeline publishes |
| [`CONTRIBUTING.md`](../CONTRIBUTING.md) | how to build, test, and send a change |
| [`specs/`](../specs/README.md) | the design records, one per module, with the reasoning behind each decision |
