// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/auth"

	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The metadata store of the unit tier: the query sets of spec 004 over maps
// rather than over Postgres, with the same answers. What the SQL means is
// the store tier's to prove; what the handlers do with the answers is this
// tier's, and it runs with no services at all.
//
// A transaction here is a snapshot and a swap: the body works on a copy, and
// a body that returns an error leaves the store as it was. That is the one
// property of a transaction a handler test depends on, which is that a
// refused write leaves no row.

// memory is the metadata store of the unit tier.
type memory struct {
	mu       sync.Mutex
	files    map[string]store.File
	versions map[string]store.Version
	stars    map[string]store.Star
	sessions map[string]store.Session
	usage    map[string]int64
	events   []Event
	// failTx is the fault a test injects between the bucket write and the
	// commit, which is what leaves bytes with no row. failGet is the one a
	// read meets, which has to reach the caller as an outage and never as a
	// missing object.
	failTx, failGet, failWrite error
	// failAll is the fault every statement but a read of one path meets, so
	// one case drives every handler against a database that answers nothing
	// and none of them reads an outage as a verdict.
	failAll error
	// nextID counts the rows the store has written, so an id is stable and
	// a test can read one.
	nextID int
}

// newMemory answers an empty store.
func newMemory() *memory {
	return &memory{
		files: map[string]store.File{}, versions: map[string]store.Version{},
		stars: map[string]store.Star{}, sessions: map[string]store.Session{},
		usage: map[string]int64{},
	}
}

// fault is the failure every statement but a read of one path answers,
// which lets a case break the writes of a handler and leave its reads.
func (m *memory) fault() error {
	if m.failAll != nil {
		return m.failAll
	}
	return m.failWrite
}

// key is how a row of one table is addressed.
func key(parts ...string) string { return strings.Join(parts, "\x00") }

// Querier answers the store itself, which every query set of this file
// reads through.
func (m *memory) Querier() store.Querier { return nil }

// Tx runs fn against a copy and keeps it only when fn returns nil.
func (m *memory) Tx(_ context.Context, fn func(store.Querier) error) error {
	if m.failTx != nil {
		return m.failTx
	}
	m.mu.Lock()
	files := maps.Clone(m.files)
	versions := maps.Clone(m.versions)
	stars := maps.Clone(m.stars)
	sessions := maps.Clone(m.sessions)
	usage := maps.Clone(m.usage)
	m.mu.Unlock()

	if err := fn(nil); err != nil {
		m.mu.Lock()
		m.files, m.versions, m.stars = files, versions, stars
		m.sessions, m.usage = sessions, usage
		m.mu.Unlock()
		return err
	}
	return nil
}

// mint answers the next row id.
func (m *memory) mint() string {
	m.nextID++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", m.nextID)
}

// Get reads one path, live or trashed.
func (m *memory) Get(_ context.Context, _ store.Querier, owner, path string) (store.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet != nil {
		return store.File{}, m.failGet
	}
	if m.failAll != nil {
		return store.File{}, m.failAll
	}
	f, ok := m.files[key(owner, path)]
	if !ok {
		return store.File{}, fmt.Errorf("memory: read %q: %w", path, pgx.ErrNoRows)
	}
	return f, nil
}

// GetForUpdate reads one path; the lock is the mutex above.
func (m *memory) GetForUpdate(ctx context.Context, q store.Querier, owner, path string) (store.File, error) {
	return m.Get(ctx, q, owner, path)
}

// Insert writes a path nothing holds.
func (m *memory) Insert(_ context.Context, _ store.Querier, f store.File) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	if _, taken := m.files[key(f.Owner, f.Path)]; taken {
		return false, nil
	}
	m.write(f, "")
	return true, nil
}

// write stores one row, keeping the creator and the publicity of the row it
// replaces, which is what the statement of spec 005 does.
func (m *memory) write(f store.File, id string) {
	now := time.Now()
	if held, ok := m.files[key(f.Owner, f.Path)]; ok {
		f.ID, f.CreatedBy, f.IsPublic, f.CreatedAt = held.ID, held.CreatedBy, held.IsPublic, held.CreatedAt
	} else {
		f.ID, f.CreatedAt = m.mint(), now
	}
	if id != "" {
		f.ID = id
	}
	f.DeletedAt = nil
	f.UpdatedAt = now
	m.files[key(f.Owner, f.Path)] = f
}

// Upsert writes the content of a path, whatever is there.
func (m *memory) Upsert(_ context.Context, _ store.Querier, f store.File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return err
	}
	m.write(f, "")
	return nil
}

// CreateOnly writes a path no live row holds.
func (m *memory) CreateOnly(_ context.Context, _ store.Querier, f store.File) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	if held, ok := m.files[key(f.Owner, f.Path)]; ok && held.DeletedAt == nil {
		return false, nil
	}
	m.write(f, "")
	return true, nil
}

// UpdateIfChecksum replaces the content of a live path holding the checksum.
func (m *memory) UpdateIfChecksum(_ context.Context, _ store.Querier, f store.File, ifMatch string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	held, ok := m.files[key(f.Owner, f.Path)]
	if !ok || held.DeletedAt != nil || held.Checksum != ifMatch {
		return false, nil
	}
	m.write(f, held.ID)
	return true, nil
}

// SoftDelete trashes a live path.
func (m *memory) SoftDelete(_ context.Context, _ store.Querier, owner, path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	held, ok := m.files[key(owner, path)]
	if !ok || held.DeletedAt != nil {
		return false, nil
	}
	at := time.Now()
	held.DeletedAt, held.UpdatedAt = &at, at
	m.files[key(owner, path)] = held
	return true, nil
}

// HardDelete removes a row and answers it.
func (m *memory) HardDelete(_ context.Context, _ store.Querier, owner, path string) (store.File, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return store.File{}, false, err
	}
	held, ok := m.files[key(owner, path)]
	if !ok {
		return store.File{}, false, nil
	}
	delete(m.files, key(owner, path))
	return held, true, nil
}

// Restore clears the trash flag of a row inside the window.
func (m *memory) Restore(_ context.Context, _ store.Querier, owner, path string, since time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	held, ok := m.files[key(owner, path)]
	if !ok || held.DeletedAt == nil || !held.DeletedAt.After(since) {
		return false, nil
	}
	held.DeletedAt = nil
	m.files[key(owner, path)] = held
	return true, nil
}

// RestoreByID answers the id-addressed arm of the restore.
func (m *memory) RestoreByID(_ context.Context, _ store.Querier, owner, id string, since time.Time) (store.File, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return store.File{}, false, err
	}
	for k, held := range m.files {
		if held.Owner != owner || held.ID != id {
			continue
		}
		if held.DeletedAt == nil || !held.DeletedAt.After(since) {
			return store.File{}, false, nil
		}
		held.DeletedAt = nil
		m.files[k] = held
		return held, true, nil
	}
	return store.File{}, false, nil
}

// ListTrash answers the trashed rows inside the window, newest first.
func (m *memory) ListTrash(_ context.Context, _ store.Querier, owner string, cursor store.TrashCursor, limit int, since time.Time) ([]store.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return nil, err
	}
	var out []store.File
	for _, f := range m.files {
		if f.Owner != owner || f.DeletedAt == nil || !f.DeletedAt.After(since) {
			continue
		}
		if !cursor.DeletedAt.IsZero() && !f.DeletedAt.Before(cursor.DeletedAt) &&
			(!f.DeletedAt.Equal(cursor.DeletedAt) || f.Path >= cursor.Path) {
			continue
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b store.File) int {
		if !a.DeletedAt.Equal(*b.DeletedAt) {
			return b.DeletedAt.Compare(*a.DeletedAt)
		}
		return strings.Compare(b.Path, a.Path)
	})
	return page(out, limit), nil
}

// PurgeTrash removes trashed rows and answers them.
func (m *memory) PurgeTrash(_ context.Context, _ store.Querier, owner, path string) ([]store.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return nil, err
	}
	var out []store.File
	for k, f := range m.files {
		if f.Owner != owner || f.DeletedAt == nil || (path != "" && f.Path != path) {
			continue
		}
		out = append(out, f)
		delete(m.files, k)
	}
	slices.SortFunc(out, func(a, b store.File) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// Move renames a live path.
func (m *memory) Move(_ context.Context, _ store.Querier, owner, from, to string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	held, ok := m.files[key(owner, from)]
	if !ok || held.DeletedAt != nil {
		return false, nil
	}
	if _, taken := m.files[key(owner, to)]; taken {
		return false, fmt.Errorf("memory: %q is taken: %w", to, store.ErrConflict)
	}
	delete(m.files, key(owner, from))
	held.Path = to
	m.files[key(owner, to)] = held
	return true, nil
}

// ListPrefix answers the live rows under a prefix, ordered by path.
func (m *memory) ListPrefix(_ context.Context, _ store.Querier, owner, prefix, cursor string, limit int) ([]store.File, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return nil, "", err
	}
	var out []store.File
	for _, f := range m.files {
		if f.Owner != owner || f.DeletedAt != nil || !strings.HasPrefix(f.Path, prefix) || f.Path <= cursor {
			continue
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b store.File) int { return strings.Compare(a.Path, b.Path) })
	return page(out, limit), "", nil
}

// Capture moves the current content of a path into its history.
func (m *memory) Capture(_ context.Context, _ store.Querier, owner, path, ifMatch string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return false, err
	}
	held, ok := m.files[key(owner, path)]
	if !ok || (ifMatch != "" && held.Checksum != ifMatch) {
		return false, nil
	}
	n := 0
	for _, v := range m.versions {
		if v.Owner == owner && v.Path == path && v.VersionNo > n {
			n = v.VersionNo
		}
	}
	n++
	m.versions[key(owner, path, fmt.Sprint(n))] = store.Version{
		ID: m.mint(), Owner: owner, Path: path, VersionNo: n, ObjectID: held.ObjectID,
		ContentType: held.ContentType, SizeBytes: held.SizeBytes, Checksum: held.Checksum,
		ChecksumKind: held.ChecksumKind, CreatedBy: held.CreatedBy, SupersededAt: time.Now(),
	}
	return true, nil
}

// versionGet reads one version.
func (m *memory) versionGet(owner, path string, n int) (store.Version, bool) {
	v, ok := m.versions[key(owner, path, fmt.Sprint(n))]
	return v, ok
}

// Get reads one version by its number. The receiver is the versions view, so
// the name does not collide with the files one.
func (m *memory) GetVersion(_ context.Context, _ store.Querier, owner, path string, n int) (store.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return store.Version{}, err
	}
	v, ok := m.versionGet(owner, path, n)
	if !ok {
		return store.Version{}, fmt.Errorf("memory: read version %d: %w", n, pgx.ErrNoRows)
	}
	return v, nil
}

// ListVersions answers one page of a path's history, oldest first.
func (m *memory) ListVersions(_ context.Context, _ store.Querier, owner, path string, after, limit int) ([]store.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return nil, err
	}
	var out []store.Version
	for _, v := range m.versions {
		if v.Owner == owner && v.Path == path && v.VersionNo > after {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b store.Version) int { return a.VersionNo - b.VersionNo })
	return page(out, limit), nil
}

// DeleteVersion removes one version.
func (m *memory) DeleteVersion(_ context.Context, _ store.Querier, owner, path string, n int) (store.Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return store.Version{}, false, err
	}
	v, ok := m.versionGet(owner, path, n)
	if !ok {
		return store.Version{}, false, nil
	}
	delete(m.versions, key(owner, path, fmt.Sprint(n)))
	return v, true, nil
}

// DeletePath removes every version of a path.
func (m *memory) DeletePath(_ context.Context, _ store.Querier, owner, path string) ([]store.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return nil, err
	}
	var out []store.Version
	for k, v := range m.versions {
		if v.Owner == owner && v.Path == path {
			out = append(out, v)
			delete(m.versions, k)
		}
	}
	return out, nil
}

// MoveVersions carries a path's history to a new path.
func (m *memory) MoveVersions(_ context.Context, _ store.Querier, owner, from, to string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return 0, err
	}
	moved := int64(0)
	for k, v := range m.versions {
		if v.Owner != owner || v.Path != from {
			continue
		}
		delete(m.versions, k)
		v.Path = to
		m.versions[key(owner, to, fmt.Sprint(v.VersionNo))] = v
		moved++
	}
	return moved, nil
}

// Add stars a path.
func (m *memory) Add(_ context.Context, _ store.Querier, subject, owner, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return err
	}
	m.stars[key(subject, owner, path)] = store.Star{
		Subject: subject, Owner: owner, Path: path, CreatedAt: time.Now(),
	}
	return nil
}

// Remove unstars a path.
func (m *memory) Remove(_ context.Context, _ store.Querier, subject, owner, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return err
	}
	delete(m.stars, key(subject, owner, path))
	return nil
}

// ListStars answers a subject's stars joined with live rows.
func (m *memory) ListStars(_ context.Context, _ store.Querier, subject string, cursor store.StarCursor, limit int) ([]store.Star, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return nil, err
	}
	var out []store.Star
	for _, s := range m.stars {
		if s.Subject != subject {
			continue
		}
		if s.Owner < cursor.Owner || (s.Owner == cursor.Owner && s.Path <= cursor.Path) {
			continue
		}
		f, ok := m.files[key(s.Owner, s.Path)]
		if !ok || f.DeletedAt != nil {
			continue
		}
		s.File = f
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.Star) int {
		if a.Owner != b.Owner {
			return strings.Compare(a.Owner, b.Owner)
		}
		return strings.Compare(a.Path, b.Path)
	})
	return page(out, limit), nil
}

// MoveStars carries a path's bookmarks to a new path.
func (m *memory) MoveStars(_ context.Context, _ store.Querier, owner, from, to string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fault(); err != nil {
		return 0, err
	}
	moved := int64(0)
	for k, s := range m.stars {
		if s.Owner != owner || s.Path != from {
			continue
		}
		delete(m.stars, k)
		s.Path = to
		m.stars[key(s.Subject, owner, to)] = s
		moved++
	}
	return moved, nil
}

// Referenced answers the reference check of spec 004 over the maps: an
// object a file row or a version row still names may not be deleted. The
// third table of the union is the upload sessions of spec 007, which this
// store holds beside the others.
func (m *memory) Referenced(_ context.Context, _ store.Querier, id object.ID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.files {
		if f.ObjectID == id {
			return true, nil
		}
	}
	for _, v := range m.versions {
		if v.ObjectID == id {
			return true, nil
		}
	}
	for _, s := range m.sessions {
		if s.ObjectID == id {
			return true, nil
		}
	}
	return false, nil
}

// page trims a slice to one page.
func page[T any](rows []T, limit int) []T {
	if len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// The three query sets, each a view of the store above, so one map serves
// every table a handler reaches.
type filesOf struct{ *memory }

type versionsOf struct{ *memory }

type starsOf struct{ *memory }

func (v versionsOf) Get(ctx context.Context, q store.Querier, owner, path string, n int) (store.Version, error) {
	return v.GetVersion(ctx, q, owner, path, n)
}

func (v versionsOf) List(ctx context.Context, q store.Querier, owner, path string, after, limit int) ([]store.Version, error) {
	return v.ListVersions(ctx, q, owner, path, after, limit)
}

func (v versionsOf) Delete(ctx context.Context, q store.Querier, owner, path string, n int) (store.Version, bool, error) {
	return v.DeleteVersion(ctx, q, owner, path, n)
}

func (v versionsOf) Move(ctx context.Context, q store.Querier, owner, from, to string) (int64, error) {
	return v.MoveVersions(ctx, q, owner, from, to)
}

func (s starsOf) List(ctx context.Context, q store.Querier, subject string, cursor store.StarCursor, limit int) ([]store.Star, error) {
	return s.ListStars(ctx, q, subject, cursor, limit)
}

func (s starsOf) Move(ctx context.Context, q store.Querier, owner, from, to string) (int64, error) {
	return s.MoveStars(ctx, q, owner, from, to)
}

// counted is the ledger of the unit tier: it records every charge and every
// event, and refuses a charge past the limit the answer carried, which is
// what internal/events does against Postgres.
type counted struct {
	memory *memory
	// refuse is a ledger that cannot be written, which is what has to take
	// the write down with it.
	refuse error
}

func (c *counted) Charge(_ context.Context, _ store.Querier, owner string, delta int64, limit Limit) (int64, error) {
	if c.refuse != nil {
		return 0, c.refuse
	}
	used := c.memory.usage[owner]
	total := max(used+delta, 0)
	if delta > 0 && limit.Set && total > limit.Bytes {
		return total, &OverLimit{Owner: owner, Used: used, Limit: limit.Bytes, Delta: delta}
	}
	c.memory.usage[owner] = total
	return total, nil
}

func (c *counted) Release(ctx context.Context, q store.Querier, owner string, bytes int64) (int64, error) {
	return c.Charge(ctx, q, owner, -bytes, Limit{})
}

func (c *counted) Append(_ context.Context, _ store.Querier, e Event) {
	c.memory.events = append(c.memory.events, e)
}

// actions answers the actions the log recorded, in order.
func (m *memory) actions() []string {
	var out []string
	for _, e := range m.events {
		out = append(out, e.Action)
	}
	return out
}

// question is one thing a handler asked the seam of spec 006.
type question struct {
	action string
	res    authz.Resource
	lookup bool
}

// recorder is the Decide seam with a record of every question in front of
// it. It wraps the real client, so an allow is the client's own answer and
// the cases that count questions see every one a handler asked, cached or
// not.
type recorder struct {
	inner Decider
	// refuse, when set, is the answer every question gets, so a case drives
	// a deny without the client's decision cache deciding which questions
	// reach the endpoint at all.
	refuse error
	asked  []question
}

func (r *recorder) Decide(ctx context.Context, action string, res authz.Resource) (auth.Decision, error) {
	return r.record(ctx, action, res, false)
}

func (r *recorder) Lookup(ctx context.Context, action string, res authz.Resource) (auth.Decision, error) {
	return r.record(ctx, action, res, true)
}

func (r *recorder) record(ctx context.Context, action string, res authz.Resource, lookup bool) (auth.Decision, error) {
	r.asked = append(r.asked, question{action: action, res: res, lookup: lookup})
	if r.refuse != nil {
		return auth.Decision{}, r.refuse
	}
	if lookup {
		return r.inner.Lookup(ctx, action, res)
	}
	return r.inner.Decide(ctx, action, res)
}

// actions answers the actions a handler asked, in order.
func (r *recorder) actions() []string {
	var out []string
	for _, q := range r.asked {
		out = append(out, q.action)
	}
	return out
}
