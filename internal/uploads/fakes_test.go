// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The metadata store of the unit tier: the query sets of spec 004 over maps.
// This package reaches four of them, so the rest answer the empty page a
// build with no such row would answer and no case reads one.
//
// A transaction is a snapshot and a swap: a body that returns an error
// leaves the store as it was, which is the one property a handler test
// depends on.

// memory is the metadata store of the unit tier.
type memory struct {
	mu       sync.Mutex
	files    map[string]store.File
	versions map[string]store.Version
	sessions map[string]store.Session
	usage    map[string]int64
	events   []files.Event
	// failTx and failWrite are the faults a case injects: one on the
	// transaction the completion commits in, one on the statements inside
	// it.
	failTx, failWrite error
	// failGet is the fault a read of a session meets, and failPath the one a
	// read of the path behind it meets. Neither may reach a caller as a
	// missing row.
	failGet, failPath error
	// failExpired is the fault the reaper's own query meets, which is a pass
	// that has found nothing rather than a run with nothing to find.
	failExpired error
	minted      int
}

func newMemory() *memory {
	return &memory{
		files: map[string]store.File{}, versions: map[string]store.Version{},
		sessions: map[string]store.Session{}, usage: map[string]int64{},
	}
}

// key is how a row of one table is addressed.
func key(parts ...string) string { return strings.Join(parts, "\x00") }

// Querier answers the store itself.
func (m *memory) Querier() store.Querier { return nil }

// Tx runs fn against a copy and keeps it only when fn returns nil.
func (m *memory) Tx(_ context.Context, fn func(store.Querier) error) error {
	if m.failTx != nil {
		return m.failTx
	}
	m.mu.Lock()
	rows, versions, sessions, usage := maps.Clone(m.files), maps.Clone(m.versions),
		maps.Clone(m.sessions), maps.Clone(m.usage)
	m.mu.Unlock()
	if err := fn(nil); err != nil {
		m.mu.Lock()
		m.files, m.versions, m.sessions, m.usage = rows, versions, sessions, usage
		m.mu.Unlock()
		return err
	}
	return nil
}

// mint answers the next row id.
func (m *memory) mint() string {
	m.minted++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", m.minted)
}

// The files table, of which this package reads one path and writes one row.

func (m *memory) Get(_ context.Context, _ store.Querier, owner, path string) (store.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPath != nil {
		return store.File{}, m.failPath
	}
	f, ok := m.files[key(owner, path)]
	if !ok {
		return store.File{}, fmt.Errorf("memory: read %q: %w", path, pgx.ErrNoRows)
	}
	return f, nil
}

func (m *memory) GetForUpdate(ctx context.Context, q store.Querier, owner, path string) (store.File, error) {
	return m.Get(ctx, q, owner, path)
}

func (m *memory) write(f store.File) {
	if held, ok := m.files[key(f.Owner, f.Path)]; ok {
		f.ID, f.CreatedBy, f.IsPublic = held.ID, held.CreatedBy, held.IsPublic
	} else {
		f.ID = m.mint()
	}
	f.DeletedAt, f.UpdatedAt = nil, time.Now()
	m.files[key(f.Owner, f.Path)] = f
}

func (m *memory) Upsert(_ context.Context, _ store.Querier, f store.File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return m.failWrite
	}
	m.write(f)
	return nil
}

func (m *memory) Insert(ctx context.Context, q store.Querier, f store.File) (bool, error) {
	m.mu.Lock()
	_, taken := m.files[key(f.Owner, f.Path)]
	m.mu.Unlock()
	if taken {
		return false, nil
	}
	return true, m.Upsert(ctx, q, f)
}

func (m *memory) CreateOnly(_ context.Context, _ store.Querier, f store.File) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return false, m.failWrite
	}
	if held, ok := m.files[key(f.Owner, f.Path)]; ok && held.DeletedAt == nil {
		return false, nil
	}
	m.write(f)
	return true, nil
}

func (m *memory) UpdateIfChecksum(_ context.Context, _ store.Querier, f store.File, ifMatch string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return false, m.failWrite
	}
	held, ok := m.files[key(f.Owner, f.Path)]
	if !ok || held.DeletedAt != nil || held.Checksum != ifMatch {
		return false, nil
	}
	m.write(f)
	return true, nil
}

// The rest of the files table, which no route of this package reaches.

func (m *memory) SoftDelete(context.Context, store.Querier, string, string) (bool, error) {
	return false, nil
}

func (m *memory) HardDelete(context.Context, store.Querier, string, string) (store.File, bool, error) {
	return store.File{}, false, nil
}

func (m *memory) Restore(context.Context, store.Querier, string, string, time.Time) (bool, error) {
	return false, nil
}

func (m *memory) RestoreByID(context.Context, store.Querier, string, string, time.Time) (store.File, bool, error) {
	return store.File{}, false, nil
}

func (m *memory) ListTrash(context.Context, store.Querier, string, store.TrashCursor, int, time.Time) ([]store.File, error) {
	return nil, nil
}

func (m *memory) PurgeTrash(context.Context, store.Querier, string, string) ([]store.File, error) {
	return nil, nil
}

func (m *memory) Move(context.Context, store.Querier, string, string, string) (bool, error) {
	return false, nil
}

func (m *memory) ListPrefix(context.Context, store.Querier, string, string, string, int) ([]store.File, string, error) {
	return nil, "", nil
}

// The versions table, of which this package reaches the capture alone.

func (m *memory) Capture(_ context.Context, _ store.Querier, owner, path, ifMatch string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return false, m.failWrite
	}
	held, ok := m.files[key(owner, path)]
	if !ok || (ifMatch != "" && held.Checksum != ifMatch) {
		return false, nil
	}
	n := len(m.versions) + 1
	m.versions[key(owner, path, fmt.Sprint(n))] = store.Version{
		ID: m.mint(), Owner: owner, Path: path, VersionNo: n, ObjectID: held.ObjectID,
		ContentType: held.ContentType, SizeBytes: held.SizeBytes, Checksum: held.Checksum,
		ChecksumKind: held.ChecksumKind, CreatedBy: held.CreatedBy, SupersededAt: time.Now(),
	}
	return true, nil
}

func (m *memory) GetVersion(context.Context, store.Querier, string, string, int) (store.Version, error) {
	return store.Version{}, pgx.ErrNoRows
}

func (m *memory) ListVersions(context.Context, store.Querier, string, string, int, int) ([]store.Version, error) {
	return nil, nil
}

func (m *memory) DeleteVersion(context.Context, store.Querier, string, string, int) (store.Version, bool, error) {
	return store.Version{}, false, nil
}

func (m *memory) DeletePath(context.Context, store.Querier, string, string) ([]store.Version, error) {
	return nil, nil
}

func (m *memory) MoveVersions(context.Context, store.Querier, string, string, string) (int64, error) {
	return 0, nil
}

// The stars table, which no route of this package reaches.

func (m *memory) Add(context.Context, store.Querier, string, string, string) error { return nil }

func (m *memory) Remove(context.Context, store.Querier, string, string, string) error { return nil }

func (m *memory) ListStars(context.Context, store.Querier, string, store.StarCursor, int) ([]store.Star, error) {
	return nil, nil
}

func (m *memory) MoveStars(context.Context, store.Querier, string, string, string) (int64, error) {
	return 0, nil
}

// The sessions table, which is this package's own.

func (m *memory) Insert2(_ context.Context, _ store.Querier, s store.Session) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return store.Session{}, m.failWrite
	}
	s.ID = m.mint()
	s.CreatedAt = time.Now()
	m.sessions[s.ID] = s
	return s, nil
}

func (m *memory) GetSession(_ context.Context, _ store.Querier, id string) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet != nil {
		return store.Session{}, m.failGet
	}
	s, ok := m.sessions[id]
	if !ok {
		return store.Session{}, fmt.Errorf("memory: read the upload %q: %w", id, pgx.ErrNoRows)
	}
	return s, nil
}

func (m *memory) DeleteSession(_ context.Context, _ store.Querier, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWrite != nil {
		return false, m.failWrite
	}
	_, held := m.sessions[id]
	delete(m.sessions, id)
	return held, nil
}

// CountOpen is the complement of Expired below, over the same map: what
// arca_upload_sessions_open reads (spec 018).
func (m *memory) CountOpen(_ context.Context, _ store.Querier, at time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failExpired != nil {
		return 0, m.failExpired
	}
	var open int64
	for _, s := range m.sessions {
		if s.ExpiresAt.After(at) {
			open++
		}
	}
	return open, nil
}

func (m *memory) Expired(_ context.Context, _ store.Querier, at time.Time, limit int) ([]store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failExpired != nil {
		return nil, m.failExpired
	}
	var out []store.Session
	for _, s := range m.sessions {
		if !s.ExpiresAt.After(at) && len(out) < limit {
			out = append(out, s)
		}
	}
	return out, nil
}

// Referenced is the reference check of spec 004 over the maps, with the
// third table of its union, which is this package's own.
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

// The three views of the store, so one map serves every table a handler
// reaches.
type filesOf struct{ *memory }

type versionsOf struct{ *memory }

type starsOf struct{ *memory }

type sessionsOf struct{ *memory }

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

func (s sessionsOf) Insert(ctx context.Context, q store.Querier, held store.Session) (store.Session, error) {
	return s.Insert2(ctx, q, held)
}

func (s sessionsOf) Get(ctx context.Context, q store.Querier, id string) (store.Session, error) {
	return s.GetSession(ctx, q, id)
}

func (s sessionsOf) Delete(ctx context.Context, q store.Querier, id string) (bool, error) {
	return s.DeleteSession(ctx, q, id)
}

// counted is the ledger of the unit tier: it records every charge and every
// event and refuses a charge past the limit the answer carried.
type counted struct {
	memory *memory
	refuse error
}

func (c *counted) Charge(_ context.Context, _ store.Querier, owner string, delta int64, limit files.Limit) (int64, error) {
	if c.refuse != nil {
		return 0, c.refuse
	}
	used := c.memory.usage[owner]
	total := max(used+delta, 0)
	if delta > 0 && limit.Set && total > limit.Bytes {
		return total, &files.OverLimit{Owner: owner, Used: used, Limit: limit.Bytes, Delta: delta}
	}
	c.memory.usage[owner] = total
	return total, nil
}

func (c *counted) Release(ctx context.Context, q store.Querier, owner string, bytes int64) (int64, error) {
	return c.Charge(ctx, q, owner, -bytes, files.Limit{})
}

func (c *counted) Append(_ context.Context, _ store.Querier, e files.Event) {
	c.memory.events = append(c.memory.events, e)
}

// Usage answers what the space holds. A session asks nothing of it; the
// method is here because the seam spec 005's root listing reads is one
// interface and this package binds the same one.
func (c *counted) Usage(_ context.Context, _ store.Querier, owner string) (files.Usage, error) {
	if c.refuse != nil {
		return files.Usage{}, c.refuse
	}
	return files.Usage{Bytes: c.memory.usage[owner]}, nil
}
