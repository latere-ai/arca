// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The fakes below stand in for a database in the unit tier. They prove what
// the Go half of a query does: the statement it sends, the arguments it
// binds, the error it maps, and the row it scans. What the SQL means is the
// store tier's, which is the only place a transaction's semantics and a
// clamped column can be proved at all.

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

// eventRows answers the rows of the events the case named, in the order of
// eventColumns.
func eventRows(es ...Event) *fakeRows {
	rows := &fakeRows{}
	for _, e := range es {
		rows.scans = append(rows.scans, eventScan(e))
	}
	return rows
}

// eventScan answers the scan of one event row.
func eventScan(e Event) func(dest ...any) error {
	return func(dest ...any) error {
		var detail []byte
		if len(e.Detail) > 0 {
			b, err := marshalDetail(e.Detail)
			if err != nil {
				return err
			}
			detail = b
		}
		return assign(dest, []any{e.ID, e.Owner, nullable(e.Path), e.Action, nullable(e.Actor), detail, e.At})
	}
}

// spaceRows answers the rows of the ledger page the case named.
func spaceRows(ss ...Space) *fakeRows {
	rows := &fakeRows{}
	for _, s := range ss {
		rows.scans = append(rows.scans, func(dest ...any) error {
			return assign(dest, []any{s.Owner, s.Bytes})
		})
	}
	return rows
}

// anEvent is one appendable event, as a case builds it.
func anEvent(owner string, action Action) Event {
	return Event{Owner: owner, Path: "files/reports/q3.pdf", Action: action, Actor: owner, At: aMoment}
}

// aMoment is the one time every case that renders a timestamp reads.
var aMoment = time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC)
