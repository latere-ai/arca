# Security

Arca stores other people's files, hands out links that need no sign-in,
and lets a program write into a space a person owns. This file is how to
tell us when it fails at that, what the project commits to, what it does
not defend, and what has and has not been checked.
[`docs/security.md`](docs/security.md) is the same for the people who run
Arca and build on it, with what the server can see. The full
[threat model](specs/015-security-and-threat-model.md) names every
threat with its control and the test that holds it.

## Reporting a vulnerability

Report it through
[GitHub private vulnerability reporting](https://github.com/latere-ai/arca/security/advisories/new),
or by email to security@latere.ai. Do not open a public issue for it.
The [organization's security policy](https://github.com/latere-ai/.github/blob/main/SECURITY.md)
says when you will hear back, how a fix is disclosed, and the safe harbor
for research that follows it.

Fixes go to the two most recent minor release series.
v0.12.2 is the current release.

These are the reports we most want: anything that lets a caller read or
change an object, a grant or an event without the authorizer's allow, or
in a space it was not given; a refusal that answers differently from a
missing object; a decision the authorizer could not give that ends in an
allow; a public link that serves anything beside the object or subtree
its token names, or that serves a file as a page of the server's origin;
a token, a presigned URL or a secret of the configuration in a log line,
a listing or a response; and a way for one installation to reach the
objects of another in a shared bucket. What
[is not defended](#what-is-not-defended) is not a vulnerability, but a
report that one of those limits is wider than written here is.

## What is protected

The commitments the design makes: every `/v1` request carries a token
from an issuer the operator listed, and nothing reads or writes an object
before the authorizer has decided, with a refused object answering
exactly as a missing one; a decision the authorizer cannot give is a
refusal, never an allow; a presigned URL names one object, one method,
and one expiry, and is never logged; a public link resolves to the object
its token names and to nothing beside it; a file a link serves is saved as
a download and never rendered as a page of the server's origin.

Each row is who could attack, what Arca commits to against them, and the
tests in this repository that hold the commitment. A control without a
test is a claim, and no claim is listed here. The threat model lists more
controls than these, each with its own tests.

| Who | What Arca commits to | Held by |
|---|---|---|
| a caller with no token, or a token minted for another service | every `/v1` request carries a token from an issuer the operator listed. A token from an unlisted issuer, for another audience, with no subject, or past its age bound is refused, and an `http://` issuer off loopback stops the server from starting | `TestServiceConformance`, `TestATokenWithNoSubjectIsRefused`, `TestTheAudienceDefaults` (`internal/auth`), `TestAnHTTPIssuerOffLoopbackNeedsTheVariable` (`internal/auth`) |
| a caller reaching another subject's space | every route asks the authorizer one question before it reads or writes, and a denied route acts on nothing | `TestEveryRouteAsksExactlyOneAction`, `TestARouteThatIsDeniedDoesNotAct` (`internal/api`), `TestEveryHandlerAsksExactlyOneActionBeforeItActs` (`internal/files`) |
| a caller probing for what exists | a refused object answers exactly as a missing one, on every route that reads a row before it asks | `TestEveryLookupDenyIsTheAnswerAnAbsenceGives`, `TestADenyOnAnotherSpaceIsAMissingObject` (`internal/files`), `TestEveryShareLookupDenyIsTheAnswerAnAbsenceGives` (`internal/shares`) |
| any caller, when the authorizer or the grants table cannot answer | a decision that cannot be given is a refusal, never an allow | `TestUnavailableIsNeverAnAllow` (`internal/auth`), `TestAnAuthorizerThatAnswersNothingIsNeverAnAllow` (`internal/api`), `TestAGrantsTableThatCannotAnswerIsNoDecision` (`internal/auth`) |
| an authorizer that allows everything | the reserved probe resource every authorizer must deny is asked by `arcad check`, and an allow fails the check | `TestTheProbeIsDeniedAndAnEndpointThatAllowsItIsReported`, `TestAnUnavailableEndpointFailsTheCheck` (`internal/auth`) |
| a holder of a narrowed key or of one end of a move | a key narrowed to some grants reaches only those, and a grant that covers one end of a move or copy confers nothing | `TestOwnerPolicyNarrowsByTheGrants`, `TestAGrantMustCoverBothEnds` (`internal/auth`) |
| a network position that sees or keeps a redirect | a presigned URL names one object, one method and one expiry | `TestStoreAPresignedReadIsOneKeyAndOneMethod`, `TestStoreAPresignedReadIsRefusedAfterItsExpiry` (`internal/blob`) |
| a reader of the logs | the request log line has a fixed set of fields, none of them a token, a presigned URL or a path, and a link token is replaced in the path a trace records | `TestTheRequestLineCarriesTheIdsAndNothingSecret`, `TestALinkTokenIsKeptOffTheRecordedPath` (`internal/api`) |
| a link holder | a public link resolves to the object its token names and to nothing beside it: the prefix is matched by segment, and a link grants reading and never more | `TestALinkServesNothingOutsideItsPrefix`, `TestALinkThatWouldGrantMoreThanReadingIsRefused` (`internal/shares`) |
| an anonymous prober | a link token is 256 bits from `crypto/rand`, an unknown, revoked, expired or out-of-prefix token is refused in one sentence naming neither token nor path, and requests with no bearer are rate limited per client address | `TestTheTokenCarriesTheEntropySpec015Requires`, `TestARefusedRedemptionNamesNoToken`, `TestATokenThatResolvesToNothingIsNotFoundBeforeAnyQuestion` (`internal/shares`), `TestTheAddressRateLimitBoundsWhatHasNoSubject` (`internal/api`) |
| anyone who opens a link to a file a program wrote | a file a link serves is saved as a download and never rendered as a page of the server's origin: every link answer carries an attachment disposition, `nosniff` and a sandboxing policy, and a redirect asks the bucket for the same disposition | `TestEveryAnswerOfTheLinkRoutesIsDownloadOnly` (`internal/shares`), `TestEveryAnswerOfTheLinkRowsIsDownloadOnly`, `TestAnAttachmentCarriesTheNameExactlyAndNothingElse` (`internal/api`), `TestALinkReadThatRedirectsAsksTheBucketForTheAttachment`, `TestALinkReadOfAPublicObjectIsPresignedAndNotSentToTheCDN` (`internal/files`) |
| a link token's trail | the token is answered once, when the link is made, and is absent from every listing; link responses carry `Referrer-Policy: no-referrer` | `TestMintingALinkAnswersTheTokenOnce`, `TestTheThreeRoutesRedeemATokenWithNoBearer` (`internal/shares`) |
| a public link over an object | a public link marks only the object its prefix names, and a bucket that will not apply the mark answers the request `503 storage_unavailable` | `TestAPublicGrantMarksTheObjectItNames`, `TestABucketThatWillNotStampIsAnUnavailableStore` (`internal/shares`) |
| a caller who chooses a path or an installation that shares a bucket | a path with a relative or empty segment is refused before the bucket is reached, the bucket key comes from the object's id and never from its path, and every key carries the installation's prefix | `TestAPathThatIsNotOneIsRefusedBeforeTheBucketIsReached` (`internal/files`), `TestKeyPutsTheShardFromTheTailUnderThePrefix` (`object`), `TestAMoveTouchesNoBucketKey` (`internal/blob`), `TestThePrefixGainsItsSlashAndRefusesAnythingElse` (`internal/config`) |
| a caller with no administrator's name | with no authorizer and no listed administrator, the administrative routes refuse everyone | `TestAnInstallationWithNoAdministratorRefusesEveryone` (`internal/admin`) |
| a caller reading a space's event log | the log is read through the same decision as any other action, and the answer's filter narrows it | `TestTheTailAsksEventReadAboutTheSpaceTheCallerNamed` (`internal/events`) |
| a flood or an oversized body | a token bucket limits each subject, and a body past the bound is refused unread | `TestTheSubjectRateLimitIsPerSubject`, `TestABodyPastTheBoundIsRefusedRatherThanRead` (`internal/api`) |
| a caller trying to write past its limit | a usage check that cannot be made refuses the write | `TestUsageFailsClosed` (`internal/events`) |
| a compromised replica | the pod runs non-root with a read-only filesystem, no capabilities and no service account token, behind an egress port list, and every credential is mounted from a Secret | `TestBaseIsConfined`, `TestTheBaseKeepsCredentialsInSecrets` (`test/deploy`) |

## What is not defended

- **Whoever holds the stores can read what they hold.** Arca stores bytes
  as the client sent them and sends no encryption setting with a write.
  There is no end-to-end encryption of file contents, and none at rest in
  Arca: whoever holds the bucket reads every object, and whoever holds
  the database reads every path, owner, grant and event. Link tokens are
  stored as issued, so a reader of the database can use every live link.
  Encryption at rest, and TLS between `arcad` and its bucket and
  database, are configured where those live.
- **A compromised `arcad` host.** It holds the bucket credential, the
  database URL and the authorizer's bearer, and so holds everything.
- **An authorizer or issuer that answers wrongly.** Arca follows the
  decision it is given. An authorizer that allows everything allows
  everything, and `arcad check` is the only guard: it asks the probe
  resource and fails the installation when it is allowed. An issuer that
  signs a token for the wrong person lets its holder act as that person.
- **Link tokens and presigned URLs are bearer credentials.** Whoever holds
  a link token reads the subtree it names until the link is revoked or
  expires, and a link holder who gives it away has given the access away.
  A presigned URL reads one object until it expires, and nothing recalls
  a signed URL, so a revoke leaves that window open.
- **A revoked permission stays honored for a short while.** An allow is
  cached per replica for the time the authorizer's answer gives it.
- **A public object may outlive its revoke in a cache.** Where
  `ARCA_PUBLIC_CDN_URL` names a CDN, what it caches and for how long is
  configured there, and a page someone stored is answered under the type
  it was written with, on that host.
- **The wire.** `arcad` serves plain HTTP. A bearer token, a link token or
  a presigned URL seen in transit is usable, so TLS in front of the public
  listener is the operator's to provide.
- **Denial of service.** Arca limits requests per subject and per
  client address and bounds each body, and nothing more. The cost of a
  large but authorized upload falls on the bucket.
- **Other installations in one bucket.** The key prefix separates them,
  and nothing beyond it does.
- **Who may share with whom.** Arca holds no policy of its own about which
  subjects may grant to which, or about an approval step between a request
  and a grant. That is the authorizer's.

## Audit

Arca has had no external security audit. What exists instead is the
[threat model](specs/015-security-and-threat-model.md): every control in it
names a test, and a test in `test/threatmodel` fails when a named test is
missing from the repository. That checks that the tests exist and that
each commitment above is a control, not that the design is sound. Weigh
that before relying on Arca where a failure would put someone at risk.

## Supply chain

Dependencies are checked for known vulnerabilities on every push. A
release is built from a tagged commit whose checks passed, through the
shared release stages in [latere-ai/ci](https://github.com/latere-ai/ci).
It carries an SPDX bill of materials for each image, `checksums.txt` over
every file of the release, cosign signatures over both images and over
`checksums.txt`, and a build provenance attestation for each image. The
signatures name the stage that made them, and the commands below hold
them to this repository. With `TAG` the release's tag, such as `vX.Y.Z`:

```sh
gh release download "$TAG" -R latere-ai/arca -D arca-release
cd arca-release
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository latere-ai/arca checksums.txt
sha256sum -c checksums.txt

cosign verify "ghcr.io/latere-ai/arcad:$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
  --certificate-github-workflow-repository latere-ai/arca
gh attestation verify "oci://ghcr.io/latere-ai/arcad:$TAG" -R latere-ai/arca \
  --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
```

The same two image commands check `arca-stubs`. The release workflow runs
these checks against every release it publishes.
[Operating Arca](docs/operations.md#what-a-release-is) has the rest,
including how to check a release older than v0.10.3.
