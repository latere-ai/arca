// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package admin is spec 012: the two routes under /v1/admin that answer
// across spaces rather than for one.
//
// Administration is a capability and not a claim. This package knows no
// administrator: every route asks space.admin, and the answer comes from the
// operator's authorizer or, with none configured, from the owner policy of
// spec 006 reading ARCA_ADMIN_SUBJECTS. An installation with neither has no
// administrator, and both routes answer 403 to everyone, which is the
// correct default for a self-hosted installation that needs none.
//
// A deny is 403 and not the 404 the service Arca replaces answered to hide
// the surface. The route names are in /openapi.json anyway, and existence
// hiding protects objects rather than route names (spec 013).
//
// Everything else an administrator does is an ordinary route of another
// spec, answered on a space the caller does not own. There is no
// administrative copy of a listing and no moderation route: reading someone
// else's objects is GET /v1/files/{owner}/{path...}?list=1, and a moderation
// delete is DELETE /v1/files/{owner}/{path...}. A second surface answering
// the same questions from a second set of handlers would double every filter
// rule and every pagination bug.
//
// Two seams keep this package to its own spec. [Restorer] is the restore
// across owners, which reaches the trash of spec 005 and the soft deleted
// workspaces of spec 009; [Links] is the token grant count of spec 008.
// Neither table is this package's, and an installation that has bound
// neither says so on the wire rather than guessing.
package admin

import (
	"context"
	"errors"
	"net/http"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// OwnerAlias is the spelling of the caller's own space, so a client that has
// not read its own subject back still addresses it. It is spec 009's alias,
// spelled the same here because one client reads both surfaces.
const OwnerAlias = "me"

// The two things a restore can bring back. The answer says which it was,
// because the route takes an id rather than a path and one id names either.
const (
	// KindFile is a trashed object returned to its path.
	KindFile = "file"
	// KindWorkspace is a soft deleted workspace returned to its slug.
	KindWorkspace = "workspace"
)

// ErrNotRestorable is an id that names nothing the space can bring back: a
// typo, or an id the reaper has already purged. The two are one answer on
// the wire, and the developer detail names the window so an administrator is
// not left guessing between them.
var ErrNotRestorable = errors.New("admin: no trashed object and no deleted workspace carries that id")

// Restored is what a restore brought back.
type Restored struct {
	// ID is the id the request named.
	ID string
	// Kind is [KindFile] or [KindWorkspace].
	Kind string
}

// Restorer returns one deleted object or workspace to a space. It restores
// across owners, which is the whole reason it exists beside the owner's own
// restore of specs 005 and 009, and it takes an id rather than a path so
// that one route returns either.
//
// It is a seam because what it undoes belongs to those two specs: the
// trashed row and the soft deleted workspace are theirs, and the restore
// touches the database only, per invariant 1 of spec 001. An installation
// whose build binds none registers the route at its right place and answers
// not_implemented, which is what spec 013 reserves that code for.
type Restorer interface {
	// Restore returns the object or the workspace the id names to the space,
	// answering which it was. An id that names nothing still restorable is
	// [ErrNotRestorable].
	Restore(ctx context.Context, owner, id string) (Restored, error)
}

// Links counts the token grants of spec 008 a space still holds, which is
// the one counter of the overview that is not in this build's schema: the
// grants and the links share one table, and that table arrives with that
// spec.
//
// A build that binds none counts none, and none is the true count: the three
// link routes of spec 013 answer not_implemented until that spec lands, so
// an installation on this build has issued no link to count.
type Links interface {
	// Counts answers the live link count of each space named, leaving out a
	// space that holds none. It takes the page's owners together rather than
	// one space at a time, so a page of a hundred spaces is one query.
	Counts(ctx context.Context, q store.Querier, owners []string) (map[string]int64, error)
}

// noLinks is the counter of a build that has bound none.
type noLinks struct{}

// Counts answers no link for any space.
func (noLinks) Counts(context.Context, store.Querier, []string) (map[string]int64, error) {
	return nil, nil
}

// Options is what the node hands this package.
type Options struct {
	// Querier is the database the overview reads. The overview is one
	// statement outside any transaction, so this package takes the pool
	// rather than the transaction seam its siblings take.
	Querier store.Querier
	// Spaces is the overview query of spec 012.
	Spaces store.Admin
	// Authorizer is the seam every handler decides through.
	Authorizer *auth.Authorizer
	// Links is the token grant counter. Nil counts none.
	Links Links
	// Restorer is the restore across owners. Nil answers not_implemented.
	Restorer Restorer
}

// Service answers the administrative routes. It is built once at start and
// serves every replica's requests; it holds no state of its own.
type Service struct {
	querier    store.Querier
	spaces     store.Admin
	authorizer *auth.Authorizer
	links      Links
	restorer   Restorer
}

// New builds the service. It refuses to build without a seam it would
// otherwise reach through a nil pointer on the first request, because each
// of those is wiring the node settles at start and a surface missing one
// would answer 500 to a route that looks registered. The two seams a build
// may legitimately lack are not among them: an unbound restorer answers
// not_implemented and an unbound link counter counts none.
func New(o Options) (*Service, error) {
	for _, missing := range []struct {
		absent bool
		what   string
	}{
		{o.Querier == nil, "no database"},
		{o.Spaces == nil, "no overview query"},
		{o.Authorizer == nil, "no authorizer, and every route asks before it acts"},
	} {
		if missing.absent {
			return nil, errors.New("admin: " + missing.what)
		}
	}
	s := &Service{
		querier: o.Querier, spaces: o.Spaces, authorizer: o.Authorizer,
		links: o.Links, restorer: o.Restorer,
	}
	if s.links == nil {
		s.links = noLinks{}
	}
	return s, nil
}

// space reads the space a request names: the subject in the path, or the
// caller's own where the path carries the alias. A response renders a
// subject in full and never an alias, so what a client stores is what it can
// send back.
func space(r *http.Request, given string) (string, error) {
	if given != "" && given != OwnerAlias {
		return given, nil
	}
	caller := auth.CallerFrom(r.Context()).Subject
	if caller == "" {
		return "", api.Refuse(api.CodeMissingField,
			"the request names no space and the caller has no subject to stand in for one").About("owner")
	}
	return caller, nil
}
