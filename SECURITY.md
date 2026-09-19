# Security

Report a vulnerability to security@latere.ai. Do not open a public issue
for one. You will hear back within three business days, and a fix for a
high severity issue ships within thirty days. Credit in the release notes
on request.

Fixes go to the two most recent minor release series. v0.1.1 is the
current release; `go tool lateregate release` rewrites that version here in
the same commit that cuts the tag, so this line never lags one.

What Arca protects, against whom, and how each threat is answered is
written down in the [threat model](specs/015-security-and-threat-model.md),
where every control names the test that proves it, so a reviewer can
check the design rather than take it on faith. The commitments the
design makes: every `/v1` request carries a token from an issuer the
operator listed, and nothing reads or writes an object before the
authorizer has decided, with a refused object answering exactly as a
missing one; a decision the authorizer cannot give is a refusal, never
an allow; a presigned URL names one object, one method, and one
expiry, and is never logged; a public link resolves to the object its
token names and to nothing beside it; a workspace has one writer at a
time and a lease that expires.

Dependencies are checked for known vulnerabilities on every push. A release
carries an SPDX bill of materials for the module graph and one per image,
cosign signatures over the images and the checksums, and an SBOM and a
build provenance attestation per image, so `gh attestation verify` answers
for the image you are about to run. The commands that check each of them
are in [docs/operations.md](docs/operations.md).
