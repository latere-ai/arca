-- The table spec 007 owns: one open multipart upload.
--
-- The row is the only durable pointer to the upload's parts. An incomplete
-- multipart's parts are invisible to object listing, so nothing else in
-- either store can find them, and deleting this row before the store's abort
-- succeeds strands them for the life of the bucket, billed and unreachable.
--
-- object_id is the key the parts assemble onto, and it is the session's own
-- id rather than the id of the object the completion will replace. Nothing a
-- session does can therefore touch bytes a live row points at.
--
-- expires_at is created_at plus twenty-four hours, written as data rather
-- than applied as a hard-coded sweep, so the deadline is a column a test can
-- move and an operator can read.

CREATE TABLE upload_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner         TEXT NOT NULL,
    path          TEXT NOT NULL,
    object_id     UUID NOT NULL,
    upload_id     TEXT NOT NULL,
    declared_size BIGINT NOT NULL,
    content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX upload_sessions_expiry_idx ON upload_sessions (expires_at);
CREATE INDEX upload_sessions_object_idx ON upload_sessions (object_id);
