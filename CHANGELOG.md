# Changelog

What changed for whoever stores an object, runs `arcad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- **A production replica reaches the decider.** arcad dials platformd's
  internal Service on port 80, and a NetworkPolicy is evaluated after the
  Service's address translation, on the pod's own port, 8081. The base
  admits 80 and the overlay did not admit 8081, so the authorizer probe
  was a dropped packet, the client reported the authorizer unavailable,
  and both replicas of the first production deploy held at 503. The
  overlay admits the pod port, with the test naming why. The platform's
  own policy also had to admit arcad on that port, which is its
  repository's change.

## v0.1.6 - 2026-09-19

- **The conformance suite reaches the bucket from outside the cluster.** A
  read above the inline threshold answers a redirect to a presigned URL and
  a multipart part goes to a presigned PUT, both signed for the bucket
  address the installation was configured with. On the kind stack that is a
  name only the cluster resolves, so a suite run on a CI runner failed
  `005/Bytes` and `007/Complete` on the dial. A presigned URL is signed over
  its host, so rewriting the URL would be refused by the store. The suite
  now takes `-s3-endpoint`, defaulting from `ARCA_TEST_S3_ENDPOINT`, and
  `Options.BucketDial` behind it: the address the bucket is reached at from
  where the suite runs. It dials that address and sends the signed host, so
  the store verifies the signature it made. The e2e tier never met this
  because its store is at one address for both sides.

## v0.1.5 - 2026-09-19

- **The kind stack's arcad becomes ready.** Its overlay gave arcad an
  authorizer URL with a path the stub authorizer never served: the shared
  stub decides at its root, and the e2e harness hands arcad the bare
  listener URL, so the tier that would have caught it never could. Every
  probe met a 404, the client reported the authorizer unavailable, and
  `/readyz` answered 503 for as long as the pod lived with nothing logged.
  Three release runs were lost to it and to a bearer named in two places
  before it. The overlay now gives the root, and a test asks the stub, as
  the binary builds it, the probe question at the overlay's URL with the
  overlay's bearer, so both the path and the bearer are held by execution
  rather than by a second copy of either value.

## v0.1.4 - 2026-09-19

- **A signature that meets a connection reset is retried.** Each signature
  reaches Sigstore's certificate and timestamp authorities over the
  network, and one reset from the timestamp authority failed the whole
  v0.1.3 release after its images were built and pushed. Signing is
  idempotent, so the release signs each artifact up to three times with a
  pause between, and reports a failure only when it survives all three.

## v0.1.3 - 2026-09-19

- **The kind stack's authorizer accepts the bearer arcad sends.** The stub
  authorizer requires a bearer and was started with none named, so it
  expected the package default while the overlay's Secret told arcad to
  send another. Every probe was refused, the client reported the authorizer
  unavailable, `/readyz` answered 503 for as long as the pod lived, and
  nothing logged: the v0.1.2 release run was lost in its conformance job
  exactly so, and the e2e tier never saw it because its harness hands both
  sides one value. The stub now reads the same Secret key arcad reads, a
  test holds the two to one value, and the release's failure dump prints
  `/readyz`'s body, which names the check that failed and would have made
  this a one-minute diagnosis.

## v0.1.2 - 2026-09-19

- **`make run` checks the installation it just started, and prints requests
  that write.** The one command from a clean clone now runs all seven of its
  steps: once the server is ready it runs `arcad check` against it, so the
  first thing a contributor reads is five `ok` lines naming the bucket, the
  database, the issuer, the authorizer and the public URL, and the lines it
  prints next put one object and read it back rather than fetching a probe.
- **A grant on a moved path follows the object.** A move rewrote the file
  row, its history and its bookmarks, and left the share behind. That is
  not only a lost grant: the old path becomes free, and the next object
  written there is covered by a grant its owner gave for something else,
  which hands the grantee an object nobody shared with them. The move's
  transaction now carries the grant too. A grant on an ancestor still
  covers a subtree and still stays where it is.
- **A space's usage counts the sessions it has open.** An upload session
  is charged its declared bytes the moment it opens, so that a caller
  cannot hold a thousand open at once, but the recomputation the reaper
  reconciles against summed only the settled tables. The correction wrote
  the lower number over the live charge, so opening a session and waiting
  one reap interval erased it, and repeating that meant a space was never
  charged for any session. The recomputation reads all three tables.
- **A namespace that instruments every workload in it instruments Arca
  too.** `internal/config` read `ARCA_OTEL_EXPORTER_OTLP_ENDPOINT` and
  nothing else, and an operator that instruments a namespace injects the
  OpenTelemetry standard `OTEL_EXPORTER_OTLP_ENDPOINT` instead. Export did
  happen, because `latere.ai/x/pkg/otel` reads that name off the process
  environment itself, but nothing in this repository said so, no test held
  it, and the criterion of spec 018 stated the opposite, so the one change
  that handed the exporter an endpoint rather than leaking the environment
  would have taken every trace and every log record with it and failed
  nothing. Both names are read now and the prefixed one wins wherever it is
  set, so an installation still points Arca at a collector of its own. The
  shape is checked against the prefixed name alone: an injected value is the
  platform's and the exporter that owns the standard name parses it, and a
  telemetry variable Arca did not ask for is not a reason a replica refuses
  to serve bytes. The collector joins the endpoints whose port every overlay
  must admit, under both names.
- **A route that would take a word out of a space's namespace fails the
  start-up.** Go's router prefers a literal over a wildcard in the same
  position and reports no conflict, so `GET /v1/workspaces/archived`
  registered beside `GET /v1/workspaces/{id}` would have made a workspace
  named `archived` unreachable on the day it was added, and nothing would
  have said so. `arcad` now refuses to start when one route's literal
  segment sits where another route of the same method has a wildcard,
  naming both routes, unless the literal is one of the four the API
  grammar reserves: `materialize`, `links`, `with-me` and `deleted`. An
  installation that registers none of its own routes sees no change.

- **A replica can reach the database it is configured with.** The base
  confines egress and admits 5432, which is where a Postgres an operator
  runs listens. A managed database listens elsewhere: this installation's
  is on 25060, with its connection pool on 25061. A port no policy names
  is a connection dropped rather than refused, so the pool would have
  waited out its own timeout, the database readiness check would have
  failed, and the rollout would have timed out with the image already
  built and signed. The production overlay admits both ports, the way the
  kind overlay admits its own stack's, and a test names the pairing
  because the ports live in a Secret no manifest test can read. The
  telemetry collector is admitted in the same policy and for the same
  reason: the namespace instruments every workload in it and the collector
  is reached on 40318, not on the 4317 and 4318 the base admits, and a
  dropped export fails nothing and says nothing.

- **The production Ingress applies.** `/openapi.json` was routed with
  `pathType: Exact`, and the nginx admission webhook refuses a path
  holding a dot under `Exact` or `Prefix`. It rejects the whole document
  rather than the one rule, so the deploy job's `kubectl apply -k` would
  have failed outright, after the image was built and signed and the
  approval given. The path is `ImplementationSpecific`, which matches the
  same requests here because nothing else on the origin begins with it,
  and the test that reads the smoke's paths now knows the webhook's rule.

- **A public object still redirects to the CDN after the cutover.** The
  production overlay sets `ARCA_PUBLIC_CDN_URL`, which the service it
  replaces served public objects from. An installation that leaves the
  base empty answers the ordinary presigned redirect instead, and nothing
  fails when it does, so every public link that already existed would have
  quietly started resolving somewhere else. The value is not a credential
  and sits beside the origin in the overlay rather than in a Secret; a
  test fails the tree when it is absent.

- A grantee is reachable under an external authorizer. Every question
  about a file, an upload session or a workspace now carries `grant`, the highest live grant
  the caller holds on a prefix of the resource's path, read from Arca's
  own grants table before the question is asked and sent in both modes.
  With `ARCA_AUTHORIZER_URL` set the endpoint saw an owner, a path, a
  plane and a size and could not tell a grantee from a stranger, so every
  read of a shared object was refused and `POST /v1/shares` wrote a row
  no decision consulted. An endpoint consumes it by admitting the actions
  of the rung's ladder: `read` admits `file.read`, `file.list`,
  `workspace.read`, `workspace.list`; `write` adds the writes, including
  the `upload.write` a multipart session asks, so a grantee's large write
  is admitted on the same rung as a small one; `manage` adds the
  `share.*` actions. A question about the caller's own space
  carries no `grant`, because ownership is not a grant. The stub
  authorizer's new `-grants` flag is that row, for a deployment to check
  its own endpoint against.

- An owner can read their own usage. A listing of a plane root, `GET
  /v1/files/{owner}/files?list=1` or the same for `workspaces`, now
  carries `space` beside `entries`: `{"bytes": N, "files": N}`, the bytes
  the usage ledger counts and the live paths of the space, trash
  excluded. It was only on `GET /v1/admin/overview` before, which takes
  `space.admin`, so a console could not show a person what they hold
  without an administrator's credential. There is no new route and no new
  action: the question is the `file.list` the listing already asks. A
  listing below a plane root carries no `space`.

- **The production overlay routes everything the release smoke reads.**
  The Ingress claimed `/readyz` and `/version` at the origin, and the
  smoke the deploy job runs after the rollout asks for `/livez` and
  `/openapi.json` as well, so a deploy would have taken a 404 at the
  origin after a rollout that worked, failed the job and skipped the
  publish. Both paths are routed. The test behind it no longer keeps its
  own copy of the list: it reads the paths out of `tools/smoke/release.sh`,
  which is how the two drifted apart, so a path the script grows is a red
  tree rather than a spent tag.

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

`v0.1.0` was tagged with every note below it and failed in its build job
before any image was pushed; this tag carries the same service with the
two release files fixed. Its own run then failed in conformance, where the
kind stack's `arcad` crash-looped against a NetworkPolicy the overlay did
not carry, so the release did not publish either. v0.1.2 is the first tag
with a published release, and it carries everything below.

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
