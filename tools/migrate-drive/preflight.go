// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"strings"
)

// Refusal is every reason a run stopped before it wrote anything, answered at
// once. An operator who has to discover the next unmapped organization on the
// next attempt reruns the copy as many times as the platform has
// organizations, so the preflight collects them all and names them together.
type Refusal struct{ Reasons []string }

func (r *Refusal) Error() string {
	return "migrate-drive: the copy is refused and nothing was written\n  " +
		strings.Join(r.Reasons, "\n  ")
}

// Refuse records one reason.
func (r *Refusal) Refuse(format string, args ...any) {
	r.Reasons = append(r.Reasons, fmt.Sprintf(format, args...))
}

// Preflight reads the source for everything the copy cannot decide for
// itself. It writes nothing, and the run does not begin until it holds.
//
// Four questions, each of which would otherwise fail part way through a table
// and leave the operator to read a driver's error:
//
//  1. Is every organization in the source mapped to a subject? Only the
//     platform's export knows what subject its identity provider assigns.
//  2. Is every path in a plane spec 019 gives a rule for? Arca has two
//     planes, and a row outside them is a row no route can reach.
//  3. Do two workspaces collide once the kind is dropped? Drive's slug was
//     unique per kind and Arca's is unique per space.
//  4. Does every key lie under the prefix the operator named? A key outside it
//     means the source is not the installation the operator thinks it is.
//  5. Is the target empty? The copy writes the ids the source chose, so a
//     second run over a database that kept the first one's rows is a conflict
//     on every primary key and never an update.
//
// It also mints the object ids, in Objects. That is not a question but the
// same rule read the other way: the manifest of spec 019 is decided before a
// row is written, so every id the copy hands out is an id the manifest
// already names.
func Preflight(ctx context.Context, r *Run) error {
	refusal := &Refusal{}
	if err := preflightTarget(ctx, r, refusal); err != nil {
		return err
	}
	if err := preflightOrgs(ctx, r, refusal); err != nil {
		return err
	}
	if err := preflightPlanes(ctx, r, refusal); err != nil {
		return err
	}
	if err := preflightSlugs(ctx, r, refusal); err != nil {
		return err
	}
	if err := preflightKeys(ctx, r, refusal); err != nil {
		return err
	}
	if len(refusal.Reasons) > 0 {
		return refusal
	}
	return Objects(ctx, r)
}

// preflightTarget refuses a target that already holds rows.
//
// This is what makes the tool idempotent in the only way a row copy can be:
// running it twice writes what one run wrote, because the second run does not
// begin. Arca owns a fresh database on the cutover (decision 1 of spec 019),
// and a rerun is a fresh one, so a target that survived a first attempt is
// dropped and migrated again rather than added to.
//
// A dry run reads a target however full it is: it writes nothing, and an
// operator rehearsing the cutover against a populated database is reading the
// source's arithmetic and not the target's.
func preflightTarget(ctx context.Context, r *Run, refusal *Refusal) error {
	if r.DryRun {
		return nil
	}
	for _, name := range TableNames() {
		rows, err := count(ctx, r.Target, name)
		if err != nil {
			return err
		}
		if rows > 0 {
			refusal.Refuse("the target already holds %d rows in %s; the copy writes into an empty database, "+
				"so drop it, apply the migrations again, and rerun", rows, name)
		}
	}
	return nil
}

// preflightOrgs reads every organization the copy will write an owner or a
// grantee for, and refuses the ones no mapping names.
//
// A grantee is read only where the grant arrives: a grant to an organization
// that is already dropped for its status needs no subject, and demanding one
// would refuse a copy over a row nobody will see again.
func preflightOrgs(ctx context.Context, r *Run, refusal *Refusal) error {
	return each(ctx, r.Source, `
		SELECT DISTINCT owner_id FROM (
			SELECT owner_type, owner_id::text AS owner_id FROM files
			UNION SELECT owner_type, owner_id::text FROM file_versions
			UNION SELECT owner_type, owner_id::text FROM stars
			UNION SELECT owner_type, owner_id::text FROM upload_sessions
			UNION SELECT owner_type, owner_id::text FROM shares
			UNION SELECT owner_type, owner_id::text FROM workspaces
			UNION SELECT owner_type, owner_id::text FROM events
			UNION SELECT grantee_type, grantee_id::text FROM shares
			       WHERE grantee_type = 'org' AND status IN ('active', 'revoked')
		) o WHERE owner_type = 'org' ORDER BY owner_id`,
		func(rows Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			if _, err := r.Rewriter.Subject("o-" + id); err != nil {
				refusal.Refuse("the organization %s is in the source and in no mapping; add it to -org-subjects, "+
					"or give -org-issuer where your platform derives an organization's subject from its id", id)
			}
			return nil
		})
}

// preflightPlanes reads the leading segment of every path the copy will
// rewrite and refuses one spec 019 gives no rule for.
func preflightPlanes(ctx context.Context, r *Run, refusal *Refusal) error {
	return each(ctx, r.Source, `
		SELECT DISTINCT split_part(path, '/', 1) AS plane FROM (
			SELECT path FROM files
			UNION SELECT path FROM file_versions
			UNION SELECT path FROM stars
			UNION SELECT path FROM upload_sessions
			UNION SELECT path_prefix FROM shares
			UNION SELECT path FROM events WHERE path IS NOT NULL
		) p ORDER BY plane`,
		func(rows Rows) error {
			var plane string
			if err := rows.Scan(&plane); err != nil {
				return err
			}
			if !PlaneKnown(plane) {
				refusal.Refuse("the source holds paths in the plane %q, and spec 019 gives no rule for it; "+
					"Arca has files/ and workspaces/, and which of them these rows belong in is a decision for the maintainer", plane)
			}
			return nil
		})
}

// preflightSlugs reads the workspaces that would collide once the kind is
// dropped. Drive keyed on (owner, kind, slug) and Arca keys on (owner, slug),
// so a workspace and a repository of one name in one space are two rows the
// target cannot hold.
func preflightSlugs(ctx context.Context, r *Run, refusal *Refusal) error {
	return each(ctx, r.Source, `
		SELECT owner_type, owner_id::text, slug FROM workspaces
		 GROUP BY owner_type, owner_id, slug HAVING count(*) > 1
		 ORDER BY owner_type, owner_id, slug`,
		func(rows Rows) error {
			var ownerType, ownerID, slug string
			if err := rows.Scan(&ownerType, &ownerID, &slug); err != nil {
				return err
			}
			address, err := Address(ownerType, ownerID)
			if err != nil {
				return err
			}
			refusal.Refuse("the space %s holds more than one workspace named %q, which collide once the kind is dropped; "+
				"rename one in the source before the copy", address, slug)
			return nil
		})
}

// preflightKeys counts the keys outside the prefix. Given the bytes finding
// the prefix carries no bytes over on its own, and this check is what says the
// source is the installation the operator named rather than another.
func preflightKeys(ctx context.Context, r *Run, refusal *Refusal) error {
	return each(ctx, r.Source, `
		SELECT count(*) FROM (
			SELECT storage_key FROM files
			UNION ALL SELECT storage_key FROM file_versions
			UNION ALL SELECT storage_key FROM upload_sessions
		) k WHERE strpos(storage_key, $1) <> 1`,
		func(rows Rows) error {
			var outside int64
			if err := rows.Scan(&outside); err != nil {
				return err
			}
			if outside > 0 {
				refusal.Refuse("%d keys in the source lie outside the prefix %q; "+
					"check -prefix names the prefix this installation wrote", outside, r.Prefix)
			}
			return nil
		}, r.Prefix)
}
