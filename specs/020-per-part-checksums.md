---
title: "Per-part checksums: a digest the client declares, the store verifies, and the completion carries"
status: validated
track: core
depends_on:
  - specs/003-object-store.md
  - specs/007-uploads.md
  - specs/013-api.md
affects: [internal/blob/, internal/uploads/, internal/store/, internal/store/migrations/, internal/api/, api/, docs/]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Per-part checksums

## Overview

A session's bytes never pass through the server, so the only party that
can hash them before they leave is the client. Today Arca takes the
client's word on a part and checks the shape of the result: the store
verifies the part labels when it assembles, and the head reads the
assembled size back. Neither catches a part whose bytes are not the
bytes the client meant to send. A part corrupted in flight assembles
into an object the row calls good.

This spec adds the one mechanism that catches it, which is the store's
own: a per-part sha256 the client declares, the store verifies on
receipt, and the completion carries so the store recomputes the
composite over the digests it already checked.

It is criterion 12 of [[007-uploads]], split out of that spec on
2026-09-19 because it is not one field in one package.

## Design

### Why it is three specs and a migration

The header that carries a part's digest is `x-amz-checksum-sha256`, and
a presigned `UploadPart` URL can only carry a header that was signed
into it. SigV4 signs the headers the request names, so a digest the
signature does not cover is a header the store either ignores or
refuses. The digest therefore has to be known when the URL is minted,
which is when the session is created, which makes the per-part digests
part of the create request.

That places one piece in each of three specs.

| Piece | Spec | What changes |
|---|---|---|
| the algorithm on an opened multipart, the digest on a signed part URL, and the digest on a completed part | [[003-object-store]] | `blob.PutOptions` gains a checksum algorithm, `CreateMultipart` passes it, `PresignPart` takes the part's digest and signs the header, and `blob.Part` gains the digest `CompleteMultipart` sends |
| the digests a create declares and the session row keeps | [[007-uploads]] | `createRequest` gains `checksum` and one digest per part; the `upload_sessions` row keeps them, because a session that resumes must mint the same URLs it minted before |
| the wire | [[013-api]] | the create request body and the error a malformed digest answers |

The store tier already holds the machinery for the whole-object path:
`service/internal/checksum` is in the dependency table of
`.lateregate.yaml` as "the trailing sha256 a put carries over TLS (spec
003)". What is missing is not a dependency, it is the multipart side of
a contract the single put already keeps. That is why the piece is
[[003-object-store]]'s before it is [[007-uploads]]'s.

The migration is the reason the split is not an hour's work by itself.
A session is resumable for twenty-four hours and a client may ask for
its URLs again, so the digests are row state and not request state, and
row state on `upload_sessions` is a migration [[004-metadata-store]]'s
ownership table assigns [[007-uploads]].

### The shape

A create that wants verified parts sends the algorithm and one digest
per part, base64 as the store spells it:

```json
POST /v1/uploads
{ "owner": "me", "path": "files/video/keynote.mp4", "size": 704643072,
  "content_type": "video/mp4", "checksum": "sha256",
  "part_checksums": ["n8Lp…", "Qa3f…", "…"] }
```

The count must equal the part count the declared size and the part size
give, which the server computes before it opens anything, so a client
that miscounted is refused before a multipart exists. A create naming
`checksum` and no digests is `invalid_field`: an algorithm with nothing
to verify against is a client that believes it is protected and is not.

A create that names no checksum keeps today's behaviour exactly. The
field is optional and stays optional: a client that cannot hash a part
before it sends it, which is every client streaming from a source it
reads once, is not locked out of resumable uploads.

### What each point catches

| Point | Catches | Answers |
|---|---|---|
| the store, on receipt of a part | bytes that are not the bytes the digest names | the store's own refusal, at the presigned URL, to the client |
| the store, at assembly | a digest that reached the completion and not the part | `400` from the completion, as a wrong ETag does today |
| the head, after assembly | a size that is not the size that was declared | `413` and the deletion of [[007-uploads]]'s criterion 4 |

The composite the row records is unchanged: an object assembled from
parts carries `checksum_kind = 'etag'` ([[004-metadata-store]]), because
the store's composite is a digest of digests and not a digest of the
content. What this spec buys is that every digest the composite is taken
over was verified against real bytes.

### Not in this spec

The whole-object path, which hashes over the stream already
([[005-files]]). A checksum the server computes for a client, which it
cannot: it never reads the bytes (invariant 4 of [[001-architecture]]).
An algorithm other than sha256; the field names one so a second can be
added without a wire change, and nothing here adds one.

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | A part uploaded with a wrong sha256 is rejected by a store that verifies checksums | the store tier against MinIO |
| 2 | A create naming `checksum` mints part URLs that carry the signed digest header, and a create naming none mints the URLs it mints today | `internal/uploads` tests over the presigned URLs |
| 3 | A create whose digest count does not match the part count the declared size gives is `invalid_field`, and opens no multipart | `internal/uploads` tests with `blob.Counting` |
| 4 | A session resumed after its URLs expired mints URLs carrying the same digests, read from the row | the store tier |
| 5 | A completion carries each part's digest, and the store's composite is taken over digests it verified | the store tier against MinIO |
