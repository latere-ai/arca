// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoMappingFileIsAnEmptyMapping(t *testing.T) {
	m, err := ReadMapping("")
	if err != nil {
		t.Fatalf("ReadMapping: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("the mapping holds %d rows, want 0", len(m))
	}
}

func TestAMissingMappingFileIsAnError(t *testing.T) {
	if _, err := ReadMapping(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a file that is not there is an error")
	}
}

func TestTheMappingReadsAsJSON(t *testing.T) {
	m := readMappingFile(t, "orgs.json", `
		{
		  "11111111-1111-4111-8111-111111111111": "https://issuer.example|org-acme",
		  "22222222-2222-4222-8222-222222222222": "https://issuer.example|org-bolt"
		}`)
	if got := m["11111111-1111-4111-8111-111111111111"]; got != "https://issuer.example|org-acme" {
		t.Errorf("the first row is %q", got)
	}
	if len(m) != 2 {
		t.Errorf("the mapping holds %d rows, want 2", len(m))
	}
}

func TestTheMappingReadsAsCSVWithAHeader(t *testing.T) {
	m := readMappingFile(t, "orgs.csv",
		"organization,subject\n11111111-1111-4111-8111-111111111111,https://issuer.example|org-acme\n")
	if len(m) != 1 || m["11111111-1111-4111-8111-111111111111"] != "https://issuer.example|org-acme" {
		t.Errorf("the mapping is %v", m)
	}
}

func TestTheMappingReadsAsCSVWithoutAHeader(t *testing.T) {
	m := readMappingFile(t, "orgs.csv",
		"11111111-1111-4111-8111-111111111111,https://issuer.example|org-acme\n\n")
	if len(m) != 1 {
		t.Errorf("the mapping holds %d rows, want 1", len(m))
	}
}

func TestTheShapeIsReadFromTheContentAndNotTheName(t *testing.T) {
	// A JSON mapping saved as .csv still reads, because an operator who
	// renames the export should not read a parse error instead of a copy.
	m := readMappingFile(t, "orgs.csv", ` {"abc": "https://issuer.example|org-acme"} `)
	if m["abc"] != "https://issuer.example|org-acme" {
		t.Errorf("the mapping is %v", m)
	}
}

func TestAMappingThatNamesNothingIsRefused(t *testing.T) {
	for _, c := range []struct{ name, body, holds string }{
		{"orgs.json", `{"abc": ""}`, "empty subject"},
		{"orgs.json", `{"": "s"}`, "id is empty"},
		{"orgs.json", `["abc"]`, "not an object"},
		{"orgs.csv", "abc\n", "a row is organization,subject"},
		{"orgs.csv", "abc,\n", "empty subject"},
		{"orgs.csv", "\"abc\ndef\n", "does not parse"},
	} {
		path := filepath.Join(t.TempDir(), c.name)
		if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		err := errFrom(ReadMapping(path))
		if err == nil {
			t.Fatalf("%s holding %q is an error", c.name, c.body)
		}
		if !strings.Contains(err.Error(), c.holds) {
			t.Errorf("%q: the error is %v, and holds neither %q", c.body, err, c.holds)
		}
	}
}

func readMappingFile(t *testing.T, name, body string) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMapping(path)
	if err != nil {
		t.Fatalf("ReadMapping: %v", err)
	}
	return m
}

func errFrom(_ map[string]string, err error) error { return err }
