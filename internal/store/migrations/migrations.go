// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package migrations embeds the schema of Arca, one file per owning spec.
// The files are applied in the order of their numbers and are never edited
// after they ship: a change to a shipped table is a new file.
package migrations

import "embed"

// Files holds the migrations, read through the iofs source of golang-migrate.
//
//go:embed *.sql
var Files embed.FS
