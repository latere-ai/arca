---
title: "N-1 conformance: the previous release's suite run against this release's binary"
status: validated
track: core
depends_on:
  - specs/016-release-and-installation.md
  - specs/017-conformance-suite.md
affects: [.github/workflows/release.yml, test/conformance/]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# N-1 conformance

## Overview

[[017-conformance-suite]] ships in the module, so
`latere.ai/x/arca/test/conformance` at `v<tag>` is the suite for the API
of that tag. That is what makes a compatibility claim checkable: run the
suite a consumer already pinned against the binary they are about to
upgrade to, and a case the old suite holds and the new binary fails is a
break rather than an opinion.

The criterion that says so is [[017-conformance-suite]]'s criterion 9,
carried here verbatim: the previous release's suite passes against this
release's binary, or the tag is a major. [[016-release-and-installation]]
cites the same test from its own criterion 9. Neither is written. The
criterion was blocked on there being no release to check a suite out of;
`v0.1.6` and `v0.1.7` closed that, and it was split out of
[[017-conformance-suite]] when that spec closed rather than built.

## Why this was split

Building it is not an edit to the suite. The previous tag's driver,
`test/conformance/contract_test.go`, carries the `tiers` build tag and
imports `latere.ai/x/arca/test/stubs/issuer` and
`latere.ai/x/arca/test/stubs/authorizer`, so checking out
`test/conformance` alone into a temporary module does not build: the
temporary module needs either the previous tag's stub packages beside it
or a driver written for this purpose. Choosing between those two is a
design decision about what "the previous release's suite" means — the
cases alone, or the cases with the driver they shipped with.

Whichever is chosen, the only place it runs is the `conformance` job of a
release, against a candidate image that exists only after `build` has
pushed it. It cannot be exercised locally and it cannot be exercised on
`main`: the first evidence it works is the next tag, and a mistake in it
fails a release rather than a push. That is a change with a release cycle
attached, not an hour before a tag, so [[017-conformance-suite]] closed
on what the release pipeline proves today and this carries what it does
not.

## Design

Two decisions, then one step.

### Which suite is "the previous release's"

The cases and the driver they shipped with. A temporary module at
`$(mktemp -d)` requires `latere.ai/x/arca@<previous tag>` and runs
`go test -tags=tiers -run '^TestContract' latere.ai/x/arca/test/conformance`
against it, so the stub packages the driver imports come from the same
tag as the cases and nothing is reconstructed by hand. The module
resolves through the proxy, which is what a consumer pinning that tag
gets, so a suite that cannot be fetched this way is a release defect
worth failing on.

### Which tag is "previous"

The highest `v*` tag below the one being released, read from the
repository at the release commit, and skipped when there is none or when
the tag being cut is a major. A release whose previous tag is a different
major is not held to it: that is what a major says.

```
previous = max { v : v is a tag, v < TAG, major(v) == major(TAG) }
none, or major(TAG) > major(previous)  ->  the step reports why and passes
```

### Where it runs

One step in the `conformance` job of `.github/workflows/release.yml`,
after the step that runs this release's own suite against the kind stack,
against the same stack and the same candidate image. A case the previous
suite fails fails the job, and a release does not publish. The evidence
goes into `release-evidence/conformance.md` beside this release's run,
naming the previous tag it ran and the case count, so the release body
says which compatibility claim was checked.

`TestPreviousSuitePasses` is the name both [[017-conformance-suite]] and
[[016-release-and-installation]] cite, and it stays the name: the step
runs a Go test in this repository that builds the temporary module,
resolves the previous tag, and runs the fetched suite, so a failure
reports as a test and the skip conditions above are read in Go rather
than in shell.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | The previous release's suite passes against this release's binary, or the tag is a major | `TestPreviousSuitePasses`, run by the `conformance` job of [[016-release-and-installation]] |
| 2 | The previous tag is the highest `v*` below the tag being cut in the same major, and a release with no such tag reports why and passes rather than silently doing nothing | the same test, over a table of tag lists |
| 3 | The suite that runs is the previous tag's cases with the previous tag's driver, fetched as a module rather than reconstructed | the temporary module's `go.mod`, which requires `latere.ai/x/arca` at that tag and nothing from the working tree |
| 4 | The release body names the previous tag the run was held to and the number of cases it passed | `release-evidence/conformance.md`, read on the first release that runs it |
