---
title: "Metadata store: the schema, migrations, transactions, the store interface"
status: complete
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
affects: [internal/store/, internal/store/migrations/, cmd/arcad/, .lateregate.yaml]
effort: large
created: 2026-09-18
updated: 2026-09-19
author: changkun
---

# Metadata store

## Overview

The database half of [[001-architecture]]'s two stores. Postgres 16 or
newer holds every fact Arca knows that is not bytes: which path exists,
who owns it, which object id backs it, who was granted what, how much a
space holds, and what happened. The database decides existence
(invariant 2), so a query here answers "is there a file" and the bucket
is asked only afterwards. This spec owns the schema, the migration
mechanism, the transaction discipline, and the interface
`internal/store` exposes. It does not own what the rows mean to a
caller: each table names the spec that gives it meaning, and that spec
ships the migration creating it.

## Current state

Built and in the tree on 2026-09-18, phase 1 of [[019-migration-from-drive]].
`internal/store` holds the pool, the transactions, the migrator, and the two
query sets this spec owns, and `arcad` has the `migrate` subcommand and the
`database` readiness check. The commits are `7647e58` (the schema, the
migrator, the transactions, the query sets), `a630c2c` (the subcommand, the
variable, the schema check) and `df9c1ca` (the store tier against Postgres).
The gate passes at each of them.

What arrived from Drive is `internal/store` and `migrations/`: the pool
bring-up, the stripping of the pool parameters the migrator's driver does not
understand, and the shape of the four tables. What changed on the way is the
table in "What arrives from Drive" below, as written.

Divergences from the design as drafted, each a decision rather than a gap:

- Migration `0001_files.up.sql` is the only one in the tree. The other four
  are their owning specs', which is what the ownership table is for.
- `ObjectReferenced` reads `files` and `file_versions`. The third member of
  the union, `upload_sessions`, joins it with [[007-uploads]], which creates
  the table; criterion 6 therefore holds for two tables of three. The
  predecessor's equivalent, `StorageKeyReferenced` in
  `drive/internal/store/refs.go`, reads the same two while calling itself
  "the single invariant deciding whether a blob may be deleted", and its
  `upload_sessions` table holds a storage key its own migration calls the
  only durable pointer to an upload's parts. What keeps that omission out of
  reach in Drive is the reaper's 24 hour orphan grace window, not the
  invariant; Arca writes the union as three tables so a shorter window
  cannot make it reachable.
- The listing is keyset paginated in SQL rather than through
  `latere.ai/x/pkg/pagination`: that package paginates a slice already in
  memory, and what a listing needs is the `WHERE path > $cursor ORDER BY
  path LIMIT n` the index answers. The cursor a caller sees is the same, the
  last value of the ordered column.
- The migrator reads a connection string of its own: the scheme selects the
  golang-migrate driver and the `pgx/v5` driver registers `pgx5`, so
  `postgres://` is rewritten before `pgxmigrate.Up` sees it.
- The start-up check of criterion 2 refuses a database that answers and is
  behind this binary. A database that does not answer is not a verdict, so
  the `database` readiness check makes the same comparison once the database
  is there, and a rolling deploy whose database is briefly unreachable does
  not crash.
- Readiness now reaches both stores, so `/readyz` in the unit tier of
  `cmd/arcad` answers 503 naming the check that failed, and the 200 is proved
  by the e2e tier of [[014-test-stubs-and-tiers]] against real stores. The
  criterion of [[002-repository-scaffold]] that both listeners answer the
  probes still holds; what changed is the body a test with no stores reads.
- Pool sizing is left at the driver's defaults. Drive capped it at four
  connections per replica because its cluster was shared, and sizing is
  [[016-release-and-installation]]'s to decide for an installation.
- The unit tier proves the Go half of every query against fakes: the
  statement, the arguments it binds, the error it maps, and the row it
  scans. What the SQL means is the store tier's, which is the only place a
  transaction's semantics and a unique constraint can be proved at all.

## Design

### One change from Drive, applied everywhere: the owner is a subject

The predecessor addressed a space with a pair, `(owner_type, owner_id)`,
where `owner_type` was `principal` or `org` and `owner_id` a UUID from
one issuer. Arca has one column, `owner TEXT`, holding the subject
`<issuer>|<sub>` ([[006-identity]]). There is no organization table and
no `owner_type`: an organization is a principal whose subject the
authorizer names, and Arca cannot tell one kind of subject from another,
which is the point. Every `created_by`, `actor`, and grantee column
carries the same shape, at most 512 bytes, compared by exact bytes and
parsed by nothing in the schema.

### Tables

Every table keys on `owner` and, where it addresses content, on `path`.
`object_id` is the id of [[003-object-store]]; the key derives from it
and is never stored.

```sql
-- The spaces that have touched Arca. Presentation only: it resolves a
-- subject to something a person recognises. No authorization reads it.
CREATE TABLE subjects (
    subject   TEXT PRIMARY KEY,             -- <issuer>|<sub>
    display   TEXT NOT NULL DEFAULT '',     -- email or name from verified claims
    last_seen TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One row per live or trashed path in a space. 005 owns it.
CREATE TABLE files (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner         TEXT NOT NULL,            -- the space
    path          TEXT NOT NULL,            -- plane-rooted: files/…, workspaces/…
    object_id     UUID NOT NULL,            -- the bytes; 003 derives the key
    created_by    TEXT NOT NULL,            -- the subject that first wrote the path
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    checksum      TEXT NOT NULL,            -- sha256 hex, or the composite ETag
    checksum_kind TEXT NOT NULL DEFAULT 'sha256' CHECK (checksum_kind IN ('sha256','etag')),
    is_public     BOOL NOT NULL DEFAULT false,  -- set by 008, never by a path
    deleted_at    TIMESTAMPTZ,              -- trash; NULL is live
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner, path)
);
CREATE INDEX files_prefix_idx ON files (owner, path text_pattern_ops);
CREATE INDEX files_object_idx ON files (object_id);
CREATE INDEX files_trash_idx  ON files (owner, deleted_at) WHERE deleted_at IS NOT NULL;
-- A superseded content of a path. Bytes are not copied: the row keeps
-- the object id the file carried before the overwrite. 005 owns it.
CREATE TABLE file_versions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner         TEXT NOT NULL,
    path          TEXT NOT NULL,
    version_no    INT NOT NULL,             -- 1 upward, per path
    object_id     UUID NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL,
    checksum      TEXT NOT NULL,
    checksum_kind TEXT NOT NULL DEFAULT 'sha256',
    created_by    TEXT NOT NULL,            -- who wrote this content
    superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner, path, version_no)
);
CREATE INDEX file_versions_path_idx ON file_versions (owner, path);  -- plus (object_id) and (superseded_at), for the reaper
-- A bookmark belonging to one subject, not to the file. 005 owns it.
CREATE TABLE stars (
    subject    TEXT NOT NULL,               -- who starred
    owner      TEXT NOT NULL,               -- the space of the starred path
    path       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subject, owner, path)
);
-- One open multipart upload. The row is the only durable pointer to its
-- parts, which object listing does not show. 007 owns it.
CREATE TABLE upload_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner         TEXT NOT NULL,
    path          TEXT NOT NULL,            -- where completion will land it
    object_id     UUID NOT NULL,            -- the key the parts assemble onto
    upload_id     TEXT NOT NULL,            -- the store's multipart id
    declared_size BIGINT NOT NULL,          -- the promise, rechecked at completion
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL      -- created_at + 24h; the reaper aborts past it
);
CREATE INDEX upload_sessions_expiry_idx ON upload_sessions (expires_at);
-- A grant over a subtree. 008 owns the ladder and the statuses.
CREATE TABLE shares (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner           TEXT NOT NULL,          -- the space granting
    path_prefix     TEXT NOT NULL,          -- the subtree the grant covers
    grantee_kind    TEXT NOT NULL CHECK (grantee_kind IN ('subject','email','link','public')),
    grantee         TEXT,                   -- the subject, or the invited address
    permission      TEXT NOT NULL CHECK (permission IN ('read','write','manage')),
    token           TEXT UNIQUE,            -- link and public shares, invite acceptance
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending','active','denied','revoked')),
    created_by      TEXT NOT NULL,
    resolved_by     TEXT,
    resolved_at     TIMESTAMPTZ,
    resolution_note TEXT,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX shares_subtree_idx ON shares (owner, path_prefix);  -- plus partial indexes on (grantee_kind, grantee) and (token)
-- A durable subtree with one writer, and who has it mounted. 009 owns both.
CREATE TABLE workspaces (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner             TEXT NOT NULL,
    slug              TEXT NOT NULL,        -- [a-z0-9-]{1,64}
    created_by        TEXT NOT NULL,
    writer_holder     TEXT,                 -- the lease holder; NULL is free
    writer_expires_at TIMESTAMPTZ,          -- the reaper's deadline for the lease
    last_sync         TIMESTAMPTZ,          -- the snapshot materialize pins
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ,          -- soft delete; the reaper purges
    UNIQUE (owner, slug)
);
CREATE TABLE workspace_attachments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    holder       TEXT NOT NULL,             -- the sandbox or process that attached
    subject      TEXT NOT NULL,             -- who it attached as
    mode         TEXT NOT NULL CHECK (mode IN ('ro','rw')),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','released','reaped')),
    manifest     JSONB NOT NULL,            -- [{path, checksum, size}] pinned at attach
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at  TIMESTAMPTZ
);
-- The usage ledger and the log. 010 owns both. There is no limit
-- column and no limits table: a limit reaches Arca only on the
-- authorizer's answer, and is never stored.
CREATE TABLE space_usage (
    owner      TEXT PRIMARY KEY,            -- the space
    bytes      BIGINT NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,       -- the cursor a consumer tails
    owner      TEXT NOT NULL,
    path       TEXT,
    action     TEXT NOT NULL,               -- 010 owns the vocabulary
    actor      TEXT,                        -- the subject that caused it
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_owner_idx ON events (owner, id);
```

### Migration ownership

Migrations live in `internal/store/migrations`, embedded. Each spec
ships the file creating its tables, so two specs written in parallel do
not claim one number.

| File | Creates | Owning spec |
|---|---|---|
| `0001_files.up.sql` | `subjects`, `files`, `file_versions`, `stars` | this spec with [[005-files]] |
| `0002_uploads.up.sql` | `upload_sessions` | [[007-uploads]] |
| `0003_shares.up.sql` | `shares` | [[008-shares-and-links]] |
| `0004_workspaces.up.sql` | `workspaces`, `workspace_attachments` | [[009-workspaces]] |
| `0005_usage_events.up.sql` | `space_usage`, `events` | [[010-events-and-reaper]] |

Migrations are forward only. There are no `.down.sql` files: the source
driver accepts an up only set, a rollback of a shipped change is a new
migration, and the test tier resets by dropping the database
([[014-test-stubs-and-tiers]]).

### The migrate subcommand

`arcad migrate` reads `ARCA_DATABASE_URL`, applies every pending
migration, and exits. It is `latere.ai/x/pkg/pgxmigrate`, the bring-up
every service in the family runs: it opens the embedded source, retries
the database open for about ten seconds so a rolling deploy that briefly
holds every connection slot does not crash the incoming pod, applies,
and closes the pool it opened for itself. Reuse brings three packages
into the build list of `./cmd/arcad`, each a row in the `depcheck` allow
list beside the `pgx` rows: `github.com/golang-migrate/migrate/v4`, its
`source/iofs`, and its `database/pgx/v5` driver, which the caller blank
imports because `pgxmigrate` deliberately imports none.

`serve` does not migrate. It reads the applied version at start-up and
refuses to start when the embedded set is ahead of the database, naming
the pending file, so a deploy that forgot its migration Job fails at once
instead of serving against a schema it does not have. A database ahead
of the binary is a rollback in progress and is allowed.

### pgx, queries, transactions

`github.com/jackc/pgx/v5` and its `pgxpool`, no ORM and no query
generator. Queries are SQL text with numbered parameters. The pool opens
once from `ARCA_DATABASE_URL`, and its `Ping` is the readiness check
[[002-repository-scaffold]] reserves. Every query function takes a
querier, so one function serves callers inside and outside a
transaction:

```go
// Querier is what a query needs. *pgxpool.Pool and pgx.Tx both satisfy
// it, so no query function has two versions.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Tx runs fn in one transaction, commits when it returns nil, rolls
// back otherwise. The rollback runs on context.WithoutCancel(ctx): a
// rollback on a cancelled context is a no-op that hands the connection
// back to the pool still inside an open transaction.
func (db *DB) Tx(ctx context.Context, fn func(Querier) error) error
```

Four rules the query layer holds to:

1. Read committed is the isolation level. Every conditional write
   carries its condition in the statement (`WHERE checksum = $n`) and
   reports the row count, so a lost race is a zero, never an overwrite.
2. A read distinguishes `pgx.ErrNoRows` from every other error. A
   transient fault is a 500, never a 404 (invariant 2).
3. Listings are keyset paginated on the ordered column with
   `latere.ai/x/pkg/pagination`. A cursor is the last value of it.
4. A transaction holds no bucket call. Bytes move before the
   transaction opens on a write and after it commits on a delete
   (invariant 1), so a slow bucket never holds a row lock.

### The interface in internal/store

One small interface per table group, taken by the handlers so a test
passes a fake:

```go
type Files interface {
	Get(ctx context.Context, q Querier, owner, path string) (File, error)
	Insert(ctx context.Context, q Querier, f File) (created bool, err error)
	UpdateIfChecksum(ctx context.Context, q Querier, f File, ifMatch string) (bool, error)
	Move(ctx context.Context, q Querier, owner, from, to string) (bool, error)
	ListPrefix(ctx context.Context, q Querier, owner, prefix, cursor string, limit int) ([]File, string, error)
}
```

`Versions`, `Stars`, `Sessions`, `Shares`, `Workspaces`, `Usage`, and
`Events` follow the same shape, each owned by its spec. `Usage` carries
the one statement every write path shares, an upsert that applies a
signed delta to `space_usage.bytes` and returns the new total, so a
charge and the check against the authorizer's limit read one row in one
transaction ([[010-events-and-reaper]]). `store.ObjectReferenced(ctx, q, id)` answers whether an object
id is still referenced by `files`, `file_versions`, or
`upload_sessions`. That union is what [[010-events-and-reaper]]
sweeps against and what every delete of superseded bytes checks first.

### What arrives from Drive

From `migrations/000001` through `000017`, and `internal/store`.

| Drive table | In Arca | Note |
|---|---|---|
| `files` | kept | `owner` replaces `(owner_type, owner_id)`, `object_id` replaces `storage_key`, `checksum_kind` is new |
| `file_versions` | kept | the same two changes |
| `stars` | kept | `subject` replaces `principal_id` |
| `principal_directory` | renamed `subjects` | keyed by the subject, presentation only, as before |
| `upload_sessions` | kept | `object_id` replaces `storage_key`, and `expires_at` is a column rather than a hard-coded sweep |
| `shares` | kept, narrowed | `grantee_kind` loses `org`, `role`, and `team`. Arca reads no claim for meaning, so a grant names a subject, an address, a link, or the public |
| `workspaces` | kept, narrowed | `writer_holder` replaces `writer_sandbox_id`; `agent_access` is dropped; the `kind` column goes, because a checked-out tree is a workspace like any other and a repository's history lives on a git host ([[009-workspaces]]) |
| `workspace_attachments` | kept | `holder` replaces `sandbox_id`, `subject` replaces `principal_id` |
| `quotas` | replaced by `space_usage` | the limit column does not arrive; what is stored is the bytes a space holds, kept current by the write paths of [[010-events-and-reaper]] |
| `events` | kept | `owner` and `actor` are subjects; an administrator's action is an event in the space it touched ([[012-administration]]) |
| `admin_audit` | dropped | the audit is the event log. A separate table recorded the same facts a second time and answered from a second surface |
| `agent_visibility` | dropped | it decided a read from the `principal_type` claim, which invariant 5 forbids. The authorizer receives the subject, the plane, and the path, so an operator that wants machine subjects kept out of a subtree says so there |
| team grants | never created | already deleted by Drive's `000017` |

Seventeen migrations become five, because Arca starts at the shape
Drive reached and drops what it does not carry. Carrying an existing Drive database across is
[[019-migration-from-drive]]'s, and the mapping above is its input.

## Not in this spec

What the rows mean to a caller, which is each owning spec's; the event
vocabulary and the reaper's passes ([[010-events-and-reaper]]);
pool sizing ([[016-release-and-installation]]); the wire ([[013-api]]).

## Acceptance criteria

| # | Criterion | Proved by |
|---|---|---|
| 1 | `arcad migrate` against an empty database creates every table above, and a second run changes nothing | the store tier against Postgres |
| 2 | `arcad serve` against a database behind the embedded set exits 1 naming the pending file | `cmd/arcad` test with a stub version reader |
| 3 | A write inside `Tx` that returns an error leaves no row, and one that returns nil leaves exactly one | `internal/store` tests against Postgres |
| 4 | A rollback after the caller's context is cancelled returns the connection usable, and `UpdateIfChecksum` with a stale checksum changes nothing and reports false | `internal/store` tests, two concurrent writers |
| 5 | A unique violation on `(owner, path)` surfaces as a distinguishable conflict, and a missing row as `pgx.ErrNoRows` while a closed pool does not | `internal/store` tests |
| 6 | `ObjectReferenced` is true for an id held by any of the three tables and false otherwise | `internal/store` tests, one case per table |
| 7 | Keyset listing returns each row once under concurrent inserts, and its cursor round-trips | the store tier, inserting during the walk |
| 8 | Every subject column accepts a 512 byte subject containing `\|`, `:`, and `/` | `internal/store` tests |
| 9 | The `depcheck` allow list names every package this spec adds | `go tool lateregate` |

## Outcome

Complete on 2026-09-19. The schema, the migrator, the transaction
discipline and the two query sets this spec owns are in `internal/store`,
the embedded migration set in `internal/store/migrations`, and the
`migrate` subcommand with the `database` readiness check in `cmd/arcad`.
All five migrations are in the tree: `0001_files.up.sql` is this spec's,
`0002` through `0005` arrived with their owning specs, and a migrated
database holds the ten tables the schema above names.

Each criterion and what proves it:

1. `TestStoreMigrationsApplyAndAreIdempotent` in
   `internal/store/store_tier_test.go`. It reads in two parts: the test
   asserts `subjects`, `files`, `file_versions` and `stars` by name, and
   `Pending` returning nothing after the run is what proves the other four
   migrations applied too. The six tables those create are exercised
   against the same migrated database by the tier tests of [[007-uploads]],
   [[008-shares-and-links]], [[009-workspaces]] and
   [[010-events-and-reaper]]. A second `Migrate` changes nothing.
2. `TestTheServerRefusesToStartAgainstADatabaseBehindIt` in
   `cmd/arcad/main_test.go`, and
   `TestReadinessCarriesTheSchemaCheckWhenTheDatabaseArrivesLate`
   for the other half: a database that does not answer at start-up is
   compared by the readiness check instead of crashing the replica.
3. `TestStoreATransactionLeavesOneRowOrNone`.
4. `TestStoreARollbackOnACancelledContextReturnsTheConnectionUsable`, which
   cancels inside eight transactions and then asks the pool for work, and
   `TestStoreAConditionalReplaceLosesTheRaceRatherThanOverwriting` at the
   tier with `TestAConditionalReplaceReportsTheRaceItLost` at the unit tier.
5. `TestStoreAUniqueViolationIsToldFromAFault` for the constraint and the
   missing row, `TestARefusalIsToldFromAFault` for `classify` and `missing`
   telling `23505` and `pgx.ErrNoRows` from a connection failure.
6. `TestObjectReferencedNamesEveryTableThatHoldsAnObjectID` and
   `TestTheGuardFindsThePredecessorsGap` in `internal/store/schema_test.go`
   hold the statement to the schema, with one row case per table:
   `TestStoreAnObjectIsReferencedByAnyTableThatNamesIt` for `files` and
   `file_versions`, and
   `TestStoreAnOpenSessionKeepsItsObjectReferencedAndExpiresOnItsColumn` in
   `internal/store/uploads_tier_test.go` for `upload_sessions`.
7. `TestStoreAKeysetWalkReturnsEachRowOnceUnderConcurrentInserts`, which
   inserts ahead of the walk between two pages, with
   `TestAListingIsKeysetPaginatedAndCarriesItsCursor` at the unit tier.
8. `TestStoreASubjectColumnHoldsWhatASubjectIs`, a 512 byte subject
   carrying `|`, `:` and `/` through `subjects` and through `files.owner`.
9. Verified by reading `.lateregate.yaml` against `go list -deps
   ./cmd/arcad`: every `github.com/jackc/**` and
   `github.com/golang-migrate/migrate/v4**` package in the build list
   resolves to an allow row. The gate itself was not run here because its
   machine-global lock was held; the gate ran green on the commits this
   spec's Current state names.

Coverage of the owning packages on the unit run, collected the way the
cover gate collects it (`go test ./... -covermode=atomic -coverpkg=./...`,
per-package floor 90): `internal/store` 97.9%, `cmd/arcad` 91.8%.

One thing a later reader needs. The Current state above says criterion 6
"holds for two tables of three" because `upload_sessions` had not been
created yet by [[007-uploads]]. That sentence is stale as of this closing:
`objectReferencedSQL` reads all three tables, the schema guard fails if a
fourth table gains an `object_id` and is left out, and the third table has
its own row case. Nothing else in Current state changed.
