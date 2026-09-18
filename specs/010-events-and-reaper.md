---
title: "Events and the reaper: the ledger, the log, the reconciliation of the two stores"
status: testing
track: core
depends_on:
  - specs/003-object-store.md
  - specs/004-metadata-store.md
  - specs/006-identity.md
affects: [authorizer/, internal/events/, internal/reaper/, internal/api/, cmd/arcad/, docs/]
effort: large
created: 2026-09-18
updated: 2026-09-18
author: changkun
---

# Events and the reaper

## Overview

Three jobs that are one job. A platform that builds on Arca has to know
how much a space holds, so there is a ledger that counts bytes per space
and keeps the number current. Everything that happens to a space must be
observable without polling it, so there is an append-only log a consumer
tails by cursor. And two stores that fail independently drift apart, so
there is a loop that finds the drift and resolves it.

They belong together because each is the other's evidence. The reaper's
recoveries are what keeps the ledger honest, the events are how a reaper
run is seen at all, and the ledger is the one number an operator cares
about enough to want both.

Arca holds no limit of its own. It counts and it reports; whether a
space may grow further is the platform's decision, and it reaches Arca
in the authorizer's answer ([[006-identity]]). Nothing here decides
access.

## Current state

Built and in the tree on 2026-09-18. `internal/events` holds the ledger, the
closed action table, the append and the cursor tail; `internal/reaper` holds
the reconciler with its findings table and its seams; `arcad` has the `reap`
subcommand and the loop inside `serve`. The commits are `1ff054d` (the
reference check held to the schema), `150a7d0` (the ledger, the log and the
tail with migration `0005_usage_events.up.sql`), `810329d` (the passes, the
findings and the seams), `ad6f1d3` (the subcommand, the two variables and the
in-serve loop) and `95d9f82` (the store and e2e tiers). The gate passes at
each of them. The route joined the `/v1` frame of [[013-api]] with the merge
to `main`, which is where the handler gave up its own error writer.

What arrived from Drive is `internal/handler/events.go` (the append and the
cursor tail), the usage accounting inside `internal/handler/quota.go` (the
admission delta and the charge, not the stored limit), `internal/gc`
(every pass and the first-seen grace window), and migrations
`000005_quotas_events` and `000008_events_move`, folded into `0005` with the
action `CHECK` dropped in favour of the table in `internal/events`. What
changed on the way is the table in "What arrives from Drive" below, as
written.

The merge with [[005-files]] and [[007-uploads]] bound the last two seams
this spec offers. `cmd/arcad`'s `fileLedger` is the usage counter and the
log behind those specs' `Ledger`: a write's delta is charged inside the
write's own transaction against the limit the authorizer's answer carried, a
delete and an abandoned session give the bytes back, and a put, a move, a
delete and a restore are rows of the closed vocabulary. A charge the limit
does not admit is rendered as the file plane's own refusal, so a space with
no room left reaches the handler as `quota_exceeded` and not as a fault of
the counter. Pass 4 is [[007-uploads]]' `Service.Sweep`, so an expired
session's parts and row leave and its declared bytes go back.

### The route

`GET /v1/events` is a handler function, `events.Handler(log, querier, guard,
refuse)`, and no mux of this package carries it. [[013-api]] owns the route
table and registers it there, behind the verifier of [[006-identity]], with
the action `event.read`; `internal/api/events.go` is the whole of the
binding, and the node hands the surface the log and the database it reads
through.

The seam the wiring binds is `events.Guard`, two methods and no more:

```go
// Guard is the authorizer seam of spec 006, as internal/events needs it.
type Guard interface {
	// Caller answers the subject the verified token identifies, rendered
	// <issuer>|<sub>, and "" for a request that carried no usable token.
	Caller(r *http.Request) string
	// Ask puts one question of the vocabulary and answers the decision
	// verbatim, Filter included, so the handler narrows its own query. An
	// error is a call that produced no decision and fails closed as 503.
	Ask(r *http.Request, action string, resource authz.Resource) (authz.Decision, error)
}
```

The answer is `latere.ai/x/pkg/authz`'s `Decision` and not a boolean, because
criterion 10 needs the `Filter` and because an adapter over the shared client
is then one method deep. The adapter is `internal/api`'s `eventGuard`, which
reads the caller off the request context and translates
`auth.Authorizer.Decide`: that seam collapses a deny and an outage into
errors, and this one reads a deny as a decision and an error as no decision
at all. `refuse` is the second seam, `events.Refuser`: the handler names a
row of [[013-api]]'s error table and the developer detail, and the frame
writes the status, the one user sentence and the request id.
`events.LimitOf(decision)` reads
`limits.quota_bytes` off the same answer for the write paths of
[[005-files]] and [[007-uploads]].

### Where each criterion stands

| # | State |
|---|---|
| 1 | Holds. `TestLimitOfReadsWhatTheAnswerCarried` and `TestChargeHonoursTheLimitTheAnswerCarried` over an answer with no limits, and `0005_usage_events.up.sql` holds no limit column |
| 2 | Holds. `TestTheLimitLivesAsLongAsTheAnswerAndNoLonger` runs the shared client over a stub authorizer on a clock the test moves |
| 3 | Holds at the ledger: `TestStoreUsageAdmitsTheLimitAndRefusesTheByteAfterIt` against Postgres carries the used and limit figures. The `413` itself waits on a write route ([[005-files]], [[013-api]]) |
| 4 | Holds. The same test releases bytes on a space over the limit |
| 5 | Holds. `TestDeltaChargesWhatAWriteAdds`, one row per case of the admission table |
| 6 | Holds. `TestUsageFailsClosed` at the seam and `TestStoreUsageFailsClosed` against Postgres, where the refused charge rolls back with the row. The `storage_unavailable` rendering is `TestTheTailAnswersEveryRowThroughTheFrame` |
| 7 | Deferred to [[007-uploads]], which creates `upload_sessions`. The recomputation sums two tables and says so, and a space with an open session will reconcile low until the third term joins it |
| 8 | Holds. `TestEveryAppendedActionIsInTheTable` walks every Go file of the tree for an action built out of a literal |
| 9 | Holds. `TestStoreTheTailIsGaplessAcrossABurst`, eight writers and a keyset walk that reads each row once |
| 10 | Holds. `TestEventFilter`, and `TestTheEventTailAnswersThroughTheFrame` over the registered route. The conformance row is [[017-conformance-suite]]'s |
| 11 | Holds. `TestStoreAPutThatFailedAfterTheBucketWriteIsReapedAfterTheWindow` against MinIO |
| 12 | Holds. `TestStoreADeleteThatFailedAfterTheRowIsReaped` |
| 13 | Holds. `upload_sessions` joined the union with the migration that creates it ([[007-uploads]]), and `TestObjectReferencedNamesEveryTableThatHoldsAnObjectID` reads the embedded schema for every table carrying an `object_id` and holds the statement to that list |
| 14 | Holds. `TestPassTwoReportsARowWithoutItsBytesAndDeletesNothing` and `TestStoreARowWithoutItsBytesIsReportedAndKept` |
| 15 | Holds. Pass 3 is a `Pass` the reconciler is given, bound in `cmd/arcad` to [[009-workspaces]]' `Service.ExpireLeases`, with a unit test on a fake here and the expiry itself tested in that package. Pass 4 is bound the same way to [[007-uploads]]' `Service.Sweep`, which counts on a dry run and aborts the parts before it drops the row |
| 16 | The trash half holds: `TestStoreTrashPastItsRetentionLeavesBothStores`. The tombstone half is deferred to [[009-workspaces]] |
| 16b | Holds at the statement: `TestPassEightDropsAStarWhoseTargetIsGoneAndKeepsOneOnATrashedTarget`. The star routes are [[005-files]]'s |
| 17 | Holds. `TestStoreLedgerReconciles` against Postgres, with the healthy run correcting nothing |
| 18 | Holds. `TestARunTwiceLeavesWhatOneRunLeft` and the settled sweep of the store tier. Two reapers at once are two conditional statements, which is what the second run is |
| 19 | Holds for the nothing-changes half, absolutely, and for the same-findings half over every pass whose statements this package owns: `TestDryRunReportsWhatARunWouldChange` runs one fixture dry and live and holds the found counts equal. Pass 3 is the one exception and is under-reported: its sweep is [[009-workspaces]]' and every statement it issues is a write, so a dry run does not call it and reports nothing for it. Closing it is a counting half in that package. Pass 4's sweep is [[007-uploads]]' and is not an exception: what it would change is a query, so a dry run counts and changes nothing, which `TestAnExpiredSessionIsSweptWithItsPartsAndItsCharge` holds |
| 20 | Holds. `TestServeSaysWhetherThisReplicaReconciles` and `TestE2EReapRunsOneSequenceAndExits` |

### Divergences

Each is a decision rather than a gap.

- **Migration 0005 applies across a gap.** [[004-metadata-store]] assigns
  `0005` to this spec and `0002` through `0004` to [[007-uploads]],
  [[008-shares-and-links]] and [[009-workspaces]], which do not exist yet.
  The migrator records one version, so a database that applies `0005` before
  those three exist will never receive them. No installation is on this
  schema, a fresh database applies all five in order, and a development stack
  that reached `0005` first is recreated with `make clean`. The file says so
  at its top and `TestStoreTheLedgerAndTheLogApplyOverTheNumbersTheirSpecsHaveNotFilled`
  proves the gap itself applies.
- **`events_created_idx` is in the migration** and not in
  [[004-metadata-store]]'s schema block. Pass 9 deletes by `created_at`, and
  the predecessor carried the same index.
- **A thirteenth finding kind, `lease_expired`.** [[018-observability]]'s
  `kind` vocabulary has twelve members and none for an expired attachment,
  because that table counts those with `arca_lease_expiries_total`. A pass
  whose findings no run reports is a pass an operator cannot see run at all,
  so the reconciler reports one. 018 absorbs the row when it is built.
- **Pass 8 follows criterion 16b and not the pass table's wording.** The
  table says "no live `files` row" and the criterion says a star on a
  trashed but restorable target is kept; the criterion is the narrower
  statement, so the pass keeps a star whose target is trashed.
- **Pass 2 asks the bucket once per row per run.** The alternative is holding
  every key of the bucket in memory for the run, which grows with the
  installation. The cost is the same order as pass 1's query per key, which
  is what the predecessor already paid.
- **Pass 10 walks `space_usage` rows.** A space that holds rows and has no
  ledger row at all is outside the walk, which is what the spec's wording
  says; every write path creates the row with its first charge.
- **The reaper's sweep statements live in `internal/reaper`.**
  [[004-metadata-store]] owns one query set per table for the handlers; the
  reaper sweeps across tables several specs own, and a query set per table
  for one caller would spread one pass over four packages.
- **`Older` joined the `Log` interface**, the count pass 9 reports in a dry
  run. Without it the dry run would carry the log's own SQL into the reaper.
- **`ARCA_TRASH_RETENTION` joined `internal/config`** beside
  `ARCA_REAP_INTERVAL`. It is [[005-files]]'s row of
  [[002-repository-scaffold]]'s table, and passes 5 and 6 read it.
- **The tail's `id` is a number and `next_cursor` a string**, which is the
  envelope this spec's own example shows. [[013-api]]'s "shapes a consumer
  meets more than once" renders an event id as a string; the tail is keyset
  paginated on a `BIGSERIAL` and the number is what a consumer compares.
- **A denied `event.read` is `403 forbidden`.** Invariant 6's 404 is about a
  reference the request named being denied at lookup, and this route names a
  space rather than an object. The predecessor answered 404.
- **A tail whose query failed is `503 storage_unavailable`.** The database
  did not answer, which is retryable, and a 500 says otherwise.
- **The handler names rows of [[013-api]]'s error table and writes none of
  them.** It carried an unexported writer of its own while no mux registered
  it; registering the route deleted that function, and the five rows it
  answers go out through `events.Refuser` in the frame's envelope, with the
  `details.request_id` the middleware that mints one puts there.
- **`events.id` is allocated before its transaction commits.** A row with a
  lower id can therefore become visible after a higher one, and a tail
  reading at that instant would step past it. The window is one statement
  wide for an append, the burst test reads after the burst, and nothing here
  adds a lag mechanism this spec did not ask for.

### The Drive bugs this port fixes

| Bug | Where | Fix |
|---|---|---|
| The reference check read two of the three tables that keep bytes alive, while calling itself the single invariant deciding whether a blob may be deleted | `drive/internal/store/refs.go`, `StorageKeyReferenced` | `store.objectReferencedSQL` is held to the schema by `TestObjectReferencedNamesEveryTableThatHoldsAnObjectID`, and `TestTheGuardFindsThePredecessorsGap` runs the comparison over the predecessor's own two table statement and reports `upload_sessions` missing |
| The dry run under-reported: `pruneStars` and `pruneEvents` returned before counting and `purgeWorkspaces` skipped both of its counters, so what a dry run printed was not what a run would do | `drive/internal/gc/reconciler.go` | Every pass counts what it found in both modes, and `TestDryRunReportsWhatARunWouldChange` holds the found counts of one fixture equal |
| `Reconcile` returned on the first pass that failed, so a bucket that was down kept the ledger from ever reconciling | the same | Every pass runs, the failures are joined, and the run reports itself failed. `TestAPassThatFailsDoesNotStopTheRest` |
| A usage query that failed admitted the write, with a warning nobody reads | `drive/internal/handler/quota.go`, `quotaAllows` | The charge is inside the write's transaction, so a ledger that cannot be written takes the write down with it. `TestUsageFailsClosed`, `TestStoreUsageFailsClosed` |

## Design

### Usage and the authorizer's limit

Arca stores no per-space limit. There is no limit column, no route that
sets one, no action that reads or writes one, and no default. What Arca
stores is usage: the bytes a space holds, in the ledger below, current
after every operation that moves bytes.

A limit reaches Arca one way. When the authorizer's answer carries
`limits.quota_bytes`, that number is the space's limit for the answer's
`ttl` ([[006-identity]]). A write whose charge would take the space past
it is refused with `413` `quota_exceeded`, with the used and limit
figures in the developer detail. An answer without the field leaves the
space with no limit at all, which is what a self-hosted installation
running the owner policy gets.

The comparison is one subtraction between a number the answer already
carried and a number the ledger already holds. It resolves nothing, it
reads no second source, and it stores nothing, which is the whole reason
the limit belongs to the platform: a platform that gives one space a
larger allowance than another changes its own answer and writes nothing
to Arca's database.

**Admission.** A write is charged what it adds, which is not always its
size. A versioned overwrite writes a fresh key and the replaced object
survives as a version, so the space grows by the full new size. A
non-versioned overwrite keeps no second copy and nets out the replaced
size. Every write path routes its charge through one function so the two
cases cannot diverge.

| Case | Charged |
|---|---|
| a new object | its size |
| a versioned overwrite | the new size |
| a non-versioned overwrite | the new size minus the replaced size |
| a delete | nothing; a negative delta always passes |

A full space must stay shrinkable, which is why a delete is never
refused.

The charge is applied to the ledger row inside the write's own
transaction, and the comparison reads that row in the same transaction.
A ledger read or write that fails therefore takes the write down with
it, and the caller is refused with `storage_unavailable`. There is no
admitting a write whose cost could not be recorded: a number that cannot
be written is a number that stops being current, and the failure belongs
to the caller rather than to the ledger.
[[015-security-and-threat-model]] states the same rule as a control.

Usage leaves Arca in two places. The administrative overview of
[[012-administration]] names a space and the bytes it holds, and the
authorizer question carries `resource.size` on every write, so a
platform that keeps its own accounting sees each charge before it lands.

### The ledger

One row per space in `space_usage` ([[004-metadata-store]]): the owner
and the bytes it holds. Every statement that moves bytes applies its
delta to that row in the transaction that moves them, so the commit that
records a file and the commit that records its bytes are one commit and
there is no window in which the two disagree.

| Operation | Delta |
|---|---|
| a put | the size written, less the size a non-versioned overwrite replaced |
| an upload session opened | its declared size ([[007-uploads]]) |
| an upload session completed | the assembled size, less the declared size already charged |
| an upload session aborted or reaped | minus its declared size |
| a delete | nothing while the object sits in trash; minus its size when the row leaves |
| a purge | minus the bytes the purge removed |
| a version reaped | minus that version's size |

An open session's declared bytes count from the moment it opens, so a
caller cannot hold a thousand sessions open and fit them all under one
limit. Trashed bytes count for the whole retention window, because a
space that wants them back empties its trash and a reader of the number
is not surprised a month later. Versions count too: a space keeping ten
copies of a large file is storing ten copies, and saying otherwise would
make the ledger disagree with the bucket.

A counter can drift where a computed sum cannot, which is the price of
reading usage in one indexed lookup rather than summing two tables on
every write. Pass 10 of the reaper pays it: once per run it recomputes
each space from the rows that hold the bytes and corrects the ledger,
reporting every correction it makes.

```sql
SELECT (SELECT COALESCE(SUM(size_bytes),0)    FROM files           WHERE owner = $1)
     + (SELECT COALESCE(SUM(size_bytes),0)    FROM file_versions   WHERE owner = $1)
     + (SELECT COALESCE(SUM(declared_size),0) FROM upload_sessions WHERE owner = $1)
```

A correction is a finding, not routine housekeeping. A ledger that keeps
needing one has a write path that forgot its delta, which is a bug the
metric of [[018-observability]] makes visible.

### The log

Every mutation appends one row after it succeeds. The append is best
effort: a failed insert is logged and never fails the operation it
describes, because an event is a notification and the operation already
happened. There is one exception, and [[012-administration]] owns it: an
administrative mutation appends its event inside the mutation's own
transaction, because that event is the record of what an administrator
did and a record that can go missing quietly is not a record.

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

Eleven actions. Adding one is a change to that table and to the
conformance rows of [[017-conformance-suite]], and it is not a migration.
A consumer that wants finer grain than an action reads `detail`, which is
why a link share appends `share_created` with its kind in the detail
rather than claiming an action of its own, and why a refused write
appends nothing at all: a write that did not happen is not something
that happened to the space.

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

This log is the whole integration point for a consumer that wants to
know what happened. It is a tail and not an archive: the reaper prunes
rows older than thirty days, administrative events included, so an
installation that needs durable history tails the log and keeps the
result somewhere built for keeping things.

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
| 10 | ledger reconciliation | a `space_usage` row that differs from the sum over the rows that hold the bytes | corrects the row and reports the difference |

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
thousand. Each applies its delta to the ledger in the transaction that
removes the rows, so pass 10 has nothing to correct after a healthy run.

Pass 10 runs last, because every pass before it moves bytes and a
reconciliation over a moving ledger corrects numbers that were about to
be right anyway. It reads and writes one row per space, so it is the one
pass whose cost grows with the number of spaces rather than with the
work the installation did.

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

Pass 3 is the one exception, and it is a hole in that arrangement today.
Its sweep is a method of [[009-workspaces]]' service, which refuses to
build without the authorizer its handlers decide through, and `arcad
reap` registers no handler and starts no verifier; so the process runs
nine of the ten passes and says which one it does not run on its start-up
line. Pass 4 is not an exception, although its sweep is [[007-uploads]]'
in the same way: that sweep puts no question of the authorizer, so the
service it is a method of builds wherever the two stores are reachable
and `arcad reap` carries it. An installation that moves the reconciler off the API replicas
therefore leaves `ARCA_REAP_INTERVAL` non-zero on one replica, or the
service grows a constructor for the pass alone.

Concurrency needs no lease. Two replicas running pass 5 at the same
moment both issue a conditional delete and the second matches no rows.
That is cheaper than a lock and it removes the failure mode where a
crashed holder stops housekeeping for everyone. Pass 10 is the one pass
that has to be told this: it writes the recomputed value only where the
row still holds the value it read, so a correction racing a live charge
loses and is recomputed on the next run instead of erasing the charge.

### What arrives from Drive

| From | What changes |
|---|---|
| `.archive/008-quotas-gc-events.md` | the ledger, the log, and the reconciliation passes arrive; the stored limit does not |
| `drive/internal/handler/quota.go` | the admission delta and the charge, now applied to a stored counter inside the write's own transaction rather than recomputed per request behind a thirty second cache |
| `drive/internal/handler/events.go` | the append and the cursor tail |
| `drive/internal/gc` | every pass, and the first-seen grace window |
| migrations `000005_quotas_events`, `000008_events_move` | folded into `0005_usage_events.up.sql` of [[004-metadata-store]], with the action `CHECK` dropped in favour of the table in `internal/events` |

Owner addressing changes from `(owner_type, owner_id)` to the subject
`<issuer>|<sub>`, and the event's `actor_id` becomes the subject
`actor`. The nightly interval becomes `ARCA_REAP_INTERVAL`, and the
workspace purge window and the trash window, two constants in the
predecessor, become the one `ARCA_TRASH_RETENTION` that
[[002-repository-scaffold]] already carries.

Left behind with the hosted service: the stored limit, its two routes,
and the two defaults beside it, one for a person and one for an
organization. Arca has one kind of owner and no opinion about how much
it may hold, so a platform that wants a limit sends one in an answer and
Arca stores none.

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
([[005-files]]). The cross-space overview and the administrative
restore, which read this spec's numbers but are their own surface
([[012-administration]]). The metric names and the alert rules
([[018-observability]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | With no `limits.quota_bytes` in the answer no write is refused for size at any usage, and no table holds a limit | `internal/events` test over a stub authorizer that sends no limits, plus a schema test |
| 2 | A limit from the authorizer's `limits` is honoured for that answer's `ttl` and no longer | `internal/events` test with a stub authorizer and a fake clock |
| 3 | A write exactly at that limit is admitted and the next byte over is refused `413` with used and limit in the developer detail | store-tier test against Postgres |
| 4 | A delete on a space over the answer's limit is admitted | the same |
| 5 | A versioned overwrite is charged the full new size and a non-versioned one the difference | `internal/events` table test |
| 6 | A ledger read or write that fails refuses the write with `storage_unavailable` and leaves neither a row nor a charge | `TestUsageFailsClosed`, with a failing stub |
| 7 | An open upload session's declared bytes count against the space, and completing, aborting, and reaping each leave the ledger equal to a fresh recomputation | `TestOpenSessionsCount`, a store-tier test with [[007-uploads]] |
| 8 | Every action a handler appends is in the closed table, and no handler appends an action outside it | a test that walks the append sites against `internal/events`'s table |
| 9 | The tail is gapless for a given cursor across a concurrent write burst | store-tier test |
| 10 | An `event.read` answer carrying a `filter` narrows the page, and no handler scopes by a claim | `TestEventFilter`, plus [[017-conformance-suite]]'s row with a recording authorizer |
| 11 | A put that fails after the bucket write leaves bytes with no row, and pass 1 reaps them after the grace window and not before | the fault test [[001-architecture]] criterion 5 names, run against MinIO |
| 12 | A delete that fails after the row is gone leaves no visible object, and pass 1 reaps the bytes | criterion 6 of the same |
| 13 | An incomplete multipart's key is never reaped while its session row lives | store-tier test against MinIO |
| 14 | A row whose key the bucket does not hold is reported and not deleted | `internal/reaper` test |
| 15 | An expired lease is cleared and its attachment marked reaped | e2e with [[009-workspaces]] |
| 16 | Trash and tombstones past `ARCA_TRASH_RETENTION` are purged from both stores, lower the ledger, and appear as `purge` events | e2e |
| 16b | A star whose target was purged is deleted by pass 8, and a star on a trashed but restorable target is kept | store-tier test with [[005-files]] |
| 17 | A ledger row altered by hand is corrected by pass 10 and reported as a finding, and a healthy run corrects nothing | `TestLedgerReconciles` in `internal/reaper`, against Postgres |
| 18 | Every pass is idempotent, and two reapers running together produce the same end state as one | a test that runs the sequence twice and concurrently |
| 19 | `-dry-run` changes nothing in either store and reports the same findings | `internal/reaper` test |
| 20 | `ARCA_REAP_INTERVAL` of `0` leaves `serve` with no reaper loop, and `arcad reap -once` runs one sequence and exits 0 | `cmd/arcad` test |
