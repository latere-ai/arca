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
holds two **planes**, and every path begins with one of them:

| Plane | Paths | What it is for |
|---|---|---|
| files | `files/...` | files a person or an agent keeps: versions on overwrite, trash on delete, stars, shares, and links |
| workspaces | `workspaces/<slug>/...` | a working tree a sandbox attaches to, holds a writer lease on, and syncs back; see [workspaces](#workspaces) |

A path under any other first segment is `400 unknown_plane`.

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
directories the page implies, and, when the listing is of a plane root,
`space`, which is `{"bytes", "files"}` for the whole space.

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
| `unknown_plane` | 400 | the path does not begin with `files/` or `workspaces/` |
| `exclusive_fields` | 400 | two fields that cannot be set together are both set |
| `unauthenticated` | 401 | no token, or one that did not verify |
| `forbidden` | 403 | the action was refused |
| `not_found` | 404 | nothing there, or nothing the caller may see |
| `not_acceptable` | 406 | the `Accept` header excludes JSON |
| `path_taken` | 409 | something already exists at the target path |
| `slug_taken` | 409 | the space already has a workspace with that name |
| `writer_held` | 409 | another attachment holds the workspace's writer lease |
| `lease_not_held` | 409 | this attachment does not hold the writer lease |
| `manifest_incomplete` | 409 | a sync names objects that were never uploaded |
| `attachment_gone` | 410 | the attachment was released or expired; attach again |
| `length_required` | 411 | a `PUT` without `Content-Length` |
| `precondition_failed` | 412 | an `If-Match` or `If-None-Match` did not hold |
| `object_too_large` | 413 | the object is above what this installation accepts by this route |
| `body_too_large` | 413 | a JSON body is above what this installation accepts |
| `quota_exceeded` | 413 | the write would take the space past the limit the authorizer named |
| `unsupported_media_type` | 415 | a JSON route received another content type |
| `too_many_parts` | 422 | the object needs more upload parts than allowed |
| `link_read_only` | 422 | a link was asked for a permission above `read` |
| `rate_limited` | 429 | the caller is over its rate; wait and retry |
| `internal` | 500 | a fault in the server |
| `not_implemented` | 501 | this installation does not serve the route |
| `storage_unavailable` | 503 | the bucket or the database did not answer; retry |
| `authorizer_unavailable` | 503 | the authorization endpoint did not answer; retry |

### Limits

A subject may send `ARCA_REQUESTS_PER_MINUTE` requests a minute to one
replica, 600 by default. Requests without a bearer, and requests whose token
was refused, share a budget per client address, 60 a minute by default.
Past either, the answer is `429 rate_limited`.

Arca stores no storage limit of its own. When the authorization endpoint's
answer names `limits.quota_bytes`, a write that would take the space past it
is `413 quota_exceeded`. Without one, a space is unlimited.

## Files

| Method | Path | Action | What it does |
|---|---|---|---|
| PUT | `/v1/files/{owner}/{path}` | `file.write` | write one object; the body is the bytes; `201` with the object when it creates one, `200` when it overwrites |
| GET | `/v1/files/{owner}/{path}` | `file.read` | read one object; see the query parameters below |
| HEAD | `/v1/files/{owner}/{path}` | `file.read` | the object's headers, no body |
| POST | `/v1/files/{owner}/{path}` | `file.write` or `file.restore` | `{"move_to": "<path>"}` moves the object; `{"restore_version": N}` makes version N current again; exactly one of the two |
| DELETE | `/v1/files/{owner}/{path}` | `file.delete` | delete the object; `?permanent=1` skips the trash; `?version=N` removes one old version |
| GET | `/v1/files/materialize` | `file.list` | a whole space, or `?prefix=` below `files/`, as a manifest of presigned URLs; `?owner=` |

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

**Writing.** A `PUT` is admitted before its first byte is read, so it needs
`Content-Length`. A body above the inline size is refused with
`413 object_too_large`; send it through an [upload](#uploads) instead.
Overwriting a path keeps the previous content as a version, which costs one
database row and no copy of the bytes.

**Deleting.** Under `files/`, a delete moves the object to the trash, where
it stays restorable for `ARCA_TRASH_RETENTION` (30 days by default). Under
`workspaces/`, and with `?permanent=1` anywhere, the delete is final.

**Materialize.** `GET /v1/files/materialize` answers
`{"root", "pinned_at", "files": [{"path", "checksum", "size", "url"}]}` with
one presigned `GET` per file, valid for five minutes. It reads the space as it
is and needs no lease.

### Trash and stars

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/v1/trash` | `file.list` | what is trashed and still restorable, newest first; `?owner=` |
| POST | `/v1/trash/restore` | `file.restore` | `{"owner", "path"}` returns one object to its path |
| DELETE | `/v1/trash` | `file.delete` | empty the trash, or one path with `?path=`; `?owner=` |
| PUT | `/v1/stars` | `file.write` | `{"owner", "path"}` stars an object; repeating it is harmless |
| DELETE | `/v1/stars` | `file.write` | `?owner=` and `?path=` unstar an object; repeating it is harmless |
| GET | `/v1/stars` | `file.list` | the caller's stars across every space |

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
| `read` | `file.read`, `file.list`, `workspace.read`, `workspace.list` |
| `write` | the above, and `file.write`, `file.delete`, `file.restore`, `upload.write`, `workspace.write`, `workspace.attach`, `workspace.sync` |
| `manage` | the above, and `share.create`, `share.read`, `share.list`, `share.revoke` |

Creating and deleting a workspace, restoring a deleted workspace, reading a
space's events, minting and revoking links, and administration are never
reachable through a grant: they belong to the space's owner and to an
administrator. That is the owner policy's reading of the ladder; an
authorization endpoint of your own receives the rung the caller holds on the
resource and decides as it likes.

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

## Workspaces

A workspace is a named subtree, `workspaces/<slug>/`, that a sandbox
attaches to. Any number of readers may attach at once. At most one
attachment holds the **writer lease** at a time, and the lease expires on
its own, so a sandbox that crashes does not hold the workspace forever.

| Method | Path | Action | What it does |
|---|---|---|---|
| POST | `/v1/workspaces` | `workspace.create` | `{"owner", "slug"}` creates a workspace; the slug is lowercase letters, digits, and dashes, at most 64 characters, starting with a letter or digit; `201` |
| GET | `/v1/workspaces` | `workspace.list` | the workspaces of a space; `?owner=` |
| GET | `/v1/workspaces/deleted` | `workspace.list` | deleted workspaces still restorable; `?owner=` |
| GET | `/v1/workspaces/{id}` | `workspace.read` | one workspace, with its lease and counters |
| PATCH | `/v1/workspaces/{id}` | `workspace.write` | `{"slug"}` renames it |
| DELETE | `/v1/workspaces/{id}` | `workspace.delete` | delete it, restorable for `ARCA_TRASH_RETENTION`; `204` |
| POST | `/v1/workspaces/{id}/restore` | `workspace.restore` | undo a delete |
| POST | `/v1/workspaces/{id}/attach` | `workspace.read` for `ro`, `workspace.attach` for `rw` | `{"sandbox_id", "mode", "ttl_seconds"}` opens an attachment; `201` with the manifest it pinned |
| POST | `/v1/workspaces/{id}/attach/{aid}/renew` | the action the attach asked | `{"ttl_seconds"}` pushes the attachment's expiry forward |
| DELETE | `/v1/workspaces/{id}/attach/{aid}` | the action the attach asked | release the attachment and any lease it holds; `204`, repeatable |
| GET | `/v1/workspaces/{id}/materialize` | `workspace.read` | `?attachment=<aid>`: the pinned manifest with one presigned URL per file |
| POST | `/v1/workspaces/{id}/sync` | `workspace.sync` | `{"attachment_id", "files": [{"path", "checksum", "size"}]}` declares the subtree's full contents after the sandbox's work |

A workspace reads back as:

```json
{"id": "01J8R4...", "owner": "https://issuer.example|9ab3",
 "slug": "build", "root_prefix": "workspaces/build/", "files": 812, "bytes": 40122388,
 "lease": {"holder": "sbx_01J8...", "mode": "rw", "expires_at": "2026-09-18T11:02:11Z"},
 "last_sync": "2026-09-18T09:41:02Z", "created_by": "https://issuer.example|9ab3",
 "created_at": "2026-09-01T08:00:00Z"}
```

`lease` is null when no writer holds it.

A writer's cycle:

1. **Attach** with `mode: "rw"` and the sandbox's own id. A second `rw`
   attach while the lease is held is `409 writer_held`. The lease lasts
   `ttl_seconds`, one hour when omitted and at most 24 hours.
2. **Materialize** and download each file from its presigned URL, skipping
   any whose checksum the sandbox already holds.
3. **Write** changed files through `PUT /v1/files/{owner}/workspaces/<slug>/...`
   or an [upload](#uploads).
4. **Sync** with the complete list of files the subtree should hold. A file
   under the root that the list omits is deleted. A file the list names
   that was never uploaded fails the whole sync with
   `409 manifest_incomplete`, which names the missing paths, and nothing is
   changed. Replaying the same sync changes nothing.
5. **Renew** before the attachment expires, and **release** when done.

An attachment that expired or was released is `410 attachment_gone` on
renew and sync; attach again and materialize again. Work a writer did not
sync before its lease expired is lost, so a long-running sandbox syncs
periodically.

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

`action` is one of `put`, `move`, `delete`, `restore`, `purge`, `attach`,
`release`, `sync`, `reap`, `share_created`, and `share_revoked`. A consumer
that wants to act on changes tails this route: it keeps the last
`next_cursor` it read and asks again from there. The log keeps 30 days;
a consumer that needs a longer history stores what it reads.

## Administration

| Method | Path | Action | What it does |
|---|---|---|---|
| GET | `/v1/admin/overview` | `space.admin` | one row per space: `owner`, `files`, `bytes`, `trashed_bytes`, `workspaces`, `leases`, `links`, `last_write_at` |
| POST | `/v1/admin/spaces/{owner}/restore` | `space.admin` | `{"id"}` restores one trashed object or deleted workspace of that space |

Everything else an administrator needs is an ordinary route asked about a
space the administrator does not own: `GET /v1/files/{owner}/files?list=1`
for its objects, `GET /v1/shares?owner=` for its grants, `GET
/v1/trash?owner=` and `GET /v1/workspaces/deleted?owner=` for what it can
still recover, and `GET /v1/events?owner=` for what happened to it.
Administrative actions are recorded in the same event log.

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

| Kind | Actions | Fields |
|---|---|---|
| `File` | `file.read`, `file.write`, `file.delete`, `file.list`, `file.restore` | `owner`, `path`, `plane`, `size`, `from` (the source of a move), `grant` |
| `Upload` | `upload.write` | `owner`, `path`, `size`, `grant` |
| `Share` | `share.create`, `share.read`, `share.list`, `share.revoke` | `owner`, `path`, `grantee`, `permission` |
| `Link` | `link.create`, `link.read`, `link.revoke` | `owner`, `path` |
| `Workspace` | `workspace.create`, `workspace.read`, `workspace.write`, `workspace.delete`, `workspace.list`, `workspace.attach`, `workspace.sync`, `workspace.restore` | `owner`, `slug`, `grant` |
| `Event` | `event.read` | `owner` |
| `Space` | `space.admin` | `owner`, absent on the overview across spaces |

`grant` is the rung the caller holds on the resource through Arca's own
grants table, `read`, `write`, or `manage`, and is absent when it holds none.
An endpoint that honors Arca's grants reads it; one that keeps its own
permissions ignores it.

The answer is `200` either way:

```
200 {"allow": true, "ttl": 60, "limits": {"quota_bytes": 53687091200}}
200 {"allow": true, "filter": {"owners": ["https://issuer.example|9ab3"]}}
200 {"allow": false, "reason": "not a member"}
```

| Field | Meaning |
|---|---|
| `allow` | required; the decision |
| `reason` | on a deny, a short reason for the log |
| `ttl` | seconds Arca may cache an allow; 60 when absent, at most 600. A deny is cached for 5 seconds |
| `limits.quota_bytes` | the most the space may hold; a write past it is `413 quota_exceeded`. Absent means no limit |
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
