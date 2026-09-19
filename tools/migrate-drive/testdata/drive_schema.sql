-- Drive's schema, as the store tier of spec 019 needs it.
--
-- This is the subset of the predecessor's migrations that `migrate-drive`
-- reads, flattened to the shape the seventeen files leave behind. It is a test
-- fixture and not a migration: nothing applies it but the tier beside it, and
-- the tool never writes to a database holding it.
--
-- Origin, file by file, in the predecessor's `migrations/` directory:
--
--   000001_files.up.sql            files
--   000002_shares.up.sql           shares
--   000003_workspaces.up.sql       workspaces
--   000004_attachments.up.sql      workspace_attachments
--   000005_quotas_events.up.sql    events (the quotas table is not here: spec
--                                  019 does not copy it, so the tier proves
--                                  the tool reads a database without it)
--   000007_principal_directory     principal_directory
--   000008_events_move.up.sql      the `move` action, folded into the CHECK
--   000009_trash_stars.up.sql      files.deleted_at, stars
--   000012_upload_sessions.up.sql  upload_sessions
--   000013_manage_permission.sql   the `manage` permission, folded into the CHECK
--   000014_file_versions.up.sql    file_versions
--   000017_drop_team_grants.sql    the grantee types, folded into the CHECK
--
-- The tables spec 019 leaves behind are absent on purpose: quotas, webhooks
-- and its lease, agent_visibility, admin_audit. A copy that read one of them
-- would fail here, which is the point.

CREATE TABLE principal_directory (
    principal_id UUID PRIMARY KEY,
    email        TEXT NOT NULL DEFAULT '',
    last_seen    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE files (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_type    TEXT NOT NULL CHECK (owner_type IN ('principal','org')),
    owner_id      UUID NOT NULL,
    path          TEXT NOT NULL,
    created_by    UUID NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    storage_key   TEXT NOT NULL,          -- drive/{owner}/{path}
    checksum      TEXT NOT NULL,
    is_public     BOOL NOT NULL DEFAULT false,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_type, owner_id, path)
);

CREATE TABLE file_versions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_type    TEXT NOT NULL CHECK (owner_type IN ('principal','org')),
    owner_id      UUID NOT NULL,
    path          TEXT NOT NULL,
    version_no    INT  NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL,
    checksum      TEXT NOT NULL,
    storage_key   TEXT NOT NULL,
    created_by    UUID NOT NULL,
    superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_type, owner_id, path, version_no)
);

CREATE TABLE stars (
    principal_id UUID NOT NULL,
    owner_type   TEXT NOT NULL CHECK (owner_type IN ('principal','org')),
    owner_id     UUID NOT NULL,
    path         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, owner_type, owner_id, path)
);

CREATE TABLE upload_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_type    TEXT NOT NULL CHECK (owner_type IN ('principal','org')),
    owner_id      UUID NOT NULL,
    path          TEXT NOT NULL,
    declared_size BIGINT NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    storage_key   TEXT NOT NULL,
    s3_upload_id  TEXT NOT NULL,
    created_by    UUID NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE shares (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_type      TEXT NOT NULL CHECK (owner_type IN ('principal','org')),
    owner_id        UUID NOT NULL,
    path_prefix     TEXT NOT NULL,
    grantee_type    TEXT NOT NULL CHECK (grantee_type IN
                      ('principal','email','org','role','link','public')),
    grantee_id      UUID,
    grantee_email   TEXT,
    grantee_role    TEXT,
    permission      TEXT NOT NULL CHECK (permission IN ('read','write','manage')),
    token           TEXT UNIQUE,
    status          TEXT NOT NULL DEFAULT 'active'
                      CHECK (status IN ('pending','active','denied','revoked')),
    created_by      UUID NOT NULL,
    resolved_by     UUID,
    resolved_at     TIMESTAMPTZ,
    resolution_note TEXT,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workspaces (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_type        TEXT NOT NULL CHECK (owner_type IN ('principal','org')),
    owner_id          UUID NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('workspace','repo')),
    slug              TEXT NOT NULL,
    created_by        UUID NOT NULL,
    writer_sandbox_id TEXT,
    writer_expires_at TIMESTAMPTZ,
    last_sync         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ,
    agent_access      TEXT NOT NULL DEFAULT 'visible'
                        CHECK (agent_access IN ('visible','hidden')),
    UNIQUE (owner_type, owner_id, kind, slug)
);

CREATE TABLE workspace_attachments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    sandbox_id   TEXT NOT NULL,
    principal_id UUID NOT NULL,
    mode         TEXT NOT NULL CHECK (mode IN ('ro','rw')),
    status       TEXT NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active','released','reaped')),
    manifest     JSONB NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at  TIMESTAMPTZ
);

CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    owner_type TEXT NOT NULL,
    owner_id   UUID NOT NULL,
    path       TEXT,
    action     TEXT NOT NULL CHECK (action IN
                 ('put','delete','sync','attach','release','reap',
                  'share_created','share_resolved','share_revoked',
                  'quota_exceeded','purge','restore','move')),
    actor_id   UUID,
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
