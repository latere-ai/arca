---
title: "Merged coverage floor: the 90% bar judged over the unit, store and e2e profiles together"
status: validated
track: core
depends_on:
  - specs/014-test-stubs-and-tiers.md
affects: [.github/workflows/verify.yml, .lateregate.yaml]
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Merged coverage floor

## Overview

[[014-test-stubs-and-tiers]] splits the suite into three tiers because
the two stores and the two endpoints Arca stands between cannot all be
faked and cannot all be required on every push. A package whose only
exercise is the store tier or the e2e tier therefore measures low in the
unit run, and the floor the gate enforces is the unit run's alone.

The criterion that says otherwise is [[014-test-stubs-and-tiers]]'s
criterion 11, carried here verbatim: coverage over the three profiles is
at least 90% for every package, `test/stubs` included. It was split out
of that spec when Arca closed at v0.1.7 rather than built, because
closing it is a change to another repository.

## Why this was split

The gate itself is ready: `go tool lateregate cover` takes `-profile`
once per tier and reads the profiles together, and `make check-all`
already produces all three locally. What is missing is the seam in CI.
`verify.yml` calls the shared reusable workflow
`latere-ai/ci/.github/workflows/lateregate.yml@v1`, which takes
`go_version`, `test_os` and `runs_on` and nothing else, and whose gate
job runs `go tool lateregate ${{ matrix.gate }}` with no place to put an
argument. `.lateregate.yaml` is not a way round it: the profiles are read
from the flag and the configuration file has no key for them. So the two
tier jobs upload their profiles as artifacts and nothing downloads them.

That makes the fix a pull request against `latere-ai/ci`, reviewed and
released there before this repository can call it — another repository's
change, outside the tree this deck governs, and not an hour's work inside
it. Until it lands the floor is the unit tier's, which every package
clears, and each tier's profile is kept as an artifact so the number can
be computed by hand.

## Design

Two halves, in order.

### What the reusable workflow needs

`latere-ai/ci/.github/workflows/lateregate.yml` gains one input,
`cover_profiles`. It names the artifacts to download before the `cover`
gate runs and to append to that one gate's invocation as repeated
`-profile=` flags. Nothing else changes: the input is empty by default,
every other gate's invocation is untouched, and a caller that sets
nothing behaves exactly as it does today.

```yaml
# latere-ai/ci/.github/workflows/lateregate.yml
inputs:
  cover_profiles:
    description: >-
      artifacts holding coverage profiles to merge into the cover gate,
      one per line or comma separated; each is downloaded before the
      cover gate runs and appended as -profile=<file>
    type: string
    required: false
    default: ""
```

The gate job downloads each named artifact, then for `matrix.gate ==
cover` runs `go tool lateregate cover -profile=a.out -profile=b.out …`.
For every other gate the command is what it is now.

### What this repository needs

`verify.yml`'s `gate` job gains `needs: [store, e2e]` and passes the two
artifact names as `cover_profiles`. Without the `needs`, the artifacts
the tier jobs upload do not exist when the gate runs, and the merged
floor would be the unit profile with two missing files.

```yaml
# .github/workflows/verify.yml
  gate:
    needs: [store, e2e]
    uses: latere-ai/ci/.github/workflows/lateregate.yml@v1
    with:
      cover_profiles: |
        store-profile
        e2e-profile
```

Ordering the `gate` job behind the two tier jobs makes every push wait
for the stack, which is the cost of the merged number and is what
[[014-test-stubs-and-tiers]]'s Design already asks for.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Coverage over the three profiles is at least 90% for every package, `test/stubs` included | the `cover` gate with three `-profile` flags, in the `gate` job of `verify.yml` |
| 2 | `lateregate.yml` takes `cover_profiles`, downloads each artifact it names, and appends one `-profile=` flag per profile to the `cover` gate alone | that repository's own test over the rendered workflow; a caller setting nothing runs the command it runs today |
| 3 | The `gate` job runs after `store` and `e2e`, so no profile it merges is missing | `TestWorkflowJobsMatchTheTable` in this repository, reading `verify.yml` |
| 4 | A package whose only exercise is a service tier counts toward the floor rather than measuring zero | the number in the gate's log against the same package's unit-run number |
