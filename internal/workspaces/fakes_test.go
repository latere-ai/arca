// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// memory is the two stores in a map: the workspace rows, the attachments,
// and the file plane a workspace reads as a subtree. It satisfies every seam
// this package takes, so the unit tier drives each handler end to end with
// no Postgres and no bucket behind it.
//
// It models the one thing a map does not model on its own, the transaction:
// [memory.Tx] takes a copy before it runs and puts it back when the function
// fails, so a handler that refuses halfway through is proved to have written
// nothing rather than assumed to have.
type memory struct {
	mu          sync.Mutex
	workspaces  map[string]store.Workspace
	attachments map[string]store.Attachment
	files       map[string]map[string]store.WorkspaceFile
	versions    map[object.ID]bool
	seq         int
	txs         int
	// depth counts the transactions running right now and stray counts the
	// reads and writes that reached the pool while one was, which is the
	// fault [memory.use] exists to catch.
	depth int
	stray int
	// fail maps a method name to the failure it answers, so a test drives
	// the path a store fault takes without a store.
	fail map[string]error
}

// leaseHeld is the clause every statement that refuses to act while a writer
// holds the workspace reads: a lease is time bounded, so a holder past its
// deadline does not hold it.
func leaseHeld(w store.Workspace, now time.Time) bool {
	return w.WriterHolder != nil && w.WriterExpiresAt != nil && w.WriterExpiresAt.After(now)
}

// newMemory answers an empty pair of stores.
func newMemory() *memory {
	return &memory{
		workspaces:  map[string]store.Workspace{},
		attachments: map[string]store.Attachment{},
		files:       map[string]map[string]store.WorkspaceFile{},
		versions:    map[object.ID]bool{},
		fail:        map[string]error{},
	}
}

// handle is what the fakes tell a transaction's querier from the pool's by.
// Nothing calls its methods: every fake here reads the value and never a
// connection behind it.
type handle struct{ tx bool }

func (handle) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (handle) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (handle) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// Querier answers the pool's handle. The real one takes a connection, which
// is why [memory.use] counts every fake call that reaches this one while a
// transaction is open.
func (m *memory) Querier() store.Querier { return handle{} }

// Tx runs fn against a copy and keeps the copy only when it returns nil.
func (m *memory) Tx(_ context.Context, fn func(store.Querier) error) error {
	m.mu.Lock()
	m.txs++
	m.depth++
	before := m.snapshot()
	m.mu.Unlock()
	err := fn(handle{tx: true})
	m.mu.Lock()
	m.depth--
	if err != nil {
		m.restore(before)
	}
	m.mu.Unlock()
	return err
}

// use records a read or a write that reached the pool while a transaction
// was open. A pool hands out one connection per caller, so a handler that
// asks for a second while it holds the first deadlocks against itself once
// enough callers do it at once: every one of them holds a connection and
// waits for one, until the acquire deadline. No handler may do it, and the
// unit tier proves none does because every harness checks this count.
func (m *memory) use(q store.Querier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := q.(handle); m.depth > 0 && (!ok || !h.tx) {
		m.stray++
	}
}

// state is one copy of the stores.
type state struct {
	workspaces  map[string]store.Workspace
	attachments map[string]store.Attachment
	files       map[string]map[string]store.WorkspaceFile
}

func (m *memory) snapshot() state {
	files := map[string]map[string]store.WorkspaceFile{}
	for owner, tree := range m.files {
		files[owner] = maps.Clone(tree)
	}
	return state{
		workspaces:  maps.Clone(m.workspaces),
		attachments: maps.Clone(m.attachments),
		files:       files,
	}
}

func (m *memory) restore(s state) {
	m.workspaces, m.attachments, m.files = s.workspaces, s.attachments, s.files
}

// next mints an id. Nothing parses one, so a counter is an id.
func (m *memory) next(kind string) string {
	m.seq++
	return fmt.Sprintf("%s-%04d", kind, m.seq)
}

// refuse answers the failure a test set for a method, and nil otherwise.
func (m *memory) refuse(method string) error { return m.fail[method] }

// ── the workspace query set ──────────────────────────────────────────────

func (m *memory) Create(_ context.Context, q store.Querier, w store.Workspace) (store.Workspace, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Create"); err != nil {
		return store.Workspace{}, err
	}
	for _, held := range m.workspaces {
		if held.Owner == w.Owner && held.Slug == w.Slug {
			return store.Workspace{}, fmt.Errorf("fake: %w", store.ErrConflict)
		}
	}
	w.ID = m.next("ws")
	w.CreatedAt, w.UpdatedAt = time.Now(), time.Now()
	m.workspaces[w.ID] = w
	return w, nil
}

func (m *memory) Get(_ context.Context, q store.Querier, id string) (store.Workspace, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Get"); err != nil {
		return store.Workspace{}, err
	}
	w, ok := m.workspaces[id]
	if !ok {
		return store.Workspace{}, fmt.Errorf("fake: %w", pgx.ErrNoRows)
	}
	return w, nil
}

func (m *memory) GetForUpdate(ctx context.Context, q store.Querier, id string) (store.Workspace, error) {
	m.use(q)
	if err := m.refuse("GetForUpdate"); err != nil {
		return store.Workspace{}, err
	}
	return m.Get(ctx, q, id)
}

func (m *memory) List(_ context.Context, q store.Querier, owner, cursor string, limit int) ([]store.Workspace, error) {
	m.use(q)
	return m.listing(owner, cursor, limit, false)
}

func (m *memory) ListDeleted(_ context.Context, q store.Querier, owner, cursor string, limit int) ([]store.Workspace, error) {
	m.use(q)
	return m.listing(owner, cursor, limit, true)
}

func (m *memory) listing(owner, cursor string, limit int, wantDeleted bool) ([]store.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("List"); err != nil {
		return nil, err
	}
	var page []store.Workspace
	for _, id := range slices.Sorted(maps.Keys(m.workspaces)) {
		w := m.workspaces[id]
		if w.Owner != owner || (w.DeletedAt != nil) != wantDeleted || id <= cursor {
			continue
		}
		page = append(page, w)
		if len(page) == limit {
			break
		}
	}
	return page, nil
}

// Live is the liveness lookup the file plane of spec 005 asks, over the map.
func (m *memory) Live(_ context.Context, q store.Querier, owner, slug string) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Live"); err != nil {
		return false, err
	}
	for _, w := range m.workspaces {
		if w.Owner == owner && w.Slug == slug && w.DeletedAt == nil {
			return true, nil
		}
	}
	return false, nil
}

func (m *memory) Rename(_ context.Context, q store.Querier, id, slug string, now time.Time) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Rename"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.DeletedAt != nil || leaseHeld(w, now) {
		return false, nil
	}
	for _, held := range m.workspaces {
		if held.Owner == w.Owner && held.Slug == slug {
			return false, fmt.Errorf("fake: %w", store.ErrConflict)
		}
	}
	w.Slug = slug
	m.workspaces[id] = w
	return true, nil
}

func (m *memory) SoftDelete(_ context.Context, q store.Querier, id string, now time.Time) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("SoftDelete"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.DeletedAt != nil || leaseHeld(w, now) {
		return false, nil
	}
	at := time.Now()
	w.DeletedAt = &at
	m.workspaces[id] = w
	return true, nil
}

func (m *memory) Restore(_ context.Context, q store.Querier, id string) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Restore"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.DeletedAt == nil {
		return false, nil
	}
	w.DeletedAt = nil
	m.workspaces[id] = w
	return true, nil
}

func (m *memory) Tombstones(_ context.Context, q store.Querier, before time.Time, limit int) ([]store.Workspace, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Tombstones"); err != nil {
		return nil, err
	}
	var out []store.Workspace
	for _, id := range slices.Sorted(maps.Keys(m.workspaces)) {
		w := m.workspaces[id]
		if w.DeletedAt == nil || !w.DeletedAt.Before(before) {
			continue
		}
		out = append(out, w)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (m *memory) Purge(_ context.Context, q store.Querier, id string) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Purge"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.DeletedAt == nil {
		return false, nil
	}
	delete(m.workspaces, id)
	return true, nil
}

func (m *memory) TakeLease(_ context.Context, q store.Querier, id, holder string, now, until time.Time) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("TakeLease"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.DeletedAt != nil {
		return false, nil
	}
	if leaseHeld(w, now) {
		return false, nil
	}
	w.WriterHolder, w.WriterExpiresAt = &holder, &until
	m.workspaces[id] = w
	return true, nil
}

func (m *memory) RenewLease(_ context.Context, q store.Querier, id, holder string, until time.Time) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("RenewLease"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.WriterHolder == nil || *w.WriterHolder != holder {
		return false, nil
	}
	w.WriterExpiresAt = &until
	m.workspaces[id] = w
	return true, nil
}

func (m *memory) ReleaseLease(_ context.Context, q store.Querier, id, holder string) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("ReleaseLease"); err != nil {
		return false, err
	}
	w, ok := m.workspaces[id]
	if !ok || w.WriterHolder == nil || *w.WriterHolder != holder {
		return false, nil
	}
	w.WriterHolder, w.WriterExpiresAt = nil, nil
	m.workspaces[id] = w
	return true, nil
}

func (m *memory) StampSync(_ context.Context, q store.Querier, id string) (time.Time, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("StampSync"); err != nil {
		return time.Time{}, err
	}
	w, ok := m.workspaces[id]
	if !ok {
		return time.Time{}, fmt.Errorf("fake: %w", pgx.ErrNoRows)
	}
	at := time.Now()
	w.LastSync = &at
	m.workspaces[id] = w
	return at, nil
}

func (m *memory) ExpiredLeases(_ context.Context, q store.Querier, now time.Time, limit int) ([]store.Workspace, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("ExpiredLeases"); err != nil {
		return nil, err
	}
	var out []store.Workspace
	for _, id := range slices.Sorted(maps.Keys(m.workspaces)) {
		w := m.workspaces[id]
		if w.WriterHolder == nil || w.WriterExpiresAt == nil || !w.WriterExpiresAt.Before(now) {
			continue
		}
		out = append(out, w)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ── the attachment query set ─────────────────────────────────────────────

func (m *memory) Insert(_ context.Context, q store.Querier, a store.Attachment) (store.Attachment, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Insert"); err != nil {
		return store.Attachment{}, err
	}
	a.ID = m.next("att")
	a.Status, a.CreatedAt = store.StatusActive, time.Now()
	m.attachments[a.ID] = a
	return a, nil
}

func (m *memory) GetAttachment(_ context.Context, q store.Querier, workspaceID, id string) (store.Attachment, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("GetAttachment"); err != nil {
		return store.Attachment{}, err
	}
	a, ok := m.attachments[id]
	if !ok || a.WorkspaceID != workspaceID {
		return store.Attachment{}, fmt.Errorf("fake: %w", pgx.ErrNoRows)
	}
	return a, nil
}

func (m *memory) SetExpiry(_ context.Context, q store.Querier, id string, until time.Time) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("SetExpiry"); err != nil {
		return false, err
	}
	a, ok := m.attachments[id]
	if !ok || a.Status != store.StatusActive {
		return false, nil
	}
	a.ExpiresAt = until
	m.attachments[id] = a
	return true, nil
}

func (m *memory) Release(ctx context.Context, q store.Querier, id string) (bool, error) {
	m.use(q)
	if err := m.refuse("Release"); err != nil {
		return false, err
	}
	return m.end(ctx, q, id, store.StatusReleased)
}

func (m *memory) Reap(ctx context.Context, q store.Querier, id string) (bool, error) {
	m.use(q)
	if err := m.refuse("Reap"); err != nil {
		return false, err
	}
	return m.end(ctx, q, id, store.StatusReaped)
}

func (m *memory) end(_ context.Context, q store.Querier, id string, status store.AttachmentStatus) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attachments[id]
	if !ok || a.Status != store.StatusActive {
		return false, nil
	}
	at := time.Now()
	a.Status, a.ReleasedAt = status, &at
	m.attachments[id] = a
	return true, nil
}

func (m *memory) SetManifest(_ context.Context, q store.Querier, id string, manifest []byte) (bool, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("SetManifest"); err != nil {
		return false, err
	}
	a, ok := m.attachments[id]
	if !ok || a.Status != store.StatusActive {
		return false, nil
	}
	a.Manifest = manifest
	m.attachments[id] = a
	return true, nil
}

func (m *memory) Expired(_ context.Context, q store.Querier, now time.Time, limit int) ([]store.Attachment, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Expired"); err != nil {
		return nil, err
	}
	var out []store.Attachment
	for _, id := range slices.Sorted(maps.Keys(m.attachments)) {
		a := m.attachments[id]
		if a.Status != store.StatusActive || !a.ExpiresAt.Before(now) {
			continue
		}
		out = append(out, a)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// open writes one attachment straight into the store, so a test drives the
// renew and the release as the first question put about that workspace. The
// allow an attach would have taken is cached by the authorizer client of
// spec 006, and a cached answer never reaches the stub.
func (m *memory) open(workspaceID, holder string, mode store.Mode, expires time.Time) store.Attachment {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := store.Attachment{
		ID: m.next("att"), WorkspaceID: workspaceID, Holder: holder,
		Mode: mode, Status: store.StatusActive, Manifest: []byte("[]"),
		ExpiresAt: expires, CreatedAt: time.Now(),
	}
	m.attachments[a.ID] = a
	if mode == store.ModeWrite {
		w := m.workspaces[workspaceID]
		w.WriterHolder, w.WriterExpiresAt = &holder, &expires
		m.workspaces[workspaceID] = w
	}
	return a
}

// attachments answers the attachment query set, whose Get has a name of its
// own on memory because the workspace query set already holds one.
type attachmentSet struct{ *memory }

func (a attachmentSet) Get(ctx context.Context, q store.Querier, workspaceID, id string) (store.Attachment, error) {
	return a.GetAttachment(ctx, q, workspaceID, id)
}

// ── the file plane ───────────────────────────────────────────────────────

// put writes one object under a path, which is what spec 005's put does.
func (m *memory) put(owner, path, checksum string, size int64) object.ID {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files[owner] == nil {
		m.files[owner] = map[string]store.WorkspaceFile{}
	}
	id := object.NewID()
	m.files[owner][path] = store.WorkspaceFile{Path: path, Checksum: checksum, Size: size, ObjectID: id}
	return id
}

func (m *memory) Manifest(_ context.Context, q store.Querier, owner, prefix string) ([]store.WorkspaceFile, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Manifest"); err != nil {
		return nil, err
	}
	var out []store.WorkspaceFile
	for _, path := range slices.Sorted(maps.Keys(m.files[owner])) {
		if strings.HasPrefix(path, prefix) {
			out = append(out, m.files[owner][path])
		}
	}
	return out, nil
}

func (m *memory) Stat(_ context.Context, q store.Querier, owner, prefix string) (int64, int64, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Stat"); err != nil {
		return 0, 0, err
	}
	var files, bytes int64
	for path, f := range m.files[owner] {
		if strings.HasPrefix(path, prefix) {
			files++
			bytes += f.Size
		}
	}
	return files, bytes, nil
}

func (m *memory) MoveSubtree(_ context.Context, q store.Querier, owner, from, to string) (int64, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("MoveSubtree"); err != nil {
		return 0, err
	}
	var moved int64
	for _, path := range slices.Sorted(maps.Keys(m.files[owner])) {
		rest, under := strings.CutPrefix(path, from)
		if !under {
			continue
		}
		f := m.files[owner][path]
		delete(m.files[owner], path)
		f.Path = to + rest
		m.files[owner][f.Path] = f
		moved++
	}
	return moved, nil
}

func (m *memory) Drop(_ context.Context, q store.Querier, owner string, paths []string) ([]object.ID, int64, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Drop"); err != nil {
		return nil, 0, err
	}
	var freed []object.ID
	var bytes int64
	for _, path := range paths {
		f, ok := m.files[owner][path]
		if !ok {
			continue
		}
		delete(m.files[owner], path)
		freed = append(freed, f.ObjectID)
		bytes += f.Size
	}
	return freed, bytes, nil
}

func (m *memory) DropSubtree(_ context.Context, q store.Querier, owner, prefix string) ([]object.ID, int64, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("DropSubtree"); err != nil {
		return nil, 0, err
	}
	var freed []object.ID
	var bytes int64
	for _, path := range slices.Sorted(maps.Keys(m.files[owner])) {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		f := m.files[owner][path]
		delete(m.files[owner], path)
		freed = append(freed, f.ObjectID)
		bytes += f.Size
	}
	return freed, bytes, nil
}

func (m *memory) Unreferenced(_ context.Context, q store.Querier, ids []object.ID) ([]object.ID, error) {
	m.use(q)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refuse("Unreferenced"); err != nil {
		return nil, err
	}
	named := map[object.ID]bool{}
	for _, tree := range m.files {
		for _, f := range tree {
			named[f.ObjectID] = true
		}
	}
	var out []object.ID
	for _, id := range ids {
		if !named[id] && !m.versions[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// ── the ledger ───────────────────────────────────────────────────────────

// recorder is the log and the usage counter of spec 010 as a list, so a test
// reads what a handler appended and what it gave back.
type recorder struct {
	mu       sync.Mutex
	events   []Event
	released map[string]int64
	fail     error
}

func newRecorder() *recorder { return &recorder{released: map[string]int64{}} }

func (r *recorder) Append(_ context.Context, _ store.Querier, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) Release(_ context.Context, _ store.Querier, owner string, bytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.released[owner] += bytes
	return nil
}

// actions answers the actions appended, in order.
func (r *recorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Action)
	}
	return out
}
