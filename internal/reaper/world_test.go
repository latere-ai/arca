// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package reaper

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// world is a database small enough to hold in a test and honest enough to
// answer every statement the passes send. It exists because the passes are
// judged on what they leave behind: a pass run twice has to leave what one
// run left, and a dry run has to report what a live run would change, and
// neither is visible to a fake that only records the statement it was sent.
//
// What the SQL means against Postgres is the store tier's, which is the only
// place a transaction and a conditional update can be proved at all.
type world struct {
	files    []fileRow
	versions []fileRow
	stars    []starRow
	events   []eventRow
	usage    map[string]int64

	// faults answers a failure for the first statement holding the text.
	faults map[string]error
	// beforeCorrect runs before the conditional update of the ledger, which
	// is how a case lands a charge in the window pass 10 races.
	beforeCorrect func()
	// sent is every statement the passes issued, in order.
	sent []string
	// txs counts the transactions the passes opened.
	txs int
}

type fileRow struct {
	owner, path string
	id          object.ID
	size        int64
	deleted     *time.Time
}

type starRow struct{ owner, path string }

type eventRow struct {
	id      int64
	owner   string
	action  string
	created time.Time
}

// newWorld answers an empty database.
func newWorld() *world {
	return &world{usage: map[string]int64{}, faults: map[string]error{}}
}

// Querier answers the world as a querier.
func (w *world) Querier() store.Querier { return w }

// Tx runs fn against the world. What a transaction means is the store tier's;
// here it counts, so a pass that must move rows and the ledger together is
// held to opening one.
func (w *world) Tx(_ context.Context, fn func(store.Querier) error) error {
	w.txs++
	return fn(w)
}

// fault answers the failure the case set for a statement, once.
func (w *world) fault(sql string) error {
	for text, err := range w.faults {
		if strings.Contains(sql, text) {
			delete(w.faults, text)
			return err
		}
	}
	return nil
}

func (w *world) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	w.sent = append(w.sent, sql)
	if err := w.fault(sql); err != nil {
		return pgconn.CommandTag{}, err
	}
	switch {
	case strings.Contains(sql, "DELETE FROM events"):
		before := args[0].(time.Time)
		n := len(w.events)
		w.events = slices.DeleteFunc(w.events, func(e eventRow) bool { return e.created.Before(before) })
		return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", n-len(w.events))), nil
	case strings.Contains(sql, "UPDATE space_usage"):
		if w.beforeCorrect != nil {
			w.beforeCorrect()
		}
		owner, was, now := args[0].(string), args[1].(int64), args[2].(int64)
		if w.usage[owner] != was {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		w.usage[owner] = now
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.CommandTag{}, fmt.Errorf("the world has no answer for %q", sql)
}

func (w *world) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	w.sent = append(w.sent, sql)
	if err := w.fault(sql); err != nil {
		return row{err: err}
	}
	switch {
	case strings.Contains(sql, "EXISTS (SELECT 1 FROM files"):
		id := args[0].(object.ID)
		return row{values: []any{w.referenced(id)}}
	case strings.Contains(sql, "count(*) FROM files WHERE deleted_at"):
		return row{values: []any{len(w.trashed(args[0].(time.Time)))}}
	case strings.Contains(sql, "count(*) FROM stars"):
		return row{values: []any{len(w.staleStars())}}
	case strings.Contains(sql, "count(*) FROM events"):
		return row{values: []any{int64(len(w.olderThan(args[0].(time.Time))))}}
	case strings.Contains(sql, "SUM(size_bytes)"):
		return row{values: []any{w.held(args[0].(string))}}
	case strings.Contains(sql, "INSERT INTO space_usage"):
		owner, delta := args[0].(string), args[1].(int64)
		w.usage[owner] = max(0, w.usage[owner]+delta)
		return row{values: []any{w.usage[owner]}}
	case strings.Contains(sql, "INSERT INTO events"):
		e := eventRow{id: int64(len(w.events) + 1), owner: args[0].(string), created: aMoment}
		e.action, _ = args[2].(string)
		w.events = append(w.events, e)
		return row{values: []any{e.id}}
	case strings.Contains(sql, "COALESCE((SELECT bytes FROM space_usage"):
		return row{values: []any{w.usage[args[0].(string)]}}
	}
	return row{err: fmt.Errorf("the world has no answer for %q", sql)}
}

func (w *world) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	w.sent = append(w.sent, sql)
	if err := w.fault(sql); err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(sql, "SELECT owner, object_id FROM files"):
		return w.walk(w.files, args[0].(string)), nil
	case strings.Contains(sql, "SELECT owner, object_id FROM file_versions"):
		return w.walk(w.versions, args[0].(string)), nil
	case strings.Contains(sql, "DELETE FROM files WHERE deleted_at"):
		gone := w.trashed(args[0].(time.Time))
		w.files = slices.DeleteFunc(w.files, func(f fileRow) bool { return slices.Contains(gone, f) })
		out := &rows{}
		for _, f := range gone {
			out.add(f.owner, f.path, f.id, f.size)
		}
		return out, nil
	case strings.Contains(sql, "DELETE FROM stars"):
		gone := w.staleStars()
		w.stars = slices.DeleteFunc(w.stars, func(s starRow) bool { return slices.Contains(gone, s) })
		out := &rows{}
		for _, s := range gone {
			out.add(s.owner)
		}
		return out, nil
	case strings.Contains(sql, "SELECT owner, bytes FROM space_usage"):
		out := &rows{}
		for _, owner := range slices.Sorted(keysOf(w.usage)) {
			if owner > args[0].(string) {
				out.add(owner, w.usage[owner])
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("the world has no answer for %q", sql)
}

// referenced is the union internal/store asks over.
func (w *world) referenced(id object.ID) bool {
	for _, f := range slices.Concat(w.files, w.versions) {
		if f.id == id {
			return true
		}
	}
	return false
}

// trashed answers the rows deleted before the cutoff.
func (w *world) trashed(cutoff time.Time) []fileRow {
	var out []fileRow
	for _, f := range w.files {
		if f.deleted != nil && f.deleted.Before(cutoff) {
			out = append(out, f)
		}
	}
	return out
}

// staleStars answers the stars whose path has no row at all, trashed
// included: a trashed object is restorable, so its star is kept.
func (w *world) staleStars() []starRow {
	var out []starRow
	for _, s := range w.stars {
		if !slices.ContainsFunc(w.files, func(f fileRow) bool { return f.owner == s.owner && f.path == s.path }) {
			out = append(out, s)
		}
	}
	return out
}

// olderThan answers the log rows past the window.
func (w *world) olderThan(before time.Time) []eventRow {
	var out []eventRow
	for _, e := range w.events {
		if e.created.Before(before) {
			out = append(out, e)
		}
	}
	return out
}

// held sums the rows that hold a space's bytes, which is what the ledger is
// checked against.
func (w *world) held(owner string) int64 {
	var bytes int64
	for _, f := range slices.Concat(w.files, w.versions) {
		if f.owner == owner {
			bytes += f.size
		}
	}
	return bytes
}

// walk answers one keyset page of a table, ordered by object id.
func (w *world) walk(table []fileRow, cursor string) *rows {
	held := slices.Clone(table)
	slices.SortFunc(held, func(a, b fileRow) int { return strings.Compare(string(a.id), string(b.id)) })
	out := &rows{}
	for _, f := range held {
		if string(f.id) > cursor {
			out.add(f.owner, f.id)
		}
	}
	return out
}

// actions answers every action the log holds, in the order it was appended.
func (w *world) actions() []string {
	out := make([]string, 0, len(w.events))
	for _, e := range w.events {
		out = append(out, e.action)
	}
	return out
}

// keysOf answers a map's keys as a sequence.
func keysOf(m map[string]int64) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// row is one answered row, or the failure the world had.
type row struct {
	values []any
	err    error
}

func (r row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assign(dest, r.values)
}

// rows is a page of answered rows.
type rows struct {
	pgx.Rows
	held   [][]any
	at     int
	err    error
	closed bool
}

func (r *rows) add(values ...any) { r.held = append(r.held, values) }

func (r *rows) Next() bool {
	r.at++
	return r.at <= len(r.held)
}

func (r *rows) Scan(dest ...any) error { return assign(dest, r.held[r.at-1]) }
func (r *rows) Err() error             { return r.err }
func (r *rows) Close()                 { r.closed = true }

// assign writes the values into the destinations a scan was given.
func assign(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("the row holds %d values and the scan asked for %d", len(values), len(dest))
	}
	for i, v := range values {
		target := reflect.ValueOf(dest[i]).Elem()
		value := reflect.ValueOf(v)
		if !value.Type().AssignableTo(target.Type()) {
			return fmt.Errorf("value %d is a %s and the scan asked for a %s", i, value.Type(), target.Type())
		}
		target.Set(value)
	}
	return nil
}
