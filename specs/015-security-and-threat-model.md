---
title: "Security and threat model: assets, actors, boundaries, every threat with its control and its test"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/003-object-store.md
  - specs/005-files.md
  - specs/006-identity.md
  - specs/007-uploads.md
  - specs/008-shares-and-links.md
  - specs/009-workspaces.md
  - specs/010-events-and-reaper.md
  - specs/012-administration.md
  - specs/013-api.md
  - specs/014-test-stubs-and-tiers.md
  - specs/016-release-and-installation.md
  - specs/018-observability.md
affects: [internal/auth/, internal/api/, internal/blob/, internal/files/, internal/shares/, internal/workspaces/, internal/events/, internal/config/, deploy/, test/e2e/, SECURITY.md]
effort: medium
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Security and threat model

## Overview

Arca holds other people's bytes, hands out URLs that need no token, and
lets an unattended sandbox write into a space a person owns. This spec
names what is worth taking, who would take it, where the boundaries
between them are, and, for every threat, the one control that stops it
and the one test that proves the control is there. A control without a
test is a claim, and a test reads this file and fails when a row loses
its proof.

It is also the source of `SECURITY.md`, so a reviewer who arrives at the
repository reads four commitments and one document rather than seven
specs.

## Design

### Assets

| Asset | Where it lives |
|---|---|
| object bytes | the operator's bucket, under `ARCA_BUCKET_PREFIX` |
| paths, owners, checksums, grants, leases, the ledger, the event log | the operator's Postgres |
| link and public grant tokens | the grants table, and every URL a holder has ever pasted |
| presigned URLs | in flight, in a `302`, in a materialize manifest, in a browser's history |
| `ARCA_BUCKET_SECRET_KEY`, `ARCA_DATABASE_URL`, `ARCA_AUTHORIZER_TOKEN` | the server's configuration, mounted from a Secret |
| the record of who did what: the event log, which is also the record of what an administrator did ([[012-administration]]) | Postgres |
| the installation's availability | `arcad`, and the two stores behind it |

### Trust boundaries

```mermaid
flowchart LR
  subgraph untrusted[untrusted]
    C[a caller with a token]
    L[a link holder with no token]
    S[a sandbox holding a lease]
  end
  subgraph core[the installation]
    A[arcad]
    P[(Postgres)]
    B[(bucket)]
  end
  subgraph operator[the operator's, trusted by configuration]
    I[the issuer]
    Z[the authorizer]
  end
  C -->|bearer| A
  L -->|token in the URL| A
  S -->|bearer, lease| A
  A --> P
  A --> B
  C -.->|presigned URL, one object, five minutes| B
  A -->|key set only| I
  A -->|one question| Z
```

Three boundaries. Between a caller and `arcad` the control is the
verifier and the authorizer. Between `arcad` and the two stores the
control is the operator's network and credentials, and a compromise
there is out of scope below. Between `arcad` and the issuer and
authorizer the control is configuration: `arcad` trusts them because the
operator named them, and calls the issuer for a key set and nothing
else.

Nothing in the core dials an address a consumer chose. Arca makes
outbound calls to the bucket, the database, the issuer, and the
authorizer, every one of them named by the operator, and a consumer that
wants to know what happened tails the event log rather than being
called back ([[010-events-and-reaper]]). That removes a whole boundary
and the request-forgery surface behind it.

### Adversaries

| Adversary | Holds | Wants |
|---|---|---|
| an authenticated caller | a valid bearer from a listed issuer | another subject's space, more room than its platform allows it, an administrator's surface, a grant it was not given |
| a link holder | one token | the rest of the space, a write, another space's link |
| an anonymous prober | the network | a token by guessing, an object by path, the shape of what exists |
| a sandbox holding a lease | a token for the audience `arca`, one lease | another workspace, a lease it should have lost, bytes outside its subtree |
| a network position | the wire | a bearer, a link token, a presigned URL |
| a compromised authorizer | the decision | to allow every action for every subject, including actions it does not recognise |
| a consumer of the module | an import of `object/`, `space/`, `authorizer/` | to derive a key for an object it does not own |

### Controls

Every row names the spec that owns the control and the test that proves
it. `Test` cells name test functions; criterion 1 holds them to that.

| Threat | Control | Spec | Test |
|---|---|---|---|
| a caller reaching another subject's space | every handler reads, asks the authorizer, then acts; a deny on the caller's own action is 403, a deny while resolving a reference is a 404 byte for byte identical to an absence | 006, 013 | `TestRouteTableActions`, `TestReadAskAct` |
| a revoked permission still honoured | an allow is cached per replica for the answer's `ttl`, 60 s by default and capped at 600 s; a deny for 5 s; unavailability never; the key is subject, action, and resource id. The cache window is the accepted staleness and the authorizer sets it per answer | 006 | `TestDecisionCache` |
| an allow that was never decided | the client fails closed on anything but a 200 carrying `allow`; one retry only when the connection failed before a response line; an `http://` endpoint off loopback is refused at start-up | 006 | `TestAuthorizerFailsClosed` |
| an authorizer that allows everything, including actions it does not know | `arcad check` asks the probe resource, the id `probe` of kind `Space`, which every authorizer must deny, and fails the installation when it is allowed | 012, 006 | `TestCheckRefusesAPermissiveAuthorizer` |
| a token minted for another service replayed at Arca | `aud` must contain `ARCA_OIDC_AUDIENCE`, `iss` must be listed, `iat` is required and no older than 24 hours, `exp` and `nbf` are enforced, and only RS256 and ES256 are accepted | 006 | `TestVerifierRefusals`, the family's audience conformance suite |
| a narrowed personal key used past its grants | `authz.Restrict` intersects the grants with the vocabulary before the question is asked; an action outside them is a deny with reason `grant`, indistinguishable on the wire | 006 | `TestRestrictedKey` |
| a presigned URL leaking from a redirect, a log, or a history | one object, one method, one expiry of five minutes, `blob.PresignTTL`; a signed URL is written to no log, no event detail, and no response body except a manifest the caller asked for | 003, 013, 018 | `TestPresignIsScoped`, `TestNoPresignedURLIsLogged` |
| a presigned URL outliving a revoke | nothing recalls a signed URL, so the exposure is exactly `PresignTTL`. Five minutes is that number, chosen against the download it must survive rather than against convenience | 003, 008 | `TestPresignTTL` |
| path traversal into another object | a path is refused when it holds `..` or `//`, starts or ends with `/`, or names a plane the server does not serve; the check runs before anything else on every route that takes a path | 005, 013 | `TestPathRule`, table-driven over the refusal cases |
| key confusion: a path that reaches another object's bytes | the bucket key derives from the object id and never from the path, so a path that slipped every check still addresses nothing. A move is one `UPDATE` and touches no key | 003, 001, 005 | `TestKeyIsTheId`, and 001's criterion 7 against a counting bucket |
| one installation reading another's objects in a shared bucket | every key carries `ARCA_BUCKET_PREFIX`, normalised at start-up, and a leading `/` in it is a configuration error | 003 | `TestKeyPrefix` |
| public link enumeration | 256 bits from `crypto/rand`, rendered base64url, under a unique index; an unknown, revoked, or expired token is the same `not_found`; the three link routes are rate limited per client address, which is what makes guessing cost more than it can pay | 008, this spec | `TestTokenEntropy`, `TestLinkRateLimit` |
| a link token in an access log or a referrer | the token is a path segment, so the access log records the grant id in place of the path for the three link routes, and a link response sets `Referrer-Policy: no-referrer` | 008, 018 | `TestLinkPathsAreNotLogged` |
| a link that grants more than reading | a token grant carries `read`; a create asking `write` or `manage` is `link_read_only` | 008 | 008's criterion 4 |
| usage accounting bypass through multipart | the session is charged its declared size, and the completion charges the difference against the assembled size read from the store, which is the charge that counts. Bytes that landed before a refusal are aborted and reaped | 007, 010 | `TestUsageOnComplete`, `TestMultipartOverrunIsReaped` |
| usage accounting bypass by holding many open sessions | an open session's declared bytes are charged from the moment it opens until it completes or is reaped, so a thousand sessions do not fit under one limit | 007, 010 | `TestOpenSessionsCount` |
| a usage charge that fails open | the charge and the check read and write one ledger row inside the write's own transaction, so a failure refuses the write with `storage_unavailable` and records no charge. The service Arca replaces recomputed usage outside the write and admitted it on failure, so its enforcement stopped exactly when the database was under pressure | 010 | `TestUsageFailsClosed` |
| a ledger that silently stops counting | the reaper recomputes every space from the rows that hold the bytes each run and reports each correction, so a write path that forgot its delta is a finding and not a slow drift | 010, 018 | `TestLedgerReconciles` |
| a workspace lease held by a sandbox that died | a lease carries an expiry, one hour by default and twenty-four hours at most, both constants of `internal/workspaces` that no configuration raises, and the reaper releases every lease past its expiry each `ARCA_REAP_INTERVAL`, so a workspace cannot wedge. A reaped writer's unsynced work is lost, which is the stated trade | 009, 010 | `TestLeaseExpires`, `TestReaperFreesAnOrphanLease` |
| two writers in one workspace | the lease is taken by one conditional `UPDATE` whose predicate is that no writer holds it, so a race yields one winner, never two | 009, 001 | 001's criterion 8 |
| a released or reaped attachment still writing | a sync checks that the attachment is active and that the workspace's lease is that attachment's; otherwise `attachment_gone` or `lease_not_held` | 009, 013 | `TestSyncRequiresTheLease` |
| a sandbox reaching outside its workspace | a lease authorizes one workspace subtree; a materialize manifest carries only the pinned paths, and a sync reconciles only under the workspace root | 009 | `TestLeaseIsConfinedToItsSubtree` |
| a bucket that ignores `If-None-Match: *` | every put carries it. A store that answers `NotImplemented` fails `arcad check`, is named by readiness, and the run is recorded as unconditional, logged once per process. The API's compare and swap is in SQL and does not depend on the bucket, so the loss is a collision guard, not the contract | 003, 012, 018 | `TestConditionalCreateRequired`, the store tier against MinIO and against a stub that refuses |
| an event log read by the wrong caller | the tail asks `event.read` like any other action, and an authorizer that wants a caller to see part of a space's log answers with a `filter` the handler applies to its own query; no event carries a byte of object content, a token, or a presigned URL | 010, 006 | `TestEventFilter`, `TestEventDetailIsMetadataOnly` |
| a flood, and the guessing that hides in one | a token bucket per subject after authentication, and one per client address before it; the second bounds both a flood of bad tokens and a search for a link token | this spec, 013 | `TestRateLimits` |
| a client request id used to inject into a log | `X-Request-Id` is kept only when it is at most 128 printable ASCII characters, and is replaced otherwise | 013 | `TestRequestId` |
| a body that exhausts memory | JSON bodies are capped and decoded with unknown fields refused; an object body needs a `Content-Length` and is capped at `ARCA_MAX_UPLOAD_BYTES`; above `ARCA_INLINE_BYTES` the bytes do not pass through the server at all | 013, 007 | `TestBodiesAndTypes` |
| an administrator acting unseen | every allow against a space the caller neither owns nor holds a covering grant on marks its event `admin`, and an administrative mutation appends that event inside the mutation's own transaction, so the record cannot miss one or record one that was rolled back. The record is the event log and no route in the core deletes from it; the reaper prunes it at thirty days, so an installation that must keep longer tails it | 012, 010 | 012's criteria 8, 9, and 10 |
| a secret in a log or a response | every key and bearer is read once at start-up and never logged; a link token is returned once at creation and is absent from every listing; the deploy manifests mount secrets from a Secret | 002, 008, 016, 018 | `TestLogsRedact`, `TestSecretsAreReturnedOnce` |
| a consumer of the module deriving another's key | `object.ID.Key` takes the prefix and an id and derives nothing from a path or an owner, so holding the package grants no ability the API does not | 003 | `TestKeyDerivationIsTotal` |
| a dependency with a known vulnerability | the `vuln` gate on every push, and a dependency list held to invariant 9 of [[001-architecture]] by `depcheck` | 002 | the gate |
| an image that is not what was released | keyless cosign signatures, an SBOM attestation, and a build provenance attestation on both images, verified from a clean runner before the release exists | 016 | the `release-verify` job |
| a test that touches a developer's real state | every tier binds `:0`, keeps files under `t.TempDir()`, uses a schema and a bucket prefix of its own, and removes both | 014 | `TestTiersAreIsolated` |

### Configuration this spec needs

Two variables, each of which joins the table of
[[002-repository-scaffold]]. Every other control here is a constant or
reads a variable that table already holds.

| Variable | Default | Meaning |
|---|---|---|
| `ARCA_REQUESTS_PER_MINUTE` | `600` | the token bucket per subject after authentication; `0` disables it |
| `ARCA_UNAUTHENTICATED_REQUESTS_PER_MINUTE` | `60` | the token bucket per client address before authentication, which bounds bad tokens and link token guessing |

The authorizer's `limits.requests_per_minute` overrides the first for
the subject it names, for the answer's `ttl` ([[006-identity]]). Every
response carries `RateLimit-Limit`, `RateLimit-Remaining`, and
`RateLimit-Reset`, and a 429 adds `Retry-After`.

### What is out of scope

- A compromised `arcad` host. It holds the bucket credential, the
  database URL, and the authorizer bearer, and therefore everything. An
  operator protects it as the root of the installation.
- A compromised bucket or database. They are the installation, not a
  boundary inside it.
- Encryption at rest and in transit between `arcad` and its two stores.
  Both are the operator's: a bucket's server-side encryption and a
  Postgres connection's TLS are configured where they live, and
  `ARCA_DATABASE_URL` carries the mode.
- The security of the issuer and of the authorizer. Arca trusts what the
  operator configured, and the blast radius of a compromised authorizer
  is every space, which is why `arcad check` tests it and why an
  unavailable answer is a refusal rather than a fallback.
- A link holder who gives the link away. The token is the capability;
  that is what a link is, and the control is that it grants reading one
  subtree and is revoked by one row.
- The hosted platform's own policy: who may share with whom, which
  principals may see which paths, and whether an approval queue stands
  between a request and a grant. Arca left all three behind
  ([[008-shares-and-links]], [[013-api]]) and they are answered in an
  authorizer.
- Denial of service beyond the two rate limits, and the cost a large but
  authorized upload imposes on a shared bucket.
- Side channels between installations sharing one bucket, beyond the key
  prefix.

### The root file

`SECURITY.md` carries the reporting address, the response times, and
four commitments, each a row above:

1. Every `/v1` request carries a token from an issuer the operator
   listed, and nothing acts before a decision. An unavailable decision
   is a refusal, and a refusal is indistinguishable from a missing
   object.
2. A byte leaves Arca through an authorized read or a presigned URL that
   names one object, one method, and five minutes, and through nothing
   else.
3. A public link is a 256 bit capability that grants reading one
   subtree, is revoked by one row with no grace window, and can never
   grant a write.
4. Arca reads no claim for meaning. Nothing about a token but its
   issuer, subject, audience, and validity changes what Arca does, so an
   installation's access policy lives in its authorizer and nowhere in
   this code.

A test reads `SECURITY.md` and this file and holds each commitment to a
row of the controls table.

### What arrives from Drive

There is no threat model to inherit. The service Arca replaces has no
`SECURITY.md` and no archived security spec; its archive's spec 016 is
about clone tuning. What arrives is code, and this spec is the first
time the reasoning behind it is written down.

| From | What arrives | What changes |
|---|---|---|
| `drive/internal/storage/s3.go` | the five minute `PresignTTL`, the 24 hour part TTL, and the discipline that no signed URL is logged | unchanged, and now stated as a control with a test |
| `drive/internal/handler/handler.go`'s `validatePath` | the path rule | unchanged in substance; the plane list is [[001-architecture]]'s two rather than five |
| `drive/internal/handler/shares.go`'s `newToken` | 256 bits from `crypto/rand` | rendered base64url rather than hex, so the URL is shorter at the same entropy |
| `drive/internal/webhook/worker.go`'s `GuardedClient`, `PublicIP`, and its signing | nothing | webhooks were retired on 2026-09-18 before drafting closed. The destination rule, the dialer control hook, the delivery signature, and the whole request-forgery surface they defended go with them; a consumer tails the event log ([[010-events-and-reaper]]) |
| `drive/internal/handler/quota.go` | the charge and the check after assembly | the stored limit does not arrive; what arrives is the accounting, moved inside the write's own transaction, so the fail-open on a usage query becomes a refusal |
| `drive/internal/handler/attach.go` | the conditional lease acquisition, the one hour default and twenty-four hour ceiling, and the reaper that frees an orphan lease | unchanged, and now stated as a control with a test |
| `drive/internal/handler/admin.go`'s `admin_audit` writes | the transactional record rule, applied to the event log | the separate table does not arrive; the mark moves to where the decision is made, so every allow against a space the caller does not own is recorded and not only the ones on admin routes ([[012-administration]]) |
| nothing | the rate limits, `Referrer-Policy` on link responses, the conditional-create requirement, the probe question, and `SECURITY.md` | new. The service Arca replaces had no rate limit of any kind, on any route, including the unauthenticated link routes |

## Not in this spec

Each control's own design, which its spec owns. The deploy manifests
that mount the secrets ([[016-release-and-installation]]). The redaction
rules and the access log's fields ([[018-observability]]). The suite an
installation must pass, which includes the refusal cases
([[017-conformance-suite]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Every `Test` cell in the controls table names a function `go test -list ./...` finds, and every test named appears in the acceptance criteria of the spec its `Spec` cell names | `TestThreatModelControlsHaveTests`, reading this file and the spec deck |
| 2 | Every commitment in `SECURITY.md` maps to a row of the controls table, and every row's spec is in this file's `depends_on` | `TestSecurityPolicyMatchesTheModel`, reading both files |
| 3 | A canary object's bytes and a canary link token appear in no log line, no event detail, and no list response across the whole e2e tier | `TestNoSecretsLeak`, grepping the tier's captured output |
| 4 | No signed URL appears in any log line, and the three link routes log a grant id rather than a path | `TestNoPresignedURLIsLogged`, `TestLinkPathsAreNotLogged` |
| 5 | A path holding `..`, `//`, a leading or trailing `/`, or an unknown plane is refused on every route that takes a path, and no bucket key is ever derived from a path | `TestPathRule`, `TestKeyIsTheId` |
| 6 | A caller that completes a multipart larger than the answer's limit allows is refused, the bytes are aborted, and the space's usage returns to what it was | `TestMultipartOverrunIsReaped` |
| 7 | A ledger read or write that fails refuses the write rather than admitting it, and a ledger altered behind the server's back is corrected and reported by the reaper | `TestUsageFailsClosed`, `TestLedgerReconciles` |
| 8 | A workspace whose writer stops calling is writable again after the lease expires and one reaper pass, with no operator action | `TestReaperFreesAnOrphanLease` |
| 9 | A bucket that answers `NotImplemented` to a conditional create fails `arcad check` and is named by readiness, and the server still serves with the condition recorded as unavailable | `TestConditionalCreateRequired` against a refusing stub |
| 10 | An `event.read` answer carrying a `filter` narrows the tail, and no event `detail` carries object content, a token, or a presigned URL | `TestEventFilter`, `TestEventDetailIsMetadataOnly` |
| 12 | The 601st authenticated request in a minute is 429 with the four headers, and the 61st unauthenticated request from one address is 429, link routes included | `TestRateLimits` |
| 13 | An authorizer that allows the probe resource fails `arcad check`, and one that never answers yields 503 and never an allow | `TestCheckRefusesAPermissiveAuthorizer`, `TestAuthorizerFailsClosed` |
| 14 | Every allow against a space the caller neither owns nor holds a grant on marks its event `admin`, and a failed mutation leaves no event | 012's criteria 8 and 9 |
| 15 | `cosign verify` and `gh attestation verify` accept both released images from a clean runner | the `release-verify` job of [[016-release-and-installation]] |
