---
title: "Object store: the bucket contract, keys, integrity, presigned reads, multipart"
status: testing
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
affects: [object/, internal/blob/, internal/config/, cmd/arcad/, .lateregate.yaml]
effort: medium
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Object store

## Overview

The bucket half of [[001-architecture]]'s two stores. One package,
`internal/blob`, holds every call Arca makes against an S3 compatible
store, and one package, `object/`, holds the key derivation a platform
built on Arca may need to reproduce. Nothing else in the tree builds a
key, signs a URL, or talks to the bucket.

The bucket is the authority on what an object contains and knows nothing
about who owns it, what path it sits at, or whether it is shared. That
separation is what makes a move a row update (invariant 8) and what lets
one bucket carry several installations under different prefixes.

## Current state

Built and in the tree on 2026-09-18, phase 1 of [[019-migration-from-drive]].
`object/` holds the id and the key derivation, `internal/blob` holds the
client with `Memory` and `Counting` beside it, `internal/config` reads the
eight variables, and `arcad` runs the `bucket` readiness check. The commits
are `c64d821` (the object model), `2576334` (the client and the two stubs),
`2bfaa4e` (the variables, the readiness check, and the dependency rows), and
`df9c1ca` (the store tier against MinIO). The gate passes at each of them.

What arrived from Drive is `internal/storage/s3.go`: the put buffering rule,
the error classification by API code, the batched delete, the presigned
reads, the four multipart calls, and `Head`. What changed on the way is the
table in "What arrives from Drive" below, as written.

Divergences from the design as drafted, each a decision rather than a gap:

- `PutMultipart`, the server side part streamer, did not arrive. Its only
  caller is the workspace writeback of [[009-workspaces]], so it comes with
  that spec rather than as a method with no caller.
- `blob.Options` carries two fields this spec does not name, `HTTPClient` and
  `MaxAttempts`. Both are seams for a test: a client that trusts the
  certificate of the endpoint it started, and one attempt so an injected
  refusal is answered rather than waited on. Neither is configuration, and no
  `ARCA_*` variable reaches either.
- The degraded mode of criterion 4 is the client's half: a store that answers
  `NotImplemented` to the conditional create is retried once without it when
  the body can rewind, recorded on `S3.Unconditional`, and logged once per
  process. Naming it on readiness and in `check` is [[012-administration]]'s,
  which is where `check` is built.
- The MinIO release the stack pins answers `NotImplemented` to an object ACL,
  so criterion 11 is proved against the real store as well as against the
  fake, and the store tier runs the shared table with publicity unsupported.
- Criterion 8 counts round trips at the endpoint rather than through
  `blob.Counting`: a wrapper over `Store` sees one call whatever the number
  of keys, and what the criterion is about is the calls the client makes to
  the store. The in-process endpoint of the unit tier counts them.
- `object.ParseID` accepts any UUID version in its canonical text. `NewID`
  mints version 7 and every key Arca writes carries one; a store filled
  before that decision still reads.
- `object/` also carries the planes and the checksum kinds
  [[001-architecture]] names for it. The path rules beyond the plane prefix
  are [[005-files]]'s.
- `Copy` joined the interface on 2026-09-19 for the object move of
  [[019-migration-from-drive]], which is one server side copy per key from a
  predecessor's key to the key an object id derives. It is the same write as
  a put, with the same `If-None-Match: *` and the same degraded mode, and no
  body: the bytes never leave the store. A source above the API's single copy
  maximum moves range by range. It carries no ACL, because `CopyObject`
  carries none, so a caller moving a public object re-stamps the destination
  through `SetPublic`. Nothing that serves a request calls it.

One question stays open for the maintainer, the one the Design already
raises: whether the store tier should run MinIO behind TLS so both integrity
mechanisms are exercised against a real store. Today the trailing digest is
exercised against the in-process endpoint over TLS and the ETag comparison
against MinIO over plain HTTP, and a body corrupted in flight is refused in
both.

## Design

### The client and its configuration

`aws-sdk-go-v2` speaks the S3 API. The shared `latere.ai/x/pkg/s3`
client covers single objects only, and Arca needs multipart, presigned
part URLs, batched deletes, and object ACLs, so the SDK is the
dependency and its packages join the `depcheck` allow list of
`./cmd/arcad` in `.lateregate.yaml`, one row each:
`github.com/aws/aws-sdk-go-v2/aws`, `.../config`, `.../credentials`,
`.../service/s3`, `.../service/s3/types`, `github.com/aws/smithy-go`.

| Variable | Use |
|---|---|
| `ARCA_BUCKET` | the bucket every key is written to |
| `ARCA_BUCKET_ENDPOINT` | the S3 endpoint; unset uses the SDK's default for the region |
| `ARCA_BUCKET_REGION` | the signing region |
| `ARCA_BUCKET_PREFIX` | the prefix every key carries, default `arca/` |
| `ARCA_BUCKET_PATH_STYLE` | path-style addressing, for stores without virtual hosts |
| `ARCA_BUCKET_ACCESS_KEY`, `ARCA_BUCKET_SECRET_KEY` | static credentials; unset falls through to the SDK's credential chain |
| `ARCA_PUBLIC_CDN_URL` | the base a public object's redirect points at |

The table is [[002-repository-scaffold]]'s; this spec gives the rows
their meaning and adds none. `ARCA_BUCKET_PREFIX` is normalised at
start-up: a missing trailing `/` is appended, a leading `/` is a
configuration error, and the value must match `[A-Za-z0-9._/-]*`.

Any store that serves the S3 API works: AWS S3, MinIO, DigitalOcean
Spaces, and Google Cloud Storage through its S3 interoperability
endpoint. The one requirement beyond the API is conditional create,
below.

### Key derivation

A key names one immutable sequence of bytes. Every write of content
mints a new object id, so a key is written once and never rewritten.

```
key = <ARCA_BUCKET_PREFIX><shard>/<object id>
shard = the last two hex characters of the object id
```

The object id is a UUIDv7 in its canonical 36 character text form. The
shard comes from the tail because UUIDv7 leads with a timestamp, so
sharding on the head would drive every write of one hour into one
prefix; the tail is random and spreads writes over 256 prefixes.

```
arca/1f/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f
```

`object/` exports the derivation so a platform can reproduce it:

```go
type ID string                               // canonical UUIDv7 text
func NewID() ID                              // a fresh id for one write
func (id ID) Key(prefix string) string       // prefix + shard + id
func ParseKey(prefix, key string) (ID, error)
```

The consequences, each of which a later spec relies on:

| Because the key is the id | The result |
|---|---|
| a path never appears in a key | a move is one `UPDATE` ([[005-files]]) |
| a write mints a new id | the superseded bytes stay addressable, so a version is metadata only |
| nothing derives a key twice | a failed write can be deleted without touching live bytes |
| a public object's URL is unguessable | publicity is a decision, not an accident of naming |

### Writing

`Put` streams the body to the bucket and returns what the store holds.
Bodies at or below 8 MiB are buffered into a `bytes.Reader` first, so
the SDK can retry a transient failure; larger bodies stream unbuffered
and accept one attempt, because a retry cannot rewind a socket.

Every put carries `If-None-Match: *`. With fresh ids per write this is
not a compare and swap, it is a collision guard: it turns the one
catastrophic outcome, a second writer silently replacing live bytes,
into `ErrPreconditionFailed`. Conditional create is a requirement of the
bucket, not a capability Arca probes ([[001-architecture]], extension
points). A store that answers `NotImplemented` fails the `check`
subcommand of [[012-administration]] and is named by readiness; `arcad`
then runs with a logged warning, once per process, and the run is
recorded as unconditional in [[018-observability]]. The compare and swap
a caller sees on the API is in SQL, not here ([[005-files]]).

Integrity has one primary mechanism and one fallback:

1. Primary. The put sets `ChecksumAlgorithm: sha256`, so the SDK sends
   the digest as a trailer and the store verifies the bytes on receipt
   and rejects a corrupted upload. `blob` computes the same digest from
   the stream it forwards and returns it.
2. Fallback. Where the store rejects trailing checksums, `blob` compares
   the ETag the store returned against the MD5 it computed in the same
   pass, and only when the ETag has the single part shape of 32 hex
   characters; a multipart or encrypted ETag has another shape and is
   skipped. A mismatch deletes the key and fails the write.

The two mechanisms are not both available on one connection. A body
that streams off the request socket is not seekable, so its payload hash
cannot be computed before the request is signed, and the put is signed
`UNSIGNED-PAYLOAD`. That is what lets a store reached over plain HTTP
accept a streamed upload at all, and it is also what defeats the signed
trailer a trailing checksum needs. So the mechanism follows the
connection: over TLS, which is every deployment, the trailing sha256;
over plain HTTP, which is the store tier against MinIO, the ETag
comparison. A test therefore exercises one mechanism per tier, and
whether the store tier should run MinIO behind TLS so both are exercised
in one suite is a decision for review.

`Written` carries `Size`, `SHA256`, and `ETag`. The caller stores the
sha256 as the object's checksum and marks its kind `sha256`
([[004-metadata-store]]). An object assembled from parts Arca never saw
carries the store's composite ETag instead, kind `etag`
([[007-uploads]]).

### Reading

| Call | Shape |
|---|---|
| `Get` | a `ReadCloser` plus content type and size; the caller closes it |
| `Head` | size, ETag, content type, without a body |
| `PresignGet` | a URL for one key, method `GET`, expiry 5 minutes |

`PresignGet` signs exactly one object, one method, and one expiry, and
sets `response-content-disposition: attachment; filename="<name>"` when
the caller asked for a download, so a browser saves the file under its
own name instead of the key. The five minute lifetime is a constant,
`blob.PresignTTL`: a presigned URL is a bearer credential for that
object, and a short one bounds what a leaked redirect is worth
([[015-security-and-threat-model]]).

A missing key is `blob.ErrNotFound`, matched on the API error code
`NoSuchKey` rather than a concrete SDK type, so it holds across stores.
Every other failure is wrapped and surfaced: a throttle or an outage
must never reach a caller as a missing object (invariant 2).

### Multipart and presigned parts

The parts of an upload above `ARCA_INLINE_BYTES` go straight from the
client to the bucket. `blob` owns the four calls and none of the policy,
which is [[007-uploads]]'s.

| Call | Does |
|---|---|
| `CreateMultipart` | opens an upload on one key, returns the store's upload id |
| `PresignPart` | signs one `PUT` for one part number of one upload, expiry 24 hours |
| `CompleteMultipart` | assembles the parts, returns the assembled object |
| `AbortMultipart` | discards the upload and its parts |

`AbortMultipart` treats `NoSuchUpload` as success: the upload is already
gone, which is the goal, and failing would make the reaper retry a dead
upload forever.

An incomplete multipart's parts are invisible to object listing, so the
only durable pointer to them is the session row of
[[004-metadata-store]]. The referenced set a sweep compares a key
against is therefore the union of three tables, `files`,
`file_versions`, and `upload_sessions`, and
[[010-events-and-reaper]] inherits that rule from here.

The browser sends part bodies to the bucket's origin, so the bucket
needs a CORS rule for the console origin the operator serves: allowed
method `PUT`, allowed headers `*`, exposed header `ETag`.
[[016-release-and-installation]] carries it as an installation step,
because a bucket policy is not something `arcad` can set for itself.

### Deleting and listing

`Delete` removes one key. `DeleteMany` removes up to 1000 keys per
round trip and aggregates per key failures into one error, so dropping a
large subtree costs `O(keys/1000)` calls instead of `O(keys)`.

`List` returns up to `max` keys under a prefix after a start key, plus
the store's truncation flag. A caller pages on truncation and never on a
short result: a store may return fewer keys than asked while more
remain. The reaper is the only caller.

### Public objects and the CDN prefix

Publicity is a property of the object, set through
[[008-shares-and-links]] and recorded in the row. Arca infers it from no
path: the predecessor treated `files/avatar` and `files/public/**` as
public by name, and a key that is not a path cannot carry that
convention.

`SetPublic` stamps the canned ACL `public-read` on an existing key, or
removes it. Because the key does not change when the path does, making
an object public is a header change and never a re-upload.

A read of a public object answers `302` to `<ARCA_PUBLIC_CDN_URL>/<key>`
when that variable is set, which is cacheable and carries no expiry.
With the variable unset, or against a store that answers
`NotImplemented` to an object ACL and offers bucket policies instead, a
public object is served by the ordinary presigned redirect. `blob`
reports that case as `ErrNotSupported` and the handler falls back
without failing the read.

### The interface in internal/blob

```go
// Package blob is the bucket behind one interface. Keys are opaque
// here; object/ derives them.
type Store interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, o PutOptions) (Written, error)
	Copy(ctx context.Context, from, to string, o PutOptions) (Object, error)
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
	Head(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
	DeleteMany(ctx context.Context, keys []string) error
	List(ctx context.Context, prefix, startAfter string, max int32) (Listing, error)
	PresignGet(ctx context.Context, key string, o PresignOptions) (string, error)
	CreateMultipart(ctx context.Context, key string, o PutOptions) (uploadID string, err error)
	PresignPart(ctx context.Context, key, uploadID string, part int32) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error
	SetPublic(ctx context.Context, key string, public bool) error
	HeadBucket(ctx context.Context) error
}

var (
	ErrNotFound            = errors.New("blob: object not found")
	ErrPreconditionFailed  = errors.New("blob: key already exists")
	ErrNotSupported        = errors.New("blob: the store does not support this call")
)
```

`HeadBucket` is the readiness check [[002-repository-scaffold]] reserves
a slot for, run under the 2 second budget.

### The stubs

Two implementations of `Store` ship beside the S3 one, both in
`internal/blob`, both used by the unit tier of
[[014-test-stubs-and-tiers]]:

- `blob.Memory`, a map of key to bytes with the same error vocabulary,
  including the conditional create and the multipart state machine, so
  a handler test needs no bucket.
- `blob.Counting`, a wrapper over any `Store` that counts calls per
  method and optionally fails the nth call of one method. It is how a
  test proves a negative: [[005-files]] asserts that a move leaves every
  counter at zero (criterion 7 of [[001-architecture]]), and
  [[010-events-and-reaper]] injects the fault that leaves bytes
  without a row.

### What arrives from Drive

From `internal/storage/s3.go` and the archived specs `002-file-plane`,
`015-multipart-uploads`, and `024-tiered-storage-latency`.

| Arrives | Changes |
|---|---|
| the S3 client, put buffering, `classifyGetError`, `DeleteMany`, presigned GET, the four multipart calls, `Head` | the package is `internal/blob`, the type is an interface, and the stubs above come with it |
| `storage_key` = `drive/{owner}/{path}` | the key is `<prefix><shard>/<object id>` and carries no path and no owner. This is the headline change: the predecessor could not move a public file, versioned by appending `@<random>` to a path key, and kept a key that leaked the owner into the bucket |
| the `drive/` tenant prefix | `ARCA_BUCKET_PREFIX`, default `arca/`, an operator's value |
| the endpoint defaulted to one provider's regional host | `ARCA_BUCKET_ENDPOINT`, with the SDK's default behind it, and no provider named in code |
| the public carve-out by path (`files/avatar`, `files/public/**`) | gone. Publicity is a column, set by [[008-shares-and-links]] |
| the CDN base URL | `ARCA_PUBLIC_CDN_URL`, optional, with the presigned redirect as the fallback |
| `PutMultipart`, the server side 16 MiB part streamer | kept for the one caller that still streams a large body through the server, the workspace writeback of [[009-workspaces]] |
| sha256 computed locally and trusted | the store verifies the digest on receipt where it can, and the ETag comparison guards the rest |
| unconditional `PutObject` | `If-None-Match: *` on every put |

## Not in this spec

Which bytes are written and when ([[005-files]], [[007-uploads]]), what
a key is compared against and how often ([[010-events-and-reaper]]),
who may read an object ([[006-identity]]), and the bucket's own
lifecycle or replication configuration, which is the operator's
([[016-release-and-installation]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | `object.ID.Key` puts the shard from the id's tail under the configured prefix, and `ParseKey` round-trips it | `object` unit tests, including a fuzz round-trip |
| 2 | A prefix without a trailing slash gains one, and a leading slash is a configuration error naming the variable | `internal/config` tests |
| 3 | A put carries `If-None-Match: *`, and a put onto an existing key returns `ErrPreconditionFailed` and leaves the stored bytes unchanged | the store tier against MinIO, and `blob.Memory` |
| 4 | A store that rejects conditional create is named by `check` and by readiness, and the process logs the degraded mode once | `internal/blob` test with a stub that answers `NotImplemented` |
| 5 | A body corrupted in flight fails the put and leaves no key, by the ETag comparison over plain HTTP and by the store's rejection of the trailing digest over TLS | the store tier, with a reader that flips a byte after the digest is taken, run against each connection the tier offers |
| 6 | A presigned GET is valid for one key and one method, and is refused for another key, another method, and after its expiry | the store tier against MinIO |
| 7 | `Get` on a missing key is `ErrNotFound`, and a throttled or failed `Get` is neither `ErrNotFound` nor a nil error | `internal/blob` tests with a stub API error |
| 8 | `DeleteMany` of 2500 keys makes three calls and reports per key failures | `blob.Counting` over the MinIO client |
| 9 | `List` pages on the truncation flag and not on a short result | the store tier, seeded past one page |
| 10 | `AbortMultipart` on an already finished upload succeeds | the store tier |
| 11 | `SetPublic` on a store without object ACLs returns `ErrNotSupported` and the read still answers | `internal/blob` test, plus [[005-files]]'s handler test |
| 12 | `blob.Memory` and the MinIO client pass one shared table of behaviours | one test table run against both, in `internal/blob` |
