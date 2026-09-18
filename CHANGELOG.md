# Changelog

What changed for whoever stores an object, runs `arcad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- The repository: the `arcad` binary serving its probes on two listeners,
  typed configuration from `ARCA_*` variables, the quality gate, and the
  design specs for the object store, the metadata store, files, identity,
  uploads, shares, workspaces, quotas and events, webhooks,
  administration, the API, the test tiers, the threat model, the release,
  the conformance suite, observability, and the migration from the
  service Arca replaces. Nothing stores a byte yet; the specs say what
  will.
- Installing it: `deploy/` holds a kustomize base, a bootstrap directory,
  and overlays for a kind cluster, AWS and DigitalOcean, so an
  installation is a copied directory with your own hostname and stores in
  it. `docs/install.md` walks from an empty cluster to a serving
  installation and `docs/operations.md` covers upgrades, rollbacks, and
  checking a release's signatures.
- Releasing it: a `v*` tag builds `arcad` for linux and darwin on both
  architectures with checksums, publishes two multi-arch images, signs
  every artifact keyless with cosign, attaches an SPDX bill of materials
  and a build provenance attestation per image, proves the published image
  on a real cluster, and publishes a GitHub release whose body is this
  file's section for the tag. `tools/smoke/release.sh` is what proves an
  installation from outside, and refuses a rollout still serving the
  previous version.
