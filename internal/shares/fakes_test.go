// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
)

// The fakes below stand in for the two stores in the unit tier. What they
// hold is what the SQL of internal/store holds, read the same way: the
// status and the expiry filter every live read, a covering read matches the
// prefixes of the path, and a token read answers nothing for an unknown, a
// revoked, or an expired token. What the SQL means is proved against
// Postgres by the store tier; what these prove is the handlers above.

// table is an in-memory grants table and the files a link lists.
type table struct {
	mu     sync.Mutex
	grants []store.Grant
	files  []store.File
	minted int

	// failures a case turns on, one per query that has a caller reading its
	// error.
	failCreate   error
	failGet      error
	failList     error
	failCovering error
	failSubtree  error
	failPublic   error
}

func newTable() *table { return &table{} }

// now is the clock the fakes read an expiry against.
func (t *table) now() time.Time { return time.Now() }

func (t *table) Create(_ context.Context, _ store.Querier, g store.Grant) (store.Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failCreate != nil {
		return store.Grant{}, t.failCreate
	}
	if g.Token != "" && slices.ContainsFunc(t.grants, func(h store.Grant) bool { return h.Token == g.Token }) {
		return store.Grant{}, fmt.Errorf("store: the token: %w", store.ErrConflict)
	}
	t.minted++
	g.ID = fmt.Sprintf("01J8GRANT%04d", t.minted)
	g.Status = store.GrantActive
	g.CreatedAt = t.now()
	t.grants = append(t.grants, g)
	return g, nil
}

func (t *table) Get(_ context.Context, _ store.Querier, id string) (store.Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failGet != nil {
		return store.Grant{}, t.failGet
	}
	for _, g := range t.grants {
		if g.ID == id {
			return g, nil
		}
	}
	return store.Grant{}, fmt.Errorf("store: read the grant: %w", pgx.ErrNoRows)
}

func (t *table) Revoke(_ context.Context, _ store.Querier, id string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.grants {
		if t.grants[i].ID == id {
			t.grants[i].Status = store.GrantRevoked
			return true, nil
		}
	}
	return false, nil
}

// Move carries every grant on exactly from to to, the way the Postgres
// statement does; a grant on an ancestor is not on from and stays.
func (t *table) Move(_ context.Context, _ store.Querier, owner, from, to string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var n int64
	for i := range t.grants {
		if t.grants[i].Owner == owner && t.grants[i].PathPrefix == from {
			t.grants[i].PathPrefix = to
			n++
		}
	}
	return n, nil
}

func (t *table) ListSpace(_ context.Context, _ store.Querier, owner, prefix, cursor string, limit int) ([]store.Grant, error) {
	return t.page(t.failList, cursor, limit, func(g store.Grant) bool {
		return g.Owner == owner && g.Status == store.GrantActive && (prefix == "" || g.PathPrefix == prefix)
	})
}

func (t *table) ListGrantee(_ context.Context, _ store.Querier, grantee, cursor string, limit int) ([]store.Grant, error) {
	return t.page(t.failList, cursor, limit, func(g store.Grant) bool {
		return g.GranteeKind == store.GranteeSubject && g.Grantee == grantee && t.live(g)
	})
}

func (t *table) ListTokens(_ context.Context, _ store.Querier, owner, cursor string, limit int) ([]store.Grant, error) {
	return t.page(t.failList, cursor, limit, func(g store.Grant) bool {
		return g.Owner == owner && g.GranteeKind != store.GranteeSubject && t.live(g)
	})
}

func (t *table) CountLinks(_ context.Context, _ store.Querier, owners []string) (map[string]int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failList != nil {
		return nil, t.failList
	}
	counts := map[string]int64{}
	for _, g := range t.grants {
		if g.GranteeKind == store.GranteeSubject || !t.live(g) || !slices.Contains(owners, g.Owner) {
			continue
		}
		counts[g.Owner]++
	}
	return counts, nil
}

func (t *table) Expired(_ context.Context, _ store.Querier, before time.Time) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failList != nil {
		return 0, t.failList
	}
	var n int64
	for _, g := range t.grants {
		if g.ExpiresAt != nil && g.ExpiresAt.Before(before) {
			n++
		}
	}
	return n, nil
}

func (t *table) PurgeExpired(_ context.Context, _ store.Querier, before time.Time) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failList != nil {
		return 0, t.failList
	}
	kept := t.grants[:0]
	var gone int64
	for _, g := range t.grants {
		if g.ExpiresAt != nil && g.ExpiresAt.Before(before) {
			gone++
			continue
		}
		kept = append(kept, g)
	}
	t.grants = kept
	return gone, nil
}

func (t *table) Covering(_ context.Context, _ store.Querier, owner, path string) ([]store.Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failCovering != nil {
		return nil, t.failCovering
	}
	prefixes := store.PrefixesOf(path)
	var out []store.Grant
	for _, g := range t.grants {
		if g.Owner == owner && t.live(g) && slices.Contains(prefixes, g.PathPrefix) {
			out = append(out, g)
		}
	}
	slices.SortStableFunc(out, func(a, b store.Grant) int { return rung(b.Permission) - rung(a.Permission) })
	return out, nil
}

func (t *table) ByToken(_ context.Context, _ store.Querier, token string) (store.Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, g := range t.grants {
		if g.Token != "" && g.Token == token && g.GranteeKind != store.GranteeSubject && t.live(g) {
			return g, nil
		}
	}
	return store.Grant{}, fmt.Errorf("store: resolve a link token: %w", pgx.ErrNoRows)
}

func (t *table) Live(_ context.Context, _ store.Querier, id, owner string) (store.Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, g := range t.grants {
		if g.ID == id && g.Owner == owner && g.GranteeKind != store.GranteeSubject && t.live(g) {
			return g, nil
		}
	}
	return store.Grant{}, fmt.Errorf("store: read the link: %w", pgx.ErrNoRows)
}

func (t *table) Subtree(_ context.Context, _ store.Querier, owner, prefix, cursor string, limit int) ([]store.File, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failSubtree != nil {
		return nil, t.failSubtree
	}
	var out []store.File
	for _, f := range t.files {
		covered := f.Path == prefix || strings.HasPrefix(f.Path, prefix+"/")
		if f.Owner == owner && covered && f.DeletedAt == nil && f.Path > cursor {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b store.File) int { return strings.Compare(a.Path, b.Path) })
	return out[:min(len(out), limit)], nil
}

func (t *table) MarkPublic(_ context.Context, _ store.Querier, owner, path string, public bool) (store.File, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failPublic != nil {
		return store.File{}, t.failPublic
	}
	for i := range t.files {
		if t.files[i].Owner == owner && t.files[i].Path == path && t.files[i].DeletedAt == nil {
			t.files[i].IsPublic = public
			return t.files[i], nil
		}
	}
	return store.File{}, fmt.Errorf("store: mark public: %w", pgx.ErrNoRows)
}

// page answers one page of the grants a case's predicate selects, ordered
// and cut the way the statement is.
func (t *table) page(fail error, cursor string, limit int, keep func(store.Grant) bool) ([]store.Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	if limit <= 0 {
		return nil, fmt.Errorf("store: the page holds %d rows", limit)
	}
	var out []store.Grant
	for _, g := range t.grants {
		if keep(g) && g.ID > cursor {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b store.Grant) int { return strings.Compare(a.ID, b.ID) })
	return out[:min(len(out), limit)], nil
}

// live is the filter every read but Get carries.
func (t *table) live(g store.Grant) bool {
	return g.Status == store.GrantActive && (g.ExpiresAt == nil || g.ExpiresAt.After(t.now()))
}

// held answers the grant a case wrote, by id.
func (t *table) held(id string) (store.Grant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, g := range t.grants {
		if g.ID == id {
			return g, true
		}
	}
	return store.Grant{}, false
}

// put writes one grant directly, for a case that needs one before the first
// request.
func (t *table) put(g store.Grant) store.Grant {
	written, err := t.Create(context.Background(), nil, g)
	if err != nil {
		panic(err)
	}
	return written
}

// file writes one live path directly, for a link's listing to read.
func (t *table) file(f store.File) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.files = append(t.files, f)
}

// public answers whether the row one path names carries the public flag.
func (t *table) public(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range t.files {
		if f.Path == path {
			return f.IsPublic
		}
	}
	return false
}

// rung orders the ladder for the covering read.
func rung(permission string) int {
	switch permission {
	case "manage":
		return 3
	case "write":
		return 2
	default:
		return 1
	}
}

// database is the pool and its transactions over the table.
type database struct {
	table *table
	// failTx is the failure a case makes the transaction answer with.
	failTx error
	// commits counts the transactions that ran to the end.
	commits int
}

func (d *database) Querier() store.Querier { return nil }

func (d *database) Tx(ctx context.Context, fn func(store.Querier) error) error {
	if d.failTx != nil {
		return d.failTx
	}
	if err := fn(nil); err != nil {
		return err
	}
	d.commits++
	return nil
}

// ledger records what a mutation appended, which is what spec 010's tail
// would carry.
type ledger struct {
	mu      sync.Mutex
	entries []shares.Event
	fail    error
}

func (l *ledger) Append(_ context.Context, _ store.Querier, e shares.Event) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return 0, l.fail
	}
	l.entries = append(l.entries, e)
	return int64(len(l.entries)), nil
}

func (l *ledger) all() []shares.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

// reader stands in for the read path of spec 005: it records what it was
// asked for and writes a body a test can recognize.
type reader struct {
	owner, path string
	asked       int
	fail        error
}

func (r *reader) ServeObject(w http.ResponseWriter, _ *http.Request, owner, path string) error {
	r.asked++
	r.owner, r.path = owner, path
	if r.fail != nil {
		return r.fail
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("the bytes of " + path))
	return nil
}

// bucket records the object ACLs a public grant stamped.
type bucket struct {
	mu     sync.Mutex
	public map[string]bool
	fail   error
}

func newBucket() *bucket { return &bucket{public: map[string]bool{}} }

func (b *bucket) SetPublic(_ context.Context, key string, public bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.public[key] = public
	return nil
}

// stamped answers the ACL one key carries, and whether it was stamped at
// all.
func (b *bucket) stamped(key string) (bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	public, ok := b.public[key]
	return public, ok
}
