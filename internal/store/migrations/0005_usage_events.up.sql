-- The two tables spec 010 owns: the usage ledger and the event log.
--
-- There is no limit column and no limits table. Arca counts what a space
-- holds and stores no allowance: a limit reaches Arca on the authorizer's
-- answer, as limits.quota_bytes, and lives for that answer's ttl (spec 006).
--
-- Numbering: spec 004 assigns 0005 to this spec, and 0002 through 0004 are
-- specs 007, 008 and 009's. Migrations are forward only and the migrator
-- records one version, so a database that applied this file before those
-- three exist will never receive them. No installation is on this schema
-- yet, and a development stack that reaches it is recreated with
-- `make clean`; a fresh database applies all five in order.

-- One row per space: the bytes it holds, kept current by every statement
-- that moves bytes, inside the transaction that moves them.
CREATE TABLE space_usage (
    owner      TEXT PRIMARY KEY,
    bytes      BIGINT NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The append-only log a consumer tails by cursor. The action vocabulary is
-- closed by the table in internal/events and not by a database constraint:
-- the predecessor's CHECK made adding an action a migration, and the
-- vocabulary is read by every writer and every filter in Go already.
CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    owner      TEXT NOT NULL,
    path       TEXT,
    action     TEXT NOT NULL,
    actor      TEXT,
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The tail reads one space in id order; the reaper's retention pass deletes
-- by age, which is why the second index is here and not in spec 004's block.
CREATE INDEX events_owner_idx   ON events (owner, id);
CREATE INDEX events_created_idx ON events (created_at);
