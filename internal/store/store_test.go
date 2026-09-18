// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// conflictError is what Postgres answers when a unique constraint refuses a
// write.
var conflictError = &pgconn.PgError{Code: "23505", Message: `duplicate key value violates unique constraint "files_owner_path_key"`}

func TestOpenReadsTheURLAndDialsNothing(t *testing.T) {
	db, err := Open(t.Context(), "postgres://arca:arca@127.0.0.1:1/arca?sslmode=disable")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.Querier() == nil {
		t.Error("Querier() is nil")
	}
	if err := db.Ping(t.Context()); err == nil {
		t.Error("Ping against a port nothing listens on answered a success")
	}
}

func TestOpenRefusesAURLItCannotRead(t *testing.T) {
	if _, err := Open(t.Context(), "://not a url"); err == nil || !strings.Contains(err.Error(), "ARCA_DATABASE_URL") {
		t.Fatalf("Open = %v, want the error that names the variable", err)
	}
}

func TestThePoolIsClosedAndPinged(t *testing.T) {
	pool := &fakePool{pingErr: errors.New("the database is away")}
	db := &DB{pool: pool}
	if err := db.Ping(t.Context()); err == nil {
		t.Error("Ping did not surface the pool's failure")
	}
	pool.pingErr = nil
	if err := db.Ping(t.Context()); err != nil {
		t.Errorf("Ping = %v", err)
	}
	db.Close()
	if !pool.closed {
		t.Error("Close did not reach the pool")
	}
}

func TestATransactionCommitsWhatItsWorkReturnedNilFor(t *testing.T) {
	tx := &fakeTx{}
	db := &DB{pool: &fakePool{tx: tx}}
	err := db.Tx(t.Context(), func(q Querier) error {
		_, err := q.Exec(t.Context(), "INSERT INTO files DEFAULT VALUES")
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed = %t, rolled back = %t", tx.committed, tx.rolledBack)
	}
	if len(tx.statements) != 1 {
		t.Fatalf("the work ran %d statements", len(tx.statements))
	}
}

func TestATransactionRollsBackOnAContextThatOutlivesTheCallers(t *testing.T) {
	tx := &fakeTx{}
	db := &DB{pool: &fakePool{tx: tx}}
	ctx, cancel := context.WithCancel(t.Context())
	boom := errors.New("the work refused")
	err := db.Tx(ctx, func(Querier) error {
		// The caller's context ends before the work returns, which is what
		// a client hanging up mid request looks like.
		cancel()
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx = %v, want the work's own error", err)
	}
	if !tx.rolledBack || tx.committed {
		t.Fatalf("rolled back = %t, committed = %t", tx.rolledBack, tx.committed)
	}
	if tx.rollbackCtx.Err() != nil {
		t.Fatalf("the rollback ran on a cancelled context: %v", tx.rollbackCtx.Err())
	}
}

func TestATransactionSurfacesEveryWayItCanFail(t *testing.T) {
	boom := errors.New("the work refused")
	t.Run("the begin fails", func(t *testing.T) {
		db := &DB{pool: &fakePool{beginErr: errors.New("no connection")}}
		if err := db.Tx(t.Context(), func(Querier) error { return nil }); err == nil ||
			!strings.Contains(err.Error(), "begin") {
			t.Fatalf("Tx = %v", err)
		}
	})
	t.Run("the commit fails", func(t *testing.T) {
		db := &DB{pool: &fakePool{tx: &fakeTx{commitErr: errors.New("the server went away")}}}
		if err := db.Tx(t.Context(), func(Querier) error { return nil }); err == nil ||
			!strings.Contains(err.Error(), "commit") {
			t.Fatalf("Tx = %v", err)
		}
	})
	t.Run("the rollback fails", func(t *testing.T) {
		db := &DB{pool: &fakePool{tx: &fakeTx{rollbackErr: errors.New("the server went away")}}}
		err := db.Tx(t.Context(), func(Querier) error { return boom })
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "roll back") {
			t.Fatalf("Tx = %v, want both the work's error and the rollback's", err)
		}
	})
	t.Run("the transaction was already closed", func(t *testing.T) {
		db := &DB{pool: &fakePool{tx: &fakeTx{rollbackErr: pgx.ErrTxClosed}}}
		if err := db.Tx(t.Context(), func(Querier) error { return boom }); !errors.Is(err, boom) ||
			strings.Contains(err.Error(), "roll back") {
			t.Fatalf("Tx = %v, want the work's error alone", err)
		}
	})
}

func TestARefusalIsToldFromAFault(t *testing.T) {
	if err := classify("write a row", conflictError); !errors.Is(err, ErrConflict) {
		t.Errorf("a unique violation surfaced as %v", err)
	}
	other := &pgconn.PgError{Code: "08006", Message: "the connection failed"}
	if err := classify("write a row", other); errors.Is(err, ErrConflict) {
		t.Errorf("a connection failure surfaced as a conflict: %v", err)
	}
	if !missing(pgx.ErrNoRows) || missing(other) {
		t.Error("a missing row is not told from a fault")
	}
	if !undefinedTable(&pgconn.PgError{Code: "42P01"}) || undefinedTable(other) {
		t.Error("a missing relation is not told from a fault")
	}
}
