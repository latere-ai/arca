// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
)

// guide reads docs/api.md, the page a builder reads, which is written by
// hand beside the document this command generates.
func guide(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestTheGuideNamesEveryRoute holds docs/api.md to the rendered document:
// every method and path the document describes is a row of one of the
// guide's tables, written the way the document spells it. The guide is
// prose and the document is generated, so without this a route could ship
// that a builder reading the guide never learns of.
func TestTheGuideNamesEveryRoute(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(Render(), &doc); err != nil {
		t.Fatalf("the document is not YAML: %v", err)
	}
	body := guide(t)
	var missing []string
	for path, methods := range doc.Paths {
		for method := range methods {
			row := "| " + strings.ToUpper(method) + " | `" + path + "` |"
			if !strings.Contains(body, row) {
				missing = append(missing, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("docs/api.md has no row for %s", m)
	}
	if len(doc.Paths) < 25 {
		t.Fatalf("the document describes %d paths, which is not the surface", len(doc.Paths))
	}
}

// TestTheGuideNamesEveryCodeAndAction holds the guide's two vocabularies to
// the code: every error code of the table in internal/api is a row of the
// guide's error table, and every action an authorization endpoint can be
// asked appears in it.
func TestTheGuideNamesEveryCodeAndAction(t *testing.T) {
	body := guide(t)
	for _, e := range api.Errors() {
		if !strings.Contains(body, "| `"+e.Code+"` |") {
			t.Errorf("docs/api.md has no error row for %s", e.Code)
		}
	}
	for _, action := range authorizer.Actions() {
		if !strings.Contains(body, "`"+action+"`") {
			t.Errorf("docs/api.md never names the action %s", action)
		}
	}
}
