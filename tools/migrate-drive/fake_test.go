// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fake is a database small enough to hold in a test. It answers each query
// with the rows a case put behind a piece of the query's text and records
// every statement the copy wrote, which is what a case reads the rewrite off:
// the tool's decisions are all above the Conn interface, and what the SQL
// means against Postgres is the store tier's.
type fake struct {
	answers []answer
	sent    []sent
	txs     int
	// queryErr fails the first query holding the text.
	queryErr map[string]error
	// rowsErr breaks the result set of the query holding the text, which the
	// walk reads after its last row.
	rowsErr map[string]error
	// execErr fails the first statement holding the text.
	execErr map[string]error
	// beginErr fails the next transaction.
	beginErr error
}

// answer is the rows one query reads.
type answer struct {
	match string
	rows  [][]any
}

// sent is one statement the copy wrote.
type sent struct {
	sql  string
	args []any
}

func newFake(answers ...answer) *fake {
	return &fake{
		answers:  answers,
		queryErr: map[string]error{},
		rowsErr:  map[string]error{},
		execErr:  map[string]error{},
	}
}

func (f *fake) Query(_ context.Context, sql string, _ ...any) (Rows, error) {
	for text, err := range f.queryErr {
		if strings.Contains(sql, text) {
			delete(f.queryErr, text)
			return nil, err
		}
	}
	for _, a := range f.answers {
		if !strings.Contains(sql, a.match) {
			continue
		}
		rows := &fakeRows{rows: a.rows, i: -1}
		for text, err := range f.rowsErr {
			if strings.Contains(sql, text) {
				rows.err = err
			}
		}
		return rows, nil
	}
	return nil, fmt.Errorf("fake: no case answers %q", collapse(sql))
}

func (f *fake) Exec(_ context.Context, sql string, args ...any) (int64, error) {
	for text, err := range f.execErr {
		if strings.Contains(sql, text) {
			delete(f.execErr, text)
			return 0, err
		}
	}
	f.sent = append(f.sent, sent{collapse(sql), args})
	return 1, nil
}

func (f *fake) Tx(_ context.Context, fn func(Conn) error) error {
	if f.beginErr != nil {
		err := f.beginErr
		f.beginErr = nil
		return err
	}
	f.txs++
	return fn(f)
}

func (f *fake) Close() {}

// wrote answers every statement holding the text, with its arguments.
func (f *fake) wrote(text string) []sent {
	var found []sent
	for _, s := range f.sent {
		if strings.Contains(s.sql, text) {
			found = append(found, s)
		}
	}
	return found
}

// one answers the single statement holding the text, and fails the case when
// there is not exactly one.
func (f *fake) one(t *testing.T, text string) sent {
	t.Helper()
	found := f.wrote(text)
	if len(found) != 1 {
		t.Fatalf("statements holding %q: %d, want 1", text, len(found))
	}
	return found[0]
}

// fakeRows walks the rows of one answer.
type fakeRows struct {
	rows [][]any
	i    int
	err  error
}

func (r *fakeRows) Next() bool {
	r.i++
	return r.i < len(r.rows)
}

func (r *fakeRows) Err() error { return r.err }

func (r *fakeRows) Close() {}

// Scan assigns each column to its destination, converting where Go would.
// A nil column is the zero of its destination, which is how a case writes a
// NULL.
func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.i]
	if len(dest) != len(row) {
		return fmt.Errorf("fake: the row holds %d columns and the scan takes %d", len(row), len(dest))
	}
	for i, d := range dest {
		into := reflect.ValueOf(d).Elem()
		if row[i] == nil {
			into.SetZero()
			continue
		}
		v := reflect.ValueOf(row[i])
		if !v.Type().AssignableTo(into.Type()) {
			if !v.Type().ConvertibleTo(into.Type()) {
				return fmt.Errorf("fake: column %d is %T and the scan takes %T", i, row[i], d)
			}
			v = v.Convert(into.Type())
		}
		into.Set(v)
	}
	return nil
}

// collapse puts a statement on one line, so a case matches text across the
// line breaks the SQL is written with.
func collapse(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// ptr is a pointer to a value, which is how a case writes a column that is
// nullable and not null.
func ptr[T any](v T) *T { return &v }

// at is a fixed time, so a case that compares an argument has one to compare.
var at = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// testRewriter is the rewrite every case reads, over the issuer the family's
// tests use and one mapped organization.
func testRewriter() Rewriter {
	return NewRewriter("https://issuer.example", "", map[string]string{
		"11111111-1111-4111-8111-111111111111": "https://issuer.example|org-acme",
	})
}

// testRun answers a run over one fake, which is both databases. A copy reads
// the source and only writes the target, so a case that reads what was written
// does not care that one object answered both.
func testRun(f *fake, dryRun bool) *Run { return testRunPair(f, f, dryRun) }

// testRunPair answers a run over a source and a target the case holds apart,
// which is what a preflight and a verification need: the two databases answer
// the same question with different numbers.
func testRunPair(source, target *fake, dryRun bool) *Run {
	return NewRun(source, target, testRewriter(), "drive/", dryRun, NewReport(TableNames()))
}

// emptyTarget is the fresh database of decision 1 of spec 019: every table of
// the plan holds no rows.
func emptyTarget() *fake {
	return newFake(answer{"count(*) FROM", [][]any{{int64(0)}}})
}
