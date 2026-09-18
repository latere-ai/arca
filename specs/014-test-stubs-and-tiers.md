---
title: "Test stubs and tiers: the stubs, the unit tier, the store tier, the e2e tier, make run, the CI jobs"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-object-store.md
  - specs/004-metadata-store.md
  - specs/006-identity.md
  - specs/011-webhooks.md
  - specs/013-api.md
affects: [test/stubs/, test/e2e/, Makefile, compose.yaml, Dockerfile.stubs, .github/workflows/verify.yml, .lateregate.yaml, docs/]
effort: medium
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Test stubs and tiers

## Overview

Arca stands in front of two stores it does not own and behind two
endpoints it does not own. A test that replaces all four with fakes
proves nothing about an installation; a test that needs all four running
cannot be the gate on every push. So the suite is three tiers. The unit
tier runs with the Go toolchain and nothing else and is what the shared
gate runs. The store tier runs `internal/blob` and `internal/store`
against a real MinIO and a real Postgres, because a bucket's conditional
create and a database's transaction semantics are the two things a fake
gets wrong. The e2e tier runs `arcad` as a process against those two
stores and against a stub issuer and a stub authorizer, which is every
dependency an installation has.

The stubs are in the tree and are a binary, `arca-stubs`, so the tiers
and `make run` start the same code, and so a contributor who clones the
repository gets a working installation with one command and no account
anywhere.

## Design

### The stubs

One package per stub under `test/stubs/`, each with a handler and a test
that drives every flag, and `test/stubs/cmd/arca-stubs` serving all three
on three ports.

| Stub | Serves | Behaviour |
|---|---|---|
| issuer | `/.well-known/openid-configuration`, `/jwks`, `POST /mint {sub, aud?, exp?, iat?, authorization_details?}` | a real OIDC issuer over an RS256 key set; mints any subject asked; `-alg es256` adds a second key and signs with it, for the ES256 case and the start-up key-set check of [[006-identity]]; `iat` and `exp` are settable so the 24 hour token-age rule is testable; `authorization_details` is passed through so a narrowed personal key is minted here and nowhere else. A token signed with an algorithm outside RS256 and ES256 is built by the verifier's own test, not offered here |
| authorizer | the envelope of [[006-identity]], over the stub `latere.ai/x/pkg/authz` ships | allows everything except the probe resource, the id `probe` of kind `Space`, which is always denied, so `arcad check` has something to check; `-deny <action>` or the request header `X-Stub-Deny: <action>` refuses one action; `-filter <json>`, `-limits <json>`, and `-ttl <seconds>` ride every allow, so the quota override of [[010-quotas-events-and-reaper]] and the list filter of [[013-api]] are exercised; `-fail-mode timeout\|malformed\|status:<code>\|no-allow\|conn-drop` produces each failure [[006-identity]] names, with `conn-drop` closing before a response line so the one retry runs; every request is recorded and served at `GET /requests` so a tier asserts the action and the resource fields a handler asked with |
| sink | a webhook receiver for [[011-webhooks]] | verifies the signature against the configured key and refuses a stale timestamp; `-fail-first N` answers 503 to the first N deliveries so the backoff and the retirement run; `-status <code>` answers that to the next one; stores deliveries and serves them at `GET /deliveries` in order per subscription |

Neither store is stubbed. A fake bucket that accepts `If-None-Match: *`
whenever a real one would is the bug invariant 1 of [[001-architecture]]
is written against, and a fake Postgres that serialises the way the
driver does is the reaper's failure mode. The tiers use the real ones.

`Dockerfile.stubs` builds the image [[016-release-and-installation]]
publishes, so the conformance suite of [[017-conformance-suite]] runs
against an installation with no Go toolchain beside it.

### The stack

One `compose.yaml` at the repository root, owned by the Makefile, holds
Postgres 18 and MinIO, plus a one-shot client container that creates the
bucket. It is the only definition of the stack: `make up` starts it,
`make down -v` removes it, CI starts it with the same file, and `make
run` and both service tiers read the same endpoints from it.

Testcontainers was the alternative and is not used. It would put a
container runtime on the test process's own path, which is a second way
to describe the same stack beside the one `make run` needs anyway, and
it would make the store tier's failure mode a Go stack trace rather than
`docker compose` output. The cost of the choice is that a tier cannot
start its own stack and must skip when the stack is absent, which is the
behaviour below. Nothing in the tree imports a container library, so the
choice is reversible in one package.

Ports are published on loopback and derive from the checkout's directory
name, so two clones run side by side. State lives in named volumes the
compose project owns; `make clean` removes them with the project.

| Variable | Default | Read by |
|---|---|---|
| `E2E_DATABASE_URL` | unset | the store and e2e tiers; the Postgres of `compose.yaml` |
| `E2E_S3_ENDPOINT` | unset | the same; MinIO's endpoint |
| `E2E_S3_KEY`, `E2E_S3_SECRET` | `minioadmin` | the same |
| `E2E_S3_BUCKET` | `arca-test` | the bucket the one-shot client creates |

These are test variables and not configuration: they are not `ARCA_*`,
they are not read by `arcad`, and they are not in the table of
[[002-repository-scaffold]]. A tier reads them, builds the `ARCA_*`
values from them, and passes those to the server it starts.

### The tiers

| Tier | Command | Needs | Runs |
|---|---|---|---|
| unit | `go tool lateregate test` | the Go toolchain | the gate, every push |
| store | `go test -tags=tiers -run '^TestStore' ./internal/blob/... ./internal/store/...` | `E2E_DATABASE_URL`, `E2E_S3_ENDPOINT` | `make test-store`, and one CI job |
| e2e | `go test -tags=tiers -run '^TestE2E' ./test/e2e/...` | the same | `make test-e2e`, and one CI job |

Two selectors, both needed. The build tag keeps every file that reaches
a service out of the untagged suite, which is why `hermetic.allow` in
`.lateregate.yaml` is empty and stays empty: the gate's suite forks
nothing and dials nothing. The name prefix selects the tier within the
tag, so one job's log names the tests that ran.

A tier whose variables are unset calls `t.Skip` with the remediation in
the message, `set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)`, so a
plain `go test ./...` on a clean clone is green with no services. A tier
that has its variables runs: it binds every listener it starts on `:0`,
keeps files under `t.TempDir()`, creates a schema of its own per package
so two packages do not share a migration state, and writes under a
bucket prefix of its own, `test-<ulid>/`, which it removes in
`t.Cleanup`. Nothing a tier does reads a developer's real
configuration, and no tier reads `ARCA_*` from the environment.

The e2e harness starts, per package: the three stubs in process, the
migrations, and `arcad serve` as an `httptest.Server` over the real
handler tree, with `ARCA_OIDC_ISSUERS` naming the stub issuer,
`ARCA_OIDC_INSECURE_ISSUERS=true` because the stub issuer serves
`http://` and [[006-identity]] otherwise admits a plaintext issuer only
on loopback, `ARCA_AUTHORIZER_URL` naming the stub authorizer, and
`ARCA_BUCKET_PREFIX` naming the tier's own prefix. One
e2e test runs `arcad` as a real process rather than an in-process
server, so the subcommand dispatch, the configuration reader, the two
listeners, and `arcad check` are exercised as an operator meets them.

### make run

A clean clone to a working installation, in this order, because the
bucket must exist before the server's readiness check passes and a token
must be minted before anything can be written:

1. Build `arcad` and `arca-stubs`.
2. `docker compose up -d --wait`: Postgres, MinIO, and the bucket.
3. Start `arca-stubs`; wait for the issuer's key set.
4. `arcad migrate` against the compose database.
5. Start `arcad serve` with `ARCA_OIDC_ISSUERS` and
   `ARCA_OIDC_INSECURE_ISSUERS` naming the stub issuer,
   `ARCA_AUTHORIZER_URL` naming the stub authorizer,
   `ARCA_ADMIN_SUBJECTS` holding the rendered `<issuer>|dev`, and the
   bucket and database of the stack; wait for `/readyz`.
6. `arcad check` against the running installation, so the first thing a
   contributor sees is four `ok` lines ([[012-administration]]).
7. Mint a token for `dev` at the issuer's `/mint` and print `export
   ARCA_URL=... ARCA_TOKEN=...`, a `curl` that puts one object, and a
   `curl` that reads it back.

`make run-down` stops the server and the stubs and leaves the stack up,
so a failed run is debuggable. `make clean` removes the compose project
with its volumes and the build output. Ports come from the directory
name, so two clones run side by side.

### Coverage

90% per package, the shared gate's `cover`, with the tiers' profiles
included: the gate takes `-profile` once per tier, so the unit, store,
and e2e profiles are read together and a package whose only exercise is
the e2e tier counts. `make check` runs the unit tier alone and reports
coverage over it; `make check-all` runs all three and is what a
contributor runs before pushing something that touches a store.

A package below the bar fails the gate. `test/stubs` is held to the same
bar, because a stub with an untested flag is a tier that silently stops
testing what it claims to.

### CI

`verify.yml` gains two jobs, both on GitHub's hosted runners, which are
free for a public repository and give each job a fresh machine.

| Job | Steps |
|---|---|
| `store` | checkout, Go, `docker compose up -d --wait`, `go test -tags=tiers -race -run '^TestStore' -covermode=atomic -coverprofile=store.out ./internal/blob/... ./internal/store/...`, upload the profile |
| `e2e` | the same stack, `go test -tags=tiers -race -run '^TestE2E' -covermode=atomic -coverprofile=e2e.out ./test/e2e/...`, upload the profile |

The `gate` job reads the two profiles beside its own and enforces the
bar over all three, so the number in the log is the number a reader of
this spec expects. Both jobs run on every push and every pull request.
Neither is self-hosted: the service Arca replaces ran its equivalent on
one company's VM for a warm cache and a private repository, and neither
reason survives.

### What arrives from Drive

| From | To | What changes |
|---|---|---|
| `drive/test/e2e/harness_test.go` | `test/e2e/harness.go` | the shared harness, the `t.Skip` on the `E2E_` variables, and the `issuertest` issuer survive; the harness starts the stub authorizer too, mints by subject rather than by claim (`OrgID`, `Roles`, `PrincipalType` are gone), and no longer shares one bucket name across packages |
| `drive/docker-compose.yml` | `compose.yaml` | Postgres 18 and the pinned MinIO release survive; the bucket is created by the stack rather than by the harness, ports derive from the directory name, and the project name does too |
| `drive/Makefile`'s `e2e`, `e2e-up`, `e2e-down` | `make test-store`, `make test-e2e`, `make up`, `make down` | one target per tier instead of one target that runs everything |
| `drive/.github/workflows/ci.yml`'s `e2e` job | the two jobs above | hosted runners instead of a self-hosted VM; compose instead of a service container beside a `docker run` step; the coverage gate moves to the shared gate reading three profiles |
| `latere.ai/x/pkg/authkit/issuertest` | the issuer stub | it becomes a binary as well as a package, and gains `/mint` over HTTP so `make run` and a non-Go consumer can get a token |
| nothing | the authorizer stub, the sink stub, `arca-stubs`, `make run` | new. The service Arca replaces decided access itself and had no authorizer to stub, and it had a hosted deployment instead of a one-command local installation |

## Not in this spec

The suite an installation must pass, which is importable and runs
against any server ([[017-conformance-suite]]). The release pipeline
that publishes `arca-stubs` ([[016-release-and-installation]]). What each
tier asserts, which every other spec's acceptance criteria own.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Each stub serves its contract and every flag in its row is driven | `TestIssuerStub`, `TestAuthorizerStub`, `TestSinkStub` |
| 2 | The authorizer stub denies the probe resource for every subject and every flag combination, and records the action and resource fields of each request | `TestAuthorizerStub`, and [[012-administration]]'s `check` test |
| 3 | The issuer stub mints a token carrying `authorization_details`, an `iat` of a chosen age, and an ES256 signature under `-alg es256` | `TestIssuerStub` |
| 4 | The sink refuses a delivery whose signature does not verify and one whose timestamp is stale, and `-fail-first` drives the backoff to retirement | `TestSinkStub`, and [[011-webhooks]]'s delivery test |
| 5 | `go test ./...` on a clean clone with no services is green, and every tier test skips with the remediation in its message | `TestTiersSkipWithoutServices`, run with the variables cleared |
| 6 | No file in the untagged suite dials a socket or forks a process, and `hermetic.allow` is empty | the `hermetic` gate |
| 7 | Every tier binds `:0`, keeps files under `t.TempDir()`, uses a schema and a bucket prefix of its own, and leaves neither behind | `TestTiersAreIsolated`, plus a bucket listing after the store tier |
| 8 | `make run` on a clean clone completes the seven steps, `arcad check` prints four `ok` lines, and the printed `curl` puts and reads one object | `TestMakeRun` in the e2e tier |
| 9 | Two clones run `make run` at once without a port or volume collision | `TestMakeRunSideBySide` |
| 10 | One e2e test drives `arcad` as a process: the subcommands, the two listeners, the probes, and `check` | `TestE2EBinary` |
| 11 | Coverage over the three profiles is at least 90% for every package, `test/stubs` included | the `cover` gate with three `-profile` flags |
| 12 | `verify.yml` has one job per service tier with the command from the table, on hosted runners | `TestWorkflowJobsMatchTheTable`, reading `verify.yml` |
