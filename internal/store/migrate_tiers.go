// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package store

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"latere.ai/x/arca/internal/store/migrations"
)

// MigrateTo applies the embedded migrations up to one version and no
// further. It exists for the tiers alone, which is why the build tag keeps
// it out of the server: a test of a migration seeds the schema an
// installation is on when it meets that file, and a tool written for an
// earlier schema is proved against the schema it writes.
//
// A server never stops part way. [Migrate] is what it runs.
func MigrateTo(databaseURL string, version uint) error {
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return fmt.Errorf("store: read the embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL(databaseURL))
	if err != nil {
		return fmt.Errorf("store: open the migrator: %w", err)
	}
	err = m.Migrate(version)
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	sourceErr, dbErr := m.Close()
	if err = errors.Join(err, sourceErr, dbErr); err != nil {
		return fmt.Errorf("store: apply the migrations up to %d: %w", version, err)
	}
	return nil
}
