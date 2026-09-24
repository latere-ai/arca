# Testing

Arca stands in front of two stores it does not own and behind two endpoints
it does not own. A suite that fakes all four proves little about an
installation, and a suite that needs all four cannot gate every push. So
the tests are tiers, each with a clear list of what it needs.

| Tier | Needs | Runs | Command |
|---|---|---|---|
| unit | the Go toolchain | every package, with fakes for the stores and the endpoints | `make`, or `go test ./...` |
| store | Postgres and MinIO | the packages that reach a store, against the real ones | `make test-store` |
| e2e | Postgres, MinIO, and the stubs | `arcad` as a process, driven over HTTP | `make test-e2e` |
| conformance | an installation | the black-box suite against the API | `make test-conformance` |

`make check-all` runs the gate, the store tier, and the e2e tier, which is
what to run before pushing a change that touches a store.

## The unit tier

`make` runs `go tool lateregate`, the shared quality bar pinned in
`go.mod` and configured in `.lateregate.yaml`. `go tool lateregate list`
prints the gates it runs. Among them: formatting, the linter,
modernization, known vulnerabilities, the suite with and without the race
detector, the suite with only the Go toolchain on `PATH` (`hermetic`), the
suite from an empty working directory (`tempdir`), per-package coverage at
90% or more (`cover`), the license notice, the dependency allow list of
`./cmd/arcad` (`depcheck`), the identity shape shared with the sibling
projects, and the spec tree.

Every store is behind an interface with a fake beside it: `internal/blob`
has an in-memory store, and the query sets of `internal/store` are
interfaces each package fakes in its own `fakes_test.go`. The tests that
must see a real store carry the build tag `tiers` and a name starting
`TestStore` or `TestE2E`; without the tag they are not compiled, so
`go test ./...` on a clean clone passes with no services.

Two groups of unit tests guard files rather than code:

- `test/deploy` reads the Kubernetes manifests, the Dockerfiles, and the
  release workflow: every overlay resolves and sets `ARCA_PUBLIC_URL`, the
  base Deployment is confined (non-root, read-only root file system, no
  capabilities) and keeps credentials in Secrets, every third-party action
  is pinned by commit, and `deploy/prod` routes every `/v1` prefix the
  OpenAPI document serves.
- `test/threatmodel` holds `SECURITY.md` to the threat model: every
  commitment in `SECURITY.md` is a control in
  `specs/015-security-and-threat-model.md`, and every control names a test
  that exists.

Several documents are held to the code the same way. `api/openapi.yaml`
must equal a fresh render (`make openapi`), `deploy/base/prometheusrule.yaml`
must equal a fresh render of the alert table (`make rules`),
`docs/configuration.md` must have a row for every variable
`internal/config` reads and no other, and `docs/api.md` must have a row for
every route of the OpenAPI document and every error code.

## The store tier

```sh
make test-store
```

`make up` starts `compose.yaml`: Postgres 18 and MinIO, with a one-shot
container that creates the bucket. The project name and every port derive
from the checkout's directory name, so two clones run side by side, and
everything listens on loopback. Docker is used when it is on `PATH`,
Podman otherwise; `DEV_ENGINE` overrides it.

The tier then runs `go test -tags=tiers -race -run '^TestStore'` over
`internal/blob`, `internal/store`, `internal/events`, `internal/reaper`,
`internal/files`, `internal/uploads`, `tools/migrate-drive`, and
`tools/move-objects`, with the stack's addresses in `E2E_DATABASE_URL`,
`E2E_S3_ENDPOINT`, `E2E_S3_KEY`, `E2E_S3_SECRET`, and `E2E_S3_BUCKET`. A
store test without those variables skips and names what to run. These are
the tests that prove what a fake cannot: a bucket's conditional create and
multipart behavior, and Postgres's transaction and constraint semantics.

`make down` stops the stack and keeps its volumes, so a failure can be
inspected. `make clean` removes the stack, its volumes, and `out/`.

## The e2e tier

```sh
make test-e2e
```

It builds `out/arcad` and `out/arca-stubs`, starts the stack, and runs
`test/e2e` with `ARCA_BINARY` pointing at the built server. Each test starts
`arcad` as a process against the stack and the stubs and drives it over
HTTP: files, uploads, shares and links, workspaces, events,
administration, and serving under a non-default base path.

## The stubs

`test/stubs` holds the two endpoints an installation depends on, and
`test/stubs/cmd/arca-stubs` runs both from flags:

```sh
out/arca-stubs -issuer-listen 127.0.0.1:8081 -authorizer-listen 127.0.0.1:8082 \
  -issuer-url http://localhost:8081
```

- **The issuer** serves discovery and a key set, and mints a token for any
  subject at `POST /mint` with a body such as `{"sub": "dev"}`. No real
  issuer has that endpoint.
- **The authorizer** answers the authorization contract, allows by default,
  always denies the probe id, and carries a control API a test drives to
  deny, fail, or set limits. It has no authentication; never run it in
  front of anything you care about.

`make run` starts the same binary beside the stack, and `Dockerfile.stubs`
builds the `arca-stubs` image the release publishes for the kind example.

## The conformance suite

`test/conformance` is the API contract as an importable, black-box test
package. `conformance.Run` drives any base URL over HTTP, asserts every
route, every error code the suite can provoke, pagination, and conditional
requests, and deletes exactly what it created. It imports nothing from
`internal/`, so another implementation, or a gateway in front of Arca, can
run it.

```sh
make test-conformance
```

starts an installation on the stack with the stubs and runs the suite
against it, then proves the suite catches a server that answers wrong: it
restarts the server once per value of `ARCA_TEST_DRIFT`, each of which makes
the build answer one part of the contract incorrectly, and expects the
suite to fail each time. It takes minutes, so it is not part of
`check-all`.

To run the suite against another installation, from a checkout:

```sh
go test -tags=tiers -count=1 -run '^TestContract$' ./test/conformance -args \
  -url https://arca.example.com -token "$TOKEN_A" -token-bob "$TOKEN_B"
```

| Flag | Environment | What it is |
|---|---|---|
| `-url` | `ARCA_TEST_URL` | the installation's origin, without `/v1` |
| `-token`, `-token-bob` | `ARCA_TEST_TOKEN`, `ARCA_TEST_TOKEN_BOB` | bearers for two different subjects |
| `-issuer` | `ARCA_TEST_ISSUER_URL` | a stub issuer to mint every subject at, instead of the two tokens |
| `-admin` | `ARCA_TEST_ADMIN` | a subject the installation treats as an administrator; empty skips the administration cases |
| `-authorizer` | `ARCA_TEST_AUTHORIZER_URL` | a stub authorizer's control URL; empty skips the deny, outage, and limit cases |
| `-anonymous` | `ARCA_TEST_ANONYMOUS` | the installation serves public links to callers without a token |
| `-s3-endpoint` | `ARCA_TEST_S3_ENDPOINT` | the address the bucket is reached at from here, when presigned URLs name a host this machine cannot resolve |

A group whose input is missing skips once and says why. A route the target
does not serve fails as pending rather than skipping. The test driver
assumes the default base path; a target under another base is driven by
calling `conformance.Run` from your own test with `Options.BasePath` set.

The release pipeline runs the same suite against the published images on
a kind cluster before anything is deployed.

## Continuous integration

`.github/workflows/verify.yml` runs on every push and pull request: the
shared gate, a check that `go.mod` and `go.sum` are tidy, a build of the
image, a parse of the alert rules, the store tier, and the e2e tier.
`.github/workflows/release.yml` runs on a tag; see
[Releasing](releasing.md).
