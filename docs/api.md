# The Arca API

For whoever writes a client, a console, or a sandbox runtime against an Arca
installation. This page covers the model, every route, and the rules every
route shares. The exact field lists are in the OpenAPI document: every
installation serves it at `GET /openapi.json`, and the same document is
committed as [`api/openapi.yaml`](../api/openapi.yaml).

This is version 1 of the API. What a version number promises across
releases is in [operations](operations.md#what-a-version-number-promises).

## Addresses

Every route below is written under `/v1`, which is the default base path. An
installation that shares its origin with other services may serve the same
routes under a longer base, such as `/v1/storage`; the operator sets it with
`ARCA_BASE_PATH`, and the served OpenAPI document names every path under the
base the installation actually uses.

A few routes sit at the origin root whatever the base path is, and none of
them takes a token:

| Method | Path | What it answers |
|---|---|---|
| GET | `/` | the build identity, as text |
| GET | `/openapi.json` | the OpenAPI 3.1 document of this installation |
| GET | `/version` | `{"version", "commit", "build_time"}` of the running build |
| GET | `/livez`, `/readyz` | the probes; `/readyz` is 200 only when the bucket, the database and its schema, every issuer, and the authorization endpoint (when one is set) answer |

## Authentication

Every request under the base path carries `Authorization: Bearer <token>`,
with three exceptions: the public link routes, where the token in the URL is
the whole authorization.

The token is a JWT from an issuer the installation lists in
`ARCA_OIDC_ISSUERS`, and its `aud` must name one of the audiences in
`ARCA_OIDC_AUDIENCE`, which is `arca` unless the operator changed it. How a
client obtains one is the issuer's business: a person through whatever
sign-in the platform runs, a service through a client credentials grant.
Arca verifies the token against the issuer's key set and reads nothing from
the issuer beyond that.

A request without a valid token is `401 unauthenticated`. Inside the base
path a route that does not exist is also `401` for a caller without a token,
so an unauthenticated caller learns nothing about which routes exist.

**Subjects.** Arca names a principal by its issuer and its `sub` together,
`<issuer>|<sub>`, with any trailing slash removed from the issuer:
`https://issuer.example|9ab3`. Two issuers that agree on a `sub` are two
subjects. Every `owner`, `grantee`, `actor`, and `created_by` field in a
response is written this way.

## Spaces and paths

Every subject has one **space**, addressed by the subject itself. A space
is one tree, and every path in it begins with `files/`:
`files/reports/q3.pdf`. A path under any other first segment is
`400 unknown_plane`.

A **folder** is a prefix. Arca stores no directory: `files/reports` is a
folder for as long as some object's path begins with `files/reports/`, and
a folder with nothing in it does not exist. A listing synthesizes the
folders below a prefix from the objects it holds, and a folder is
[deleted or copied](#folders) in one call.

`{owner}` in a route is the owner's subject, percent-encoded as one path
segment, or `me` for the caller's own space. `https://issuer.example|9ab3`
becomes `https%3A%2F%2Fissuer.example%7C9ab3`. A route that takes `?owner=`
reads the caller's own space when the parameter is absent. A response always
writes the full subject, never `me`, so a client can send back what it read.

Writing and reading one object in your own space:

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/plain' \
  --data-binary 'hello' "$ARCA_URL/v1/files/me/files/notes/hello.txt"

curl -H "Authorization: Bearer $TOKEN" "$ARCA_URL/v1/files/me/files/notes/hello.txt"
```

## Authorization

Before every action Arca asks one question: may this subject perform this
action on this resource? The answer comes from one of two places.

- **The owner policy**, when the operator sets no `ARCA_AUTHORIZER_URL`. A
  subject may do anything in its own space. The subjects in
  `ARCA_ADMIN_SUBJECTS` may do anything in every space. Anyone else is
  allowed exactly what a [grant](#shares-and-links) gives them, and a link
  holder may read what the link names.
- **Your authorization endpoint**, when the operator sets
  `ARCA_AUTHORIZER_URL`. Arca posts the question to it and does what it
  answers. The [authorization endpoint](#the-authorization-endpoint) section
  says what the endpoint receives and must return.

Each route below names the action it asks. A caller refused its own action
gets `403 forbidden`. A reference the request named that the caller may not
see is `404 not_found`, the same answer as one that does not exist, so a
refusal never reveals that something is there.

## Requests and responses

- A request body that is not object bytes is JSON with
  `Content-Type: application/json`. An unknown field is refused
  (`400 unknown_field`), so a misspelled field fails at once rather than
  writing a default.
- Object bytes go to `PUT /v1/files/{owner}/{path}` with the object's own
  `Content-Type`, which is stored and served back, and a `Content-Length`,
  which is required (`411 length_required`).
- Every response body is JSON unless it is object bytes. Every timestamp is
  RFC 3339 in UTC. Every size is a number of bytes.
- Every response carries `X-Request-Id`. Send your own, up to 128 printable
  ASCII characters, and it is kept; otherwise Arca mints one. It is in every
  error body and in the server's log line for the request.
- An installation that exports traces also answers `X-Trace-Id`, the trace
  the request was recorded under. Quote it beside `X-Request-Id` when you
  report a problem.

### Lists

Every route that lists answers one shape:

```json
{"entries": [...], "next_cursor": "..."}
```

`entries` is never null. `next_cursor` is present only when another page
exists; send it back unchanged as `?cursor=` and stop when a page has none.
`?limit=` is 100 by default and at most 1000, and a value outside that range
is `400 invalid_field` rather than silently clamped. Pages are keyset, so a
page is stable while other clients write.

The file listing adds two members beside the envelope: `prefixes`, the
folders the page implies, and, when the listing is of `files`, the root of
the tree, `space`, which is `{"bytes", "files"}` for the whole space. A
listing that [finds files](#finding-files) answers `prefixes` as an empty
array and no `space`.

### Conditional requests

Every object read and every object write answers an `ETag`: the sha-256 of
the bytes for an object Arca streamed, or the store's own ETag for an object
assembled from uploaded parts. An object's JSON carries `checksum` and
`checksum_kind` (`sha256` or `etag`) so a client can tell which it holds.

| Header | On | Effect |
|---|---|---|
| `If-None-Match: "<etag>"` | `GET`, `HEAD` | `304` with no body when the object still has that checksum |
| `If-None-Match: *` | `PUT`, completing an upload | create only: an existing object is `412 precondition_failed` |
| `If-Match: "<etag>"` | `PUT`, `DELETE`, completing an upload | proceed only if the object still has that checksum; otherwise `412 precondition_failed` |

No route requires a precondition. A client whose lost update would be
expensive, such as an agent writing back state it read earlier, sends one.
Quotes are optional.

### Errors

Every refusal is one JSON body:

```json
{"error": {"code": "precondition_failed",
  "message": "The object is not in the state the request required.",
  "details": {"request_id": "req_01J8R4...",
    "detail": "If-Match named 9f2c...; the object's checksum is 4d81..."}}}
```

Branch on `code`. Show `message` to a person; it never varies for a code.
`details.detail` is a sentence for a developer reading a log and is not
meant to be parsed. `details.fields` lists the fields at fault when there
are any.

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | the body could not be read as JSON |
| `missing_field` | 400 | a required field is absent |
| `invalid_field` | 400 | a field or parameter has a value it cannot take |
| `unknown_field` | 400 | the body names a field the route does not know |
| `invalid_path` | 400 | the path is not one Arca accepts |
| `unknown_plane` | 400 | the path does not begin with `files/` |
| `exclusive_fields` | 400 | two fields that cannot be set together are both set |
| `unauthenticated` | 401 | no token, or one that did not verify |
| `forbidden` | 403 | the action was refused |
| `not_found` | 404 | nothing there, or nothing the caller may see |
| `not_acceptable` | 406 | the `Accept` header excludes JSON |
| `path_taken` | 409 | something already exists at the target path |
| `length_required` | 411 | a `PUT` without `Content-Length` |
| `precondition_failed` | 412 | an `If-Match` or `If-None-Match` did not hold |
| `object_too_large` | 413 | the object is above what this installation accepts by this route |
| `body_too_large` | 413 | a JSON body is above what this installation accepts |
| `quota_exceeded` | 413 | the write would take the space past the limit the authorizer named |
| `unsupported_media_type` | 415 | a JSON route received another content type |
| `too_many_parts` | 422 | the object needs more upload parts than allowed |
| `too_many_objects` | 422 | the folder holds more objects than one copy accepts; copy its subfolders |
| `link_read_only` | 422 | a link was asked for a permission above `read` |
| `rate_limited` | 429 | the caller is over its rate; wait and retry |
| `internal` | 500 | a fault in the server |
| `not_implemented` | 501 | this installation does not serve the route |
| `storage_unavailable` | 503 | the bucket or the database did not answer, or the authorization endpoint's answer carried limits Arca could not read; retry |
| `authorizer_unavailable` | 503 | the authorization endpoint did not answer; retry |

### Limits

A subject may send `ARCA_REQUESTS_PER_MINUTE` requests a minute to one
replica, 600 by default. Requests without a bearer, and requests whose token
was refused, share a budget per client address, 60 a minute by default.
Past either, the answer is `429 rate_limited`.

Arca stores no storage limit of its own. When the authorization endpoint's
answer names `limits.quota_bytes`, a write that would take the space past it
is `413 quota_exceeded`. Without one, a space is unlimited.

The same answer may turn versions off for the write it admits, turn the
trash off for the delete it admits, and leave subtrees out of what a caller
lists. A client therefore does not assume that every overwrite keeps a
version, that every delete can be restored, or that a listing names
everything a space holds: those are the installation's to decide, path by
path. [What an answer may limit](#what-an-answer-may-limit) has the rules.

## Files

| Method | Path | Action | What it does |
|---|---|---|---|
| PUT | `/v1/files/{owner}/{path}` | `file.write` | write one object; the body is the bytes; `201` with the object when it creates one, `200` when it overwrites |
| GET | `/v1/files/{owner}/{path}` | `file.read` | read one object; see the query parameters below |
| HEAD | `/v1/files/{owner}/{path}` | `file.read` | the object's headers, no body |
| POST | `/v1/files/{owner}/{path}` | `file.write` or `file.restore` | `{"move_to": "<path>"}` moves the object; `{"copy_to": "<path>"}` copies it, or with `"recursive": true` the [folder](#folders); `{"restore_version": N}` makes version N current again; exactly one of the three |
| DELETE | `/v1/files/{owner}/{path}` | `file.delete` | delete the object; `?permanent=1` skips the trash; `?version=N` removes one old version; `?recursive=1` deletes the [folder](#folders) |
| GET | `/v1/files/materialize` | `file.list` | a whole space, or the folder `?prefix=` names below `files/`, as a manifest of presigned URLs; `?owner=` |

**Reading.** An object at or below the installation's inline size
(`ARCA_INLINE_BYTES`, 16 MiB by default) is answered with its bytes. A larger
one is a `302` to a presigned URL on the bucket, valid for five minutes, so
large bytes never pass through the server. `?inline=0` asks for the redirect
at any size; `?inline=1` asks for the bytes and is honored only up to the
inline size. `?download=1` makes the presigned URL save the object under its
own file name. An object a public link marked is always a redirect.

`GET` also selects another representation of the same path, checked in this
order:

| Parameter | Action | Answer |
|---|---|---|
| `?list=1` | `file.list` | the subtree under the path, one page at a time |
| `?versions=1` | `file.read` | the path's earlier versions, oldest first |
| `?version=N` | `file.read` | the bytes of version N |

A listing also [finds files](#finding-files) by name with `?q=`, and answers
the newest ones with `?recent=1`.

**Writing.** A `PUT` is admitted before its first byte is read, so it needs
`Content-Length`. A body above the inline size is refused with
`413 object_too_large`; send it through an [upload](#uploads) instead.
Overwriting a path keeps the previous content as a version, which costs one
database row and no copy of the bytes. When the authorizer's answer carries
`limits.versions: false`, the write keeps none: the content it replaces is
dropped, and the space is charged the difference in size.

**Moving and copying.** `move_to` and `copy_to` name a path in the same
space, and a destination that is taken is `409 path_taken`. A move changes
the object's path and touches no bytes; its versions, its stars, and a
grant on exactly that path follow it. A move is always one object. A copy
is one object or a whole folder; see [folders](#folders).

**Deleting.** A delete moves the object to the trash, where it stays
restorable for `ARCA_TRASH_RETENTION` (30 days by default). With
`?permanent=1`, or when the authorizer's answer carries
`limits.trash: false`, the delete is final: the object, its versions, and
their bytes.

**Materialize.** `GET /v1/files/materialize` answers a folder as a manifest
of download addresses:

```json
{"root": "files/", "pinned_at": null,
 "files": [{"path": "reports/q3.pdf", "checksum": "9f2c...", "size": 48213,
            "url": "https://..."}]}
```

Each `url` is a presigned `GET` of that object, valid for five minutes, and
each `path` is written below `root`. `pinned_at` is always null: the route
reads the space as it is and pins nothing. A client that keeps a local copy
of a folder downloads the files whose checksum it does not already hold.

### Finding files

A listing takes two more parameters, for a client that lets a person find
a file by typing part of its name, or offers the files they changed last.
Both ask the same `file.list` and answer the same shape.

| Parameter | Answer |
|---|---|
| `?q=<text>` | the files under the path whose name contains the text, ignoring case, in path order and paged with `?cursor=` and `?limit=` like any listing |
| `?recent=1` | the files under the path, the most recently modified first, at most `?limit=` of them, in one page with no `next_cursor` |

```
GET /v1/files/{owner}/files?list=1&q=plan
GET /v1/files/{owner}/files?list=1&recent=1&limit=20
GET /v1/files/{owner}/files/notes?list=1&recent=1&q=report
```

- A file's name is the last segment of its path, so `q=plan` finds
  `files/notes/q3-plan.md` and not `files/plan/readme.md`.
- The text is matched literally: `%`, `_` and `\` are ordinary characters.
  It is at most 200 bytes of UTF-8; a longer one is `400 invalid_field`.
- Letters outside ASCII match their other case when the database was
  created with a UTF-8 locale, as the official Postgres image creates it;
  a database created with the `C` locale matches the case of those letters
  exactly.
- An empty `q`, or one of only spaces, is the plain listing.
- `recent=1` with `q` answers the newest files whose name holds the text.
  `recent=1` with a `cursor` is `400 exclusive_fields`.
- Neither answers `prefixes` or `space`, and both leave out every subtree
  the authorizer's answer [omits](#leaving-subtrees-out-of-an-enumeration),
  so a match inside one is never listed.

### Folders

A folder is every object whose path begins with the folder's path and a
slash. It is never the object at exactly that path, and never a sibling
whose name begins with it: `files/notes` as a folder holds
`files/notes/a.md`, and holds neither a file named `files/notes` nor
`files/notes-archive/b.md`.

Two operations take a folder whole, a delete and a copy. Nothing else does:
a move is one object, and a folder is renamed by moving its objects.

**Deleting a folder.**

```
DELETE /v1/files/{owner}/{path}?recursive=1
DELETE /v1/files/{owner}/{path}?recursive=1&permanent=1
```

The first form trashes every live object under the folder. Each is in the
trash by itself and is restored one at a time. The second form, and either
form when the authorizer's answer carries `limits.trash: false`, removes
the folder for good: every object under it, live and trashed, their
versions, every grant and link whose prefix is the folder or under it, and
the bytes. The grants and links go so that the next object written there
does not inherit them. An upload session open under the folder is left
alone.

| Request | Answer |
|---|---|
| a folder that holds objects, and one that holds none | `204` with no body. A folder with nothing in it and a folder that never existed are the same thing, so the call can be repeated |
| the path is `files`, the root of the tree | `400 invalid_path`. Emptying a space is not a folder operation |
| `recursive=1` together with `version=` | `400 exclusive_fields` |

One call removes a folder of any size, and asks `file.delete` once, for the
folder. It appends one `delete` event at the folder's path, with the detail
`{"recursive": true, "files": <n>, "bytes": <b>, "trashed": <bool>}`:
`files` is the objects the call took, and `bytes` is their sizes when they
were trashed, or what the space's usage gave back, versions included, when
they were removed. A call that found nothing under the folder appends
nothing.

**Copying.**

```
POST /v1/files/{owner}/{path}
{"copy_to": "files/reports/2026"}

POST /v1/files/{owner}/{path}
{"copy_to": "files/reports/2026", "recursive": true}
```

Without `recursive` the source is one object, and the answer is the new
object, as a move answers it. With `"recursive": true` the source is the
folder at the path: every object under it is copied to the same path below
`copy_to`, and the answer is `200` with
`{"path": "<copy_to>", "files": <n>, "bytes": <b>}`.

The bucket copies. No byte of the content passes through the server, and
each copy is an object of its own with its own versions from then on. A
folder copy is whole or absent: a caller sees every object of it or none.

A copy carries the current content of each live object: the bytes, the
content type, and the size. A `sha256` checksum is carried over, since the
bytes are the same; an `etag` checksum is the one the store answers for the
copy. A copy carries no versions, no trashed objects, no stars, no grants,
no links, and no publicity: the copy of a public object is private, and its
`created_by` is the caller.

| Request | Answer |
|---|---|
| `copy_to` is the source, or for a folder a path inside it | `400 invalid_field` |
| `recursive` without `copy_to` | `400 invalid_field` |
| more than one of `move_to`, `restore_version`, and `copy_to` | `400 exclusive_fields` |
| no object at the source, or nothing under the source folder | `404 not_found` |
| a live object holds any path the copy would write | `409 path_taken`, and nothing is copied |
| the source changed while the copy ran | `412 precondition_failed`, and nothing is written; send the request again |
| the copy would take the space past the answer's `limits.quota_bytes` | `413 quota_exceeded`, and nothing is copied |
| the folder holds more than 10,000 objects | `422 too_many_objects`; copy its subfolders |

A copy asks `file.write` once, with both ends on the question, and appends
one `copy` event at the destination, with the detail
`{"from": "<source>", "recursive": <bool>, "files": <n>, "bytes": <b>}`.

### Trash and stars

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/v1/trash` | `file.list` | what is trashed and still restorable, newest first; `?owner=` |
| POST | `/v1/trash/restore` | `file.restore` | `{"owner", "path"}` returns one object to its path |
| DELETE | `/v1/trash` | `file.delete` | empty the trash, or one path with `?path=`; `?owner=` |
| PUT | `/v1/stars` | `file.write` | `{"owner", "path"}` stars an object; repeating it is harmless |
| DELETE | `/v1/stars` | `file.write` | `?owner=` and `?path=` unstar an object; repeating it is harmless |
| GET | `/v1/stars` | `file.list` | the caller's stars, on the spaces the `file.list` answer's filter names |

## Uploads

An object above the inline size is uploaded in parts that go from the client
straight to the bucket. The server signs the part URLs and assembles the
result; it never carries the bytes.

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/uploads` | `upload.write` | `{"owner", "path", "size", "content_type"}` opens a session; `201` with the part URLs |
| POST | `/v1/uploads/{id}/complete` | `upload.write` | `{"parts": [{"n", "etag"}]}` assembles the parts into the object at the session's path |
| DELETE | `/v1/uploads/{id}` | `upload.write` | abort the session and discard its parts; `204` |

The flow:

1. Open a session with the object's total `size`. The answer carries `id`,
   `part_size` (16 MiB), `part_count`, `part_urls`, and `expires_at`.
2. `PUT` part `n` (counting from 1) to `part_urls[n-1]`. Every part is
   `part_size` bytes except the last. Keep the `ETag` the bucket answers for
   each part.
3. Complete the session with every part's number and ETag. `If-Match` and
   `If-None-Match: *` apply here as they do to a `PUT`.

A session and its part URLs live for 24 hours. An object needs at most 1000
parts, and no object may exceed `ARCA_MAX_UPLOAD_BYTES` (5 GiB by default).
The reconciler aborts a session that outlives its expiry.

## Shares and links

A **grant** gives one subject a permission on a subtree of a space. There
are three rungs, and each includes the ones below it:

| Rung | Allows |
|---|---|
| `read` | `file.read`, `file.list` |
| `write` | the above, and `file.write`, `file.delete`, `file.restore`, `upload.write` |
| `manage` | the above, and `share.create`, `share.read`, `share.list`, `share.revoke` |

Reading a space's events, minting and revoking links, and administration
are never reachable through a grant: they belong to the space's owner and
to an administrator. That is the owner policy's reading of the ladder; an
authorization endpoint of your own receives the rung the caller holds on the
resource and decides as it likes.

A move and a copy name two paths, and a grant has to cover both: the rung
that counts is the lower of the two the caller holds, so a grant on one end
and none on the other allows neither.

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/shares` | `share.create` | `{"owner", "path_prefix", "grantee", "permission", "expires_at"}` grants a subject a rung on a subtree; `expires_at` is optional; `201` |
| GET | `/v1/shares` | `share.list` | the grants on a space; `?owner=`, `?path_prefix=` |
| GET | `/v1/shares/{id}` | `share.read` | one grant |
| DELETE | `/v1/shares/{id}` | `share.revoke` | revoke a grant; `204` |
| GET | `/v1/shares/with-me` | `share.list` | the grants whose grantee is the caller |

A **link** is a grant to whoever holds a token. It carries `read` and nothing
more.

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/shares/links` | `link.create` | `{"owner", "path_prefix", "kind", "expires_at"}` mints a link; `kind` is `link` (the default) or `public`; `201` with the token and the URL to redeem it at |
| GET | `/v1/shares/links` | `link.read` | the links on a space, without their tokens; `?owner=` |
| DELETE | `/v1/shares/links/{id}` | `link.revoke` | revoke a link; `204` |
| GET | `/v1/shares/links/{token}/meta` | none | what the token names, before anything is fetched |
| GET | `/v1/shares/links/{token}` | none | a listing of the subtree the token names |
| GET | `/v1/shares/links/{token}/files/{path}` | none | one object under that subtree |

The token is answered once, when the link is minted, and no route reads it
back. A caller that loses it revokes the link and mints another. The three
routes that redeem a token carry no bearer, answer with
`Referrer-Policy: no-referrer`, and still ask the authorizer `link.read` for
an anonymous subject, so an operator turns public reading off by denying
that action.

A `public` link whose prefix names one object also marks that object public
in the bucket, so it can be served without Arca in the path. A read of it
redirects to `ARCA_PUBLIC_CDN_URL` when the operator set one, and to a
presigned URL otherwise.

## Events

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/v1/events` | `event.read` | a space's log, oldest first; `?owner=`, `?cursor=`, `?limit=` |

Every change to a space appends one event:

```json
{"id": 4182, "action": "put", "owner": "https://issuer.example|9ab3",
 "path": "files/reports/q3.pdf", "actor": "https://issuer.example|9ab3",
 "detail": {"size": 48213}, "at": "2026-09-18T10:02:11Z"}
```

`action` is one of `put`, `move`, `copy`, `delete`, `restore`, `purge`,
`reap`, `share_created`, and `share_revoked`. `detail` depends on the
action: a `move` and a `copy` carry `from`, the source path, and a
`delete` of a folder carries `recursive` with the counts
[folders](#folders) describes, so one event may stand for many objects. A
consumer that wants to act on changes tails this route: it keeps the last
`next_cursor` it read and asks again from there. The log keeps 30 days;
a consumer that needs a longer history stores what it reads.

## Administration

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/v1/admin/overview` | `space.admin` | one row per space: `owner`, `files`, `bytes`, `trashed_bytes`, `links`, `last_write_at` |
| POST | `/v1/admin/spaces/{owner}/restore` | `space.admin` | `{"id"}` restores one trashed object of that space; the answer is `{"id", "kind", "status"}`, where `kind` is `file` |

Everything else an administrator needs is an ordinary route asked about a
space the administrator does not own: `GET /v1/files/{owner}/files?list=1`
for its objects, `GET /v1/shares?owner=` for its grants, `GET
/v1/trash?owner=` for what it can still recover, and `GET
/v1/events?owner=` for what happened to it. Administrative actions are
recorded in the same event log.

## The authorization endpoint

This section is for whoever writes the endpoint `ARCA_AUTHORIZER_URL` points
at. The same contract serves Arca's sibling projects, so one endpoint can
answer for several of them behind one bearer. In Go, import
`latere.ai/x/arca/authorizer` for the action names and resource shapes
rather than copying the strings from this page.

Before every action, Arca sends:

```
POST <ARCA_AUTHORIZER_URL>
Authorization: Bearer <ARCA_AUTHORIZER_TOKEN>
Content-Type: application/json

{
  "subject":  "https://issuer.example|9ab3",
  "issuer":   "https://issuer.example",
  "sub":      "9ab3",
  "claims":   {...},
  "action":   "file.write",
  "resource": {"kind": "File", "owner": "https://issuer.example|9ab3",
               "path": "files/reports/q3.pdf", "plane": "files", "size": 48213},
  "request":  {"id": "req_01J8R4...", "ip": "203.0.113.4", "user_agent": "curl/8.7"}
}
```

`claims` is every verified claim of the token, as it arrived; Arca reads none
of them itself, so a plan, a team, or a role is read here. `subject` is empty
for an anonymous request, which only the link routes make. `resource` is one
flat object: its `kind`, its `id` when it has one, and the fields of that
kind. A field Arca does not know for a question is left out rather than sent
empty.

The vocabulary is fifteen actions over six kinds:

| Kind | Actions | Fields |
|---|---|---|
| `File` | `file.read`, `file.write`, `file.delete`, `file.list`, `file.restore` | `owner`, `path`, `plane`, `size`, `from`, `recursive`, `copy`, `grant` |
| `Upload` | `upload.write` | `owner`, `path`, `size`, `grant` |
| `Share` | `share.create`, `share.read`, `share.list`, `share.revoke` | `owner`, `path`, `grantee`, `permission` |
| `Link` | `link.create`, `link.read`, `link.revoke` | `owner`, `path` |
| `Event` | `event.read` | `owner` |
| `Space` | `space.admin` | `owner`, absent on the overview across spaces |

Four fields of a `File` say what kind of act the question is about:

| Field | Present on | Meaning |
|---|---|---|
| `plane` | every `File` question that names a path | always `files`. A space is one tree; the member stays so that an endpoint that reads it keeps working |
| `from` | a move and a copy | the source path. `path` is then the destination |
| `copy` | a copy, as `true` | `from` is read and stays where it is. A question that carries `from` without `copy` is a move |
| `recursive` | a folder delete and a folder copy, as `true` | `path`, and `from` where there is one, name a folder and everything under it |

`recursive` and `copy` are absent unless true. A question about a folder
carries no `id`. A folder delete carries no `size`, and a copy's `size` is
the sum of the bytes it would write, counted before the answer. An endpoint
that knows neither member answers a folder delete as a delete of one path
and a copy as a move, which is never wider than what was asked. An endpoint
that manages folders of its own needs `recursive`: a delete of a folder is
a delete of every folder under it.

`grant` is the rung the caller holds on the resource through Arca's own
grants table, `read`, `write`, or `manage`, and is absent when it holds none.
On a question that carries `from`, a move or a copy, it is the lower of the
rungs the caller holds on `path` and on `from`, so a grant that covers one
end and not the other gives none. An endpoint that honors Arca's grants
reads it; one that keeps its own permissions ignores it.

The answer is `200` either way:

```
200 {"allow": true, "ttl": 60, "limits": {"quota_bytes": 53687091200}}
200 {"allow": true, "ttl": 300, "limits": {"versions": false, "trash": false}}
200 {"allow": true, "ttl": 60, "limits": {"omit": ["files/team/payroll", "files/agents/7c1e"]}}
200 {"allow": true, "filter": {"owners": ["https://issuer.example|9ab3"]}}
200 {"allow": false, "reason": "not a member"}
```

| Field | Meaning |
|---|---|
| `allow` | required; the decision |
| `reason` | on a deny, a short reason for the log |
| `ttl` | seconds Arca may cache an allow; 60 when absent, at most 600. A deny is cached for 5 seconds |
| `limits.quota_bytes` | the most the space may hold; a write past it is `413 quota_exceeded`. Absent means no limit |
| `limits.versions` | `false`: the write this answer admits keeps no version of the content it replaces. Read on the answer to `file.write`, to `upload.write`, and to `file.restore` when it brings a version forward. Absent or `true`: a version is kept |
| `limits.trash` | `false`: the delete this answer admits is permanent, exactly as `?permanent=1` is. Read on the answer to `file.delete`. Absent or `true`: the delete goes to the trash |
| `limits.omit` | a list of path prefixes of the space, read on the answer to a question that enumerates. The enumeration leaves out every path at or under one of them. Absent or empty: nothing is left out. [Its whole rule](#leaving-subtrees-out-of-an-enumeration) is below |
| `filter` | on a `list` action, narrows the page to the named `owners`; a list outside the filter is an empty page, not a `403` |

Rules an endpoint must keep:

1. **Answer 200 for both verdicts.** Anything else, a 5xx, a timeout after
   five seconds, or a body without `allow`, is a refusal and the client sees
   `503 authorizer_unavailable`. There is no fail-open.
2. **Deny the probe.** The resource id
   `00000000-0000-0000-0000-000000000001` is reserved: deny it for every
   subject and every action. `arcad check` sends it and fails when the
   endpoint allows it, because such an endpoint is not reading the request.
3. **Treat its availability as Arca's.** Every request waits on it. Run it
   near the replicas and answer from memory.

An endpoint written in Go can start from `latere.ai/x/pkg/authz/server`,
which handles the bearer, the decoding, and the validation against
`authorizer.Vocabulary()`, and can be tested with
`latere.ai/x/pkg/authz/conformance`.

### What an answer may limit

Arca stores no limit. An answer's `limits` object holds for that answer's
`ttl` and for the question it answered, and an answer without one limits
nothing. An installation that keeps folders of its own in its users'
spaces, such as the output of a program it runs for them, decides who
writes there in its endpoint and shapes the write through the answer. Arca
holds no list of such folders and reads no path for meaning.

- **A limit narrows and never widens.** No value of `limits.trash` makes a
  delete soft that asked for `?permanent=1`, no entry of `limits.omit` adds
  a row to an enumeration, and no member is read on a deny.
- **Each member is read off the answer to the request's own question.** An
  upload reads `limits.versions` off the answer to the `upload.write` its
  completing step asks, and nothing about a limit is kept with the session.
- **The object is read whole.** A member that is not the type the table
  above gives it, such as a `versions` that is not a boolean or a
  `quota_bytes` that is not a number, is the endpoint's error and is no
  decision: the request is refused with `503 storage_unavailable`, nothing
  is written, and no member of that answer is applied. A member Arca does
  not know is not read, so an endpoint may send members for a newer server.
- **`limits.versions: false` is about the write, not the path.** The
  content the write replaces is dropped once nothing else names its bytes,
  and the space is charged the difference in size. Versions the path
  already has are untouched. A version brought forward under it becomes the
  current content, and the content it replaces is dropped, not kept as a
  version.
- **`limits.trash: false` removes what `?permanent=1` removes:** the
  object, its versions, and their bytes, and for a folder delete what
  [a permanent folder delete](#folders) removes.
- **The built-in owner policy sends no limits.** An installation without an
  authorization endpoint keeps versions and the trash everywhere and leaves
  nothing out of an enumeration.

A limit is a switch or a list, never a number of versions to keep or a
retention of its own: `ARCA_TRASH_RETENTION` stays the installation's one
window.

### Leaving subtrees out of an enumeration

In a space several subjects may read, an endpoint answers an enumerating
question with "allowed, but leave these subtrees out", and `limits.omit` is
that answer.

| Rule | Value |
|---|---|
| The member | `limits.omit`, a JSON array of strings, read on the answer to a question that enumerates. Absent, `null`, or empty leaves nothing out |
| An entry | a path prefix of the space, in the shape a path has: it begins with `files/` and names at least one segment under it, with no leading or trailing slash, no empty segment, no `.` or `..`, and no control character. `files` on its own is not an entry: an endpoint that leaves everything out denies the question |
| What an entry covers | the path equal to it and every path under it, matched on whole segments. `files/team/pay` covers `files/team/pay` and `files/team/pay/may.pdf` and does not cover `files/team/payroll` |
| An answer Arca cannot read | an `omit` that is not an array of strings, or an entry that is not in the shape above, is the endpoint's error and is no decision: `503 storage_unavailable`, with nothing listed and nothing written. It is never ignored and never applied in part |
| Where it is applied | in the query. A page is as full as the rows the caller may enumerate allow, a count counts only those rows, and a cursor is the position of a row the caller read |
| What it does not shape | a question about one object or one folder. Each is its own question to the endpoint, which allows or refuses it, and Arca stores nothing from one answer to apply to the next |
| Usage | a space's usage stays the whole space's: the `space` object of a root listing, each row of the administrative overview, and what a write is charged against `limits.quota_bytes`. It is the counter the space is charged against, so it is not narrowed |
| The built-in owner policy | sends no `omit` |

One example. The answer to `file.list` for `files/team` carries
`"omit": ["files/team/payroll"]`. The listing of `files/team` then holds
no entry under `files/team/payroll` and does not synthesize that folder.
`GET /v1/files/{owner}/files/team/payroll/may.pdf` is not that question:
it asks `file.read` for that path, and the caller reads the object or is
refused by that answer alone. `GET
/v1/files/{owner}/files/team/payroll?list=1` asks `file.list` for that
folder, and its page is shaped by what that answer's own `limits.omit`
holds and by nothing an earlier answer said.

Every route that returns the path, the name, a count, or the bytes of
anything other than the one object asked for honors the member:

| Route | Answer it is read on | What an entry leaves out |
|---|---|---|
| `GET /v1/files/{owner}/{path}?list=1` | `file.list` | every entry it covers, and a folder in `prefixes` that holds nothing else. The listing is deep, one page of every object under the prefix, so one rule serves the listing of a folder and of every folder above it. A search with `?q=` and a page with `?recent=1` leave out the same entries |
| `GET /v1/files/materialize` | `file.list` | the files of the manifest it covers |
| `GET /v1/trash` | `file.list` | the trashed entries it covers |
| `DELETE /v1/trash` without `path` | `file.delete` | the trashed entries it covers stay in the trash and out of `purged` |
| `GET /v1/stars` | `file.list` | the stars it covers. The page crosses spaces (below) |
| `DELETE /v1/files/{owner}/{path}?recursive=1` | `file.delete` | the objects it covers are neither trashed nor removed, with their versions. A grant or a link at, under, or above an omitted subtree of the folder stays. The event's counts leave them out, and the call is still `204` |
| `POST /v1/files/{owner}/{path}` with `copy_to` and `recursive` | `file.write` | an object is not copied when an entry covers its source path or its destination path, and `files` and `bytes` leave it out. A folder all of whose objects are left out is `404 not_found`, as a folder with nothing under it is |
| `GET /v1/events` | `event.read` | an event whose `path`, or whose `detail.from`, it covers. Both ends of a move and of a copy are read. `next_cursor` is the id of a row the caller read, so the next page passes over every omitted row after it |
| `GET /v1/shares` | `share.list` | a grant whose `path_prefix` it covers. A grant on a prefix above an omitted subtree is listed |
| `GET /v1/shares/with-me` | `share.list` | the same. The page crosses spaces (below) |
| `GET /v1/shares/links` | `link.read` | a link whose `path_prefix` it covers |
| `GET /v1/shares/links/{token}` | `link.read` | the entries of the link's listing it covers |

Not shaped, each for the reason beside it:

| Route | Why |
|---|---|
| `GET` and `HEAD` on a path, `?version=N`, `?versions=1` | one object. The version listing names one path and cannot span two |
| `PUT` on a path, `POST` with `move_to`, `restore_version`, or `copy_to` without `recursive` | one object, named by the request |
| `DELETE` of one path, `?version=N`, `DELETE /v1/trash?path=`, `POST /v1/trash/restore`, `PUT` and `DELETE /v1/stars` | one object, named by the request |
| `POST /v1/uploads`, its complete and its abort | one session. No route lists sessions |
| `POST /v1/shares`, `GET` and `DELETE /v1/shares/{id}`, `POST /v1/shares/links`, `DELETE /v1/shares/links/{id}` | one grant or one link |
| `GET /v1/shares/links/{token}/meta`, `GET /v1/shares/links/{token}/files/{path}` | the link itself, and one object under it |
| `GET /v1/admin/overview` | usage per space, which stays the whole space's |
| `POST /v1/admin/spaces/{owner}/restore` | one id |
| the routes at the origin root | no space |

There is no search route.

**Two pages cross spaces.** `GET /v1/stars` and `GET /v1/shares/with-me`
ask one question that names the caller, and their rows are in every space
the answer's `filter` admits. One answer carries one list, so an entry
leaves out the matching path in every space on the page. An endpoint that
has to tell two spaces apart on these two pages narrows by `filter.owners`.
An entry scoped to one space is not offered.
