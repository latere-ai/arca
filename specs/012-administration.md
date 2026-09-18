---
title: "Administration: the overview across spaces, moderation, restore, the record of what was done, the check command"
status: testing
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/004-metadata-store.md
  - specs/005-files.md
  - specs/006-identity.md
  - specs/008-shares-and-links.md
  - specs/009-workspaces.md
  - specs/010-events-and-reaper.md
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
touch that was made. Those are two routes under `/v1/admin`, the
ordinary routes of every other spec answered on somebody else's space,
and the event log of [[010-events-and-reaper]] as the record.

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

## Current state

Built and in the tree on 2026-09-18. `internal/admin` holds the two
routes and contributes them through `api.Route`/`Options.Routes`;
`internal/store/admin.go` holds the overview's one statement;
`internal/check` holds `arcad check`, which `cmd/arcad` dispatches as the
fourth subcommand of [[002-repository-scaffold]]'s table. The gate passes
with every gate on and every package above 90%.

Criteria 1, 2, 3, 4, 5, 6, 12, 13 and 14 have passing tests. Criterion 7
is half open: the route, its question and its answers are proved, and
what it restores waits on the `Restorer` binding below. Criteria 8, 9, 10
and 11 belong to the mutations and the log, so they land with
[[005-files]] and [[008-shares-and-links]]; nothing here deletes from the
log, and this spec's own mutation is the restore. The spec stays at
`testing` until they close.

What the implementation decided, where this spec was silent or where the
tree made another reading better:

- **The owner policy does not admit a space's own owner for
  `space.admin`.** The policy of [[006-identity]] handed the shared frame
  an owned object for every action, so the frame's owner step admitted an
  owner asking this one, and the restore under `/v1/admin` answered a
  caller no installation had made an administrator. The action now
  reaches the frame with no object to own; the probe is unaffected,
  because the frame refuses the reserved id first. That spec's prose said
  the eight ungranted actions were "the owner's or an administrator's",
  which was the reading the code followed, and it now names this one as
  the administrator's alone.
- **The restore is behind a `Restorer` seam and unbound in this build.**
  What it undoes is [[005-files]]'s trashed row and [[009-workspaces]]'
  soft deleted workspace, neither of which this build answers, so the row
  is registered at its right place and answers `not_implemented`, which
  is what [[013-api]] reserves that code for. An id that names nothing
  still restorable is `ErrNotRestorable`, which the handler answers 404
  to with `ARCA_TRASH_RETENTION` named in the developer detail.
- **`links` arrives through a second seam and counts none here.** The
  token grants are [[008-shares-and-links]]'s table. A build that binds
  no counter counts zero, and zero is the true count: the three link
  routes answer `not_implemented` on this build, so no installation on it
  has issued one. The seam takes the page's owners together rather than
  one space at a time, so a page of a hundred spaces costs one query.
  This is the one divergence from "seven counters per space, in one
  statement": six come from the statement and the seventh from the seam.
- **The overview's row set is the tables that hold contents, not the
  subject directory.** A subject that made one request and stored nothing
  is not a space that holds anything, and the directory cannot tell the
  two apart. A space every counter of which is zero is dropped, which is
  criterion 6's second half as one predicate.
- **The cursor is the subject as the row carries it**, not the
  percent-encoded form this spec's example shows. It is opaque and a
  client sends it back as `?cursor=` unchanged; encoding it into a query
  is what a client does with any value, and a cursor already encoded in
  the body would be encoded twice on the way back.
- **`last_write_at` is the newest `updated_at` among the space's file
  rows**, and null for a space that holds none. The ledger's own
  `updated_at` is when bytes last moved, which a rename is not.
- **The table gains a fifth row, `public-url`.** `ARCA_PUBLIC_URL` is the
  base of every URL the server writes, and a value naming somewhere else
  sends every client there, which no other line would catch. A URL that
  answers something other than this server's version document is a
  failure; one that cannot be reached at all is not, because an ingress
  often does not answer from inside its own cluster and the check runs
  beside the server as often as in front of it.
- **The probe key is a fixed name and not a ulid.** Criterion 12 asks for
  two identical runs, and a key naming a fresh id would put a value on
  the line that differs every time. One key per installation is enough,
  because the check deletes it on every path out of the bucket line.
- **The check builds the authorizer client directly** rather than through
  `auth.Start`, which warms the verifier against every issuer: an issuer
  that does not answer would otherwise fail the authorizer line too, and
  each line answers for one dependency.
- **`arcad check` reads the same bucket mapping as the node.** The
  mapping lives in both `cmd/arcad` and `internal/check`, and a test in
  `cmd/arcad` holds the two equal, because a check reaching a different
  bucket than the server would pass an installation the server cannot
  serve.

## Design

### Who is an administrator

One action, `space.admin`, asked before every route below. The resource
is kind `Space`; `owner` is the space the restore names, and is absent
on the overview, which reads across every space.

| Decider | Answer |
|---|---|
| the authorizer, when `ARCA_AUTHORIZER_URL` is set | whatever it returns for `space.admin` on that resource; it may admit an administrator for one space and refuse another |
| the owner policy, otherwise | allow when the caller's subject is listed in `ARCA_ADMIN_SUBJECTS`, deny otherwise, including for a space's own owner |

A space's owner is not an administrator of its own space. The routes
here read across other people's spaces and restore across owners; the
owner's own equivalents are [[005-files]]'s and [[009-workspaces]]'s and
need no `space.admin`.

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

Two, both under `/v1/admin`, both asking `space.admin`. `{owner}` is a
subject, URL-encoded, or `me`.

| Method | Path | Does | `resource.owner` |
|---|---|---|---|
| GET | `/v1/admin/overview` | one row per space that holds anything, with its usage and its counts | absent |
| POST | `/v1/admin/spaces/{owner}/restore` | restore one deleted object or workspace | the named space |

Everything else an administrator does is an ordinary route of another
spec, answered on a space the caller does not own. Reading someone's
objects is `GET /v1/files/{owner}/{path...}?list=1`, their grants is
`GET /v1/shares?owner=`, what they can still recover is `GET
/v1/trash?owner=` and `GET /v1/workspaces/deleted?owner=`, and what
happened to their space is `GET /v1/events?owner=`. Each asks the action
it always asks, and what makes the call administrative is that the
authorizer allowed a caller who neither owns the space nor holds a grant
on it. A second surface that answered the same questions from a second
set of handlers would double every listing, every filter rule, and every
pagination bug.

There is no moderation route either. A moderation delete is `DELETE
/v1/files/{owner}/{path...}` asking `file.delete` on someone else's
space, which the authorizer allows for an administrator and the owner
policy allows for a subject in `ARCA_ADMIN_SUBJECTS`. One route, one
question, one delete implementation, and no id-addressed route that
skips the space.

The service Arca replaces served deleted objects at `/v1/admin/deleted`
across every space and moderation at `/v1/admin/files/{id}`. The listing
has no successor: it is `GET /v1/trash?owner=` and `GET
/v1/workspaces/deleted?owner=` with a different subject asking. The
moderation delete folds into `DELETE /v1/files/{owner}/{path...}`, which
is the same route the owner uses and the reason there is one delete
implementation instead of two.

### The overview

One route, one row per space, the list envelope and cursor pagination of
[[013-api]]:

```json
GET /v1/admin/overview?cursor=&limit=
200
{
  "entries": [
    {"owner": "https://issuer.example|9ab3...", "files": 214, "bytes": 88213004,
     "trashed_bytes": 402118, "workspaces": 2, "leases": 1, "links": 3,
     "last_write_at": "2026-09-17T08:41:02Z"}
  ],
  "next_cursor": "https%3A%2F%2Fissuer.example%7C9ab3..."
}
```

Seven counters per space, in one statement, keyed by subject. `bytes` is
the space's usage as [[010-events-and-reaper]]'s ledger holds it, which
is the number a platform bills and compares against whatever limit its
authorizer hands out; Arca stores no limit, so no column here names one.
`trashed_bytes` is the part of that usage trash still holds, because the
difference is what a reaper run would recover. `leases` counts live
workspace leases, which is the number of sandboxes holding a writer
lease right now ([[009-workspaces]]). The service Arca replaces also
counted pending share approvals; the approval queue is not Arca's
([[008-shares-and-links]]), so that counter is gone.

The installation's totals are not a second route. An operator that wants
one number sums the page or reads `arca_stored_bytes` from the metrics
of [[018-observability]], which is already the aggregate and costs no
query.

No email, no display name, and no directory. The service Arca replaces
kept a `principal_directory` table populated from the `email` claim so
its admin browser could show people instead of identifiers; that table
reads a claim for meaning and does not arrive. A console that wants
names resolves the subject against its own identity provider.

### Restore across owners

`POST /v1/admin/spaces/{owner}/restore` takes `{"id": "<object or
workspace id>"}` and answers `200 {"id": ..., "kind": ..., "status":
"restored"}`. It restores across owners, which is the whole reason it
exists beside [[005-files]]'s own restore, and it takes an id rather
than a path so that one route returns either a trashed object or a
soft-deleted workspace; `kind` in the answer says which it was. An id
already purged is a 404 whose developer detail says the retention window
has passed, so the administrator is not left guessing between a typo and
an expiry. The restore touches the database only, per invariant 1 of
[[001-architecture]].

An administrator finds the id in the owner's own listings, `GET
/v1/trash?owner=` and `GET /v1/workspaces/deleted?owner=`, both of which
carry `purges_at` derived from `ARCA_TRASH_RETENTION`.

### The record of what an administrator did

There is no audit table. The record is the event log of
[[010-events-and-reaper]], the same log a consumer tails, because an
administrative action is a thing that happened to a space and the space's
log is where things that happened to it are written. A second table
recorded the same facts a second time and answered them from a second
surface with its own filters, its own cursor, and its own way of falling
behind.

An administrative mutation appends its event inside the same transaction
as the mutation. Either both commit or neither does, so the log cannot
miss a moderation and cannot record one that was rolled back. That is
the one exception to the best-effort append of [[010-events-and-reaper]],
and it exists because a notification that may be dropped and a record
that may not are different things.

What marks an event as administrative is the actor. Every event carries
the subject that caused it, so an event whose `actor` is not the space's
owner was somebody else acting in the space, and `detail` carries
`admin: true` when neither ownership nor a covering grant explains the
allow. The test is mechanical: the caller's subject is not the space's
owner, and `shares.Covering` returns nothing for the path
([[008-shares-and-links]]). An installation cannot see why its
authorizer said yes, but it can see that neither ownership nor a grant
explains the yes, and that is the definition worth recording.

The mark is set where the decision is made and not in a handler, so a
route added later is recorded without being told to be. Under the owner
policy it costs nothing, because `Covering` was already computed to
reach the decision. Under an authorizer it is one extra indexed lookup
on requests against a space the caller does not own, and it buys the
property that no route can be added that reads another space unrecorded.

A reader asks for the record the way a consumer asks for anything else:

```json
GET /v1/events?owner=<subject>&cursor=<id>&limit=100
200
{"entries": [
  {"id": 8841, "action": "delete", "path": "files/reports/q3.pdf",
   "actor": "https://issuer.example|11c4...",
   "detail": {"admin": true, "reason": "moderation"},
   "at": "2026-09-17T09:14:51Z"}
], "next_cursor": "8841"}
```

`GET /v1/events` asks `event.read`, which an administrator is allowed on
any space and an owner only on its own, so the same route serves both
readers. No `detail` carries a byte of object content and none carries a
token.

The log is pruned at thirty days ([[010-events-and-reaper]]). An
installation that must keep a longer record tails the log and keeps the
result somewhere built for keeping things, which is the same answer any
consumer gets and is now the only answer, where the predecessor's table
grew without bound until somebody noticed.

### The check subcommand

`arcad check` reads the whole configuration table of
[[002-repository-scaffold]], tests every requirement of an installation
once, prints one line per requirement, and exits 1 if any line failed.
It opens no listener, runs no migration, and writes nothing that it does
not delete.

| Requirement | Passes when | Line names |
|---|---|---|
| bucket | `HeadBucket` answers, and a put of a small object under `<ARCA_BUCKET_PREFIX>_check/probe` with `If-None-Match: *`, a get of it, and a delete of it all succeed | the bucket, the endpoint, the prefix |
| database | the connection opens, the server answers, and the schema version equals the highest embedded migration with no dirty flag | the server version and the migration the schema is at |
| issuer | for each entry of `ARCA_OIDC_ISSUERS`: discovery answers, the key set parses, and it holds at least one key of an accepted algorithm | the issuer, the key count, the algorithms |
| authorizer | `ARCA_AUTHORIZER_URL` answers the probe question of [[006-identity]], the resource id `probe` of kind `Space`, with a well-formed `200` carrying `allow: false` | the endpoint and the decision |
| public-url | `ARCA_PUBLIC_URL` answers the version endpoint of [[002-repository-scaffold]] with this server's build identity. A URL nothing answers at all is not a failure: an ingress often does not answer from inside its own cluster, and the check runs beside the server as often as in front of it | the URL and what answered there |

```
$ arcad check
ok    bucket      arca-prod at https://s3.example, prefix arca/: wrote, read, deleted
ok    database    PostgreSQL 16.4, schema at 0005_usage_events, clean
ok    issuer      https://issuer.example: discovery ok, 3 keys, RS256 ES256
fail  authorizer  https://authz.example/decide: allowed the probe resource
ok    public-url  https://arca.example: answers the version endpoint
arcad: 1 of 5 checks failed
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
| the same file's `handleAdminOverview` | the overview | `pending_approvals` goes with the approval queue; `active_locks` becomes `leases`; `trashed_bytes` and `links` are added; the installation-wide totals become the metrics of [[018-observability]] and the route answers one row per space |
| the same file's `/v1/admin/deleted` and `DELETE /v1/admin/files/{id}` | `GET /v1/trash?owner=`, `GET /v1/workspaces/deleted?owner=`, and `DELETE /v1/files/{owner}/{path...}` | no administrative copy of a listing or a delete; an administrator asks the owner's own route about somebody else's space |
| migration `000006_admin_audit` | nothing | the `admin_audit` table does not arrive. The record is the event log of [[010-events-and-reaper]], written in the mutation's transaction |
| `drive/specs/.archive/009-admin-governance.md` | this spec | the surface and the transactional record survive; the separate audit table and its routes go; the live `/tokeninfo` re-check on mutations goes, because Arca calls an issuer for a key set and nothing else |
| `drive/internal/handler/directory.go`, `principal_directory` | nothing | display data built from the `email` claim; a console resolves names against its identity provider |
| nothing | `internal/check/`, `arcad check` | new; the service Arca replaces had one deployment and no installer, so it had nothing to check |

## Not in this spec

What the owner's own trash, restore, and workspace routes do once an
administrator is allowed on them ([[005-files]], [[009-workspaces]]).
The ledger this spec's overview reads and the log it uses as its record
([[010-events-and-reaper]]). Any limit on a space: Arca stores none, so
there is nothing here to set ([[010-events-and-reaper]]). The status
codes and error bodies ([[013-api]]). The metrics and traces `check`
does not emit ([[018-observability]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | Both routes under `/v1/admin` ask `space.admin` before they act, with `resource.owner` set on the restore and absent on the overview | `TestAdminRouteActions` against a recording authorizer, one case per row |
| 2 | A caller the authorizer denies gets 403 `forbidden`; a space's own owner with no `space.admin` gets 403 on its own space | the same test |
| 3 | With no authorizer and an empty `ARCA_ADMIN_SUBJECTS`, both admin routes answer 403 to every caller including the space owner | `internal/admin` policy test |
| 4 | A non-human caller the authorizer allows may moderate; no handler reads `principal_type` | `TestAdminReadsNoClaims`, plus the `identity` gate's `claims` rule |
| 5 | The overview's seven counters per space equal a direct count of the fixtures, `bytes` equals the ledger, and no counter reads the approvals table, which does not exist | e2e against Postgres |
| 6 | The overview pages by `cursor` across more spaces than one page holds, and lists no space that holds nothing | e2e |
| 7 | A restore across owners returns the object to its path and a soft-deleted workspace to its slug; an id past the retention window is 404 with the window named in the developer detail | e2e |
| 8 | A moderation delete and its event commit together: a forced failure after the delete leaves neither | `internal/admin` test on a transaction that is made to fail at commit |
| 9 | An allow on a space the caller neither owns nor holds a covering grant on marks its event `admin` wherever it happened; a read of the caller's own space and a read through a grant mark none | e2e: a moderation delete on `/v1/files/...` appears on `/v1/events?owner=` marked `admin`, and a grantee's read of the same path does not |
| 10 | `GET /v1/events` serves that record to an administrator for any space and to an owner for its own, and no route in this spec deletes from the log | e2e |
| 11 | No event `detail` carries object content or a link token | `TestEventDetailIsMetadataOnly` over the shapes the writers pass |
| 12 | `arcad check` prints one line per requirement in table order, exits 0 when all pass and 1 when any fails, and two runs against a healthy installation print identical output | `internal/check` test against the stubs of [[014-test-stubs-and-tiers]] |
| 13 | `check` fails on an unreachable bucket, a bucket it cannot write under the prefix, an unreachable database, a schema behind the embedded migrations, an issuer whose discovery does not answer, and an authorizer that allows the probe | `internal/check` table test, one case per failure |
| 14 | `check` deletes the object it wrote, and a bucket listing after a run holds nothing under `_check/` | the store tier of [[014-test-stubs-and-tiers]] |
