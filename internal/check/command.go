// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"flag"
	"fmt"
	"io"

	"latere.ai/x/arca/internal/config"
)

// Command is `arcad check`, the subcommand cmd/arcad dispatches: load the
// configuration the node loads, run every requirement, print the report, and
// answer the process exit code.
//
// The whole table of spec 002 is read, because a check that ran against a
// different configuration than the server would pass an installation the
// server cannot serve. A value that is missing or malformed therefore fails
// here, before any dependency is reached, with the same one line naming every
// problem that a start-up gives: there is nothing to check about an
// installation that will not start.
func Command(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("arcad check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "arcad: %v\n", err)
		return 1
	}
	return Report(Run(ctx, Options{Config: cfg}), stdout, stderr)
}
