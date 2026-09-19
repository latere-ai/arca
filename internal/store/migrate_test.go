// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestTheMigratorReadsAURLOfItsOwn(t *testing.T) {
	for raw, want := range map[string]string{
		"postgres://arca:arca@db:5432/arca?sslmode=disable":         "pgx5://arca:arca@db:5432/arca?sslmode=disable",
		"postgresql://arca@db/arca":                                 "pgx5://arca@db/arca",
		"postgres://arca@db/arca?pool_max_conns=4&sslmode=disable":  "pgx5://arca@db/arca?sslmode=disable",
		"postgres://arca@db/arca?pool_max_conns=4&pool_min_conns=1": "pgx5://arca@db/arca",
		"pgx5://arca@db/arca":                                       "pgx5://arca@db/arca",
		"://not a url":                                              "://not a url",
	} {
		if got := migrateURL(raw); got != want {
			t.Errorf("migrateURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTheEmbeddedSetIsNumberedAndReadable(t *testing.T) {
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the binary embeds no migration")
	}
	if files[0] != "0001_files.up.sql" {
		t.Errorf("the first migration is %q", files[0])
	}
	for _, name := range files {
		if _, err := versionOf(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if strings.HasSuffix(name, ".down.sql") {
			t.Errorf("%s: migrations are forward only", name)
		}
	}
}

func TestAFileWithoutANumberIsNoMigration(t *testing.T) {
	for _, name := range []string{"files.up.sql", "x_files.up.sql"} {
		if _, err := versionOf(name); err == nil {
			t.Errorf("versionOf(%q) answered a number", name)
		}
	}
}

func TestPendingIsWhatTheDatabaseHasNotApplied(t *testing.T) {
	files := []string{"0001_files.up.sql", "0002_uploads.up.sql", "0003_shares.up.sql"}
	for applied, want := range map[int64]int{0: 3, 1: 2, 3: 0, 9: 0} {
		if got := after(applied, files); len(got) != want {
			t.Errorf("after(%d) = %v, want %d files", applied, got, want)
		}
	}
	if got := after(1, files); got[0] != "0002_uploads.up.sql" {
		t.Errorf("the first pending file is %q", got[0])
	}
}

func TestPendingReadsTheAppliedVersion(t *testing.T) {
	t.Run("a database with no schema has applied nothing", func(t *testing.T) {
		q := &fakeQuerier{row: failing(&pgconn.PgError{Code: "42P01"})}
		pending, err := Pending(t.Context(), q)
		if err != nil || len(pending) == 0 {
			t.Fatalf("Pending = %v, %v", pending, err)
		}
	})
	t.Run("a database at the embedded version has nothing pending", func(t *testing.T) {
		files, err := migrationFiles()
		if err != nil {
			t.Fatal(err)
		}
		newest, err := versionOf(files[len(files)-1])
		if err != nil {
			t.Fatal(err)
		}
		q := &fakeQuerier{row: values(newest, false)}
		pending, err := Pending(t.Context(), q)
		if err != nil || len(pending) != 0 {
			t.Fatalf("Pending = %v, %v", pending, err)
		}
	})
	t.Run("a database ahead of this binary has nothing pending", func(t *testing.T) {
		// A rollback in progress is allowed: the old binary has to serve
		// against the new schema while the replicas turn over, and a
		// forward-only migration is written so it can (spec 016). The guard
		// that refuses is the other direction, a database behind the binary,
		// which is spec 004's criterion 2.
		files, err := migrationFiles()
		if err != nil {
			t.Fatal(err)
		}
		newest, err := versionOf(files[len(files)-1])
		if err != nil {
			t.Fatal(err)
		}
		q := &fakeQuerier{row: values(newest+1, false)}
		pending, err := Pending(t.Context(), q)
		if err != nil || len(pending) != 0 {
			t.Fatalf("Pending = %v, %v", pending, err)
		}
	})
	t.Run("an empty version table has applied nothing", func(t *testing.T) {
		q := &fakeQuerier{row: failing(pgx.ErrNoRows)}
		if pending, err := Pending(t.Context(), q); err != nil || len(pending) == 0 {
			t.Fatalf("Pending = %v, %v", pending, err)
		}
	})
	t.Run("a half applied change is named", func(t *testing.T) {
		q := &fakeQuerier{row: values(int64(1), true)}
		_, err := Pending(t.Context(), q)
		if err == nil || !strings.Contains(err.Error(), "dirty") {
			t.Fatalf("Pending = %v", err)
		}
	})
	t.Run("a fault is not a version", func(t *testing.T) {
		boom := errors.New("the connection failed")
		q := &fakeQuerier{row: failing(boom)}
		if _, err := Pending(t.Context(), q); !errors.Is(err, boom) {
			t.Fatalf("Pending = %v", err)
		}
	})
}
