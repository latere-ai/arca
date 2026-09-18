---
title: "Administration: the view across spaces, moderation, restore, the audit log, the check command"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/004-metadata-store.md
  - specs/005-files.md
  - specs/006-identity.md
  - specs/008-shares-and-links.md
  - specs/009-workspaces.md
  - specs/010-quotas-events-and-reaper.md
affects: [internal/admin/, internal/check/, internal/api/, internal/store/, cmd/arcad/, docs/]
effort: medium
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Administration

## Overview

Every other spec answers for one space. This one answers across all of
them. An operator who runs an installation needs to see how much it
holds and who holds it, to open any space when a person asks what
happened to a file, to remove content that must not stay, to undo a
delete someone regrets, and to read back afterwards every administrative
touch that was made. Those are seven routes under `/v1/admin` and one
table.

It also answers the question an operator asks before any of that: is
this installation correctly configured. `arcad check` prints one line
per requirement and exits non-zero on the first failure, so a
deployment's readiness is a command rather than a reading of logs.

Administration is a capability, not a claim. Arca does not know who is
an administrator; it asks `space.admin` like every other action
([[006-identity]]), and the answer comes from the operator's authorizer
or, when none is configured, from `ARCA_ADMIN_SUBJECTS`. An installation
with no authorizer and no listed subjects has no administrator, and
every route in this spec answers 403 to everyone. That is the correct
default for a self-hosted single-user installation, which needs no
administrator to work.

## Design

### Who is an administrator

One action, `space.admin`, asked before every route below. The resource
is kind `Space`; `owner` is the space the route names on the four routes
that name one, and is absent on the three that read across all spaces.

| Decider | Answer |
|---|---|
| the authorizer, when `ARCA_AUTHORIZER_URL` is set | whatever it returns for `space.admin` on that resource; it may admit an administrator for one space and refuse another |
| the owner policy, otherwise | allow when the caller's subject is listed in `ARCA_ADMIN_SUBJECTS`, deny otherwise, including for a space's own owner |

A space's owner is not an administrator of its own space. The routes
here read other people's spaces, write audit rows, and restore across
owners; the owner's own equivalents are [[005-files]]'s and
[[009-workspaces]]'s and need no `space.admin`.

A deny is a 403 with code `forbidden`, the same as any other refused
action, and not the 404 the service Arca replaces answered in order to
hide the surface. [[006-identity]] fixes 404 for one case only, a deny
reached while resolving a reference, and the route names in
`/openapi.json` are public anyway. A named space that does not exist is
a 404 after the allow, never before it.

Arca applies no second gate of its own. In particular it does not refuse
a mutation to a non-human caller: the service Arca replaces read
`principal_type` for that, and reading a claim for meaning is what
invariant 5 of [[001-architecture]] forbids. An installation that wants
only people to moderate says so in its authorizer.

### The routes

Seven, all under `/v1/admin`, all asking `space.admin`, all returning
the list envelope and cursor pagination of [[013-api]]. `{owner}` is a
subject, URL-encoded, or `me`.

| Method | Path | Does | `resource.owner` |
|---|---|---|---|
| GET | `/v1/admin/overview` | the counters below | absent |
| GET | `/v1/admin/spaces` | one row per space that holds anything | absent |
| GET | `/v1/admin/spaces/{owner}/files` | the objects of one space, any plane | the named space |
| GET | `/v1/admin/spaces/{owner}/shares` | the grants on one space, revoked ones included | the named space |
| GET | `/v1/admin/spaces/{owner}/deleted` | what is deleted in one space and still restorable | the named space |
| POST | `/v1/admin/spaces/{owner}/restore` | restore one deleted object or workspace | the named space |
| GET | `/v1/admin/audit` | the audit log | absent |

The service Arca replaces served deleted objects at `/v1/admin/deleted`
across every space and moderation at `/v1/admin/files/{id}`. Both move
under `spaces/{owner}` so that one prefix has one owner and one
authorizer question carries a space ([[013-api]]'s grammar).

There is no moderation route at all. A moderation delete is `DELETE
/v1/files/{owner}/{path...}` asking `file.delete` on someone else's
space, which the authorizer allows for an administrator and the owner
policy allows for a subject in `ARCA_ADMIN_SUBJECTS`. One route, one
question, one delete implementation, and no id-addressed route that
skips the space. What makes it administrative is not a second question
but the audit row below.

### The overview

```json
GET /v1/admin/overview
200
{
  "spaces": 412,
  "files": 1840223,
  "bytes": 9418273645,
  "trashed_bytes": 402118234,
  "workspaces": 96,
  "leases": 3,
  "links": 28
}
```

Seven counters in one statement. `bytes` is live content only and
`trashed_bytes` is what trash still holds, because the difference is
what a reaper run would recover ([[010-quotas-events-and-reaper]]).
`leases` counts live workspace leases, which is the number of sandboxes
holding a writer lease right now ([[009-workspaces]]). The service Arca
replaces also counted pending share approvals; the approval queue is not
Arca's ([[008-shares-and-links]]), so that counter is gone.

`GET /v1/admin/spaces` pages the same counters per space, keyed by
subject, with `?cursor=` and `?limit=`:

```json
{
  "entries": [
    {"owner": "https://issuer.example|9ab3...", "files": 214, "bytes": 88213004,
     "quota_bytes": 10737418240, "workspaces": 2, "last_write_at": "2026-09-17T08:41:02Z"}
  ],
  "next_cursor": "https%3A%2F%2Fissuer.example%7C9ab3..."
}
```

No email, no display name, and no directory. The service Arca replaces
kept a `principal_directory` table populated from the `email` claim so
its admin browser could show people instead of identifiers; that table
reads a claim for meaning and does not arrive. A console that wants
names resolves the subject against its own identity provider.

### Listings across a space

`files` and `shares` are the owner's own listings of [[005-files]] and
[[008-shares-and-links]] with two differences: the authorizer question
is `space.admin` rather than `file.list` or `share.list`, and neither
applies the authorizer's `filter`, because an administrator's page is
not narrowed by a grant. The row shapes are the same shapes, so a
console renders one component for both surfaces.

`deleted` answers everything in the space that is deleted and still
restorable: trashed objects inside `ARCA_TRASH_RETENTION`, and
soft-deleted workspaces inside the same window. One list, one `kind`
field per row, because an administrator asking what is recoverable does
not know in advance which of the two a person lost.

```json
GET /v1/admin/spaces/me/deleted?kind=workspace
200
{"entries": [
  {"kind": "workspace", "id": "01J8...", "slug": "build", "deleted_at": "2026-09-16T11:02:33Z",
   "purges_at": "2026-10-16T11:02:33Z", "bytes": 44012},
  {"kind": "file", "id": "01J7...", "path": "files/reports/q3.pdf", "deleted_at": "...",
   "purges_at": "...", "bytes": 48213}
]}
```

`POST /v1/admin/spaces/{owner}/restore` takes `{"id": "<object or
workspace id>"}` and answers `200 {"id": ..., "kind": ..., "status":
"restored"}`. It restores across owners, which is the whole reason it
exists beside [[005-files]]'s own restore. An id already purged is a 404
whose developer detail says the retention window has passed, so the
administrator is not left guessing between a typo and an expiry. The
restore touches the database only, per invariant 1 of
[[001-architecture]].

### The audit log

One table, `admin_audit`, whose columns join the schema of
[[004-metadata-store]]:

| Column | Holds |
|---|---|
| `id` | `BIGSERIAL`, the cursor |
| `actor` | the administrator's subject `<issuer>\|<sub>` |
| `method`, `route` | the request line, the route pattern and not the raw path |
| `owner` | the subject of the space touched, null on the three routes that read across every space |
| `detail` | JSONB: the ids and paths the action names, never a byte of content and never a token |
| `at` | `TIMESTAMPTZ` |

A mutating administrative action writes its row inside the same
transaction as the mutation. Either both commit or neither does, so the
log cannot miss a moderation and cannot record one that was rolled back.
A read of another subject's space writes its row best effort, outside the
transaction, because a failed audit write must not fail a read.

Two kinds of request write a row, and the second is the one that
matters. Every route under `/v1/admin` writes one. So does every allow
on a space the caller neither owns nor holds a covering grant on,
wherever that allow happened, because such an allow was administrative
whatever action carried it: a moderation delete on `/v1/files/...`, a
read of someone's private object, a list of someone's workspace. The
test is mechanical: the caller's subject is not the space's owner, and
`shares.Covering` returns nothing for the path
([[008-shares-and-links]]). An installation cannot see why its
authorizer said yes, but it can see that neither ownership nor a grant
explains the yes, and that is the definition worth auditing.

The test runs on every allow against a space the caller does not own,
which includes every read a grantee makes. Under the owner policy that
costs nothing, because `Covering` was already computed to reach the
decision. Under an authorizer it is one extra indexed lookup on those
requests, and it buys the property that no route can be added that
reads another space unaudited.

The row is written where the decision is made and not in a handler, so
a route added later is audited without being told to be.

```json
GET /v1/admin/audit?actor=<subject>&owner=<subject>&cursor=<id>&limit=100
200
{"entries": [
  {"id": 8841, "actor": "https://issuer.example|11c4...", "method": "DELETE",
   "route": "/v1/files/{owner}/{path...}", "owner": "https://issuer.example|9ab3...",
   "detail": {"id": "01J7...", "path": "files/reports/q3.pdf", "reason": "moderation"},
   "at": "2026-09-17T09:14:51Z"}
], "next_cursor": "8841"}
```

The log is append only. No route deletes from it and the reaper does not
sweep it; an installation that must expire it does so in the database,
which is the operator's decision to record.

### The check subcommand

`arcad check` reads the whole configuration table of
[[002-repository-scaffold]], tests every requirement of an installation
once, prints one line per requirement, and exits 1 if any line failed.
It opens no listener, runs no migration, and writes nothing that it does
not delete.

| Requirement | Passes when | Line names |
|---|---|---|
| bucket | `HeadBucket` answers, and a put of a small object under `<ARCA_BUCKET_PREFIX>_check/<ulid>` with `If-None-Match: *`, a get of it, and a delete of it all succeed | the bucket, the endpoint, the prefix |
| database | the connection opens, `SELECT 1` answers, and the schema version equals the highest embedded migration with no dirty flag | the server version and the migration the schema is at |
| issuer | for each entry of `ARCA_OIDC_ISSUERS`: discovery answers, the key set parses, and it holds at least one key of an accepted algorithm | the issuer, the key count, the algorithms |
| authorizer | `ARCA_AUTHORIZER_URL` answers the probe question of [[006-identity]], the resource id `probe` of kind `Space`, with a well-formed `200` carrying `allow: false` | the endpoint and the decision |

```
$ arcad check
ok    bucket      arca-prod at https://s3.example, prefix arca/: wrote, read, deleted
ok    database    PostgreSQL 16.4, schema at 000012, clean
ok    issuer      https://issuer.example: discovery ok, 3 keys, RS256 ES256
fail  authorizer  https://authz.example/decide: allowed the probe resource
arcad: 1 of 4 checks failed
$ echo $?
1
```

Every line is `ok` or `fail`, then the requirement, then one sentence
naming what was reached and what happened. Lines go to stdout and the
summary to stderr, so a script reads the table and a human reads both.
Each check has a five second budget and they run concurrently; the
output is printed in the order of the table, not the order they
finished, so two runs of a healthy installation print identical output.

An authorizer that allows the probe is a failure and not a warning. An
authorizer that allows an action it does not recognise allows every
action Arca will ever add, which is the one misconfiguration that cannot
be noticed from the outside. `ARCA_AUTHORIZER_URL` unset is not a
failure: the line reads `ok authorizer not configured; the owner policy
applies, administrators are ARCA_ADMIN_SUBJECTS (2 listed)`.

`check` is what a release smoke calls ([[016-release-and-installation]]),
what an operator runs after editing a deployment, and what a container
image's `HEALTHCHECK` may call, because it costs one round trip to each
dependency and holds no connection open.

### What arrives from Drive

| From | To | What changes |
|---|---|---|
| `drive/internal/handler/admin.go` | `internal/admin/` | the gate becomes the `space.admin` question; the `principal_type` gate on mutations goes; a deny is 403, not a hidden 404 |
| the same file's `handleAdminOverview` | the overview | `pending_approvals` goes with the approval queue; `active_locks` becomes `leases`; `trashed_bytes` and `links` are added |
| the same file's `/v1/admin/deleted` and `DELETE /v1/admin/files/{id}` | `/v1/admin/spaces/{owner}/deleted`, and `DELETE /v1/files/{owner}/{path...}` | one prefix, one owner; moderation reuses the file delete rather than a second implementation |
| migration `000006_admin_audit` | the `admin_audit` table of [[004-metadata-store]] | `actor_id UUID` and `(owner_type, owner_id)` become subject strings; the indexes are the same two |
| `drive/specs/.archive/009-admin-governance.md` | this spec | the surface and the transactional audit rule survive; the live `/tokeninfo` re-check on mutations goes, because Arca calls an issuer for a key set and nothing else |
| `drive/internal/handler/directory.go`, `principal_directory` | nothing | display data built from the `email` claim; a console resolves names against its identity provider |
| nothing | `internal/check/`, `arcad check` | new; the service Arca replaces had one deployment and no installer, so it had nothing to check |

## Not in this spec

The owner's own trash, restore, and workspace routes ([[005-files]],
[[009-workspaces]]). Setting a quota, which is `quota.write` on
`/v1/quotas/{owner}` and not an administrative route
([[010-quotas-events-and-reaper]]). The per-space event log, which is a
consumer's tail and not an audit of administrators
([[010-quotas-events-and-reaper]]). The status codes and error bodies
([[013-api]]). The metrics and traces `check` does not emit
([[018-observability]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Every route under `/v1/admin` asks `space.admin` before it acts, with `resource.owner` set on the four that name a space and absent on the three that do not | `TestAdminRouteActions` against a recording authorizer, one case per row |
| 2 | A caller the authorizer denies gets 403 `forbidden`; a space's own owner with no `space.admin` gets 403 on its own space | the same test |
| 3 | With no authorizer and an empty `ARCA_ADMIN_SUBJECTS`, every admin route answers 403 to every caller including the space owner | `internal/admin` policy test |
| 4 | A non-human caller the authorizer allows may moderate; no handler reads `principal_type` | `TestAdminReadsNoClaims`, plus the `identity` gate's `claims` rule |
| 5 | The overview's seven counters equal a direct count of the fixtures, and no counter reads the approvals table, which does not exist | e2e against Postgres |
| 6 | `deleted` lists trashed objects and soft-deleted workspaces of one space with `purges_at` derived from `ARCA_TRASH_RETENTION`, and `?kind=` filters | e2e |
| 7 | A restore across owners returns the object to its path; an id past the retention window is 404 with the window named in the developer detail | e2e |
| 8 | A moderation delete and its audit row commit together: a forced failure after the delete leaves neither | `internal/admin` test on a transaction that is made to fail at commit |
| 9 | An allow on a space the caller neither owns nor holds a covering grant on writes an audit row wherever it happened; a read of the caller's own space and a read through a grant write none | e2e: a moderation delete on `/v1/files/...` appears in `/v1/admin/audit`, and a grantee's read of the same path does not |
| 10 | `/v1/admin/audit` pages by `cursor`, filters by `actor` and `owner`, and no route deletes from the table | e2e |
| 11 | No audit `detail` carries object content or a link token | `TestAuditDetailIsMetadataOnly` over the shapes the writers pass |
| 12 | `arcad check` prints one line per requirement in table order, exits 0 when all pass and 1 when any fails, and two runs against a healthy installation print identical output | `internal/check` test against the stubs of [[014-test-stubs-and-tiers]] |
| 13 | `check` fails on an unreachable bucket, a bucket it cannot write under the prefix, an unreachable database, a schema behind the embedded migrations, an issuer whose discovery does not answer, and an authorizer that allows the probe | `internal/check` table test, one case per failure |
| 14 | `check` deletes the object it wrote, and a bucket listing after a run holds nothing under `_check/` | the store tier of [[014-test-stubs-and-tiers]] |
