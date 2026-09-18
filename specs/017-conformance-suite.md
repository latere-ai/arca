---
title: "Conformance suite: the contract as an importable test package against any base URL"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/013-api.md
  - specs/014-test-stubs-and-tiers.md
affects: [test/conformance/, internal/auth/, internal/config/, .github/workflows/verify.yml, .github/workflows/release.yml, docs/]
effort: large
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Conformance suite

## Overview

The contract of [[013-api]] is worth something only if it is checked.
`test/conformance` is an importable Go test package that, given a base
URL and a way to mint tokens, drives every route of [[013-api]] and
every invariant of [[001-architecture]] that is visible on the wire,
and reports which cases held, which failed, and which were skipped and
why.

Three parties run it. `arcad` runs it on every tier
([[014-test-stubs-and-tiers]]) and a release does not publish until it
is green from the published images ([[016-release-and-installation]]).
A consumer that puts its own front in front of Arca runs it against
that front, to prove the edge did not change what a request means. An
alternative implementation runs it to claim it serves the Arca API; the
suite is the whole of that claim.

The suite is black box. It holds no import of `internal/`, opens no
database connection, and reaches no bucket except through a presigned
URL the server handed it. Anything that needs a hand inside the
installation is not a case here.

## Current state

Not built.

## Design

### The package

```go
// Run executes every group the inputs allow and returns what happened.
func Run(t *testing.T, cfg Config) Report

type Config struct {
	URL string // the base URL of the installation, without /v1

	// Token mints a bearer for a subject. The suite asks for three:
	// "alice" and "bob", two unrelated principals, and whatever subject
	// Admin names. A target with a fixed set of tokens returns them.
	Token func(ctx context.Context, subject string) (string, error)
	Admin string // the subject the target treats as an administrator

	AuthorizerControl string // the stub authorizer's control URL; empty skips the deny, outage, and byte-limit cases
	Anonymous         bool   // the target serves public links to an unauthenticated caller

	Skip []string // group or case names to skip, each reported as skipped by request
}

type Report struct {
	Passed, Failed, Skipped []string          // case names, <NNN>/<Name>
	Reasons                 map[string]string // why each skipped case skipped
	Created                 []string          // every object the run made and deleted
	Unverified              []string          // cases whose assertion the target could not converge
}
```

Cases are `case<NNN><Name>` functions, run as subtests `<NNN>/<Name>`,
named after the spec whose criterion they prove. Each runs under a two
minute timeout. The client is `net/http` and the structures of
[[013-api]]; the suite decodes into its own types, never the server's,
so a rename inside `internal/api` that changes the wire fails a case
instead of compiling.

### Isolation

Every path, workspace slug, share, and link the run creates carries the
prefix `arca-conformance-<run>-`, where `<run>` is
drawn at start. The suite records the id of everything it creates and
deletes exactly those ids at the end, never by prefix and never by
listing, so two runs against one installation touch nothing of each
other's and a run against a live installation leaves the objects
somebody else put there alone. A case that cannot delete what it made,
because a case deliberately filled the space to the limit the stub
authorizer was told to answer with, records the id in `Report.Created`
anyway and the teardown lifts that limit before it deletes.

### What the suite learns from the server

The suite never assumes a limit. It reads the part size and the inline
threshold from the upload session the server creates ([[007-uploads]]),
the space's usage from the administrative overview when `Admin` is set
([[012-administration]]), the byte limit from the answer it told the
stub authorizer to give ([[010-events-and-reaper]]), and the routes the
target serves from the document [[013-api]] publishes. A route [[013-api]] marks
optional is skipped only when the target answers `501` for it, with the
route in the reason; a target that answers `404` for an optional route
fails, because a missing capability and a missing object are different
answers.

### Groups

Every group names its routes and the codes it provokes. A group whose
input is empty skips with a reason in the report and never silently.

| Group | Proves | Routes and codes | From |
|---|---|---|---|
| identity | a request with no bearer, an expired one, one from an issuer the target does not list, and one whose audience is not `arca` are each refused with `unauthenticated`; a request for another principal's space is `not_found` and never `forbidden` (invariant 6); an action a principal may not take on its own space is `forbidden` | every route; `unauthenticated`, `forbidden`, `not_found` | 001, [[006-identity]] |
| authorizer | under `AuthorizerControl`: a flipped deny refuses before lookup, an authorizer that is unreachable refuses every request with `authorizer_unavailable` and never allows, and a target with no authorizer configured applies the owner policy, which the same cases assert by their answers | every route; `authorizer_unavailable` | 001, [[006-identity]] |
| paths | the two planes of [[001-architecture]] answer under their prefixes and a path under a third prefix is refused; a path that escapes its space is refused; a name the object model forbids is `invalid_field` | the file routes; `invalid_path`, `invalid_field` | 001, [[005-files]] |
| files | put, get, head, list, move, delete over the `files/` plane; the ETag and the checksum the server returns match the bytes sent; a conditional put with `If-None-Match: *` creates once and is refused the second time; a move leaves the ETag equal, which is invariant 8 seen from outside | the file routes; `already_exists`, `not_found` | 005, 003 |
| bytes | an object below the inline threshold streams through the server, one above it answers a redirect; the redirect names one object and one method, is refused after its expiry, and is refused for a second object; this is invariant 4 seen from outside | `GET` on a file; `not_found` on the expired URL from the store | 001, [[003-object-store]] |
| versions | a second put makes a version, the list is ordered, an old version is readable and restorable, and a delete of the current version does not delete the history | the version routes | 005 |
| trash | a delete moves an object to trash, a trashed object is absent from the listing and readable from the trash route, a restore returns it to its path, a purge removes it, and a purge of an object another case starred clears the star | the trash routes; `not_found` | 005 |
| stars | a star and an unstar, the starred listing, and a star on an object the caller may not read refused as missing | the star routes | 005 |
| uploads | a session above the part size, part URLs used directly against the bucket, completion with the part ETags, a completion with a part missing refused, an abort, a session that is not completed expiring, and a declared size above `ARCA_MAX_UPLOAD_BYTES` refused at creation | the upload routes; `too_large`, `invalid_field`, `not_found` | [[007-uploads]] |
| shares | each rung of the permission ladder grants what it names and nothing above it, a grant is revocable, a revoked grant takes effect on the next request, and the shared-with-me listing shows the grant to the grantee and to nobody else | the share routes; `forbidden`, `not_found` | [[008-shares-and-links]] |
| links | a public link serves the object to an unauthenticated caller when `Anonymous` is set, an expired link is refused, a revoked link is refused, and a link to an object whose grant was revoked is refused | the link routes | 008 |
| workspaces | two attaches yield one lease and the second is refused, which is invariant 7 seen from outside; a renew extends it, a release ends it, a lease expires without a renew, and materialize and sync round-trip a subtree byte for byte | the workspace routes; `lease_held`, `lease_expired` | [[009-workspaces]] |
| conditional writes | a conditional write with the current ETag applies, one with a stale ETag is refused, a create-only write onto an existing path is refused, and a write with neither header succeeds on every path | the file routes; `precondition_failed` | [[005-files]], [[013-api]] |
| usage | under `AuthorizerControl`, with a byte limit on the answer: a write that would cross it is refused and the object does not appear, a delete frees room and the same write then succeeds, and with no limit on the answer no write is refused for size; under `Admin`, the overview's `bytes` for the space equals the sum of what the run put | the file routes and the overview; `quota_exceeded` | [[010-events-and-reaper]] |
| events | every mutation the run makes appears on the event cursor, in the order the run made them, with the cursor resumable from a saved position; no event of another principal's space appears; a token value planted in a file name appears in no event | the event routes | 010 |
| administration | under `Admin`: the overview pages one row per space with its usage, a restore returns an object from another space, an administrator's own moderation delete appears on that space's event tail marked `admin`, and each of those is refused with `not_found` for a non-administrator | the two admin routes, the file routes, the event routes; `forbidden`, `not_found` | [[012-administration]] |
| errors | every code of [[013-api]]'s table the suite can provoke arrives with the status and the sentence that table fixes, and the two public documents answer without a bearer | every route | 013 |

### The invariants this suite can see

| Invariant of [[001-architecture]] | Seen on the wire as | Or, if not |
|---|---|---|
| 1, bucket first on write, database first on delete | not visible without a fault between the two stores | the e2e tier of [[014-test-stubs-and-tiers]] |
| 2, the database decides existence, the bucket decides content | not visible without deleting bytes underneath a row | the e2e tier |
| 3, stateless replicas | a session created against one replica completes against another, which a target behind more than one replica exercises by itself | asserted, and recorded in `Report.Unverified` against a single-replica target |
| 4, bytes off the hot path | the `bytes` group | asserted |
| 5, verify and ask | the `identity` and `authorizer` groups | asserted |
| 6, a refused object answers as a missing one | the `identity` group | asserted |
| 7, one writer per workspace | the `workspaces` group | asserted |
| 8, the key is not the path | the `files` group's move case: the ETag survives the move | asserted |
| 9, no cloud SDK, no Kubernetes client | a build-list property | the `depcheck` gate |
| 10, nothing Latere in the tree | a tree property | the `identity` gate |

### The family audience suite

`latere.ai/x/pkg/authkit/conformance` is the family's rule that a
service verifies `aud` as itself and reads no flag in place of a role.
It is in-process and takes a constructor rather than a base URL, so it
is not a case of `Run`. It runs as `TestConformance` in
`internal/auth`, beside the verifier it tests:

```go
conformance.Run(t, conformance.Service{
	Audience: "arca",
	New: func(tb testing.TB, issuerURL, jwksURL string) authkit.Authenticator {
		// the verifier arcad installs in production, over the stub issuer
	},
})
```

The six checks are the suite's: the audience `ARCA_OIDC_AUDIENCE`
names is admitted, a token addressed to the issuer itself is refused, a
token addressed to another service is refused, a token that names no
subject is refused, the issuer is called for its key set and for
nothing else, and the retired superadmin flag grants no role. The
constructor is the one `cmd/arcad` calls, so the verdict is the
server's and not a copy, and the suite hands it a stub issuer, so the
test needs nothing running.

Both suites together are the contract. The `conformance` job of
[[016-release-and-installation]] runs the in-process suite with the
unit tier and the URL-driven suite against the candidate image, and a
release that is green on one and not the other does not publish.

### Where it runs

`TestContract`, under the `e2e` build tag, wraps `Run`. It reads
`-url`, either `-issuer` (a stub issuer to mint every subject from) or
`-token`, `-token-bob` and `-admin`, plus `-authorizer`, and skips
whole with the reason when `-url` is empty.

| Target | Command | Groups that skip |
|---|---|---|
| the unit tier's in-process server | `go test -tags e2e -run '^TestContract' ./test/conformance -args -url $ARCA_TEST_URL -issuer $ARCA_TEST_ISSUER` | none; the stubs of [[014-test-stubs-and-tiers]] supply the authorizer control |
| the kind stack | the same against the stack's node port | none |
| a released installation | the same with `-token`, `-token-bob`, `-admin` and no `-issuer` | `authorizer` and the byte-limit cases of `usage` when the installation runs no stub, each reported by name |
| a consumer's own front | the same, from that consumer's CI | whatever their `Skip` list names |

A CI job, not a Go test, proves the external form: a clean checkout
runs the documented command against a URL, so the package a consumer
imports pulls in no helper from this repository's test tree.

### The marker

A criterion of any spec is a conformance case when its `Proved by`
column contains the literal `caseNNNName`.
`TestEveryCriterionHasACase` reads every file under `specs/` and
`specs/.archive/`, collects every marker, and fails on a marker with no
function of that name in `test/conformance` or a function with no
marker. It resolves the specs directory from its own source file
through `runtime.Caller`, because the `tempdir` gate runs the suite
from an empty directory.

### The drift seam

`ARCA_TEST_DRIFT`, a variable that joins the table of
[[002-repository-scaffold]], names one case the server is to answer
wrong: with `paths` set, `arcad` accepts a fifth path prefix that is
not a plane; with `codes` set, one route answers `404` where
[[013-api]] fixes `409`; with `etag` set, a move returns a fresh ETag
instead of the one the object had. Every value makes the server refuse
something it should serve or answer with the wrong shape, and no value
grants an authority the server would otherwise withhold, so a seam set
by accident on a real installation is a visible defect and never a way
past a decision. It is empty in every deployment.
`TestSuiteCatchesADrift` starts `arcad` with each value and asserts
exactly the named group's case fails and no other.

### Versioning with the API

The suite ships in the module, so `latere.ai/x/arca/test/conformance`
at `v<tag>` is the suite for the API of that tag. A case for a new
route arrives in the same minor as the route. A case for a route a
major removes is removed in that major and in no earlier release. A
case is never weakened to make a release green: a target that no longer
passes a case either has a defect or is a major bump.

N-1 compatibility ([[016-release-and-installation]]) is proved by
running the previous release's suite against this release's binary. The
`conformance` job checks out `test/conformance` at the previous tag
into a temporary module and runs it against the candidate image; a case
the previous suite holds and the new binary fails is a break that must
become a major or be fixed before the tag.

### What arrives from Drive

| From the service | What it becomes |
|---|---|
| `drive/internal/handler/conformance_test.go` | the family audience suite above, moved to `internal/auth` beside the verifier and with `Audience: "arca"`; the service verified two audiences named after its own hostnames, and the core verifies one name that is not a hostname |
| `drive/test/e2e` | the case list. The service's thirty-odd e2e files are in-process tests against a harness that holds the handler, the pool, and the bucket, so they read the database to assert. Each assertion that survives only through a response becomes a case here, named for the spec that owns it; each that needs a hand inside the installation stays an e2e test in [[014-test-stubs-and-tiers]] |
| `drive/test/e2e/faults_test.go`, `targeted_faults_test.go`, `multipart_faults_test.go`, `versions_faults_test.go`, `visibility_faults_test.go`, `errmap_faults_test.go` | the e2e tier's, not this suite's. They cut the bucket or the pool underneath a request, which no caller outside the installation can do. Invariants 1 and 2 are proved there |
| `drive/test/e2e/openapi_conformance_test.go` | the `errors` group: the served document and the handlers agree on every code and status |
| `drive/test/e2e/audience_test.go`, `roles_test.go` | the `identity` group, plus the in-process audience suite above |
| `drive/test/e2e/mount_conformance_test.go` | the `workspaces` group's materialize and sync cases, without the filesystem mount the service ran for its own client |

## Not in this spec

The stubs and the tiers the suite runs on
([[014-test-stubs-and-tiers]]). Fault injection: two-store ordering
under an injected failure is the e2e tier's and no `Fault` interface is
part of this package, so a consumer importing it needs no cluster
helper. The routes and the codes themselves ([[013-api]]). Load,
timing, and cost; a case asserts an answer and never a latency.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Every group in the table exists with the scope named, and skips with a reason in the report when its input is empty | `TestGroupsAndSkips` |
| 2 | Every route of [[013-api]] is named by at least one case, and every code of its error table the suite can provoke is asserted | `TestEveryRouteHasACase` reading the served document against the case list |
| 3 | Every marker in the deck has a case and every case a marker | `TestEveryCriterionHasACase` over `specs/` and `specs/.archive/` |
| 4 | A run leaves nothing behind, and two concurrent runs against one installation touch none of each other's objects | `TestRunCleansUp`, `TestConcurrentRuns` |
| 5 | `arcad` started with each value of `ARCA_TEST_DRIFT` fails exactly the named group's case and no other | `TestSuiteCatchesADrift` |
| 6 | A target that answers `404` for an optional route fails, and one that answers `501` skips with the route in the reason | `TestOptionalRouteDiscipline` against a lying server |
| 7 | The verifier `arcad` installs admits `arca` and refuses the issuer's audience, another audience, and a token with no subject, calls the issuer only for its key set, and reads no flag in place of a role | `TestConformance` in `internal/auth`, the family suite |
| 8 | The package a consumer imports pulls in no helper from this repository's test tree, and the documented command runs from a clean checkout against a URL | the `conformance-external` CI job |
| 9 | The previous release's suite passes against this release's binary, or the tag is a major | `TestPreviousSuitePasses`, run by the `conformance` job of [[016-release-and-installation]]; this spec owns the test and that spec cites it |
