// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package shares is spec 008: the grants a space hands out, the permission
// ladder they are read against, what a caller has been given, and the public
// links whose token is the whole of the authorization.
//
// A grant says that one grantee holds one permission on one subtree of one
// space. Grants are data, not decisions. This package stores them, lists
// them, revokes them, and offers them to the decision spec 006 makes; it
// never reads a claim to work out who a grantee is. An organization is a
// subject the authorizer names, exactly like a person or a service, so there
// is no group table, no membership cache, and no claim read for meaning
// anywhere below.
//
// Every route asks its action through the seam of internal/auth before it
// acts, with one exception that is the point of the exception: the three
// routes that redeem a token carry no bearer. They resolve the token first,
// answer not_found when it names nothing, and only then ask link.read with
// an anonymous subject, so an operator turns public reading off by denying
// that one action and every link in the database stops at once.
package shares

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// The three seams a service cannot be built without. Each is a wiring
// failure at start rather than a refusal at a request, so a deployment is
// fixed before it serves.
var (
	errNoAuthorizer = errors.New("shares: no authorizer, and every route asks before it acts")
	errNoDatabase   = errors.New("shares: no database, and a grant is a row")
	errNoStore      = errors.New("shares: no query set over the grants table")
)

// missing reports whether a read found no row, which every handler here
// tells from a fault: a transient failure is a 500 and never a 404
// (spec 001, invariant 2).
func missing(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// Database is what the handlers need of the store: a querier for a read, and
// a transaction for a write that also appends to the log. *store.DB
// satisfies it, and a test passes its own.
type Database interface {
	Querier() store.Querier
	Tx(ctx context.Context, fn func(store.Querier) error) error
}

// Event is one row of the log of spec 010 as a mutation here writes it. The
// field names and the append below are that spec's shape, so the node binds
// its log through one adapter and this package carries no dependency on it.
type Event struct {
	// Owner is the space the event happened to.
	Owner string
	// Path is what it happened to, which for a grant is the subtree it
	// covers.
	Path string
	// Action is the member of spec 010's closed vocabulary: share_created
	// or share_revoked.
	Action string
	// Actor is the subject that caused it.
	Actor string
	// Detail is the finer grain a consumer reads when the action is not
	// enough. A link is a grant, so a link create appends share_created
	// with its kind here rather than an action of its own.
	Detail map[string]any
}

// The two actions this package appends. They are members of the closed
// vocabulary of spec 010, which is small on purpose: a consumer that wants
// links alone reads the kind out of the detail.
const (
	ActionShareCreated = "share_created"
	ActionShareRevoked = "share_revoked"
)

// Ledger is the log of spec 010. The append runs inside the mutation's own
// transaction, so a grant that was made is a grant that was recorded: a
// change to who may act on a space is not a notification that may go
// missing.
//
// It is an interface because the log arrives with its own spec. A build that
// binds none appends nowhere, which is what NoLedger is.
type Ledger interface {
	Append(ctx context.Context, q store.Querier, e Event) (int64, error)
}

// NoLedger is the default: an installation whose log has not been bound.
type NoLedger struct{}

// Append records nothing and fails at nothing.
func (NoLedger) Append(context.Context, store.Querier, Event) (int64, error) { return 0, nil }

// ObjectReader serves one object of a space, which is what the third link
// route answers with once the token has resolved and link.read is allowed.
//
// It is an interface because the read path is spec 005's: the bytes at or
// below ARCA_INLINE_BYTES, a redirect to a presigned URL above it, and a
// redirect to ARCA_PUBLIC_CDN_URL for an object a public grant marked. A
// link route decides who may read and confines what may be read; what a read
// looks like once allowed is not this spec's.
//
// A build that binds none answers not_implemented from that one route and
// serves the other two, which is this phase of spec 019: internal/files
// arrives with spec 005 and the node binds it then.
type ObjectReader interface {
	// ServeObject writes the object at path in owner's space. A nil error
	// means the response is written. An error is answered in the envelope
	// of spec 013, so a reader refuses with a *api.Refusal and anything
	// else is internal.
	ServeObject(w http.ResponseWriter, r *http.Request, owner, path string) error
}

// Publisher stamps the bucket's public-read ACL on one object, or removes
// it. It is the one call of spec 003 this package makes: a public grant on a
// prefix that names one object marks that object public, and a revoke clears
// it.
type Publisher interface {
	SetPublic(ctx context.Context, key string, public bool) error
}

// Options is what the node hands this package. Everything but the clock is
// required except the two seams of later specs, which a build without them
// leaves nil.
type Options struct {
	// Authorizer is the seam every handler decides through.
	Authorizer *auth.Authorizer
	// DB is the pool and its transactions.
	DB Database
	// Store is the query set over the grants table.
	Store store.Shares
	// Ledger is the log of spec 010. NoLedger when nil.
	Ledger Ledger
	// Reader serves one object under a link's subtree. A build with none
	// answers not_implemented from that route (spec 005).
	Reader ObjectReader
	// Publisher is the bucket, for the object a public grant marks. A build
	// with none records the row and stamps no ACL.
	Publisher Publisher
	// BucketPrefix is ARCA_BUCKET_PREFIX, which the key of an object
	// derives from (spec 003).
	BucketPrefix string
	// BasePath is ARCA_BASE_PATH, the base the surface is served under
	// (spec 027). The URL a minted link answers with is written under it,
	// because that URL is where the caller redeems the token. Empty is
	// api.DefaultBasePath, the root of the version.
	BasePath string
	// Now is the clock an expiry is read against. time.Now when nil.
	Now func() time.Time
}

// Service answers the routes of spec 008. It is built once at start and
// serves every request; it holds its seams and no state.
type Service struct {
	authorizer   *auth.Authorizer
	db           Database
	store        store.Shares
	ledger       Ledger
	reader       ObjectReader
	publisher    Publisher
	bucketPrefix string
	basePath     string
	clock        func() time.Time
}

// New builds the service. It refuses to build without the three seams every
// route needs, because a surface missing any of them would answer a question
// nobody decided or write a grant nowhere.
func New(o Options) (*Service, error) {
	switch {
	case o.Authorizer == nil:
		return nil, errNoAuthorizer
	case o.DB == nil:
		return nil, errNoDatabase
	case o.Store == nil:
		return nil, errNoStore
	}
	s := &Service{
		authorizer: o.Authorizer, db: o.DB, store: o.Store,
		ledger: o.Ledger, reader: o.Reader, publisher: o.Publisher,
		bucketPrefix: o.BucketPrefix, basePath: cmp.Or(o.BasePath, api.DefaultBasePath),
		clock: o.Now,
	}
	if s.ledger == nil {
		s.ledger = NoLedger{}
	}
	return s, nil
}

// now is the clock an expiry is read against, time.Now unless a test gave
// one.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}
