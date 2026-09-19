// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"strings"
	"testing"
)

// A column the copy cannot read is a copy that stops, not a row it writes
// half of. The cases below give one table a row of the wrong shape, which is
// what a schema older than this tool would answer, and read the error back.

func TestARowTheCopyCannotReadStopsItsTable(t *testing.T) {
	for _, match := range []string{
		"FROM principal_directory", "FROM files ORDER BY id", "FROM file_versions ORDER BY id",
		"FROM stars ORDER BY", "FROM upload_sessions ORDER BY id", "FROM shares ORDER BY id",
		"FROM workspaces ORDER BY id", "FROM workspace_attachments ORDER BY id",
		"FROM events ORDER BY id",
	} {
		f := source()
		replace(f, match, [][]any{{"one column"}})
		err := testRun(f, false).Copy(t.Context())
		if err == nil {
			t.Errorf("%s over a row of one column succeeded", match)
			continue
		}
		if !strings.Contains(err.Error(), "the scan takes") {
			t.Errorf("%s answered %v", match, err)
		}
	}
}

func TestARowThePreflightCannotReadStopsTheRun(t *testing.T) {
	for _, match := range []string{
		"SELECT DISTINCT owner_id", "split_part(path, '/', 1)",
		"HAVING count(*) > 1", "strpos(storage_key",
	} {
		f := clean()
		replace(f, match, [][]any{{"a", "b", "c", "d"}})
		err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false))
		if err == nil {
			t.Errorf("%s over a row of four columns succeeded", match)
			continue
		}
		if !strings.Contains(err.Error(), "the scan takes") {
			t.Errorf("%s answered %v", match, err)
		}
	}
}

func TestAResultSetThatBreaksPartWayIsAnErrorAndNotAShortCopy(t *testing.T) {
	broken := errors.New("the result set broke")
	f := source()
	f.rowsErr["FROM files ORDER BY id"] = broken
	if err := testRun(f, false).Copy(t.Context()); !errors.Is(err, broken) {
		t.Fatalf("Copy answered %v, want the result set's error", err)
	}

	p := clean()
	p.rowsErr["split_part(path, '/', 1)"] = broken
	if err := Preflight(t.Context(), testRunPair(p, emptyTarget(), false)); !errors.Is(err, broken) {
		t.Fatalf("Preflight answered %v, want the result set's error", err)
	}
}
