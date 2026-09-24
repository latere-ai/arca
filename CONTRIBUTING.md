# Contributing

Thanks for looking. This file is for people and agents changing Arca.
Users read the [README](README.md) and [`docs/`](docs/README.md). How the
server is built is in [`docs/internals/`](docs/internals/README.md), and the
design records with the reasoning behind each decision are in
[`specs/`](specs/README.md).

## Getting set up

You need Go 1.27 or newer and `git`. Docker or Podman gets you the two
stores; without one, the gate and the unit suite still run.

```sh
make       # the quality gate: formatting, linting, the suite, coverage, the specs
make run   # Postgres, MinIO, the stubs, the migrations, arcad, and a check of it
```

`make` needs only the Go toolchain and git. Everything it pins comes from
public modules, so it runs the same on your machine as in CI.

The suite is tiered. The unit tier is what `make` runs and needs nothing
beside the toolchain. The store tier runs the packages that reach a store
against the real MinIO and the real Postgres of `compose.yaml`, because a
bucket's conditional create and a database's transaction semantics are the
two things a fake gets wrong. The e2e tier runs `arcad` as a process
against both, and the conformance tier runs the API suite against an
installation. [`docs/internals/testing.md`](docs/internals/testing.md)
covers each.

```sh
make test-store   # the store tier, starting the stack first
make test-e2e     # arcad as a process against the stack
make check-all    # the gate and both tiers, before pushing something that touches a store
```

A tier without the stack skips itself and says what to run, so
`go test ./...` on a clean clone is green with no services.

Install the hooks once with `make hooks`. They run formatting and license
checks before a commit and the linter before a push, so you see a finding
before CI does.

## Sending a change

Fork the repository, work on a branch, and open a pull request. Keep one
logical change per commit, stage the files explicitly, and write the
subject in the imperative, saying what changed for whoever reads the log.
Maintainers push to `main` directly; the pipeline runs the gate on every
push and pull request, and a `v*` tag cuts a release, as
[`docs/internals/releasing.md`](docs/internals/releasing.md) describes.

If you are planning something large, open an issue first. A design that
lands without a spec is harder to review than one that arrives with the
reasoning attached.

## The bar

`make` runs the whole gate (`go tool lateregate`): formatting, the
linter, modernization, a build without cgo, known vulnerabilities, the
suite with and without the race detector, per-package coverage at 90% or
more, the suite with only the toolchain on `PATH`, the suite against an
empty temporary directory, the license notice, the dependency allow list,
the identity shape shared with the sibling projects, and the spec tree.
`go tool lateregate list` names the gates and `go tool lateregate <name>`
runs one.

Some documents are checked against the code: `api/openapi.yaml` and
`deploy/base/prometheusrule.yaml` are generated (`make openapi`,
`make rules`), and `docs/configuration.md` and `docs/api.md` are written by
hand but fail a test when they miss a variable, a route, or an error
code.

A bug fix carries a test that fails without it. A change that lowers a
threshold or adds a waiver records the reason in `.lateregate.yaml`, so
the exception is reviewable rather than invisible.

## Specs first

A feature starts as a spec with acceptance criteria that are testable
sentences. The implementation follows the spec, and a divergence is
recorded in the spec's Outcome section rather than left in the code.
Names in a spec (API fields, error codes, environment variables,
metrics) are the names the code uses.

Small fixes do not need a spec. Anything that changes the `/v1` API, an
event payload, the bucket key layout, a database migration, or a
configuration variable does, and it updates the matching page under
`docs/` in the same change.

## Where a package belongs

Three places, by who imports it:

- The module root (`object/`, `authorizer/`) and `test/conformance`
  hold the packages a platform built on Arca imports. A change there
  keeps existing call sites compiling or names the break in the
  CHANGELOG.
- `internal/` holds what only `arcad` needs: the HTTP API, identity,
  configuration, the bucket and database stores, the reaper.
- A generic package with a plausible second consumer outside Arca
  belongs in [`latere.ai/x/pkg`](https://github.com/latere-ai/pkg), the
  shared library Arca already depends on. If you are unsure, put it in
  `internal/` and say so in the pull request.

## Three registers

Every sentence is written for one reader, and the register follows the
reader: the user in API `message` fields, the contributor in specs, this file, package documentation, and
commit messages, the developer in logs, `/readyz`, and error details. The
rule and the review checklist are in
[`docs/writing/registers.md`](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md).

## Reporting a vulnerability

Do not open an issue. [`SECURITY.md`](SECURITY.md) says where to send it.
