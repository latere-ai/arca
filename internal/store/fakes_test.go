// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The fakes below stand in for a database in the unit tier. They prove what
// the Go half of a query does: the statement it sends, the arguments it
// binds, the error it maps, and the row it scans. What the SQL means is
// proved by the store tier against Postgres, which is the only place a
// transaction's semantics and a unique constraint can be proved at all.

// fakeQuerier records what was asked and answers what the case set.
type fakeQuerier struct {
	tag      pgconn.CommandTag
	execErr  error
	row      pgx.Row
	rows     pgx.Rows
	queryErr error

	statements []string
	args       [][]any
}

func (q *fakeQuerier) record(sql string, args []any) {
	q.statements = append(q.statements, sql)
	q.args = append(q.args, args)
}

func (q *fakeQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.record(sql, args)
	return q.tag, q.execErr
}

func (q *fakeQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.record(sql, args)
	return q.rows, q.queryErr
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.record(sql, args)
	return q.row
}

// fakeRow answers one row, or the failure the case set.
type fakeRow struct{ scan func(dest ...any) error }

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

// failing answers a row that scans to the given error.
func failing(err error) fakeRow {
	return fakeRow{scan: func(...any) error { return err }}
}

// values answers a row that scans to the given values, in order.
func values(vs ...any) fakeRow {
	return fakeRow{scan: func(dest ...any) error { return assign(dest, vs) }}
}

// assign writes the values into the destinations a scan was given.
func assign(dest []any, vs []any) error {
	if len(dest) != len(vs) {
		return fmt.Errorf("the row holds %d values and the scan asked for %d", len(vs), len(dest))
	}
	for i, v := range vs {
		target := reflect.ValueOf(dest[i]).Elem()
		value := reflect.ValueOf(v)
		if !value.Type().AssignableTo(target.Type()) {
			return fmt.Errorf("value %d is a %s and the scan asked for a %s", i, value.Type(), target.Type())
		}
		target.Set(value)
	}
	return nil
}

// fakeRows answers a page of rows.
type fakeRows struct {
	pgx.Rows
	scans  []func(dest ...any) error
	at     int
	err    error
	closed bool
}

func (r *fakeRows) Next() bool {
	r.at++
	return r.at <= len(r.scans)
}

func (r *fakeRows) Scan(dest ...any) error { return r.scans[r.at-1](dest...) }
func (r *fakeRows) Err() error             { return r.err }
func (r *fakeRows) Close()                 { r.closed = true }

// rowsOf answers the rows of the files the case named.
func rowsOf(fs ...File) *fakeRows {
	rows := &fakeRows{}
	for _, f := range fs {
		rows.scans = append(rows.scans, fileScan(f))
	}
	return rows
}

// fileScan answers the scan of one file row, in the order of fileColumns.
func fileScan(f File) func(dest ...any) error {
	return func(dest ...any) error {
		return assign(dest, []any{
			f.ID, f.Owner, f.Path, f.ObjectID, f.CreatedBy, f.ContentType, f.SizeBytes,
			f.Checksum, f.ChecksumKind, f.IsPublic, f.DeletedAt, f.CreatedAt, f.UpdatedAt,
		})
	}
}

// fakePool is a pool that hands out one transaction.
type fakePool struct {
	fakeQuerier
	tx       pgx.Tx
	beginErr error
	pingErr  error
	closed   bool
}

func (p *fakePool) Begin(context.Context) (pgx.Tx, error) { return p.tx, p.beginErr }
func (p *fakePool) Ping(context.Context) error            { return p.pingErr }
func (p *fakePool) Close()                                { p.closed = true }

// fakeTx records how the transaction ended and on which context.
type fakeTx struct {
	pgx.Tx
	fakeQuerier
	commitErr   error
	rollbackErr error
	committed   bool
	rolledBack  bool
	rollbackCtx context.Context //nolint:containedctx // the assertion is about the context the rollback ran on
}

func (t *fakeTx) Commit(context.Context) error { t.committed = true; return t.commitErr }

func (t *fakeTx) Rollback(ctx context.Context) error {
	t.rolledBack, t.rollbackCtx = true, ctx
	return t.rollbackErr
}

func (t *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.fakeQuerier.Exec(ctx, sql, args...)
}

func (t *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.fakeQuerier.Query(ctx, sql, args...)
}

func (t *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.fakeQuerier.QueryRow(ctx, sql, args...)
}
