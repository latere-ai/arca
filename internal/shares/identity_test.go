// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHandlerReadsAClaimForMeaning is criterion 11 of spec 008, beside the
// identity gate's own rule: a grantee is a subject the authorizer names, and
// a package that reads a membership claim, a role, or an address is one that
// decides what the authorizer decides.
//
// It reads this package's own files rather than a build tag or a linter
// configuration, because the rule is about what the source says.
func TestNoHandlerReadsAClaimForMeaning(t *testing.T) {
	// The words that would mean a claim was read for meaning. They are
	// spelled in pieces so this test does not match itself.
	// The quoted forms catch a claim read by name out of a map, which is
	// what criterion 11 names; the quotes keep prose in a comment from
	// matching.
	forbidden := []string{
		"org" + "_id", "princip" + "al_type", "claims.Rol" + "es", "claims.Ema" + "il",
		`"ema` + `il"`, `"rol` + `es"`, `"org` + `_id"`,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the package has no files")
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, word := range forbidden {
			if strings.Contains(string(raw), word) {
				t.Errorf("%s names %q; a grantee is a subject and no claim is read here", name, word)
			}
		}
	}
}
