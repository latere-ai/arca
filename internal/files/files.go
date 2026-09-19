// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package files is the object plane of spec 005: a path in a space with
// bytes behind it, written, read, listed, moved, deleted, versioned,
// trashed, restored and starred.
//
// Two properties of spec 001 shape all of it. A bucket key derives from an
// object id and never from a path (invariant 8), so a move is one UPDATE and
// an overwrite leaves the superseded bytes addressable at their own key.
// Bytes stay off the hot path (invariant 4), so a read of a large object is
// a redirect and a write of one is not this package's at all, it is spec
// 007's.
//
// Every handler asks one question of spec 006's vocabulary before it acts,
// through [Service.Ask], and reads no claim for anything. A refusal on a
// space that is not the caller's own is answered as a missing object, so a
// request cannot be written to learn what somebody else owns.
//
// The order of the two stores is invariant 1's. A write puts the bytes and
// then the row, so a failure between them leaves bytes no row points at,
// which the handler deletes at once because the key is fresh and nothing
// else can reference it. A hard delete removes the row and then the bytes,
// so a failure between them leaves bytes the reaper finds and never a row
// whose object is gone.
package files

import (
	"context"
	"encoding/json"
	"fmt"

	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// Decider is the seam of spec 006 every handler decides through. It is
// satisfied by *auth.Authorizer, which is an operator's endpoint or the
// owner policy and this package cannot tell which.
//
// Decide is the caller's own action, refused with forbidden. Lookup is a
// reference the request named, refused with not_found, which is invariant 6
// of spec 001: a refused object and a missing one are one answer.
type Decider interface {
	Decide(ctx context.Context, action string, res authz.Resource) (auth.Decision, error)
	Lookup(ctx context.Context, action string, res authz.Resource) (auth.Decision, error)
}

// Ledger is the usage ledger and the event log of spec 010, as the write
// paths of specs 005 and 007 need them. internal/events implements it and
// the node binds it; until then the no-op below counts nothing and records
// nothing, and every write is admitted.
//
// Charge and Release take a querier because a delta is applied inside the
// transaction that moves the bytes: the commit that records a file and the
// commit that records its bytes are one commit, and a ledger failure takes
// the write down with it rather than admitting a charge nobody recorded.
//
// Append is spec 010's best-effort append, which is its Log.Note and not its
// Log.Append: an ordinary mutation has already happened when its event is
// written, so a failed insert is a warning in the log and never a refusal to
// the caller. It answers nothing for that reason.
// Usage is the one read on this seam that answers no question about a write:
// what the space holds right now, which a root listing reports beside its
// page (spec 005). It is here rather than on a query set of its own because
// the bytes are the ledger's number and nobody else's.
type Ledger interface {
	Charge(ctx context.Context, q store.Querier, owner string, delta int64, limit Limit) (int64, error)
	Release(ctx context.Context, q store.Querier, owner string, bytes int64) (int64, error)
	Append(ctx context.Context, q store.Querier, e Event)
	Usage(ctx context.Context, q store.Querier, owner string) (Usage, error)
}

// Usage is what a space holds: the bytes the ledger of spec 010 counts and
// the live paths under them. It is the shape of the `space` object of a root
// listing, and a build whose ledger counts nothing answers zeroes.
type Usage struct {
	Bytes int64
	Files int64
}

// Event is one row of the log. Action is a member of spec 010's closed
// vocabulary, and this package appends four of the eleven: put, move,
// delete, restore.
type Event struct {
	Owner  string
	Path   string
	Action string
	Actor  string
	Detail map[string]any
}

// The four actions of spec 010's closed vocabulary the object plane
// appends. Spec 007's package appends EventPut too, because a session that
// completes is an object written.
const (
	EventPut     = "put"
	EventMove    = "move"
	EventDelete  = "delete"
	EventRestore = "restore"
)

// Metrics is where the object plane's half of spec 018's table goes. That
// spec owns the registry and the names; this is the seam it binds, so this
// package registers nothing and a replica exporting nothing still serves.
//
// Only the bytes this server carried are recorded here. A read above the
// inline size is answered with a presigned URL and the transfer is between
// the client and the bucket, which is invariant 4 of spec 001: what a
// redirect hands out is counted as a signed URL and never as bytes out.
type Metrics interface {
	// In records bytes accepted into a space, by spec 018's vocabulary.
	In(kind string, n int64)
	// Out records bytes served out of a space.
	Out(kind string, n int64)
	// LimitRejected records one write refused against the limit the
	// authorizer's answer carried.
	LimitRejected()
}

// uncounted is the seam of a node that bound none, so every call site is one
// line rather than a branch.
type uncounted struct{}

func (uncounted) In(string, int64) {}

func (uncounted) Out(string, int64) {}

func (uncounted) LimitRejected() {}

// References answers whether an object id is still named by a row of any
// table that holds one, which is the statement of spec 004 and the one thing
// every delete of bytes checks first. It is a seam so the unit tier can
// answer it without a database; the node binds the statement itself.
type References interface {
	Referenced(ctx context.Context, q store.Querier, id object.ID) (bool, error)
}

// Workspaces reports whether the workspace a path names is there and live,
// which spec 005 requires before anything under workspaces/ is read or
// written: bytes cannot exist under a workspace that was never created, and
// a soft-deleted one answers as missing to everyone.
//
// Spec 009 owns the table and implements this. Until it lands every
// workspace is live, which is the honest answer from a build that holds no
// workspace at all.
type Workspaces interface {
	Live(ctx context.Context, q store.Querier, owner, slug string) (bool, error)
}

// Limit is the byte allowance one authorizer answer carried, and nothing
// else. Arca stores no limit: there is no column, no route and no default,
// so a limit exists only as long as the answer that carried it.
type Limit struct {
	// Bytes is the allowance, meaningless when Set is false.
	Bytes int64
	// Set reports whether the answer carried one. The zero value is a space
	// with no limit at all, which is what an installation running the owner
	// policy gets.
	Set bool
}

// LimitOf reads limits.quota_bytes off an authorizer's answer. An answer
// with no limits object, or one naming no quota_bytes, leaves the space
// unlimited.
func LimitOf(d auth.Decision) (Limit, error) {
	if len(d.Limits) == 0 {
		return Limit{}, nil
	}
	var limits struct {
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if err := json.Unmarshal(d.Limits, &limits); err != nil {
		return Limit{}, fmt.Errorf("files: read limits.quota_bytes: %w", err)
	}
	if limits.QuotaBytes == nil {
		return Limit{}, nil
	}
	return Limit{Bytes: *limits.QuotaBytes, Set: true}, nil
}

// OverLimit is a charge the answer's limit does not admit. A handler answers
// it 413 quota_exceeded with the two figures in the developer detail.
type OverLimit struct {
	// Owner is the space, Used what it held before the charge, Limit what
	// the answer allowed, and Delta what the write would have added.
	Owner              string
	Used, Limit, Delta int64
}

func (e *OverLimit) Error() string {
	return fmt.Sprintf("the space holds %d bytes of the %d this answer allows, and the write adds %d",
		e.Used, e.Limit, e.Delta)
}

// Database is the metadata store as a handler needs it: a querier outside a
// transaction, and one transaction at a time. *store.DB satisfies it, and a
// test passes its own, which is what lets the unit tier of spec 014 run with
// no services at all.
type Database interface {
	Querier() store.Querier
	Tx(ctx context.Context, fn func(store.Querier) error) error
}

// Options is what the node hands this package and spec 007's.
type Options struct {
	// DB is the metadata store, and Bucket the object store.
	DB     Database
	Bucket blob.Store
	// Files, Versions and Stars are the query sets of spec 004. Nil takes
	// the ones over Postgres, which is every caller but a test.
	Files    store.Files
	Versions store.Versions
	Stars    store.Stars
	// Decide is the seam of spec 006. A surface built without one would act
	// where nobody decided, so New refuses it.
	Decide Decider
	// Ledger is the usage ledger and the log of spec 010. Nil counts
	// nothing and records nothing.
	Ledger Ledger
	// Metrics is spec 018's seam for the bytes this plane moves and the
	// writes a limit refused. Nil records nothing.
	Metrics Metrics
	// Workspaces is the liveness check of spec 009. Nil answers that every
	// workspace is live.
	Workspaces Workspaces
	// References is the reference check of spec 004. Nil takes the
	// statement over Postgres.
	References References
	// Config carries the three variables specs 005 and 007 read, the bucket
	// prefix of spec 003 and the CDN base a public object redirects to.
	Config config.Config
	// Now is the clock the trash window is measured against. time.Now when
	// nil.
	Now func() time.Time
}

// Service holds the file surface. It is built once at start and serves every
// replica's requests.
type Service struct {
	db         Database
	bucket     blob.Store
	files      store.Files
	versions   store.Versions
	stars      store.Stars
	decide     Decider
	ledger     Ledger
	workspaces Workspaces
	references References
	metrics    Metrics
	cfg        config.Config
	clock      func() time.Time
}

// New builds the file surface.
func New(o Options) *Service {
	s := &Service{
		db: o.DB, bucket: o.Bucket,
		files: o.Files, versions: o.Versions, stars: o.Stars,
		decide: o.Decide, ledger: o.Ledger, workspaces: o.Workspaces,
		references: o.References, cfg: o.Config, clock: o.Now,
		metrics: o.Metrics,
	}
	if s.references == nil {
		s.references = schemaReferences{}
	}
	if s.files == nil {
		s.files = store.NewFiles()
	}
	if s.versions == nil {
		s.versions = store.NewVersions()
	}
	if s.stars == nil {
		s.stars = store.NewStars()
	}
	if s.ledger == nil {
		s.ledger = noLedger{}
	}
	if s.workspaces == nil {
		s.workspaces = everyWorkspaceLive{}
	}
	if s.metrics == nil {
		s.metrics = uncounted{}
	}
	return s
}

// DB, Bucket, Config, Ledger and Files are what spec 007's package reaches,
// so a session's completion writes its row through the same code path a put
// does and the two cannot drift apart in their conditional writes, their
// version capture or their event.
func (s *Service) DB() Database           { return s.db }
func (s *Service) Bucket() blob.Store     { return s.bucket }
func (s *Service) Config() config.Config  { return s.cfg }
func (s *Service) Ledger() Ledger         { return s.ledger }
func (s *Service) Files() store.Files     { return s.files }
func (s *Service) Now() time.Time         { return s.now() }
func (s *Service) Prefix() string         { return s.cfg.BucketPrefix }
func (s *Service) Querier() store.Querier { return s.db.Querier() }

// now is the clock the trash window is read against, time.Now unless a test
// gave one.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// Ask puts one question of spec 006's vocabulary and renders a deny by whose
// space it was asked about.
//
// A deny on the caller's own space is forbidden: the caller can see the
// space, and hiding it from its owner would say nothing. A deny on any other
// space is not_found, byte for byte the answer a missing object gives, so a
// refusal never tells a caller that a space it may not see exists. That is
// invariant 6 of spec 001 and the rule spec 005 states as answering a
// refusal on a space the caller cannot see as a missing object.
func (s *Service) Ask(ctx context.Context, owner, action string, res authz.Resource) (auth.Decision, error) {
	if owner == auth.CallerFrom(ctx).Subject {
		return s.decide.Decide(ctx, action, res)
	}
	return s.decide.Lookup(ctx, action, res)
}

// Refused renders the deny of one question about a named object. A deny at
// lookup carries the whole answer an absence carries, developer detail
// included, so a caller reading every byte of the envelope cannot tell a
// refusal from a missing object: absent is the sentence the same handler
// writes when the path is not there, and repeating it here is what keeps
// the two one answer. Any other deny keeps the reason the authorizer gave,
// because the caller may see the object the question was about.
func (s *Service) Refused(err error, absent string, args ...any) error {
	if auth.CodeOf(err) == auth.CodeNotFound {
		return api.Refuse(api.CodeNotFound, absent, args...)
	}
	return api.FromAuth(err)
}

// noLedger counts nothing and records nothing, which is a build whose spec
// 010 has not landed. Every charge is admitted, because a core that is not
// counting has no number to refuse a write against.
type noLedger struct{}

func (noLedger) Charge(context.Context, store.Querier, string, int64, Limit) (int64, error) {
	return 0, nil
}

func (noLedger) Release(context.Context, store.Querier, string, int64) (int64, error) {
	return 0, nil
}

func (noLedger) Append(context.Context, store.Querier, Event) {}

func (noLedger) Usage(context.Context, store.Querier, string) (Usage, error) {
	return Usage{}, nil
}

// schemaReferences is the reference check over the schema, which reads every
// table that holds an object id.
type schemaReferences struct{}

func (schemaReferences) Referenced(ctx context.Context, q store.Querier, id object.ID) (bool, error) {
	return store.ObjectReferenced(ctx, q, id)
}

// everyWorkspaceLive is the answer of a build that holds no workspace table:
// nothing under workspaces/ is behind a tombstone, because nothing has one.
type everyWorkspaceLive struct{}

func (everyWorkspaceLive) Live(context.Context, store.Querier, string, string) (bool, error) {
	return true, nil
}
