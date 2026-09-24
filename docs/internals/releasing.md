# Releasing

A release is a `vX.Y.Z` tag on `main`. The tag is cut by one command, and
everything after it is the release workflow's.

## The changelog comes first

`CHANGELOG.md` holds one section per release, and that section becomes the
body of the GitHub release. Write under `## Unreleased` as changes land,
for whoever stores an object, runs `arcad`, or builds on the packages. A
break is named as one, with what an operator or a client has to change.

A tag whose commit has no section for it is refused before it is pushed:
the pre-push hook and the release workflow both read the section with
`go tool lateregate release-notes vX.Y.Z` and fail when it is missing or
empty.

## Cutting the tag

```sh
go tool lateregate release vX.Y.Z
```

The command refuses a dirty tree, an existing tag, and an empty
`## Unreleased`. It reads CI first and refuses when the latest run of any
workflow on `main` since the previous tag did not pass, when the previous
tag's release run failed, or when the previous tag published no GitHub
release. Then it runs the gate, renames `## Unreleased` to
`## vX.Y.Z - <date>` under a fresh `## Unreleased`, rewrites the version in
the files `.lateregate.yaml` lists under `release.stamp` (the current
release named in `SECURITY.md`, and the image tag in
`deploy/prod/kustomization.yaml`), commits `changelog: vX.Y.Z`, tags, and
pushes the commit and the tag together.

A release that adds a database migration needs the migration applied to
any installation the pipeline deploys before the tag is pushed. The
pipeline never runs the migration Job, because two processes applying the
same forward-only migrations at once is the one thing the schema cannot
survive.

## What the workflow does

`.github/workflows/release.yml` runs on the tag, in four jobs.

**build** produces everything from the tag's tree and nothing reaches a
cluster:

- `arcad` archives for linux and darwin, amd64 and arm64, and
  `checksums.txt` over them;
- the `arcad` and `arca-stubs` images, each multi-arch, pushed under
  `ghcr.io/<repository owner>` or under the repository variable
  `ARCA_RELEASE_IMAGE_NAMESPACE`, so a fork publishes to its own packages;
- keyless cosign signatures over both images and over `checksums.txt`,
  signed with the workflow's own identity;
- three SPDX documents, one for the module graph and one per image, and
  an SBOM attestation and a build provenance attestation per image;
- a render of every overlay under `deploy/`.

A tag with build metadata (`+...`) is refused.

**conformance** brings up the kind stack of `deploy/examples/kind` from the
images just published and runs the conformance suite against it. A release
that does not serve the API does not publish.

**deploy** runs only when the repository variable `ARCA_RELEASE_DEPLOY` is
set, so a fork deploys nothing. It applies `deploy/prod` with the
kubeconfig in the `ARCA_KUBECONFIG` secret, waits for the rollout, and runs
`tools/smoke/release.sh` against the live origin, which checks the probes,
the OpenAPI document, the served version against the tag, and that the
origin routes the API prefix to the new build.

**publish** creates the GitHub release with the changelog section as its
body, the archives, checksums, signatures, and SBOMs as assets, and the
conformance and smoke evidence appended.

How an operator verifies what the workflow signed is in
[operations](../operations.md#what-a-release-is).

## Versions

The version promise is in [operations](../operations.md#what-a-version-number-promises).
Before `v1.0.0` a minor may remove a field, with a changelog entry that
names the break. The OpenAPI document carries its own version, `1`, which
names the contract and does not move with the binary's tag.
