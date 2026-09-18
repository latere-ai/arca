// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares

import (
	"context"
	"fmt"

	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// The two seams spec 006 left for this spec. The owner policy answers from
// Arca's own grants table, which is the one place this core reads its own
// state to decide: a grant is a resource of the core, not a claim about a
// person. A platform that wants grants decided elsewhere configures an
// authorizer, and the owner policy is not consulted at all.
//
// Both are read only and take no transaction. A table that cannot answer is
// no decision: a failure here reaches the seam as an error, which spec 006
// turns into authorizer_unavailable and never into a deny.

// Grants answers the grant step of spec 006's flowchart from the table of
// spec 008.
func Grants(db Database, queries store.Shares) auth.GrantLookup {
	return grants{db: db, queries: queries}
}

// Links answers the link step.
func Links(db Database, queries store.Shares) auth.LinkResolver {
	return links{db: db, queries: queries}
}

// grants is the grant step over the table.
type grants struct {
	db      Database
	queries store.Shares
}

// Permission answers the highest live permission the subject holds on a
// prefix of path in owner's space.
//
// Only a subject grant answers here. A token grant is the link step's, one
// branch further down the flowchart, and reading one here would hand every
// signed-in caller whatever the public holds: the service Arca replaces did
// exactly that, and it is the one fork this port takes from the code it
// carries over.
func (g grants) Permission(ctx context.Context, owner, subject, path string) (auth.Permission, error) {
	if owner == "" || subject == "" || path == "" {
		return auth.PermissionNone, nil
	}
	covering, err := g.queries.Covering(ctx, g.db.Querier(), owner, path)
	if err != nil {
		return auth.PermissionNone, fmt.Errorf("the covering grants: %w", err)
	}
	// The page is ordered by the ladder, so the first grant for this subject
	// is the highest permission it holds.
	for _, held := range covering {
		if held.GranteeKind == store.GranteeSubject && held.Grantee == subject {
			return auth.Permission(held.Permission), nil
		}
	}
	return auth.PermissionNone, nil
}

// links is the link step over the table.
type links struct {
	db      Database
	queries store.Shares
}

// Live reports whether id names a live token grant on owner's space that
// covers path.
//
// What reaches here is a link a request already resolved from its token.
// This confirms that it is still live and still covers what is being read,
// which is what lets an operator turn public reading off by denying
// link.read: the answer is true for every live link, and the deny is the
// endpoint's to give.
func (l links) Live(ctx context.Context, id, owner, path string) (bool, error) {
	if id == "" || owner == "" {
		return false, nil
	}
	link, err := l.queries.Live(ctx, l.db.Querier(), id, owner)
	switch {
	case missing(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("the link: %w", err)
	}
	return path == "" || covers(link.PathPrefix, path), nil
}
