---
title: "Conformance suite: the contract as an importable test package against any base URL"
status: complete
track: core
depends_on:
  - specs/001-architecture.md
  - specs/013-api.md
  - specs/014-test-stubs-and-tiers.md
affects: [test/conformance/, internal/auth/, internal/config/, .github/workflows/verify.yml, .github/workflows/release.yml, docs/]
effort: large
created: 2026-09-18
updated: 2026-09-19
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

Built and in the tree on 2026-09-19. `test/conformance` holds the suite,
`contract_test.go` under the `tiers` tag drives it, `make
test-conformance` runs it against the compose stack, and the
`conformance` job of [[016-release-and-installation]] runs it against
the published image on the kind stack.

Against this build, which answers all forty-one of [[013-api]]'s
routes, a run is **fifty-three cases passed, none failed, and none
skipped**. The pending group passes with no route outstanding, which is
the evidence [[016-release-and-installation]]'s gate reads: the
installation this build makes serves the whole contract and not part of
it. That is a measured run and not a local one: the `conformance` job of
`.github/workflows/release.yml` drove those cases against the image the
tag's `build` job had just pushed, on a kind cluster, and logged `53
passed, 0 failed, 0 skipped, 48 objects created and deleted` twice — in
run 35467474612 for `v0.1.7`, which published, and in run 35464362441 for
`v0.1.6`, whose conformance job was green and whose deploy step then
failed.

Twenty-six of the thirty rows of [[013-api]]'s error table are provoked
by a case, which declares the codes it makes the target answer, and the
remaining four are recorded with the reason no caller outside the
installation can make one: `internal` and `storage_unavailable` are a
fault inside it, `rate_limited` would be a denial of service against
whoever else is using a shared installation, and `not_implemented` is
read off the served document by the pending group.

| Group | On this build | Recorded |
|---|---|---|
| identity | passes, 4 cases | the expired token and the wrong audience are `Unverified`, below |
| authorizer | passes, 4 cases | |
| paths | passes, 1 case | the file routes add nothing it cannot already see through `POST /v1/shares` |
| files | passes, 4 cases | |
| bytes | passes, 1 case | |
| versions | passes, 1 case | |
| trash | passes, 1 case | |
| stars | passes, 1 case | the star a caller may not read is `Unverified`, below |
| conditional writes | passes, 1 case | |
| uploads | passes, 4 cases | |
| shares | passes, 3 cases | |
| links | passes, 4 cases | the anonymous half is `Unverified`, below |
| workspaces | passes, 6 cases | |
| events | passes, 4 cases | |
| usage | passes, 3 cases | |
| administration | passes, 3 cases | the ordinary caller's half is `Unverified`, below |
| errors | passes, 7 cases | |
| pending | passes, no route outstanding | |

Five assertions are recorded in `Report.Unverified` rather than made,
each because the target gave no way to make it and none because it
failed. Two are the identity rows below: a token past its expiry and one
addressed to another audience. Three more are answered away by the
stack's stub authorizer, which allows every caller every space and every
action, so there is no refusal to read: a refused space
(`006/AnotherSpace`), a star on an object the caller may not read
(`005/Stars`), and an administrative route seen by an ordinary caller
(`012/NotAnAdministrator`). The fifth is a public link served with no
bearer (`008/Link`), which needs `Options.Anonymous`.

The pending group stays, and it is not a formality. It is one case that
fails with every route [[013-api]] names and the target does not answer,
and each case that drives such a route reports it by name rather than
skipping. It is empty here; against a partial build, an alternative
implementation, or a consumer's own front, it is the difference between
a target that serves the contract and one that serves part of it.

The administration group runs because the tier now names an
administrator. `make test-conformance` passes `ARCA_TEST_ADMIN`, which
is one name read twice: the installation the tier starts is told to
treat that subject as an administrator, and the suite mints its token at
the stub issuer. The release job's step passes the same name against the
kind stack.

Criteria 1, 2, 3, 4, 5, 6 and 7 have passing tests. Three of them are
named in the table below by a test that was folded into others while the
suite was built, and the table names the real ones now: criterion 1 is
`TestEveryGroupHasACase` and `TestSkipsCarryAReason`, criterion 4's
first half is `TestRunCleansUpWhatItMade` with
`TestCleanupReportsAFailureAndKeepsGoing` beside it, and criterion 6 is
`TestSurfaceReadsTheServedDocument` and
`TestPendingFailsUntilTheRoutesLand`, for the reason in the
optional-route entry below. Criterion 2's route half is
`TestEveryRouteHasACase` and its code half is
`TestEveryCodeIsProvokedOrNamed`, which reads the codes each case
declares rather than what a run happened to see, so the rule holds
against a build that serves none of the routes. Criterion 4's second
half is `TestConcurrentRuns`, with one narrowing recorded below.

Criteria 8 and 9 were both open on 2026-09-19 morning. A release closed
one and not the other.

Criterion 8 is met. Its import graph half was always proved by
`TestTheSuiteReachesNoHelperOfThisTree`. Its other half, the documented
command running from a clean checkout against a URL, is what the
`conformance` job of `.github/workflows/release.yml` does: the job checks
the repository out fresh, brings the kind stack up from the published
images, and runs the command of the table below against a URL, with
`TestTheSuiteReachesNoHelperOfThisTree` named in the same `-run` pattern
so the command and the import graph are proved in one invocation. It ran
green in run 35464362441 for `v0.1.6` and run 35467474612 for `v0.1.7`,
each logging `53 passed, 0 failed, 0 skipped`.
The `conformance-external` job this criterion first named was never
written and is not needed: the job that exists does what the criterion
asks. The one thing it does not do is run from outside this repository's
checkout, and that is the half `TestTheSuiteReachesNoHelperOfThisTree`
proves by reading the import graph instead.

Criterion 9 as first written is not met, and it left rather than waited;
the row below now says what the pipeline proves, which is this release's
suite against this release's published image with no route pending. A
release now exists, so the blocker on the N-1 half is gone, but building
it is not an edit to the suite: the previous tag's driver carries the
`tiers` tag and imports
`test/stubs/issuer` and `test/stubs/authorizer`, so checking out
`test/conformance` alone into a temporary module does not build, and the
only place the step runs is a release's own `conformance` job against a
candidate image that exists after `build` has pushed it. It is
[[024-conformance-against-a-published-release]] now, at `validated`,
carrying the criterion verbatim; [[016-release-and-installation]]'s
criterion 9 cites the same test.

What the implementation decided, where this spec was silent or where the
tree made another reading better:

- The input is `Options` rather than the `Config` this spec first wrote,
  because `Options` is the word the tree already uses for what a package
  is built from (`api.Options`). The field set is this spec's unchanged,
  and the block below says `Options`.
- `Report` gains `SkippedGroups`, so a group that skipped whole is named
  once rather than once per case, and `Pending`, which is the routes of
  [[013-api]] the target does not answer.
- The build tag is `tiers`, the one every tier of this repository
  carries ([[014-test-stubs-and-tiers]]), rather than the `e2e` this
  spec first wrote. That tag was settled before this spec was built, and
  a second one would be a second way to say the same thing. "Where it
  runs" below says `tiers`. The files that carry `Run` are untagged, so a
  consumer imports the suite and none of the driver.
- A route [[013-api]] names and the target does not answer puts every
  case that drives it in the pending group, above. This spec wrote the
  optional-route rule for a `501`; [[013-api]] marks no route optional,
  and a build part way through [[019-migration-from-drive]] is the case
  that actually arises, so the suite reads the served document and
  reports the difference.
- The forty-one routes are declared in the suite rather than read from
  the server's registrations, and the error table is a copy held equal
  to [[013-api]] by `TestErrorTableMatchesTheSpec`. A suite that read
  the build's own list would drop a route with it.
- `TestEveryCriterionHasACase` runs in the direction that catches a lie:
  every marker in the deck has a case. The reverse is not required. A
  case drives a route of [[013-api]]'s table, which criterion 2 holds
  whole, and a marker per case would put forty-one rows into the
  acceptance tables of specs this one does not own.
- `TestSuiteCatchesADrift` runs the suite in a process of its own and
  reads its report. A case that fails fails the test it was given, so a
  test that wants a failure cannot also be the test that takes it. It
  compares a set of case names rather than one: a drift is applied where
  an answer reaches the wire, and every case that reads that answer sees
  it.
- The three drifts are applied at the one place that owns each answer:
  `paths` at the frame's plane check, which `internal/shares` now asks
  instead of reading [[001-architecture]]'s two prefixes itself; `codes`
  at `WriteError`; `etag` at `SetETag`. All three are caught against this
  build. `etag` is read by the three cases that assert a validator, and
  not by `005/Versions`: a version history names a checksum in the body
  of a listing and no validator, so a build whose `ETag` is not the
  object's checksum answers that listing exactly as a conforming build
  does.
  `ARCA_TEST_DRIFT` is read in the ordinary build rather than refused
  outside a test build, which is what this spec's own argument asks for:
  every value refuses something or answers with the wrong shape, none
  grants an authority, and `arcad` says so on the line an operator reads
  at start-up.
- Two rows of the identity group are recorded in `Report.Unverified`
  rather than asserted: a token past its expiry and one addressed to
  another audience are signatures a suite with a mint function and no
  signing key cannot produce. Both are proved in process by the family's
  audience suite, which is already in the tree.
- The family audience suite is `TestServiceConformance` in
  `internal/auth`, landed with [[006-identity]] rather than here, which
  is where this spec placed it. Criterion 7 is that test.
- `test/conformance` is exempt from the coverage floor with the reason
  in `.lateregate.yaml`: a case is a request against a running
  installation, so its statements run in this tier and not in the unit
  run. The unit run measures the machinery, and the tests beside it hold
  that to the contract.
- `TestConcurrentRuns` skips the authorizer and usage groups. Object
  isolation is what criterion 4 is about, and those two are the two
  groups that do not touch it: both reach through one stub's rule table,
  so two runs would be changing each other's verdicts rather than each
  other's objects. What the test asserts is that two runs against one
  installation each pass, each create something, and create nothing the
  other created.
- A case declares the codes of [[013-api]]'s table it provokes, beside
  the routes it drives. Counting what a run answered would make the rule
  hold only where the routes are served, and criterion 2 is about the
  suite rather than about a build.
- `deploy/examples/kind` gave the stub issuer no `-issuer-url`, so it
  named itself `http://0.0.0.0:8081` while `ARCA_OIDC_ISSUERS` listed
  `http://arca-stubs:8081`, and `arcad` refused every token that stack
  minted. Nothing caught it: the release smoke reads the probes, which
  carry no bearer. The overlay now names the in-cluster address, which
  is what this spec's job is the first thing to need.
- `make test-conformance` is not part of `make check-all`. It starts a
  server per drift and runs the suite against each in a subprocess,
  which is minutes of stores and processes rather than the seconds the
  default bar is written to take.
- Six cases were written from [[013-api]]'s shapes before the routes
  they drive were answered, and six read a shape the served build does
  not have: a version history holds what an overwrite kept and not the
  row at the path, a purge answers how many entries went, a star answers
  no body, an upload manifest names a part by `n`, and the two cases
  that follow a presigned URL were holding the bucket's answer to this
  API's request-id header. Each was corrected against the spec that owns
  the shape, and one assertion was added where a case could not
  otherwise see the `etag` drift: a move answers an `ETag` and renders a
  `checksum`, and [[013-api]] makes them one value. The server was not
  changed. No case found a route answering other than [[005-files]],
  [[007-uploads]] and [[012-administration]] fix it.

## Design

### The package

```go
// Run executes every group the inputs allow and returns what happened.
func Run(t *testing.T, opts Options) Report

type Options struct {
	URL string // the base URL of the installation, without /v1

	// Token mints a bearer for a subject. The suite asks for three:
	// "alice" and "bob", two unrelated principals, and whatever subject
	// Admin names. A target with a fixed set of tokens returns them.
	Token func(ctx context.Context, subject string) (string, error)
	Admin string // the subject the target treats as an administrator

	AuthorizerControl string // the stub authorizer's control URL; empty skips the deny, outage, and byte-limit cases
	Anonymous         bool   // the target serves public links to an unauthenticated caller
	BucketDial        string // the address the bucket is reached at from where the suite runs; empty dials presigned URLs as given

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

`TestContract`, in `test/conformance/contract_test.go` under the `tiers`
build tag, wraps `Run`. It reads `-url`, either `-issuer` (a stub issuer
to mint every subject from) or `-token`, `-token-bob` and `-admin`, plus
`-authorizer`, `-anonymous` and `-s3-endpoint`, each falling back to the
`ARCA_TEST_*` variable of the same name. With no `-url` it starts an
installation of its own on the compose stack of [[014-test-stubs-and-tiers]],
which is what `make test-conformance` runs; with no stack either there is
nothing to drive and it skips whole with the remediation.

`-s3-endpoint` (`ARCA_TEST_S3_ENDPOINT`) is `BucketDial`, and it exists
because a presigned URL is signed over the host it names. A target on a
cluster signs for the bucket address it was configured with, which is a
name the cluster resolves and the runner does not, so the two cases that
follow one, `005/Bytes` and `007/Complete`, cannot dial it. Rewriting the
URL's host would break the signature at the store, so the suite dials the
address named here and sends the signed host in the request's `Host`
header, leaving the path and the query the signature also covers alone. A
run beside the target, where one address serves both sides, leaves it
empty.

| Target | Command | Groups that skip |
|---|---|---|
| an installation the tier starts on the compose stack | `make test-conformance`, which is `go test -tags=tiers ./test/conformance/...` over `TestContract`, `TestConcurrentRuns`, `TestSuiteCatchesADrift` and `TestTheSuiteReachesNoHelperOfThisTree` with no `-url`: the tier reads the stack from the `E2E_` variables of [[014-test-stubs-and-tiers]], starts `arcad` and both stubs itself, and drives that | none; the stubs of [[014-test-stubs-and-tiers]] supply the authorizer control |
| the kind stack | the same with `-url`, `-issuer`, `-authorizer` and `-admin` on the stack's node ports, and `ARCA_TEST_S3_ENDPOINT` on MinIO's, because the target signs a presigned URL for an address only the cluster resolves | none |
| a released installation | the same with `-token`, `-token-bob`, `-admin` and no `-issuer` | `authorizer` and the byte-limit cases of `usage` when the installation runs no stub, each reported by name |
| a consumer's own front | the same, from that consumer's CI | whatever their `Skip` list names |

A CI job, not a Go test, proves the external form: a clean checkout runs
the documented command against a URL, so the package a consumer imports
pulls in no helper from this repository's test tree. That job is
`conformance` in `.github/workflows/release.yml`. It checks the
repository out fresh, brings the kind stack up from the images the
release just published, and runs the kind row's command with
`TestTheSuiteReachesNoHelperOfThisTree` in the same invocation, so the
command and the import graph are proved together against a published
image rather than against a working tree.

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
running the previous release's suite against this release's binary: a
case the previous suite holds and the new binary fails is a break that
must become a major or be fixed before the tag. That run is
[[024-conformance-against-a-published-release]] and is not built. The
`conformance` job today runs this release's own suite against the
candidate image, which proves the tag serves the contract as this tag
writes it and not that it still serves the contract the tag before it
wrote.

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
timing, and cost; a case asserts an answer and never a latency. Running a
previous release's suite against this release's binary, which is a step
in the release pipeline and a temporary module rather than a case here
([[024-conformance-against-a-published-release]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Every group in the table exists with the scope named, and skips with a reason in the report when its input is empty | `TestEveryGroupHasACase`, `TestSkipsCarryAReason` |
| 2 | Every route of [[013-api]] is named by at least one case, and every code of its error table the suite can provoke is asserted | `TestEveryRouteHasACase` reading the served document against the case list |
| 3 | Every marker in the deck has a case and every case a marker | `TestEveryCriterionHasACase` over `specs/` and `specs/.archive/` |
| 4 | A run leaves nothing behind, and two concurrent runs against one installation touch none of each other's objects | `TestRunCleansUpWhatItMade`, `TestCleanupReportsAFailureAndKeepsGoing`, `TestConcurrentRuns` |
| 5 | `arcad` started with each value of `ARCA_TEST_DRIFT` fails exactly the named group's case and no other | `TestSuiteCatchesADrift` |
| 6 | A route of [[013-api]]'s table the target does not answer puts every case that drives it in the pending group, which fails naming the route and the spec that owns it, and a target serving no document is held to the whole table | `TestSurfaceReadsTheServedDocument`, `TestPendingFailsUntilTheRoutesLand`. The `501` rule this criterion first wrote is superseded: [[013-api]] marks no route optional, so the difference the suite reports is what the served document names against what the contract does |
| 7 | The verifier `arcad` installs admits `arca` and refuses the issuer's audience, another audience, and a token with no subject, calls the issuer only for its key set, and reads no flag in place of a role | `TestConformance` in `internal/auth`, the family suite |
| 8 | The package a consumer imports pulls in no helper from this repository's test tree, and the documented command runs from a clean checkout against a URL | `TestTheSuiteReachesNoHelperOfThisTree` for the import graph, and the `conformance` job of `.github/workflows/release.yml` for the command: it checks out clean, brings the kind stack up from the published images, and runs both in one invocation. Green in run 35464362441 (`v0.1.6`) and run 35467474612 (`v0.1.7`), each `53 passed, 0 failed, 0 skipped` |
| 9 | This release's suite passes against this release's published image, with no route in the pending group | the same `conformance` job and the same two runs; a failed case, the pending group included, stops the release ([[016-release-and-installation]]). Running the **previous** release's suite against this release's binary was this criterion's first form and is [[024-conformance-against-a-published-release]], which carries it verbatim: the previous tag's driver imports this repository's stub packages, so the step is a temporary module and a release-only run rather than an edit here |

## Outcome

Complete on 2026-09-19. `test/conformance` is in the tree with
fifty-three cases across eighteen groups, `contract_test.go` under the
`tiers` tag drives them, `make test-conformance` runs them against the
compose stack, and the `conformance` job of
`.github/workflows/release.yml` runs them against the image a tag just
pushed, on a kind cluster. That job logged `53 passed, 0 failed, 0
skipped` in run 35464362441 for `v0.1.6` and in run 35467474612 for
`v0.1.7`, and `v0.1.7` is the release that serves `api.latere.ai`. A
release does not publish on a failed case, the pending group included, so
the suite is the gate [[016-release-and-installation]] reads and not a
report.

| Criterion | What closed it |
|---|---|
| 1, 2, 3 | `TestEveryGroupHasACase`, `TestSkipsCarryAReason`, `TestEveryRouteHasACase`, `TestEveryCodeIsProvokedOrNamed` and `TestEveryCriterionHasACase`, each reading the deck or the served document rather than a run's output |
| 4, 5 | `TestRunCleansUpWhatItMade`, `TestCleanupReportsAFailureAndKeepsGoing`, `TestConcurrentRuns`, and `TestSuiteCatchesADrift` over the three `ARCA_TEST_DRIFT` values |
| 6 | `TestSurfaceReadsTheServedDocument` and `TestPendingFailsUntilTheRoutesLand`; the `501` rule the criterion first wrote is superseded, as its row records |
| 7 | `TestConformance` in `internal/auth`, the family audience suite |
| 8 | met by the `conformance` job, which runs the documented command from a clean checkout against a URL with `TestTheSuiteReachesNoHelperOfThisTree` in the same invocation. The `conformance-external` job it first named was never written and is not needed |
| 9 | narrowed to what the pipeline proves: this release's suite against this release's published image, with no route pending |

Five statements were corrected against the tree while closing, none of
them a change to code:

- the Design block named the input `Config`, where `test/conformance` has
  `Options`;
- "Where it runs" put `TestContract` under the `e2e` tag, where
  `contract_test.go` is `//go:build tiers`;
- the same section said the tier skips whole when `-url` is empty. It
  starts an installation of its own on the compose stack instead, which
  is what `make test-conformance` runs, and skips only with no stack
  either. The command table's rows now say what each target's run is, and
  the kind row names `ARCA_TEST_S3_ENDPOINT`;
- the run was recorded as fifty-one cases, with the authorizer group at
  three and usage at two, where the suite has fifty-three, four and
  three. The counts above are what the two release runs printed;
- "Versioning with the API" claimed the `conformance` job checks out the
  previous tag's suite. It does not, and that sentence now names the spec
  that will.

One criterion was split rather than built.
[[024-conformance-against-a-published-release]] carries criterion 9's
first form verbatim. Its blocker is gone — a release exists and carries a
`test/conformance` — but closing it is a temporary module requiring
`latere.ai/x/arca` at the previous tag, because that tag's driver imports
this repository's stub packages, and a step that runs only inside a
release's `conformance` job against a candidate image. It cannot be
exercised on `main`, so it is a change with a release cycle attached
rather than an hour before a tag. Every other divergence from the draft
is in Current state above.
