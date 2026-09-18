// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"context"
	"errors"
	"sync"

	"latere.ai/x/arca/internal/store"
)

// The stand-ins this package's unit tier drives. The overview query is
// proved against Postgres in internal/store's own tier; what is proved here
// is the handler over it, so the query set is a slice and the two seams are
// whatever the case needs them to be.

// fakeSpaces answers the overview page a case set, and records what it was
// asked for so a test reads the cursor and the limit the handler bound.
type fakeSpaces struct {
	mu     sync.Mutex
	page   []store.SpaceOverview
	err    error
	cursor string
	limit  int
	calls  int
}

// Overview answers the page the case set, after the cursor it was given.
func (f *fakeSpaces) Overview(_ context.Context, _ store.Querier, cursor string, limit int) ([]store.SpaceOverview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursor, f.limit, f.calls = cursor, limit, f.calls+1
	if f.err != nil {
		return nil, f.err
	}
	out := make([]store.SpaceOverview, 0, limit)
	for _, row := range f.page {
		if row.Owner <= cursor {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, row)
	}
	return out, nil
}

// fakeLinks answers the link count of each space the case gave it.
type fakeLinks struct {
	counts map[string]int64
	err    error
	asked  []string
}

// Counts answers the counts of the owners it was asked about.
func (f *fakeLinks) Counts(_ context.Context, _ store.Querier, owners []string) (map[string]int64, error) {
	f.asked = owners
	return f.counts, f.err
}

// fakeRestorer is the restore seam a case binds. It records what it was
// asked to bring back, so a test reads the space the path named.
type fakeRestorer struct {
	back  Restored
	err   error
	owner string
	id    string
	calls int
}

// Restore answers what the case set.
func (f *fakeRestorer) Restore(_ context.Context, owner, id string) (Restored, error) {
	f.owner, f.id, f.calls = owner, id, f.calls+1
	if f.err != nil {
		return Restored{}, f.err
	}
	back := f.back
	if back.ID == "" {
		back.ID = id
	}
	return back, nil
}

// fakeQuerier is the pool the handlers carry to the query set. Nothing in
// this package reaches it: the query set is the fake above, and a call that
// arrived here would be a handler talking to a database of its own.
type fakeQuerier struct{ store.Querier }

// errFault is a store failure that is not a refusal a caller branches on.
var errFault = errors.New("the connection is gone")
