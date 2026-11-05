// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain builds arcad once for the whole run when ARCA_BINARY is unset, so
// the tier needs no step before it: `go test -tags=tiers ./test/e2e/...` and
// the e2e gate both start from a clean tree. A run that sets ARCA_BINARY, as
// `make test-e2e` does, tests that binary instead.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("ARCA_BINARY") == "" {
		dir, err := os.MkdirTemp("", "arca-e2e-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: make the build directory:", err)
			return 1
		}
		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Fprintln(os.Stderr, "e2e: remove the build directory:", err)
			}
		}()
		path := filepath.Join(dir, "arcad")
		build := exec.Command("go", "build", "-o", path, "latere.ai/x/arca/cmd/arcad")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build arcad: %v\n%s", err, out)
			return 1
		}
		if err := os.Setenv("ARCA_BINARY", path); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: name the binary:", err)
			return 1
		}
	}
	return m.Run()
}
