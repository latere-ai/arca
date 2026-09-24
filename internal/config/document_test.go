// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"testing"
)

// page reads docs/configuration.md, resolved from this file rather than from
// the working directory, because the tempdir gate runs the suite from an
// empty one.
func page(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test cannot find its own source")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// read is every variable Load and Database ask the environment for. Every
// read in this package is unconditional, so an empty environment reaches
// them all even though it loads nothing.
func read() []string {
	seen := map[string]bool{}
	record := func(k string) string { seen[k] = true; return "" }
	_, _ = Load(record)
	_, _ = Database(record)
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// row matches the first cell of a reference table: a backticked variable
// name opening a line of the page.
var row = regexp.MustCompile("(?m)^\\| `([A-Z][A-Z0-9_]*)` \\|")

// TestTheReferenceNamesEveryVariable holds docs/configuration.md to the code
// in both directions: a variable the configuration reads has a row, and a row
// names a variable the configuration reads. The page is written by hand, so
// this is what keeps a new variable from shipping undocumented and a removed
// one from staying on the page.
func TestTheReferenceNamesEveryVariable(t *testing.T) {
	body := page(t)
	rows := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(body, -1) {
		rows[m[1]] = true
	}
	names := read()
	if len(names) < 20 {
		t.Fatalf("the configuration read %d variables, which is not the configuration: %v", len(names), names)
	}
	for _, name := range names {
		if !rows[name] {
			t.Errorf("the configuration reads %s and docs/configuration.md has no row for it", name)
		}
	}
	for name := range rows {
		if !slices.Contains(names, name) {
			t.Errorf("docs/configuration.md has a row for %s, which the configuration does not read", name)
		}
	}
}
