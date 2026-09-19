# Changelog

What changed for whoever stores an object, runs `arcad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- **The kind stack comes up.** `deploy/examples/kind` reaches its stub
  issuer on 8081, its stub authorizer on 8082 and MinIO on 9000, and the
  base's egress policy admits 53, 80, 443, 5432 and the two OTLP ports
  and nothing else. The default CNI of a kind cluster enforces
  NetworkPolicy, which the overlay and `docs/operations.md` both said it
  does not, so every connection `arcad` opened to its issuer was dropped
  and the replica crash-looped against a stub that was up and answering.
  The overlay now carries the egress its own dependencies need, and a
  test reads the ports each overlay is configured to dial out of its own
  manifests and fails when no policy admits one. Point an installation at
  a bucket, an issuer or an authorizer on a port other than 443 or 80 and
  you must admit that port yourself; `docs/operations.md` says how.
- **An issuer that is not up yet no longer crash-loops a replica.** The
  discovery every issuer is warmed with at start is best-effort: a warm
  that fails writes one line naming `ARCA_OIDC_ISSUERS`, the process
  serves, the new `issuers` readiness check holds the replica out of
  rotation, and a retry from one second doubling to thirty brings it in
  as soon as the issuer answers. A request that arrives before the retry
  pays for the discovery itself. An installation's start no longer
  depends on the order its issuer and its API come up in, and a transient
  issuer outage no longer restarts the pod.

## v0.1.1 - 2026-09-19

The first release that publishes. `v0.1.0` was tagged with every note
below it and failed in its build job before any image was pushed; this
tag carries the same service with the two release files fixed.

- The release image reads its platform. `Dockerfile.ci` declared the two
  platform arguments before the runtime stage's `FROM`, where an argument
  is in scope for `FROM` lines only, so the stage's `COPY` read them empty
  and the build looked for a binary at `bin/_/arcad`. The arguments are
  declared inside the stage, and a test reads where they are declared.
- The stubs image exists. `Dockerfile.stubs`, which the pipeline builds
  and the kind example and the conformance job run, compiles the stub
  issuer and the stub authorizer onto the same static runtime with their
  own ports and entrypoint. A test holds the file to what the workflow
  builds.

- `tools/migrate-drive`, the row copy of spec 019: it reads the
  predecessor's database, rewrites every owner to a subject, drops what
  did not arrive and counts each dropped row, writes one transaction per
  table, refuses a target that already holds rows, and verifies counts
  and a sample of checksums. Building it found that the predecessor's
  bucket keys hold no object id, so the bytes need a move the cutover
  has yet to plan; the spec records the finding and the tool mints a
  fresh id per key and says so.

Everything `v0.1.0`'s section says applies to this tag.

## v0.1.0 - 2026-09-19

Arca's first release is the storage service Drive was, moved into an
open core and simplified on the way: files, uploads, shares and links,
workspaces, an event log, a reaper, and the identity, API, tiers, threat
model, release, conformance and observability every core of the family
carries. Nothing here is deployed by the tag on its own; the deploy job
waits on the operator's variable and their approval.

- **Storage for people, agents and sandboxes.** A space per principal,
  addressed by the subject the token identifies. Two planes inside it:
  `files/` with versions, trash, restore and stars, and `workspaces/`
  with a writer lease a sandbox attaches to, materializes and syncs
  back. Bytes go to any S3 compatible bucket first and metadata to
  Postgres second, and a failure between the two leaves an invisible
  object the reaper removes, never a visible lie. Reads above the inline
  size redirect to a presigned URL; uploads above it go to the bucket in
  parts the client sends directly.
- **Shares and public links.** A grant on a subtree at read, write or
  manage; what a caller sees shared with them; a public link whose token
  resolves before any question is asked, and whose refusal names no
  token. The three link routes are the only routes outside the verifier.
- **Verify and ask.** Every `/v1` request carries a token from an OIDC
  issuer you list; Arca calls the issuer for its key set and nothing
  else. Whether the caller may act is a question to an authorizer you
  write, twenty-three actions over seven kinds, or the built-in owner
  policy: an owner acts on its own space, a grantee within its grant, an
  administrator on every space, and `space.admin` is the administrator's
  alone. A personal key narrowed by grants reaches only what its grants
  name. A deny at lookup answers exactly as a missing object.
- **Usage, not quota.** Bytes per space are counted in the write's own
  transaction and answered on the administrator's overview and to the
  authorizer; when the authorizer's answer carries a byte limit, a write
  past it is refused. Arca stores no limit of its own.
- **Events and the reaper.** Every change is an event a consumer tails
  by cursor. Ten reconciliation passes run inside `serve` and as
  `arcad reap`: orphan bytes and orphan rows, expired leases, upload
  sessions and links, trash and tombstones past retention, and a ledger
  check that corrects the usage counter when it drifts.
- **Operating it.** `arcad migrate` applies the forward-only schema,
  `arcad check` answers one line per requirement of an installation and
  exits non-zero on any failure, `/metrics` on the internal listener
  serves one registry whose table and thirteen alert rules are held equal
  to the manifest, and traces and logs leave over OTLP when an endpoint
  is set. `deploy/` holds a kustomize base, a bootstrap directory and
  overlays for kind, AWS and DigitalOcean; `docs/install.md` walks from
  an empty cluster to a serving installation.
- **Proving it.** `test/conformance` is an importable suite a consumer or
  another implementation runs against any base URL; the release
  pipeline runs it against the published image on a real cluster before
  the release is published. The gate holds every package at ninety
  percent, the store and end-to-end tiers run against MinIO and
  Postgres, and every artifact is signed keyless with an SPDX bill of
  materials and a provenance attestation.
- **What did not arrive from Drive, by decision:** a stored quota,
  outbound webhooks, provenance zones, the repos plane, a memory plane
  of its own, per-space administrator listings and an audit table,
  share approvals, agent visibility, grants to roles, teams or email
  addresses, and every reading of a token's claims for meaning. Spec 019
  records each with the reason and where the need is met.
- **Bugs fixed on the way, each under a test that fails without the
  fix:** the reference check that decided whether bytes may be deleted
  did not count upload sessions; a refused sync had already destroyed
  the subtree; the sync deleted bytes under a recomputed key and never
  the stored one, with no reference check; the reaper's dry run
  under-reported and one failed pass stopped the rest; the usage check
  admitted a write when its query failed; a share listing answered every
  link token to any caller who could list; a public grant admitted any
  authenticated caller; eight lookup refusals leaked the authorizer's
  reason where an absence gives a fixed sentence; `If-None-Match`
  admitted a matching write; the `?limit=` clamp was silent; a
  deadline was answered at a finer precision than it was stored; a
  fixed probe key would have failed every second `check`; and the kind
  example minted tokens under an issuer the server did not list.
