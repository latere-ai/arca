-- Spec 028: a space is one tree. Everything a space held under the second
-- plane, workspaces/<slug>/, moves into an ordinary folder under files/, and
-- the two tables spec 009 owned are dropped.
--
-- A move is a row update. A bucket key derives from an object id and never
-- from a path (spec 001, invariant 8), so this file changes no object id, no
-- size and no time, makes no bucket call, and leaves the usage ledger of
-- every space as it was: the bytes were counted before and are counted now.
--
-- The whole file is one statement, so it commits together or not at all.
-- Migrations are forward only (spec 004): a rollback is a later migration
-- that creates the two tables again, empty, and leaves the objects where
-- this one put them.

DO $$
DECLARE
    w         RECORD;
    root      TEXT;   -- workspaces/<slug>, what the rows are under today
    pattern   TEXT;   -- root as a LIKE pattern of everything under it
    candidate TEXT;
    suffix    INT;
    moved     BIGINT;
BEGIN
    -- What moves: one row per workspace, live or soft deleted, and one per
    -- slug that still has rows under workspaces/ with no workspace naming
    -- it. The second kind is unreachable today; it is moved and trashed at
    -- this migration's time, so it is neither lost nor brought back into a
    -- listing. trashed_at is NULL for what stays live.
    CREATE TEMP TABLE one_tree_moves (
        owner      TEXT NOT NULL,
        slug       TEXT NOT NULL,
        trashed_at TIMESTAMPTZ,
        recorded   BOOL NOT NULL,
        dest       TEXT,
        PRIMARY KEY (owner, slug)
    ) ON COMMIT DROP;

    INSERT INTO one_tree_moves (owner, slug, trashed_at, recorded)
    SELECT owner, slug, deleted_at, true FROM workspaces;

    INSERT INTO one_tree_moves (owner, slug, trashed_at, recorded)
    SELECT DISTINCT named.owner, named.slug, now(), false
      FROM (
                SELECT owner, split_part(path, '/', 2) AS slug FROM files            WHERE path LIKE 'workspaces/%/%'
          UNION SELECT owner, split_part(path, '/', 2)         FROM file_versions    WHERE path LIKE 'workspaces/%/%'
          UNION SELECT owner, split_part(path, '/', 2)         FROM stars            WHERE path LIKE 'workspaces/%/%'
          UNION SELECT owner, split_part(path, '/', 2)         FROM upload_sessions  WHERE path LIKE 'workspaces/%/%'
          UNION SELECT owner, split_part(path_prefix, '/', 2)  FROM shares           WHERE path_prefix LIKE 'workspaces/%'
      ) named
     WHERE named.slug <> ''
    ON CONFLICT (owner, slug) DO NOTHING;

    -- The spaces that already hold something at or under files/workspaces.
    -- A grant on the whole second plane is read against this below.
    CREATE TEMP TABLE one_tree_held ON COMMIT DROP AS
    SELECT DISTINCT owner FROM files
     WHERE path = 'files/workspaces' OR path LIKE 'files/workspaces/%';

    -- The destination, chosen for every move before any row moves, so no
    -- path collides and no row is overwritten. A folder is free when
    -- nothing in the space names it: no path, no version, no star, no open
    -- upload and no grant at or under it, and no other move took it.
    --
    -- First the folder of the slug's own name, wherever that is free.
    UPDATE one_tree_moves m SET dest = 'files/workspaces/' || m.slug
     WHERE NOT EXISTS (SELECT 1 FROM files x WHERE x.owner = m.owner
                          AND (x.path = 'files/workspaces/' || m.slug OR starts_with(x.path, 'files/workspaces/' || m.slug || '/')))
       AND NOT EXISTS (SELECT 1 FROM file_versions x WHERE x.owner = m.owner
                          AND (x.path = 'files/workspaces/' || m.slug OR starts_with(x.path, 'files/workspaces/' || m.slug || '/')))
       AND NOT EXISTS (SELECT 1 FROM stars x WHERE x.owner = m.owner
                          AND (x.path = 'files/workspaces/' || m.slug OR starts_with(x.path, 'files/workspaces/' || m.slug || '/')))
       AND NOT EXISTS (SELECT 1 FROM upload_sessions x WHERE x.owner = m.owner
                          AND (x.path = 'files/workspaces/' || m.slug OR starts_with(x.path, 'files/workspaces/' || m.slug || '/')))
       AND NOT EXISTS (SELECT 1 FROM shares x WHERE x.owner = m.owner
                          AND (x.path_prefix = 'files/workspaces/' || m.slug OR starts_with(x.path_prefix, 'files/workspaces/' || m.slug || '/')));

    -- Then <slug>-2, <slug>-3 and so on for the rest, in a fixed order.
    FOR w IN SELECT owner, slug FROM one_tree_moves WHERE dest IS NULL ORDER BY owner, slug LOOP
        suffix := 2;
        LOOP
            candidate := 'files/workspaces/' || w.slug || '-' || suffix;
            EXIT WHEN NOT EXISTS (SELECT 1 FROM one_tree_moves m WHERE m.owner = w.owner AND m.dest = candidate)
                  AND NOT EXISTS (SELECT 1 FROM files x WHERE x.owner = w.owner
                                     AND (x.path = candidate OR starts_with(x.path, candidate || '/')))
                  AND NOT EXISTS (SELECT 1 FROM file_versions x WHERE x.owner = w.owner
                                     AND (x.path = candidate OR starts_with(x.path, candidate || '/')))
                  AND NOT EXISTS (SELECT 1 FROM stars x WHERE x.owner = w.owner
                                     AND (x.path = candidate OR starts_with(x.path, candidate || '/')))
                  AND NOT EXISTS (SELECT 1 FROM upload_sessions x WHERE x.owner = w.owner
                                     AND (x.path = candidate OR starts_with(x.path, candidate || '/')))
                  AND NOT EXISTS (SELECT 1 FROM shares x WHERE x.owner = w.owner
                                     AND (x.path_prefix = candidate OR starts_with(x.path_prefix, candidate || '/')));
            suffix := suffix + 1;
        END LOOP;
        UPDATE one_tree_moves SET dest = candidate WHERE owner = w.owner AND slug = w.slug;
    END LOOP;

    FOR w IN SELECT * FROM one_tree_moves ORDER BY owner, slug LOOP
        root := 'workspaces/' || w.slug;
        pattern := replace(replace(replace(root, '\', '\\'), '%', '\%'), '_', '\_') || '/%';

        -- The objects. The id, the object id, the checksum, the size and
        -- the times are untouched. What a soft deleted workspace held, and
        -- what no workspace named, is trashed: it is in the trash of spec
        -- 005 for what is left of the retention window, restorable one
        -- object at a time, and the trash pass purges it.
        UPDATE files
           SET path = w.dest || substring(path FROM char_length(root) + 1),
               deleted_at = COALESCE(deleted_at, w.trashed_at)
         WHERE owner = w.owner AND path LIKE pattern ESCAPE '\';
        GET DIAGNOSTICS moved = ROW_COUNT;

        -- What keys on a path follows it: the history, the bookmarks, the
        -- open uploads, and the grants and links on the folder or inside it.
        UPDATE file_versions
           SET path = w.dest || substring(path FROM char_length(root) + 1)
         WHERE owner = w.owner AND path LIKE pattern ESCAPE '\';
        UPDATE stars
           SET path = w.dest || substring(path FROM char_length(root) + 1)
         WHERE owner = w.owner AND path LIKE pattern ESCAPE '\';
        UPDATE upload_sessions
           SET path = w.dest || substring(path FROM char_length(root) + 1)
         WHERE owner = w.owner AND path LIKE pattern ESCAPE '\';
        UPDATE shares
           SET path_prefix = w.dest || substring(path_prefix FROM char_length(root) + 1)
         WHERE owner = w.owner AND (path_prefix = root OR path_prefix LIKE pattern ESCAPE '\');

        -- The record: one move in the space's log, with no actor.
        INSERT INTO events (owner, path, action, detail)
        VALUES (w.owner, w.dest, 'move',
                jsonb_build_object('from', root, 'files', moved, 'trashed', w.trashed_at IS NOT NULL));
    END LOOP;

    -- A grant or a link on the whole second plane covered every workspace
    -- of the space, which is now the folder files/workspaces. Where the
    -- space already held something there, moving the grant would hand the
    -- grantee paths nobody shared with them, so the grant is revoked
    -- instead: a migration narrows access and never widens it.
    UPDATE shares s SET path_prefix = 'files/workspaces'
     WHERE s.path_prefix = 'workspaces'
       AND NOT EXISTS (SELECT 1 FROM one_tree_held h WHERE h.owner = s.owner);
    UPDATE shares SET status = 'revoked' WHERE path_prefix = 'workspaces';

    -- The tables, with their indexes. An attachment that is active ends
    -- with its table.
    DROP TABLE workspace_attachments;
    DROP TABLE workspaces;
END
$$;
