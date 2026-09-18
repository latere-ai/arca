// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"

	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// scheme pgxmigrate selects the driver by

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/arca/internal/store/migrations"
)

// Migrate applies every pending migration and returns. It is the bring-up
// every service in the family runs: the embedded source, a retried database
// open so a rolling deploy that briefly holds every connection slot does not
// crash the incoming pod, and the pool the migrator opened for itself closed
// afterwards.
func Migrate(databaseURL string) error {
	if err := pgxmigrate.Up(migrateURL(databaseURL), migrations.Files, "."); err != nil {
		return fmt.Errorf("store: apply the migrations: %w", err)
	}
	return nil
}

// migrateURL rewrites the connection string for the migrator. Two things
// differ from what the pool reads: the scheme selects the golang-migrate
// driver, and the driver this package registers answers to pgx5; and the
// pool_* parameters pgxpool understands are forwarded to the server as
// settings by anything else, where they fail the connection.
func migrateURL(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return databaseURL
	}
	if u.Scheme == "postgres" || u.Scheme == "postgresql" {
		u.Scheme = "pgx5"
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(k, "pool_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Pending answers the migrations the database has not applied, in the order
// they would be applied. A database with no schema at all answers every
// migration; a database ahead of this binary answers none, because a
// rollback in progress is allowed.
func Pending(ctx context.Context, q Querier) ([]string, error) {
	applied, err := appliedVersion(ctx, q)
	if err != nil {
		return nil, err
	}
	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	return after(applied, files), nil
}

// appliedVersion reads the version golang-migrate records. A database that
// holds no such table has applied nothing, which is version zero.
func appliedVersion(ctx context.Context, q Querier) (int64, error) {
	var version int64
	var dirty bool
	err := q.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	switch {
	case missing(err), undefinedTable(err):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("store: read the applied migration: %w", err)
	case dirty:
		return 0, fmt.Errorf("store: migration %d is marked dirty; the database holds a half applied change", version)
	default:
		return version, nil
	}
}

// migrationFiles answers the embedded migrations in the order of their
// numbers.
func migrationFiles() ([]string, error) {
	entries, err := fs.Glob(migrations.Files, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("store: read the embedded migrations: %w", err)
	}
	sort.Strings(entries)
	return entries, nil
}

// after answers the files whose number is above the applied version.
func after(applied int64, files []string) []string {
	pending := make([]string, 0, len(files))
	for _, name := range files {
		number, err := versionOf(name)
		if err != nil || number <= applied {
			continue
		}
		pending = append(pending, name)
	}
	return pending
}

// versionOf reads the number a migration file leads with.
func versionOf(name string) (int64, error) {
	number, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("store: migration %q carries no number", name)
	}
	version, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: migration %q carries no number: %w", name, err)
	}
	return version, nil
}
