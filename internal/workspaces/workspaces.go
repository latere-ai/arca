// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package workspaces is spec 009: a durable subtree of a space that a
// sandbox attaches to at the start of a run and writes back at the end, so
// tomorrow's sandbox starts where yesterday's stopped.
//
// The storage model is copy in, sync back, with snapshot reads. Arca never
// mounts anything and never sees a file system: it hands out a manifest with
// presigned URLs and accepts a manifest back, which is what makes a workspace
// work for a runtime Arca does not control.
//
// One property makes the model safe to build on, and this package is where
// it is enforced: at most one writer at a time. A rw attach is a conditional
// update on the workspace row in the same transaction as the attachment
// insert, so a second one matches no row and is a conflict. The lease is time
// bounded, because a sandbox that crashes cannot release.
//
// A checked-out repository is a workspace like any other. There is no second
// kind of workspace and no column that would name one: a repository's history
// lives on a git host, and what Arca holds is a working tree.
//
// Three seams keep this package to its own spec. [Objects] is the file plane
// of spec 005, which owns what a put under a workspace root does; [Ledger] is
// the log and the usage counter of spec 010, whose default here writes
// nothing; and [Database] is the transaction, so a test drives every handler
// with no Postgres behind it.
package workspaces

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The bounds of the writer lease. They are constants and not configuration:
// a client asks for a TTL in seconds and gets the smaller of its ask and the
// ceiling, so no installation can hand out a lease long enough to wedge a
// workspace for a day and no client can ask for one.
const (
	// DefaultTTL is the lease a client that asks for none gets.
	DefaultTTL = time.Hour
	// MaxTTL is the ceiling on what a client may ask for.
	MaxTTL = 24 * time.Hour
)

// OwnerAlias is the spelling of the caller's own space, so a client that has
// not read its own subject back still addresses it.
const OwnerAlias = "me"

// slugPattern is the name a workspace carries inside its space. It is the
// one thing a path under the root derives from, so it holds no separator, no
// case, and nothing a shell or a URL would have to escape.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Root is the subtree of the workspaces plane a workspace owns. It derives
// from the slug and is never stored, which is what makes a rename a row
// update: the bucket key derives from the object id and not from the path
// (spec 001, invariant 8).
func Root(slug string) string { return object.PlaneWorkspaces.Prefix() + slug + "/" }

// Database is the transaction a handler runs in. *store.DB satisfies it, and
// a test passes its own, so the unit tier drives every handler without
// Postgres and the store tier proves what the SQL means.
type Database interface {
	// Querier answers the pool, for a read outside a transaction.
	Querier() store.Querier
	// Tx runs fn in one transaction, committing when it returns nil.
	Tx(ctx context.Context, fn func(store.Querier) error) error
}

// Objects is what a workspace needs of the file plane of spec 005. Files
// under a workspace root are ordinary objects: they are listed, trashed,
// charged and shared by exactly the code paths that serve every other
// object, and this is the narrow half spec 009 reads and reconciles as a
// subtree rather than as a path.
//
// It is an interface so the owner of the file plane binds its own
// implementation without this package importing it.
type Objects interface {
	// Manifest lists the live objects under a root prefix, ordered by path,
	// each with the object id whose key materialize presigns.
	Manifest(ctx context.Context, q store.Querier, owner, prefix string) ([]store.WorkspaceFile, error)
	// Stat counts the live objects under a root prefix and their bytes.
	Stat(ctx context.Context, q store.Querier, owner, prefix string) (files, bytes int64, err error)
	// MoveSubtree rewrites every path under one prefix to sit under
	// another and answers how many moved. It reaches no bucket key.
	MoveSubtree(ctx context.Context, q store.Querier, owner, from, to string) (int64, error)
	// Drop removes the named live rows and answers the objects they held
	// and the bytes they counted.
	Drop(ctx context.Context, q store.Querier, owner string, paths []string) (freed []object.ID, bytes int64, err error)
	// Unreferenced narrows a set of objects to the ones no row still names,
	// which is the one question deciding whether bytes may be deleted.
	Unreferenced(ctx context.Context, q store.Querier, ids []object.ID) ([]object.ID, error)
}

// Event is one row of the log of spec 010 as this package appends it.
type Event struct {
	// Owner is the space the mutation happened in.
	Owner string
	// Path is the workspace's root prefix, so a consumer tailing a subtree
	// sees the attach and the sync beside the puts.
	Path string
	// Action is one word of spec 010's closed vocabulary: attach, release,
	// sync, reap, or restore.
	Action string
	// Actor is the subject that caused it.
	Actor string
	// Detail is what the row carries beside the action.
	Detail map[string]any
}

// The actions of spec 010's vocabulary this package appends.
const (
	// ActionAttach is a ro or a rw attach.
	ActionAttach = "attach"
	// ActionRelease is an attachment given back.
	ActionRelease = "release"
	// ActionSync is a completed write-back.
	ActionSync = "sync"
	// ActionReap is a lease or an attachment whose time ran out.
	ActionReap = "reap"
	// ActionRestore is a soft deleted workspace brought back.
	ActionRestore = "restore"
)

// Ledger is the log and the usage counter of spec 010. Both calls run inside
// the caller's transaction, so a mutation and the row recording it are
// written together or not at all.
//
// Create, rename and delete of the record itself are not appended: the enum
// of spec 010 is closed, and a consumer that wants workspace lifecycle reads
// the put and delete events on the root prefix.
type Ledger interface {
	// Append writes one row of the log.
	Append(ctx context.Context, q store.Querier, e Event) error
	// Release gives bytes back to a space's usage, which is what a sync
	// that dropped rows did.
	Release(ctx context.Context, q store.Querier, owner string, bytes int64) error
}

// silent is the ledger of an installation whose log has not landed. It
// writes nothing, so every handler here calls the seam unconditionally and
// the binding of spec 010 is one field rather than a branch per call site.
type silent struct{}

func (silent) Append(context.Context, store.Querier, Event) error { return nil }

func (silent) Release(context.Context, store.Querier, string, int64) error { return nil }

// Options is what the node hands this package.
type Options struct {
	// DB is the transaction seam.
	DB Database
	// Workspaces and Attachments are the query sets of spec 009.
	Workspaces  store.Workspaces
	Attachments store.Attachments
	// Objects is the file plane of spec 005.
	Objects Objects
	// Bucket presigns the reads a materialize hands out and removes the
	// bytes a sync dropped.
	Bucket blob.Store
	// Prefix is ARCA_BUCKET_PREFIX, what every key carries.
	Prefix string
	// Authorizer is the seam every handler decides through.
	Authorizer *auth.Authorizer
	// Ledger is the log and the usage counter. Nil writes nothing.
	Ledger Ledger
	// Now is the clock the lease is measured on. time.Now when nil.
	Now func() time.Time
}

// Service answers the workspace routes. It is built once at start and serves
// every replica's requests; it holds no state of its own.
type Service struct {
	db          Database
	workspaces  store.Workspaces
	attachments store.Attachments
	objects     Objects
	bucket      blob.Store
	prefix      string
	authorizer  *auth.Authorizer
	ledger      Ledger
	clock       func() time.Time
}

// New builds the service. It refuses to build without a seam it would
// otherwise reach through a nil pointer on the first request: every one of
// these is wiring the node settles at start, and a surface missing one would
// answer 500 to a route that looks registered.
func New(o Options) (*Service, error) {
	for _, missing := range []struct {
		absent bool
		what   string
	}{
		{o.DB == nil, "no database"},
		{o.Workspaces == nil, "no workspace queries"},
		{o.Attachments == nil, "no attachment queries"},
		{o.Objects == nil, "no file plane"},
		{o.Bucket == nil, "no bucket"},
		{o.Authorizer == nil, "no authorizer, and every handler asks before it acts"},
	} {
		if missing.absent {
			return nil, &missingSeam{what: missing.what}
		}
	}
	s := &Service{
		db: o.DB, workspaces: o.Workspaces, attachments: o.Attachments,
		objects: o.Objects, bucket: o.Bucket, prefix: o.Prefix,
		authorizer: o.Authorizer, ledger: o.Ledger, clock: o.Now,
	}
	if s.ledger == nil {
		s.ledger = silent{}
	}
	return s, nil
}

// missingSeam is a service the node did not finish wiring.
type missingSeam struct{ what string }

func (e *missingSeam) Error() string { return "workspaces: " + e.what }

// now is the clock the lease is measured on.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// resource renders a workspace as the authorizer question carries it. The
// fields are spec 006's: the id, the space, and the slug, and nothing about
// the caller.
func resource(w store.Workspace) authorizer.Workspace {
	return authorizer.Workspace{ID: w.ID, Owner: w.Owner, Slug: w.Slug}
}

// space reads the space a request names: the owner parameter, the alias for
// the caller's own, or the caller's own when the parameter is absent. A
// response renders a subject in full and never an alias, so what a client
// stores is what it can send back.
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

// checkSlug holds a slug to the one shape a root prefix derives from.
func checkSlug(slug string) error {
	if slug == "" {
		return api.Refuse(api.CodeMissingField, "a workspace is created with a slug").About("slug")
	}
	if !slugPattern.MatchString(slug) {
		return api.Refuse(api.CodeInvalidField,
			"the slug %q is not %s", slug, slugPattern.String()).About("slug")
	}
	return nil
}

// lease renders a workspace's writer lease, and nothing for a workspace
// whose lease is free or lapsed. A lease past its deadline is not held: the
// next attach takes it, so reporting it as held would be reporting a state
// the next request disagrees with.
func (s *Service) lease(w store.Workspace) *Lease {
	if w.WriterHolder == nil || w.WriterExpiresAt == nil || !w.WriterExpiresAt.After(s.now()) {
		return nil
	}
	return &Lease{Holder: *w.WriterHolder, Mode: string(store.ModeWrite), ExpiresAt: w.WriterExpiresAt.UTC()}
}

// held reports whether a writer still holds the workspace, which is what a
// rename and a delete are refused against.
func (s *Service) held(w store.Workspace) bool { return s.lease(w) != nil }

// ttl is the lease a client asked for, bounded. A client asks in seconds and
// gets the smaller of its ask and the ceiling; an ask of nothing is the
// default, and an ask below zero is a field it cannot take.
func ttl(seconds int) (time.Duration, error) {
	switch {
	case seconds == 0:
		return DefaultTTL, nil
	case seconds < 0:
		return 0, api.Refuse(api.CodeInvalidField,
			"ttl_seconds is %d; a lease lasts a positive number of seconds", seconds).About("ttl_seconds")
	default:
		return min(time.Duration(seconds)*time.Second, MaxTTL), nil
	}
}
