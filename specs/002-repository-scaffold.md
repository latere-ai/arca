---
title: "Repository scaffold: module, binary, configuration, quality gate, image, workflow"
status: testing
track: core
depends_on: []
affects: [cmd/arcad/, internal/config/, internal/version/, Makefile, .lateregate.yaml, Dockerfile, .github/workflows/, .githooks/, docs/]
effort: small
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Repository scaffold

## Overview

A compiling, testable repository that passes its whole quality gate
before any storage code exists: the Go module, the `arcad` binary with
its two listeners and probes, typed configuration, the gate, the
developer image, the verify workflow, and the community files an open
source repository is judged by. The shape is chosen so the repository
reads as an ordinary open source Go service to a newcomer and so every
later spec lands into a tree that already enforces the bar.

This spec is also the configuration reference. It owns every `ARCA_*`
variable the server reads, including the ones later specs give a
meaning to, so an operator has one table. It fixes the layout
[[001-architecture]] names without waiting for that design to settle,
because a repository that passes its gate is needed to write and test
anything else.

## Current state

Built and in the tree on 2026-09-18. `cmd/arcad` serves the probes,
`internal/config` reads two variables, `internal/version` carries the
build identity, `.lateregate.yaml` configures the shared gate pinned as
a Go tool, and `.github/workflows/verify.yml` calls the shared pipeline.
The gate passes locally with fourteen gates; the spec moves to
`complete` with the first green verify run on `main`, recorded in the
Outcome.

## Design

### Layout

[[001-architecture]] holds the full layout. This spec builds the
entries below; every other entry names the spec that builds it.

```
cmd/arcad/              main: the subcommand dispatcher, configuration, listeners, run group
internal/config/        typed configuration from the environment; every problem in one message
internal/version/       build identity set by -ldflags
.lateregate.yaml        the gate's repository-specific values: cover, spec, hermetic, depcheck, license, identity
.github/workflows/      verify.yml: the shared gate, a tidy module file, the developer image builds
.githooks/              pre-commit and pre-push, both delegating to the gate
Makefile                check (the default), build, run, fmt, hooks, clean
Dockerfile              the developer image; the release image of 016 reuses its runtime stage
docs/README.md          the index for operators and builders
```

### Binary and listeners

| Listener | Default address | Serves |
|---|---|---|
| public | `:8080` (`ARCA_PUBLIC_ADDR`) | `GET /` with the build identity, `/v1/*` (013), and `GET /livez`, `GET /readyz`, `GET /version` so a release smoke reaches them through an ingress |
| internal | `:8081` (`ARCA_INTERNAL_ADDR`) | the four probes below, for the cluster |

The probes are `latere.ai/x/pkg/health`, mounted whole on the internal
listener and path by path on the public one.

| Method | Path | Body |
|---|---|---|
| GET | `/livez` | 200 `ok`, touches no dependency |
| GET | `/readyz` | 200 `ok` when every check passes; 503 `not ready: <check>: <error>` otherwise, and `not ready: draining: shutting down` during shutdown; text, the developer register |
| GET | `/version` | `{"version","commit","build_time"}` from `internal/version`, set by `-ldflags` |
| GET | `/metrics` | the `latere.ai/x/pkg/metrics` registry in the Prometheus text format (018); internal listener only |

Readiness runs its checks with a 2 second budget: `draining` today, the
bucket's `HeadBucket` once [[003-object-store]] lands, the database's
ping once [[004-metadata-store]] does. `arcad` keeps no state on local
disk, so there is no disk check and no data directory. Shutdown on
`SIGTERM` or `SIGINT`: readiness answers 503 at once, the process waits
a 3 second drain delay, then closes the HTTP servers with a 60 second
grace period. A Deployment sets `terminationGracePeriodSeconds` 90.

`arcad -version` prints `arcad <version> (<commit>, <date>)` and exits
0; a bad flag exits 2; a configuration or start-up failure exits 1 with
one line on stderr prefixed `arcad:`. The first argument that does not
start with `-` selects a subcommand; without one the binary serves.

| Subcommand | Reads | Does | Spec |
|---|---|---|---|
| `serve` (default) | the whole table | the API, the stores, the probes | this spec |
| `reap` | the store variables and `ARCA_REAP_*` | the reconciler as a process of its own | 010 |
| `migrate` | `ARCA_DATABASE_URL` | applies the migrations and exits | 004 |
| `check` | the whole table | one line per requirement, exit 1 on any failure | 012 |

An unknown subcommand is a usage error, exit 2. `reap`, `migrate`, and
`check` are unknown subcommands until their specs land.

### Configuration

Every variable the server reads, read once at start-up through one
lookup function so a test passes a map. A problem is reported with
every other problem in one message sorted by variable name, so an
operator fixes a deployment in one round. A blank value is unset.

| Variable | Default | Meaning | Spec |
|---|---|---|---|
| `ARCA_PUBLIC_ADDR` | `:8080` | the public listener | this spec |
| `ARCA_INTERNAL_ADDR` | `:8081` | the internal listener; must differ from the public one unless both are port 0 | this spec |
| `ARCA_PUBLIC_URL` | required | the address clients reach the public listener at; the base of every URL the server writes | 013 |
| `ARCA_BUCKET` | required | the bucket name | 003 |
| `ARCA_BUCKET_ENDPOINT` | the region's default | the S3 endpoint | 003 |
| `ARCA_BUCKET_REGION` | required | the signing region | 003 |
| `ARCA_BUCKET_PREFIX` | `arca/` | the prefix under which every key is written | 003 |
| `ARCA_BUCKET_PATH_STYLE` | `false` | path-style addressing, for MinIO and stores without virtual hosts | 003 |
| `ARCA_BUCKET_ACCESS_KEY`, `ARCA_BUCKET_SECRET_KEY` | the SDK's chain | static credentials, when the environment has no other | 003 |
| `ARCA_PUBLIC_CDN_URL` | unset | the prefix a public object's redirect points at, when one fronts the bucket | 003 |
| `ARCA_DATABASE_URL` | required | the Postgres connection string | 004 |
| `ARCA_MAX_UPLOAD_BYTES` | `5368709120` | the largest object accepted | 007 |
| `ARCA_INLINE_BYTES` | `16777216` | the largest object streamed through the server; above it, parts go direct | 007 |
| `ARCA_DEFAULT_QUOTA_BYTES` | `10737418240` | a space's quota until the authorizer or an administrator sets one | 010 |
| `ARCA_OIDC_ISSUERS` | required | the issuers whose tokens are verified, comma separated | 006 |
| `ARCA_OIDC_AUDIENCE` | `arca` | the audience every token must carry | 006 |
| `ARCA_OIDC_INSECURE_ISSUERS` | `false` | admit an `http://` issuer off loopback; for the test tiers | 006 |
| `ARCA_AUTHORIZER_URL` | unset | the authorizer endpoint; unset selects the owner policy | 006 |
| `ARCA_AUTHORIZER_TOKEN` | unset | the bearer the authorizer expects | 006 |
| `ARCA_ADMIN_SUBJECTS` | unset | the subjects the owner policy treats as administrators, comma separated | 006 |
| `ARCA_WEBHOOK_SIGNING_KEY` | required when webhooks are enabled | the key deliveries are signed with | 011 |
| `ARCA_REAP_INTERVAL` | `5m` | how often the reconciler runs | 010 |
| `ARCA_TRASH_RETENTION` | `720h` | how long a trashed object is restorable | 005 |
| `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` | unset | where traces and metrics go; unset exports nothing | 018 |
| `ARCA_REQUESTS_PER_MINUTE` | `600` | the token bucket per subject after authentication; `0` disables it | 015 |
| `ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE` | `60` | the token bucket per client address before authentication, which bounds bad tokens and link token guessing | 015 |
| `ARCA_TEST_DRIFT` | unset | makes the server drift from the contract in one named way, so the conformance suite is proved to catch it; refused outside the test build | 017 |

The rows for later specs are reference entries; the spec named builds
what reads each.

### The quality gate

`make` runs `go tool lateregate`, the family's shared gate pinned in
`go.mod`. `.lateregate.yaml` holds only what is specific to this
repository: the coverage prefix, the spec lifecycle of [specs/README.md](README.md),
the hermetic allow list (empty: `arcad` forks nothing), the dependency
allow list of invariant 9, the licence identifier, and the identity block
of the family's shape with `role: core`. Two identity rules, `verifier`
and `authorizer`, carry dated waivers until [[006-identity]] mounts the
shared packages; the waivers name that spec and expire on 2026-10-31.

### Workflow and image

`verify.yml` runs on every push to `main` and every pull request: the
shared gate on GitHub's hosted runners, `go mod tidy -diff`, and a
build of the developer image that must report its version. The
repository is public, so the hosted runners cost nothing. The tiers
that need MinIO and Postgres beside them are [[014-test-stubs-and-tiers]]'s.

The developer image compiles `arcad` from the checkout on
`golang:1.27-alpine` and runs it on `distroless/static`, non-root,
ports 8080 and 8081, no volume: the server has no local state.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | `arcad -version` prints the identity and exits 0; an unknown subcommand exits 2; a bad flag exits 2 | `cmd/arcad` tests |
| 2 | A bad configuration exits 1 with one line naming every problem, sorted | `cmd/arcad` and `internal/config` tests |
| 3 | Both listeners answer `/livez`, `/readyz`, `/version`; the public one answers `/` and not `/metrics` | `TestServeAnswersTheProbesOnBothListenersAndStopsCleanly` |
| 4 | Readiness fails once draining begins, and a stop signal ends the process with exit 0 | the same, and `TestReadinessFailsOnceDrainingBegins` |
| 5 | An occupied address exits 1 naming the variable | `TestOccupiedAddressExitsOne` |
| 6 | The gate passes with every gate on, or a dated waiver naming its spec | `go tool lateregate` locally and the `gate` job |
| 7 | The developer image builds and reports its version | the `image` job |
| 8 | The first push to `main` is green on every job | the verify run, cited in the Outcome |

## Outcome

Written when criterion 8 holds.
