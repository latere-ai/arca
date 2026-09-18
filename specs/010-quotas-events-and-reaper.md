---
title: "Quotas, events, and the reaper: the limits, the ledger, the reconciliation of the two stores"
status: drafted
track: core
depends_on:
  - specs/003-object-store.md
  - specs/004-metadata-store.md
  - specs/006-identity.md
affects: [authorizer/, internal/quota/, internal/events/, internal/reaper/, internal/api/, cmd/arcad/, docs/]
effort: large
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Quotas, events, and the reaper

## Overview

Three jobs that are one job. A space must not grow without bound, so
there is a limit and a way to read what a space is using. Everything that
happens to a space must be observable without polling it, so there is an
append-only log a consumer tails by cursor. And two stores that fail
independently drift apart, so there is a loop that finds the drift and
resolves it.

They belong together because each is the other's evidence. The reaper's
recoveries are what keeps the usage number honest, the events are how a
reaper run is seen at all, and the quota is the one number an operator
cares about enough to want both.

Nothing here decides access. A quota is a number attached to a space, not
a permission: reading it asks `quota.read`, setting it asks
`quota.write`, and [[006-identity]] answers both. The authorizer can also
carry a limit in its answer, which is how a hosted platform gives one
space a bigger allowance than another without writing to Arca's database
at all.

## Design

### The limit

A space's limit is resolved per request, from three sources in order:

| Order | Source | When |
|---|---|---|
| 1 | `limits.quota_bytes` on the authorizer's answer | the answer carried one; valid for that answer's `ttl` ([[006-identity]]) |
| 2 | the `quotas` row for the space | an administrator has set one through `quota.write` |
| 3 | `ARCA_DEFAULT_QUOTA_BYTES` | otherwise |

The authorizer wins because it is the operator's live policy and the row
is a stored override an installation may not use at all. An installation
with no authorizer never sees rule 1 and works off the row and the
default. There is one default for every space, not one per kind of owner,
because Arca has one kind of owner: a subject.

### The ledger

Usage is computed, not counted. One statement sums `size_bytes` over
`files` and `file_versions` for the space, and the result is cached in
process for thirty seconds:

```sql
SELECT (SELECT COALESCE(SUM(size_bytes),0) FROM files         WHERE owner = $1)
     + (SELECT COALESCE(SUM(size_bytes),0) FROM file_versions WHERE owner = $1)
```

There is no counter and therefore no drift to reconcile. The cost is one
indexed sum per space per window instead of per write, and the price is
that a burst can overshoot by at most one window's worth of uploads. Any
mutation that moves bytes drops the space's cache entry, so the common
case of a write followed by a read of the meter is exact.

Versions count against the space. A space that keeps ten copies of a
large file is storing ten copies, and saying otherwise would make the
meter disagree with the bucket.

**Admission.** A write is charged what it adds, which is not always its
size. A versioned overwrite writes a fresh key and the replaced object
survives as a version, so the space grows by the full new size. A
non-versioned overwrite reuses the key and may net out the replaced size.
Every write path routes its check through one function so the two cases
cannot diverge.

| Case | Charged |
|---|---|
| a new object | its size |
| a versioned overwrite | the new size |
| a non-versioned overwrite | the new size minus the replaced size |
| a delete | nothing; a negative delta always passes |

A full space must stay shrinkable, which is why a delete is never
refused. A write that would cross the limit is refused with the used and
limit figures in the body, and appends a `quota_exceeded` event.

The usage query fails open. A quota is a cost control, not a safety
property, and a database blip that took every write down with it would be
a worse outage than a space that overshot for a minute. The log records
every fail-open, and [[018-observability]] counts them.

| Method | Path | Action | Answers |
|---|---|---|---|
| GET | `/v1/quotas/{owner}` | `quota.read` | `{"owner","used","limit","source"}` |
| PUT | `/v1/quotas/{owner}` | `quota.write` | sets the row |

`source` names which of the three rules produced the limit, so an
operator reading a surprising number knows where to change it.

### The log

Every mutation appends one row after it succeeds. The append is best
effort: a failed insert is logged and never fails the operation it
describes, because an event is a notification and the operation already
happened.

The action vocabulary is closed. [[004-metadata-store]] ships the column
without a database constraint, so the closure is a table in
`internal/events` that every writer and every filter reads:

| Action | Appended by |
|---|---|
| `put` | [[005-files]], [[007-uploads]] |
| `move` | [[005-files]] |
| `delete` | [[005-files]] |
| `restore` | [[005-files]], [[009-workspaces]] |
| `purge` | this spec's reaper |
| `attach` | [[009-workspaces]] |
| `release` | [[009-workspaces]] |
| `sync` | [[009-workspaces]] |
| `reap` | this spec's reaper |
| `share_created` | [[008-shares-and-links]] |
| `share_revoked` | [[008-shares-and-links]] |
| `quota_exceeded` | this spec |
| `webhook_disabled` | [[011-webhooks]] |

Thirteen actions. Adding one is a change to that table and to the
conformance rows of [[017-conformance-suite]], and it is not a migration.
A consumer that wants finer grain than an action reads `detail`, which is
why a link share appends `share_created` with its kind in the detail
rather than claiming an action of its own.

**The tail.** `GET /v1/events?owner=&cursor=&limit=` is a keyset tail on
the primary key, authorized with `event.read`:

```json
200
{
  "entries": [
    {"id": 41822, "action": "put", "path": "files/reports/q3.pdf",
     "actor": "https://issuer.example|0f5c...", "at": "2026-09-18T10:02:11Z",
     "detail": {"size": 48213}}
  ],
  "next_cursor": "41822"
}
```

A consumer replays from any cursor it kept; the envelope is [[013-api]]'s, `entries` and `next_cursor`, and an absent `next_cursor` is the end of the log. The ids are monotonic per
space, so a tail is gapless for a given cursor. An authorizer that wants
a caller to see only part of a space's log answers the `event.read`
question with a `filter` and the handler narrows its own query
([[006-identity]]); Arca applies no scoping of its own.

The log is a tail, not an archive. The reaper prunes rows older than
thirty days. An installation that needs durable history subscribes with
[[011-webhooks]] and keeps it somewhere built for it.

### The reaper

One loop, a sequence of passes, each idempotent and each safe to run
while another replica runs it, because every destructive statement is
conditional on the state it read. The passes exist because the two stores
of [[001-architecture]] fail independently and its invariants 1 and 2 say
what each failure leaves behind.

| # | Pass | Finds | Does |
|---|---|---|---|
| 1 | orphan bytes | a key under `ARCA_BUCKET_PREFIX` that no `files`, `file_versions`, or `upload_sessions` row references | deletes it, once it has been a candidate for longer than the grace window |
| 2 | orphan rows | a row whose key the bucket does not hold | reports it; never deletes |
| 3 | expired leases | an `active` attachment past `expires_at` | marks it `reaped` and clears the writer lease it held ([[009-workspaces]]) |
| 4 | expired uploads | an upload session idle past its TTL | aborts the multipart, then deletes the row ([[007-uploads]]) |
| 5 | trash | a trashed object past `ARCA_TRASH_RETENTION` | deletes the rows, then the keys |
| 6 | tombstones | a workspace soft deleted past `ARCA_TRASH_RETENTION` | deletes the subtree's rows, then its keys, then the row |
| 7 | grant hygiene | a grant expired long ago | revokes it ([[008-shares-and-links]]) |
| 8 | stale stars | a star whose path has no live `files` row | deletes it ([[005-files]]) |
| 9 | log retention | an event older than thirty days | deletes it |

Pass 1 is invariant 1's cleanup: a put writes the bucket first, so a
crash between the two stores leaves bytes nothing points at. The
referenced set is the union of three tables and not just `files`, because
an incomplete multipart's parts are invisible to object listing and the
session row is their only pointer ([[003-object-store]]). A key seen for
the first time inside the grace window is left alone, which is what stops
the pass from deleting bytes whose row is one statement away from
existing. First-seen times live in the process, so a restart re-arms the
window rather than shortening it.

Pass 2 is invariant 2's alarm, and it is deliberately not a deletion. A
row without bytes is a lie the database is telling, and the right answer
is a server error on read and a finding an operator looks at, never a
quiet delete of the only record that something existed.

Passes 5 and 6 delete rows before keys, which is invariant 1's order for
a delete, so a failure between them leaves bytes that pass 1 will find.
Deletes are batched through `DeleteMany` ([[003-object-store]]), so a
large subtree costs a number of calls proportional to the keys over a
thousand.

Each pass counts what it found and what it changed, appends a `reap`
event when it changed anything in a space, and reports through the
metrics of [[018-observability]]. A `purge` event marks a tombstone or a
trashed object leaving for good, which is the last thing said about it.

**Where it runs.** The loop runs inside `serve` on every replica, every
`ARCA_REAP_INTERVAL`. An installation that wants it off the API replicas
sets `ARCA_REAP_INTERVAL` to `0` there, which disables the in-process
loop, and runs `arcad reap` as a Deployment or a CronJob of its own
([[002-repository-scaffold]] owns the subcommand table). `arcad reap`
runs the same passes on the same interval and takes `-dry-run`, which
logs every finding and changes nothing; `-once` runs one pass sequence
and exits, which is the CronJob shape.

Concurrency needs no lease. Two replicas running pass 5 at the same
moment both issue a conditional delete and the second matches no rows.
That is cheaper than a lock and it removes the failure mode where a
crashed holder stops housekeeping for everyone. [[011-webhooks]] does
need a lease, for a reason that does not apply here: a duplicate delete
is nothing and a duplicate delivery is a wrong fact sent to a stranger.

### What arrives from Drive

| From | What changes |
|---|---|
| `.archive/008-quotas-gc-events.md` | quotas, the computed ledger, the log, the reconciliation passes |
| `drive/internal/handler/quota.go` | the thirty second usage cache, the admission delta, the fail-open |
| `drive/internal/handler/events.go` | the append and the cursor tail |
| `drive/internal/gc` | every pass, and the first-seen grace window |
| migrations `000005_quotas_events`, `000008_events_move` | folded into `0005_quotas_events.up.sql` of [[004-metadata-store]], with the action `CHECK` dropped in favour of the table in `internal/events` |

Owner addressing changes from `(owner_type, owner_id)` to the subject
`<issuer>|<sub>`, and the event's `actor_id` becomes the subject
`actor`. The two defaults, one for a person and one for an organization,
become the single `ARCA_DEFAULT_QUOTA_BYTES`, because an organization is
a subject like any other. The nightly interval becomes
`ARCA_REAP_INTERVAL`, and the workspace purge window and the trash
window, two constants in the predecessor, become the one
`ARCA_TRASH_RETENTION` that [[002-repository-scaffold]] already carries.

Removed with the org claim: the event tail's scoping, which let an
organization's administrator see every actor and a member only itself.
That was a decision read from `roles` and `org_id`; it is now the
authorizer's `filter` on `event.read`, and with no authorizer the owner
policy answers for the space.

Also removed: the `share_resolved` action, which recorded an approval,
because the approval queue does not move into the core
([[008-shares-and-links]]). The star-pruning pass stays, as pass 8: a
star is its own row and [[005-files]] hides a star whose target is gone
by joining against live rows, so nothing is ever wrong before the pass
runs and nothing grows without bound after it.

## Not in this spec

The tables ([[004-metadata-store]]). The bucket calls a pass makes
([[003-object-store]]). What a trashed object or a version is
([[005-files]]). Delivery of an event to a subscriber
([[011-webhooks]]). The cross-space overview and the administrative
restore, which read this spec's numbers but are their own surface
([[012-administration]]). The metric names and the alert rules
([[018-observability]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | The limit resolves authorizer, then row, then default, and `source` names which | `internal/quota` test over the three cases |
| 2 | A limit from the authorizer's `limits` is honoured for that answer's `ttl` and no longer | `internal/quota` test with a stub authorizer and a fake clock |
| 3 | A write exactly at the limit is admitted and the next byte over is refused with used and limit | store-tier test against Postgres |
| 4 | A delete on a space over its limit is admitted | the same |
| 5 | A versioned overwrite is charged the full new size and a non-versioned one the difference | `internal/quota` table test |
| 6 | A failing usage query admits the write and records the fail-open | `internal/quota` test with a failing stub |
| 7 | A refused write appends `quota_exceeded` | the tail test |
| 8 | Every action a handler appends is in the closed table, and the table matches the one the webhook filter validates against | a test that holds `internal/events`'s table equal to the actions [[011-webhooks]] accepts |
| 9 | The tail is gapless for a given cursor across a concurrent write burst | store-tier test |
| 10 | An `event.read` answer carrying a `filter` narrows the page, and no handler scopes by a claim | [[017-conformance-suite]]'s row, with a recording authorizer |
| 11 | A put that fails after the bucket write leaves bytes with no row, and pass 1 reaps them after the grace window and not before | the fault test [[001-architecture]] criterion 5 names, run against MinIO |
| 12 | A delete that fails after the row is gone leaves no visible object, and pass 1 reaps the bytes | criterion 6 of the same |
| 13 | An incomplete multipart's key is never reaped while its session row lives | store-tier test against MinIO |
| 14 | A row whose key the bucket does not hold is reported and not deleted | `internal/reaper` test |
| 15 | An expired lease is cleared and its attachment marked reaped | e2e with [[009-workspaces]] |
| 16 | Trash and tombstones past `ARCA_TRASH_RETENTION` are purged from both stores and appear as `purge` events | e2e |
| 16b | A star whose target was purged is deleted by pass 8, and a star on a trashed but restorable target is kept | store-tier test with [[005-files]] |
| 17 | Every pass is idempotent, and two reapers running together produce the same end state as one | a test that runs the sequence twice and concurrently |
| 18 | `-dry-run` changes nothing in either store and reports the same findings | `internal/reaper` test |
| 19 | `ARCA_REAP_INTERVAL` of `0` leaves `serve` with no reaper loop, and `arcad reap -once` runs one sequence and exits 0 | `cmd/arcad` test |
