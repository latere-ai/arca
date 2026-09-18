-- The table spec 008 owns: one grant over one subtree of one space.
--
-- A grant says that one grantee holds one permission on one subtree. The
-- grantee is a subject the authorizer names, or nobody at all, in which case
-- the token is the grantee and whoever holds it reads what the prefix names.
--
-- The narrowing spec 019 records is in the two CHECK constraints below. The
-- predecessor's table carried the grantee kinds `org`, `role`, `team` and an
-- invited address, and the statuses `pending` and `denied` with the three
-- columns that resolved them. A kind read from a claim is a decision Arca
-- does not make, and an approval queue is the platform's, so the column set
-- here is the one spec 008 describes and nothing more.

CREATE TABLE shares (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner        TEXT NOT NULL,           -- the space granting
    path_prefix  TEXT NOT NULL,           -- the subtree the grant covers
    grantee_kind TEXT NOT NULL CHECK (grantee_kind IN ('subject','link','public')),
    grantee      TEXT,                    -- the grantee subject, on a subject grant
    permission   TEXT NOT NULL CHECK (permission IN ('read','write','manage')),
    token        TEXT UNIQUE,             -- the capability, on a link or public grant
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
    created_by   TEXT NOT NULL,
    expires_at   TIMESTAMPTZ,             -- NULL is no expiry
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A subject grant names a grantee and carries no token; a token grant
    -- names no grantee and carries a token. The row is the authorization a
    -- redemption reads, so the pairing is the schema's rather than a rule a
    -- handler is trusted to keep.
    CONSTRAINT shares_grantee_shape CHECK ((grantee_kind = 'subject') = (grantee IS NOT NULL)),
    CONSTRAINT shares_token_shape   CHECK ((grantee_kind = 'subject') = (token IS NULL)),
    -- A token grant carries read and nothing more (spec 008). A create that
    -- asks for write or manage on one is refused before it reaches here;
    -- this is the same rule written where it cannot be bypassed.
    CONSTRAINT shares_token_is_read_only CHECK (grantee_kind = 'subject' OR permission = 'read')
);

-- The covering query of spec 008 reads one space's grants by the prefixes of
-- a path, and every listing of a space's grants pages by id.
CREATE INDEX shares_subtree_idx ON shares (owner, path_prefix);
CREATE INDEX shares_space_idx   ON shares (owner, id);

-- shares/with-me reads the grants whose grantee is the caller, which is one
-- walk of this index and no membership anywhere.
CREATE INDEX shares_grantee_idx ON shares (grantee, id) WHERE grantee IS NOT NULL;

-- A token resolves through the unique index the column already carries, so
-- there is no second index on it.
