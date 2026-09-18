// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// routeRow matches one row of a route table of spec 013: the method, the
	// path, and the action column.
	routeRow = regexp.MustCompile(`^\| (GET|POST|PUT|PATCH|DELETE|HEAD) \| ` + "`([^`]+)`" + ` \| ([^|]+) \|`)
	// tickedAction matches the action a row names, where it names one.
	tickedAction = regexp.MustCompile("`([a-z]+\\.[a-z]+)`")
)

// routesOfSpec013 reads every route table of specs/013-api.md: each row's
// method and path, mapped to the action its third column names. A row whose
// path carries a query selects a representation of a route already named, so
// the query is dropped and the first row of a path wins, which is the row
// that names the route's action for the method.
func routesOfSpec013(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "specs", "013-api.md"))
	if err != nil {
		t.Fatalf("spec 013: %v", err)
	}
	out := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		m := routeRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		path, _, _ := strings.Cut(m[2], "?")
		key := m[1] + " " + path
		if _, seen := out[key]; seen {
			continue
		}
		action := ""
		if a := tickedAction.FindStringSubmatch(m[3]); a != nil {
			action = a[1]
		}
		out[key] = action
	}
	if len(out) == 0 {
		t.Fatal("spec 013 has no route table")
	}
	return out
}
