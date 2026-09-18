---
title: "Metadata store: the schema, migrations, transactions, the store interface"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
affects: [internal/store/, internal/store/migrations/, cmd/arcad/, .lateregate.yaml]
effort: large
created: 2026-09-18
updated: 2026-09-18
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
    path          TEXT NOT NULL,            -- plane-rooted: files/…, memory/…
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
    kind              TEXT NOT NULL CHECK (kind IN ('workspace','repo')),
    slug              TEXT NOT NULL,        -- [a-z0-9-]{1,64}
    created_by        TEXT NOT NULL,
    writer_holder     TEXT,                 -- the lease holder; NULL is free
    writer_expires_at TIMESTAMPTZ,          -- the reaper's deadline for the lease
    last_sync         TIMESTAMPTZ,          -- the snapshot materialize pins
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ,          -- soft delete; the reaper purges
    UNIQUE (owner, kind, slug)
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
-- The limit and the log. 010 owns both.
CREATE TABLE quotas (
    owner       TEXT PRIMARY KEY,
    limit_bytes BIGINT NOT NULL             -- ARCA_DEFAULT_QUOTA_BYTES until set
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
-- Outbound delivery. 011 owns it.
CREATE TABLE webhooks (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner            TEXT NOT NULL,
    url              TEXT NOT NULL,         -- https only
    secret           TEXT NOT NULL,         -- the signing key, shown once
    path_prefix      TEXT NOT NULL DEFAULT '',
    actions          TEXT[] NOT NULL DEFAULT '{}',
    active           BOOL NOT NULL DEFAULT true,
    created_by       TEXT NOT NULL,
    failure_count    INT NOT NULL DEFAULT 0,    -- consecutive, across events
    attempts         INT NOT NULL DEFAULT 0,    -- retries for the current event
    next_attempt_at  TIMESTAMPTZ,               -- backoff gate; NULL is due now
    cursor_event_id  BIGINT NOT NULL DEFAULT 0,
    last_delivery_at TIMESTAMPTZ,
    locked_until     TIMESTAMPTZ,               -- one replica delivers at a time
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX webhooks_due_idx ON webhooks (active, next_attempt_at);
-- Every administrative call. 012 owns it.
CREATE TABLE admin_audit (
    id         BIGSERIAL PRIMARY KEY,
    actor      TEXT NOT NULL,
    method     TEXT NOT NULL,
    route      TEXT NOT NULL,
    owner      TEXT,                        -- the space acted on, when there is one
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
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
| `0005_quotas_events.up.sql` | `quotas`, `events` | [[010-quotas-events-and-reaper]] |
| `0006_webhooks.up.sql` | `webhooks` | [[011-webhooks]] |
| `0007_admin_audit.up.sql` | `admin_audit` | [[012-administration]] |

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

`Versions`, `Stars`, `Sessions`, `Shares`, `Workspaces`, `Quotas`,
`Events`, `Webhooks`, and `Audit` follow the same shape, each owned by
its spec. `store.ObjectReferenced(ctx, q, id)` answers whether an object
id is still referenced by `files`, `file_versions`, or
`upload_sessions`. That union is what [[010-quotas-events-and-reaper]]
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
| `workspaces` | kept | `writer_holder` replaces `writer_sandbox_id`; `agent_access` is dropped |
| `workspace_attachments` | kept | `holder` replaces `sandbox_id`, `subject` replaces `principal_id` |
| `quotas`, `events`, `admin_audit` | kept | `owner` and `actor` are subjects |
| `webhooks` | kept | the lease column of the second migration is folded in |
| `agent_visibility` | dropped | it decided a read from the `principal_type` claim, which invariant 5 forbids. The authorizer receives the subject, the plane, and the path, so an operator that wants machine subjects kept out of a subtree says so there |
| team grants | never created | already deleted by Drive's `000017` |

Seventeen migrations become seven, because Arca starts at the shape
Drive reached. Carrying an existing Drive database across is
[[019-migration-from-drive]]'s, and the mapping above is its input.

## Not in this spec

What the rows mean to a caller, which is each owning spec's; the event
vocabulary and the reaper's passes ([[010-quotas-events-and-reaper]]);
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
