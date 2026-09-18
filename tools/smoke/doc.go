// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package smoke holds the test for release.sh, the script the release
// pipeline runs against an installation after a rollout and an operator
// runs by hand after an upgrade.
//
// The script is shell so that an operator can read it and run it without a
// Go toolchain; this package is what keeps it honest, because a smoke that
// passes against a version other than the one just deployed is worse than
// no smoke at all.
package smoke
