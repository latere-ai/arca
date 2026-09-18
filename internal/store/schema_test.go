// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/store/migrations"
)

// The sweep of spec 010 deletes bytes no row names, so the statement it asks
// and the schema it asks over have to say the same thing. This file holds
// the two together.
//
// It exists because the service Arca replaces got it wrong: its
// StorageKeyReferenced read files and file_versions while its own migration
// called the upload session row the only durable pointer to an upload's
// parts, so an in-flight multipart's bytes were one grace window away from
// being deleted under it. Arca's grace window is shorter than Drive's, so
// the omission would be reachable here, and a comment is not what keeps it
// out of reach.

func TestObjectReferencedNamesEveryTableThatHoldsAnObjectID(t *testing.T) {
	schema := tablesWithColumn(t, "object_id")
	if len(schema) < 2 {
		t.Fatalf("the schema holds %v, and files and file_versions both carry an object id", schema)
	}
	if missing := tablesMissing(schema, objectReferencedSQL); len(missing) > 0 {
		t.Fatalf("the reference check leaves out %v, and bytes a row of those tables names would be deleted", missing)
	}
}

// TestTheGuardFindsThePredecessorsGap holds the comparison above to what it
// claims to do, with the predecessor's own omission as the fixture: three
// tables in the schema and a statement naming two is the bug, and naming
// three is the fix.
func TestTheGuardFindsThePredecessorsGap(t *testing.T) {
	const drive = `
		SELECT EXISTS (SELECT 1 FROM files WHERE object_id = $1)
		    OR EXISTS (SELECT 1 FROM file_versions WHERE object_id = $1)`
	const whole = drive + `
		    OR EXISTS (SELECT 1 FROM upload_sessions WHERE object_id = $1)`
	schema := []string{"files", "file_versions", "upload_sessions"}

	if missing := tablesMissing(schema, drive); !slices.Equal(missing, []string{"upload_sessions"}) {
		t.Fatalf("the guard read the predecessor's check as leaving out %v", missing)
	}
	if missing := tablesMissing(schema, whole); len(missing) > 0 {
		t.Fatalf("the guard read the whole union as leaving out %v", missing)
	}
}

// createTable reads one table out of the schema: its name and the columns
// between its parentheses.
var createTable = regexp.MustCompile(`(?s)CREATE TABLE (\w+) \((.*?)\n\);`)

// tablesWithColumn answers the tables the embedded schema gives the column,
// in the order the migrations create them.
func tablesWithColumn(t *testing.T, column string) []string {
	t.Helper()
	names, err := fs.Glob(migrations.Files, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	var tables []string
	for _, name := range names {
		source, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createTable.FindAllStringSubmatch(string(source), -1) {
			for line := range strings.SplitSeq(m[2], "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), column+" ") {
					tables = append(tables, m[1])
					break
				}
			}
		}
	}
	return tables
}

// tablesMissing answers the tables of the schema the statement does not
// name.
func tablesMissing(schema []string, statement string) []string {
	var missing []string
	for _, table := range schema {
		if !strings.Contains(statement, "FROM "+table+" ") {
			missing = append(missing, table)
		}
	}
	return missing
}
