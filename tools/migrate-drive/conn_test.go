// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAConnectionStringLosesItsPassword(t *testing.T) {
	got := redact("postgres://arca:hunter2@db.example:5432/arca?sslmode=disable")
	if strings.Contains(got, "hunter2") {
		t.Errorf("redact left the password in %q", got)
	}
	if !strings.Contains(got, "db.example") {
		t.Errorf("redact left no database to name: %q", got)
	}
	if got := redact("postgres://%zz"); got != "the database" {
		t.Errorf("a URL that does not parse reads as %q", got)
	}
}

func TestADatabaseThatDoesNotAnswerIsAnError(t *testing.T) {
	if _, err := openPool(t.Context(), unreachable); err == nil {
		t.Fatal("a pool over a port nothing listens on opened")
	}
	if _, err := openPool(t.Context(), "postgres://\x00"); err == nil {
		t.Fatal("a connection string that does not parse opened")
	}
}

// TestTheDriverSeamCarriesTheDriversErrors drives the adapter over a pool
// that reaches nothing, which is the one thing about it a unit tier can
// prove: every failure arrives as an error of this command and not as a
// panic. What the statements mean is the store tier's.
func TestTheDriverSeamCarriesTheDriversErrors(t *testing.T) {
	// pgxpool.New does not connect, so the failure lands on the statement.
	p, err := pgxpool.New(t.Context(), unreachable)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer p.Close()
	c := &pool{statements{p}, p}

	if _, err := c.Query(t.Context(), "SELECT 1"); err == nil {
		t.Error("a query against nothing succeeded")
	} else if !strings.HasPrefix(err.Error(), "migrate-drive: query:") {
		t.Errorf("the error is %v", err)
	}
	if _, err := c.Exec(t.Context(), "SELECT 1"); err == nil {
		t.Error("a statement against nothing succeeded")
	} else if !strings.HasPrefix(err.Error(), "migrate-drive: exec:") {
		t.Errorf("the error is %v", err)
	}
	if err := c.Tx(t.Context(), func(Conn) error { return nil }); err == nil {
		t.Error("a transaction against nothing began")
	} else if !strings.HasPrefix(err.Error(), "migrate-drive: begin:") {
		t.Errorf("the error is %v", err)
	}
}

func TestATransactionOpensNoSecondTransaction(t *testing.T) {
	// The copy takes one transaction per table, and the plan is what holds
	// that rule; a Conn inside a transaction runs the body where it is.
	inner := inTx{}
	reached := false
	if err := inner.Tx(t.Context(), func(c Conn) error {
		reached = true
		if c != Conn(inner) {
			t.Error("a nested transaction handed out another connection")
		}
		return nil
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !reached {
		t.Error("the body did not run")
	}
	inner.Close()
}
