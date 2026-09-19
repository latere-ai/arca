// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"latere.ai/x/arca/internal/events"
)

// Run is one copy: the two databases, the rewrite, and what the report
// accumulates on the way.
type Run struct {
	Source, Target Conn
	Rewriter       Rewriter
	Prefix         string
	DryRun         bool
	Report         *Report
	// Manifest is where -manifest puts the file that ties a copied row to a
	// byte, and is empty when the operator named none.
	Manifest string

	// usage is the ledger, summed as files and versions copy. space_usage is
	// recomputed rather than copied, and this is the sum it is recomputed
	// from: the same two tables events.Recompute reads, so the reaper's
	// reconciliation pass finds nothing to correct on its first run.
	usage map[string]int64
	// objects is one line per distinct Drive key, so a file and the version
	// that superseded it keep pointing at one object, which is what the
	// reference union of spec 004 counts, and so the move of spec 019 has
	// one destination per key to write.
	objects map[string]*manifestLine
	// order is the keys as the object pass read them, which is the source's
	// key order and therefore the manifest's.
	order []string
}

// NewRun answers a run over two connections.
func NewRun(source, target Conn, r Rewriter, prefix string, dryRun bool, report *Report) *Run {
	return &Run{
		Source: source, Target: target, Rewriter: r, Prefix: prefix,
		DryRun: dryRun, Report: report,
		usage: map[string]int64{}, objects: map[string]*manifestLine{},
	}
}

// Table is one step of the copy: the name the report carries and the step
// that moves it.
type Table struct {
	Name string
	Copy func(context.Context, *Run) error
}

// Tables is every table spec 019 says arrives, in the order the copy takes
// them. The order is a dependency order and not an alphabet:
// workspace_attachments references a workspace, space_usage is recomputed
// from the files and versions already copied, and the log goes last because
// nothing references it.
//
// The tables spec 019 leaves behind are absent by being absent: quotas,
// webhooks, agent_visibility, admin_audit, and the approval rows of the
// shares table, which the classification drops.
func Tables() []Table {
	return []Table{
		{"subjects", copySubjects},
		{"files", copyFiles},
		{"file_versions", copyFileVersions},
		{"stars", copyStars},
		{"upload_sessions", copyUploadSessions},
		{"shares", copyShares},
		{"workspaces", copyWorkspaces},
		{"workspace_attachments", copyAttachments},
		{"space_usage", copySpaceUsage},
		{"events", copyEvents},
	}
}

// TableNames is the plan's order as plain names, which is what the report is
// built over.
func TableNames() []string {
	tables := Tables()
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Name)
	}
	return names
}

// Copy runs the plan. Each table is one transaction on the target, so a table
// either arrives whole or not at all, and the run stops at the first table
// that fails rather than leaving the rest to a second opinion.
func (r *Run) Copy(ctx context.Context) error {
	for _, t := range Tables() {
		if err := t.Copy(ctx, r); err != nil {
			return fmt.Errorf("migrate-drive: copy %s: %w", t.Name, err)
		}
	}
	return nil
}

// insert writes one row, or writes nothing on a dry run.
type insert func(sql string, args ...any) error

// table opens the transaction one table's rows are written in. A dry run runs
// every read and every rewrite and opens none, which is what makes its report
// the report the copy prints.
func (r *Run) table(ctx context.Context, fn func(insert) error) error {
	if r.DryRun {
		return fn(func(string, ...any) error { return nil })
	}
	return r.Target.Tx(ctx, func(tx Conn) error {
		return fn(func(sql string, args ...any) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	})
}

// path rewrites one row's path and counts the folds spec 019 names by name.
// Every table's copy goes through it rather than through Path, so a row that
// changed plane is a number in the report and not a silent rewrite.
func (r *Run) path(table, path string) (string, error) {
	moved, err := Path(path)
	if err != nil {
		return "", err
	}
	if Folded(path) {
		r.Report.Note(table, NoteAgentsFolded)
	}
	return moved, nil
}

// charge adds one row's bytes to the ledger the run recomputes space_usage
// from.
func (r *Run) charge(owner string, size int64) { r.usage[owner] += size }

// each reads one query and hands every row to fn, which is the shape every
// table's copy and every preflight check takes.
func each(ctx context.Context, c Conn, sql string, fn func(Rows) error, args ...any) error {
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// copySubjects moves Drive's principal directory. The table is presentation
// only in both schemas: it resolves a subject to something a person
// recognises and no authorization reads it.
func copySubjects(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT principal_id, email, last_seen FROM principal_directory ORDER BY principal_id`,
			func(rows Rows) error {
				var id, email string
				var lastSeen time.Time
				if err := rows.Scan(&id, &email, &lastSeen); err != nil {
					return err
				}
				if err := write(
					`INSERT INTO subjects (subject, display, last_seen) VALUES ($1, $2, $3)`,
					r.Rewriter.Principal(id), email, lastSeen); err != nil {
					return err
				}
				r.Report.Copy("subjects")
				return nil
			})
	})
}

// copyFiles moves every live and trashed path. The id is kept, so a row that
// referenced it still does; the owner becomes a subject, the path takes its
// plane's rule, and the storage key becomes an object id.
func copyFiles(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT id, owner_type, owner_id, path, created_by, content_type, size_bytes,
			        storage_key, checksum, is_public, deleted_at, created_at, updated_at
			   FROM files ORDER BY id`,
			func(rows Rows) error {
				var id, ownerType, ownerID, path, createdBy, contentType, storageKey, checksum string
				var size int64
				var isPublic bool
				var deletedAt *time.Time
				var createdAt, updatedAt time.Time
				if err := rows.Scan(&id, &ownerType, &ownerID, &path, &createdBy, &contentType,
					&size, &storageKey, &checksum, &isPublic, &deletedAt, &createdAt, &updatedAt); err != nil {
					return err
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				path, err = r.path("files", path)
				if err != nil {
					return err
				}
				if err := write(
					`INSERT INTO files (id, owner, path, object_id, created_by, content_type,
					                    size_bytes, checksum, checksum_kind, is_public,
					                    deleted_at, created_at, updated_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
					id, owner, path, string(r.objectID(storageKey)), r.Rewriter.Principal(createdBy),
					contentType, size, checksum, ChecksumKind(checksum), isPublic,
					deletedAt, createdAt, updatedAt); err != nil {
					return err
				}
				r.charge(owner, size)
				r.Report.Copy("files")
				return nil
			})
	})
}

// copyFileVersions moves the superseded contents of a path. A version and the
// file that superseded it may name one Drive key, and objectID keeps them on
// one object id so the reference union of spec 004 still holds.
func copyFileVersions(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT id, owner_type, owner_id, path, version_no, content_type, size_bytes,
			        checksum, storage_key, created_by, superseded_at
			   FROM file_versions ORDER BY id`,
			func(rows Rows) error {
				var id, ownerType, ownerID, path, contentType, checksum, storageKey, createdBy string
				var versionNo int32
				var size int64
				var supersededAt time.Time
				if err := rows.Scan(&id, &ownerType, &ownerID, &path, &versionNo, &contentType,
					&size, &checksum, &storageKey, &createdBy, &supersededAt); err != nil {
					return err
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				path, err = r.path("file_versions", path)
				if err != nil {
					return err
				}
				if err := write(
					`INSERT INTO file_versions (id, owner, path, version_no, object_id, content_type,
					                            size_bytes, checksum, checksum_kind, created_by, superseded_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
					id, owner, path, versionNo, string(r.objectID(storageKey)), contentType,
					size, checksum, ChecksumKind(checksum), r.Rewriter.Principal(createdBy),
					supersededAt); err != nil {
					return err
				}
				r.charge(owner, size)
				r.Report.Copy("file_versions")
				return nil
			})
	})
}

// copyStars moves the bookmarks. A star belongs to a subject and not to the
// file, which both schemas already say.
func copyStars(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT principal_id, owner_type, owner_id, path, created_at
			   FROM stars ORDER BY principal_id, owner_type, owner_id, path`,
			func(rows Rows) error {
				var principal, ownerType, ownerID, path string
				var createdAt time.Time
				if err := rows.Scan(&principal, &ownerType, &ownerID, &path, &createdAt); err != nil {
					return err
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				path, err = r.path("stars", path)
				if err != nil {
					return err
				}
				if err := write(
					`INSERT INTO stars (subject, owner, path, created_at) VALUES ($1,$2,$3,$4)`,
					r.Rewriter.Principal(principal), owner, path, createdAt); err != nil {
					return err
				}
				r.Report.Copy("stars")
				return nil
			})
	})
}

// copyUploadSessions moves the open multipart uploads, if the source still
// holds any. Drive is read-only for the length of the cutover, so a session
// here is one that was open when the flag went on; its row arrives and its
// parts do not, which is the bytes finding the report carries.
func copyUploadSessions(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT id, owner_type, owner_id, path, declared_size, content_type,
			        storage_key, s3_upload_id, created_by, created_at
			   FROM upload_sessions ORDER BY id`,
			func(rows Rows) error {
				var id, ownerType, ownerID, path, contentType, storageKey, uploadID, createdBy string
				var declared int64
				var createdAt time.Time
				if err := rows.Scan(&id, &ownerType, &ownerID, &path, &declared, &contentType,
					&storageKey, &uploadID, &createdBy, &createdAt); err != nil {
					return err
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				path, err = r.path("upload_sessions", path)
				if err != nil {
					return err
				}
				if err := write(
					`INSERT INTO upload_sessions (id, owner, path, object_id, upload_id, declared_size,
					                              content_type, created_by, created_at, expires_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
					id, owner, path, string(r.objectID(storageKey)), uploadID, declared,
					contentType, r.Rewriter.Principal(createdBy), createdAt,
					createdAt.Add(SessionLifetime)); err != nil {
					return err
				}
				r.Report.Copy("upload_sessions")
				r.Report.Note("upload_sessions", NoteNoObjectYet)
				return nil
			})
	})
}

// SessionLifetime is what Arca's migration 0002 gives a session: created_at
// plus twenty-four hours, which Drive applied as a sweep and never stored.
const SessionLifetime = 24 * time.Hour

// copyShares moves the grants. The classification of grants.go decides which
// arrive, and every row that does not is counted under the name spec 019
// removes it by.
func copyShares(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT id, owner_type, owner_id, path_prefix, grantee_type, grantee_id,
			        permission, token, status, created_by, expires_at, created_at
			   FROM shares ORDER BY id`,
			func(rows Rows) error {
				var id, ownerType, ownerID, prefix, granteeType, permission, status, createdBy string
				var granteeID, token *string
				var expiresAt *time.Time
				var createdAt time.Time
				if err := rows.Scan(&id, &ownerType, &ownerID, &prefix, &granteeType, &granteeID,
					&permission, &token, &status, &createdBy, &expiresAt, &createdAt); err != nil {
					return err
				}
				grant, dropped, err := r.Rewriter.Grant(DriveShare{
					GranteeType: granteeType,
					GranteeID:   deref(granteeID),
					Permission:  permission,
					Token:       deref(token),
					Status:      status,
				})
				if err != nil {
					return err
				}
				if dropped != "" {
					r.Report.Drop("shares", dropped)
					return nil
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				prefix, err = r.path("shares", prefix)
				if err != nil {
					return err
				}
				if err := write(
					`INSERT INTO shares (id, owner, path_prefix, grantee_kind, grantee, permission,
					                     token, status, created_by, expires_at, created_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
					id, owner, prefix, grant.Kind, nullable(grant.Grantee), permission,
					nullable(grant.Token), grant.Status, r.Rewriter.Principal(createdBy),
					expiresAt, createdAt); err != nil {
					return err
				}
				if grant.MintedToken {
					r.Report.Note("shares", "tokens minted for a grant that carried none")
				}
				if grant.ClearedToken {
					r.Report.Note("shares", "invite tokens cleared on a subject grant")
				}
				r.Report.Copy("shares")
				return nil
			})
	})
}

// copyWorkspaces moves the workspaces and drops the kind. A checked-out
// repository is a workspace like any other, so its slug keeps its name and
// the paths under it move plane, which Path does.
func copyWorkspaces(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT id, owner_type, owner_id, kind, slug, created_by, writer_sandbox_id,
			        writer_expires_at, last_sync, created_at, updated_at, deleted_at
			   FROM workspaces ORDER BY id`,
			func(rows Rows) error {
				var id, ownerType, ownerID, kind, slug, createdBy string
				var holder *string
				var writerExpires, lastSync, deletedAt *time.Time
				var createdAt, updatedAt time.Time
				if err := rows.Scan(&id, &ownerType, &ownerID, &kind, &slug, &createdBy, &holder,
					&writerExpires, &lastSync, &createdAt, &updatedAt, &deletedAt); err != nil {
					return err
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				if err := write(
					`INSERT INTO workspaces (id, owner, slug, created_by, writer_holder,
					                         writer_expires_at, last_sync, created_at, updated_at, deleted_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
					id, owner, slug, r.Rewriter.Principal(createdBy), holder,
					writerExpires, lastSync, createdAt, updatedAt, deletedAt); err != nil {
					return err
				}
				if kind == "repo" {
					r.Report.Note("workspaces", "repositories that became workspaces")
				}
				r.Report.Copy("workspaces")
				return nil
			})
	})
}

// copyAttachments moves the mounts. The manifest is copied as it is: both
// schemas hold paths relative to the workspace root, so moving the plane of
// the workspace does not reach inside it.
func copyAttachments(ctx context.Context, r *Run) error {
	return r.table(ctx, func(write insert) error {
		return each(ctx, r.Source,
			`SELECT id, workspace_id, sandbox_id, principal_id, mode, status, manifest,
			        expires_at, created_at, released_at
			   FROM workspace_attachments ORDER BY id`,
			func(rows Rows) error {
				var id, workspaceID, sandbox, principal, mode, status string
				var manifest []byte
				var expiresAt, createdAt time.Time
				var releasedAt *time.Time
				if err := rows.Scan(&id, &workspaceID, &sandbox, &principal, &mode, &status,
					&manifest, &expiresAt, &createdAt, &releasedAt); err != nil {
					return err
				}
				if err := write(
					`INSERT INTO workspace_attachments (id, workspace_id, holder, subject, mode,
					                                    status, manifest, expires_at, created_at, released_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
					id, workspaceID, sandbox, r.Rewriter.Principal(principal), mode, status,
					manifest, expiresAt, createdAt, releasedAt); err != nil {
					return err
				}
				r.Report.Copy("workspace_attachments")
				return nil
			})
	})
}

// copySpaceUsage writes the ledger. Drive had no such table: it summed the
// rows on every admission. The sum written here is the one events.Recompute
// reads, files plus file_versions, so the reaper's reconciliation pass finds
// nothing to correct the first time it runs after the cutover.
func copySpaceUsage(ctx context.Context, r *Run) error {
	owners := make([]string, 0, len(r.usage))
	for owner := range r.usage {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return r.table(ctx, func(write insert) error {
		for _, owner := range owners {
			if err := write(
				`INSERT INTO space_usage (owner, bytes) VALUES ($1, $2)`,
				owner, r.usage[owner]); err != nil {
				return err
			}
			r.Report.Copy("space_usage")
		}
		return nil
	})
}

// copyEvents moves the log as it is and keeps every id, so a consumer holding
// a cursor holds it still. The sequence is set past the last id afterwards,
// because a copy that writes ids explicitly leaves the sequence where a fresh
// database left it and the first append would collide.
//
// The action is copied verbatim. Spec 019 calls Arca's vocabulary a superset
// of Drive's; internal/events names eleven actions where Drive's constraint
// named thirteen, so the two Drive kept that Arca does not are counted here
// rather than rewritten, which is a decision for the maintainer and not for a
// copy.
func copyEvents(ctx context.Context, r *Run) error {
	known := map[string]bool{}
	for _, a := range events.Actions() {
		known[string(a)] = true
	}
	return r.table(ctx, func(write insert) error {
		err := each(ctx, r.Source,
			`SELECT id, owner_type, owner_id, path, action, actor_id, detail, created_at
			   FROM events ORDER BY id`,
			func(rows Rows) error {
				var id int64
				var ownerType, ownerID, action string
				var path, actor *string
				var detail []byte
				var createdAt time.Time
				if err := rows.Scan(&id, &ownerType, &ownerID, &path, &action, &actor,
					&detail, &createdAt); err != nil {
					return err
				}
				owner, err := r.Rewriter.Owner(ownerType, ownerID)
				if err != nil {
					return err
				}
				var eventPath *string
				if path != nil {
					moved, err := r.path("events", *path)
					if err != nil {
						return err
					}
					eventPath = &moved
				}
				var eventActor *string
				if actor != nil {
					moved := r.Rewriter.Principal(*actor)
					eventActor = &moved
				}
				if err := write(
					`INSERT INTO events (id, owner, path, action, actor, detail, created_at)
					 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
					id, owner, eventPath, action, eventActor, detail, createdAt); err != nil {
					return err
				}
				if !known[action] {
					r.Report.Note("events", "actions outside Arca's vocabulary")
				}
				r.Report.Copy("events")
				return nil
			})
		if err != nil {
			return err
		}
		return write(
			`SELECT setval(pg_get_serial_sequence('events', 'id'),
			               GREATEST(COALESCE((SELECT MAX(id) FROM events), 0), 1),
			               COALESCE((SELECT MAX(id) FROM events), 0) > 0)`)
	})
}

// deref reads a nullable text column as a string, where the empty string and
// NULL mean the same to the caller.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nullable writes the empty string as NULL, which is what the shape
// constraints of migration 0003 compare against.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
