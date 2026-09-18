// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package store is the database half of the two stores: every fact Arca
// knows that is not bytes. The database decides existence (spec 001,
// invariant 2), so a query here answers whether there is a file and the
// bucket is asked only afterwards.
//
// Queries are SQL text with numbered parameters, no ORM and no generator.
// Every query function takes a Querier, so one function serves a caller
// inside a transaction and one outside it.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is what a query needs. A pool and a transaction both satisfy it,
// so no query function has two versions.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// pool is what DB needs of a connection pool. *pgxpool.Pool satisfies it,
// and a test passes its own.
type pool interface {
	Querier
	Begin(context.Context) (pgx.Tx, error)
	Ping(context.Context) error
	Close()
}

// DB is the pool and the transactions over it.
type DB struct{ pool pool }

// ErrConflict is a write refused by a unique constraint: the row is already
// there. A caller tells it from a fault, which is a 500 and never a 409.
var ErrConflict = errors.New("store: the row already exists")

// Open opens the pool. It dials nothing: the first query opens the first
// connection, so a database that is briefly unreachable at start-up delays
// readiness rather than crashing the process.
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: ARCA_DATABASE_URL: %w", err)
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open the pool: %w", err)
	}
	return &DB{pool: p}, nil
}

// Querier answers the pool as a querier, for a caller outside a
// transaction.
func (db *DB) Querier() Querier { return db.pool }

// Ping is the readiness check of spec 002: the database answers.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Close returns every connection.
func (db *DB) Close() { db.pool.Close() }

// Tx runs fn in one transaction, commits when it returns nil, and rolls back
// otherwise.
//
// The rollback runs on a context that outlives the caller's: a rollback on a
// cancelled context is a no-op that hands the connection back to the pool
// still inside an open transaction, and the next caller of that connection
// inherits it.
func (db *DB) Tx(ctx context.Context, fn func(Querier) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rollback := tx.Rollback(context.WithoutCancel(ctx)); rollback != nil && !errors.Is(rollback, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("store: roll back: %w", rollback))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// conflict reports whether the database refused the write because the row is
// already there, which is SQLSTATE 23505.
func conflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// classify maps a write refusal a caller branches on and wraps the rest. A
// transient fault must never reach a caller as a conflict, any more than as
// a missing row.
func classify(what string, err error) error {
	if conflict(err) {
		return fmt.Errorf("store: %s: %w", what, ErrConflict)
	}
	return fmt.Errorf("store: %s: %w", what, err)
}

// missing reports whether a read found no row, which the caller tells from
// every other failure.
func missing(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// undefinedTable reports whether the relation does not exist, which is
// SQLSTATE 42P01 and is what a database with no migrations applied answers.
func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
