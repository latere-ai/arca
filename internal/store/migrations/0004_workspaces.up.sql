-- The tables spec 009 owns: a durable subtree of a space that a sandbox
-- mounts and writes back, and the attachments that hold its writer lease.
--
-- There is no kind column. A checked-out repository is a workspace like any
-- other: its history lives on a git host and what Arca holds is a working
-- tree, so a column naming a second kind would name a distinction the
-- service does not make (spec 019).
--
-- (owner, slug) is unique and the uniqueness is not conditional on
-- deleted_at. That is what makes restore guard-free: a tombstone keeps its
-- slug reserved until the reaper purges it, so no live workspace can have
-- taken the name while the tombstone lived.

CREATE TABLE workspaces (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner             TEXT NOT NULL,        -- the space, the subject <issuer>|<sub>
    slug              TEXT NOT NULL,        -- [a-z0-9][a-z0-9-]{0,63}
    created_by        TEXT NOT NULL,        -- the subject that created it
    writer_holder     TEXT,                 -- the lease holder; NULL is free
    writer_expires_at TIMESTAMPTZ,          -- the reaper's deadline for the lease
    last_sync         TIMESTAMPTZ,          -- the boundary the newest snapshot was taken at
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ,          -- soft delete; the reaper purges
    UNIQUE (owner, slug)
);

-- The listing is keyset paginated on the id within one space.
CREATE INDEX workspaces_owner_idx ON workspaces (owner, id);
-- The reaper reads a lease whose holder crashed before it could release.
CREATE INDEX workspaces_lease_idx ON workspaces (writer_expires_at) WHERE writer_holder IS NOT NULL;

-- One sandbox's session against one workspace. The manifest is the snapshot
-- pinned at attach, which is what makes a materialize during another
-- writer's sync internally consistent rather than a torn mix.
CREATE TABLE workspace_attachments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    holder       TEXT NOT NULL,             -- the sandbox or process that attached, opaque to Arca
    subject      TEXT NOT NULL,             -- who it attached as
    mode         TEXT NOT NULL CHECK (mode IN ('ro','rw')),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','released','reaped')),
    manifest     JSONB NOT NULL,            -- [{path, checksum, size}] pinned at attach
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at  TIMESTAMPTZ
);

-- An attachment is read by its workspace, and the reaper reads the active
-- ones whose time has passed.
CREATE INDEX workspace_attachments_workspace_idx ON workspace_attachments (workspace_id, id);
CREATE INDEX workspace_attachments_expiry_idx ON workspace_attachments (expires_at) WHERE status = 'active';
