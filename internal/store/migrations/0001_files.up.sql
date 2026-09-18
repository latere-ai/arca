-- The tables spec 004 and spec 005 own: the subject directory, the live and
-- trashed paths of every space, the superseded contents of a path, and the
-- bookmarks a subject keeps.
--
-- Every space is addressed by one column, `owner`, holding the subject
-- <issuer>|<sub>. There is no owner_type and no organization table: an
-- organization is a principal whose subject the authorizer names, and Arca
-- cannot tell one kind of subject from another, which is the point.
--
-- Migrations are forward only. A rollback of a shipped change is a new
-- migration, and a test resets by dropping the database.

-- The spaces that have touched Arca. Presentation only: it resolves a
-- subject to something a person recognises, and no authorization reads it.
CREATE TABLE subjects (
    subject   TEXT PRIMARY KEY,
    display   TEXT NOT NULL DEFAULT '',
    last_seen TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per live or trashed path in a space.
CREATE TABLE files (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner         TEXT NOT NULL,
    path          TEXT NOT NULL,
    object_id     UUID NOT NULL,
    created_by    TEXT NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    checksum      TEXT NOT NULL,
    checksum_kind TEXT NOT NULL DEFAULT 'sha256' CHECK (checksum_kind IN ('sha256','etag')),
    is_public     BOOL NOT NULL DEFAULT false,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner, path)
);

CREATE INDEX files_prefix_idx ON files (owner, path text_pattern_ops);
CREATE INDEX files_object_idx ON files (object_id);
CREATE INDEX files_trash_idx  ON files (owner, deleted_at) WHERE deleted_at IS NOT NULL;

-- A superseded content of a path. Bytes are not copied: the row keeps the
-- object id the file carried before the overwrite, so capturing a version
-- costs one row.
CREATE TABLE file_versions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner         TEXT NOT NULL,
    path          TEXT NOT NULL,
    version_no    INT NOT NULL,
    object_id     UUID NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes    BIGINT NOT NULL,
    checksum      TEXT NOT NULL,
    checksum_kind TEXT NOT NULL DEFAULT 'sha256' CHECK (checksum_kind IN ('sha256','etag')),
    created_by    TEXT NOT NULL,
    superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner, path, version_no)
);

CREATE INDEX file_versions_path_idx      ON file_versions (owner, path);
CREATE INDEX file_versions_object_idx    ON file_versions (object_id);
CREATE INDEX file_versions_superseded_idx ON file_versions (superseded_at);

-- A bookmark belonging to one subject, not to the file.
CREATE TABLE stars (
    subject    TEXT NOT NULL,
    owner      TEXT NOT NULL,
    path       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subject, owner, path)
);
