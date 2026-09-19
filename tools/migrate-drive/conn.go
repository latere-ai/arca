// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Rows is one result set, read forward and once.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// Conn is what the copy needs of a database. Every decision the tool makes is
// taken above this interface, so the unit tier drives the whole copy against a
// fake and the store tier drives the same code against two Postgres databases.
type Conn interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
	// Tx runs fn in one transaction, commits when it returns nil and rolls
	// back otherwise. The rollback runs on a context the caller's cancellation
	// does not reach, because a rollback on a cancelled context hands the
	// connection back to the pool inside an open transaction.
	Tx(ctx context.Context, fn func(Conn) error) error
	Close()
}

// querier is the half of pgx that a pool and a transaction answer alike.
type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// statements adapts pgx's two statement calls to Conn's. It is the whole of
// the driver seam: no statement text and no decision lives here.
type statements struct{ q querier }

// Query hands the result set to the caller, which is what makes this the seam
// and not a reader: every walk of a result set in this command is `each`, and
// `each` defers the close.
func (s statements) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := s.q.Query(ctx, sql, args...) //nolint:sqlclosecheck // the caller closes; each() defers it
	if err != nil {
		return nil, fmt.Errorf("migrate-drive: query: %w", err)
	}
	return rows, nil
}

func (s statements) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := s.q.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("migrate-drive: exec: %w", err)
	}
	return tag.RowsAffected(), nil
}

// pool is one database.
type pool struct {
	statements
	p *pgxpool.Pool
}

// openPool opens a pool and proves the database answers, so a wrong URL is one
// error at the start of the run rather than a failure part way through a copy.
func openPool(ctx context.Context, rawURL string) (Conn, error) {
	p, err := pgxpool.New(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("migrate-drive: open %s: %w", redact(rawURL), err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("migrate-drive: reach %s: %w", redact(rawURL), err)
	}
	return &pool{statements{p}, p}, nil
}

func (p *pool) Close() { p.p.Close() }

func (p *pool) Tx(ctx context.Context, fn func(Conn) error) error {
	tx, err := p.p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate-drive: begin: %w", err)
	}
	if err := fn(inTx{statements{tx}}); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate-drive: commit: %w", err)
	}
	return nil
}

// inTx is a Conn inside a transaction. It opens no second transaction: the
// copy takes one per table and the plan is what holds that rule.
type inTx struct{ statements }

func (inTx) Close() {}

func (t inTx) Tx(_ context.Context, fn func(Conn) error) error { return fn(t) }

// redact is a connection string with its password removed, which is what an
// error message carries. A run prints the database it could not reach, and a
// password in a log is a password in a log.
func redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "the database"
	}
	return u.Redacted()
}
