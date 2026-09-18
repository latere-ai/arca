# Arca

**Durable storage for people, agents, and sandboxes.** Files with versions
and trash, shares with a permission ladder, workspaces a sandbox mounts and
syncs back, usage per space, and an event log, over any S3 compatible
bucket and a Postgres database. Bytes go to the bucket and metadata to the database;
the server is stateless and any replica serves any request.

Latere runs Arca as the storage section of its platform console, behind
its own API origin; Arca is not a hosted service on its own. Anyone with
a bucket, a Postgres database, and an OIDC issuer can run their own.

[![CI](https://github.com/latere-ai/arca/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/arca/actions/workflows/verify.yml)
[![Release](https://img.shields.io/github/v/release/latere-ai/arca)](https://github.com/latere-ai/arca/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/arca)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## The problem

A platform that runs agents and sandboxes needs a place for what is
neither a commit nor a sandbox's local disk: the file a person uploads,
the artifact an agent produces, the working tree a sandbox needs back
tomorrow, the object an application serves. A git host holds commits.
A sandbox's volume dies with the sandbox. An object store holds bytes
but knows nothing about who owns them, who may read them, how much a
space may hold, or what happened to them last week.

Arca is that place. It gives every principal a space, every object a
version history and a trash, every share a permission, every space a
usage count the platform decides a limit over, and every change an
event a consumer can tail.

## How it works

- **The bucket holds bytes, the database holds meaning.** A write goes
  to the bucket first and to the database second; a delete goes to the
  database first and to the bucket second. A failure between the two
  leaves an invisible object, and a reaper removes it. Nothing is
  acknowledged before the bytes are durable.
- **Bytes stay off the hot path.** A private download is a redirect to
  a short-lived presigned URL; a large upload goes to the bucket in
  parts the client sends directly. The server sees metadata,
  authorization, and small streams.
- **One space per principal, two planes.** A person or a service owns
  a space; inside it, files and workspaces are two prefixes with their
  own rules, not separate systems.
- **A workspace is a lease.** A sandbox attaches to a workspace and
  holds the writer lock while it materializes the tree and syncs it
  back; a lease that is not renewed expires, so a crashed sandbox does
  not hold a workspace hostage.
- **Any replica serves any request.** No node-local state, no leader.
  Scale the deployment, not the design.

## Identity

Arca verifies and asks. Every request carries a token from an OIDC
issuer you list; Arca verifies it against the issuer's key set and calls
the issuer for nothing else. Whether the caller may act is a question
Arca sends to an authorizer endpoint you write, with the verified claims
and the action; without one, the built-in owner policy applies: an owner
acts on its own space, an administrator on every space. The contract is
the family's, shared with [Cella](https://github.com/latere-ai/cella),
[Lux](https://github.com/latere-ai/lux), and
[Origo](https://github.com/latere-ai/origo), so one authorizer answers
for all four.

## Status

The stores are in the tree: `arcad` reaches an S3 compatible bucket and
a Postgres database, migrates its schema, answers its probes from both,
and passes its quality gate and its store and end-to-end tiers against
MinIO and Postgres. The release pipeline and the deploy manifests are
written. The API, identity, files, shares, workspaces, events and the
rest are specified and arrive in the order the specs number them; the
migration spec says how the code arrives from the service it replaces.

## Documentation

[`docs/`](docs/README.md) is for people who run `arcad` or build against
it. [`specs/`](specs/README.md) is the design, one spec per module, for
people changing it. [`CONTRIBUTING.md`](CONTRIBUTING.md) is how to build
and test a change.
