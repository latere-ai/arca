// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package store

import (
	"fmt"

	"latere.ai/x/pkg/pgxmigrate"

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
	dsn, err := pgxmigrate.DriverURL(databaseURL)
	if err != nil {
		return fmt.Errorf("store: read the database url for the migrator: %w", err)
	}
	if err := pgxmigrate.To(dsn, migrations.Files, ".", version); err != nil {
		return fmt.Errorf("store: apply the migrations up to %d: %w", version, err)
	}
	return nil
}
