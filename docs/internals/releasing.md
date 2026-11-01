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
lateregate's note reader (`go tool lateregate release-notes vX.Y.Z`) and
fail when it is missing or empty.

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

`.github/workflows/release.yml` runs on the tag, in five jobs. The build,
the signatures and the publish run through the shared image release stages
of [latere-ai/ci](https://github.com/latere-ai/ci): `image-build.yml`,
`image-publish.yml` and `release-verify.yml`. The conformance run and the
deploy are Arca's own and run between them, against the digests the build
pushed.

**build** produces everything from the tag's tree and nothing reaches a
cluster. It first requires that the tag's commit is on `main`, that the
commit's `verify.yml` run on the branch push passed, and that
`CHANGELOG.md` has a section for the tag; a tag with build metadata
(`+...`) is refused. The commit `lateregate release` pushes changes
`deploy/prod/kustomization.yaml` as well as the changelog, so the build
waits for that commit's own verify run, for up to an hour. Then:

- `arcad` archives for linux and darwin, amd64 and arm64;
- the `arcad` and `arca-stubs` images, each multi-arch, pushed by digest
  and under no tag to `ghcr.io/<repository owner>`, lowered, or to
  `ghcr.io/<RELEASE_IMAGE_NAMESPACE>` where that repository variable is
  set, so a fork publishes to its own packages;
- a check that the `arcad` image reports the tag's version;
- keyless cosign signatures over both images, made by the build stage for
  this repository;
- an SPDX document per image, and an SBOM attestation and a build
  provenance attestation per image.

**conformance** renders every overlay under `deploy/`, brings up the kind
stack of `deploy/examples/kind` from the digests just pushed, and runs the
conformance suite against it. A release that does not serve the API does
not publish.

**deploy** runs only when the repository variable `ARCA_RELEASE_DEPLOY` is
set, so a fork deploys nothing. The images carry no tag yet, so it pins the
`arcad` digest the build pushed in place of the tag `deploy/prod` names and
applies the overlay with the kubeconfig in the `ARCA_KUBECONFIG` secret, so
`arcad` and the reaper, which runs the same image, roll to this release and
to no other build on the way. It waits for both rollouts and runs
`tools/smoke/release.sh` against the live origin, which checks the probes,
the OpenAPI document, the served version against the tag, and that the
origin routes the API prefix to the new build.

**publish** gives each digest the release tag, writes `checksums.txt` over
the archives and the SPDX documents and signs it with the publish stage's
identity, and creates the GitHub release with the changelog section as its
body, those files as assets, and the conformance and smoke evidence
appended. The release is created as a draft and published once every file
is attached.

**verify** reads the published release back from a runner with no
checkout: the signature over `checksums.txt`, every asset against it, each
image's tag to a signed and attested digest, and the body against the
changelog section.

How an operator verifies what the workflow signed is in
[operations](../operations.md#what-a-release-is).

## Versions

The version promise is in [operations](../operations.md#what-a-version-number-promises).
Before `v1.0.0` a minor may remove a field, with a changelog entry that
names the break. The OpenAPI document carries its own version, `1`, which
names the contract and does not move with the binary's tag.
