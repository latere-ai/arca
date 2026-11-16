// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"flag"
	"slices"
	"testing"

	"latere.ai/x/pkg/statemachine/statemachinetest"
)

var update = flag.Bool("update", false, "rewrite the formal models under formal/")

// TestFormalModels holds formal/, which the lean and tla gates check, to the
// lifecycle tables.
func TestFormalModels(t *testing.T) {
	statemachinetest.Models(t, "../../formal", *update, GrantTable.Graph(), FileTable.Graph())
}

// TestEveryStatusIsInItsTable keeps each status type and its table to one
// set of states, so a status added to one is added to the other.
func TestEveryStatusIsInItsTable(t *testing.T) {
	sameStates(t, "Grant", GrantTable.States(), GrantActive, GrantRevoked)
	sameStates(t, "File", FileTable.States(), FileLive, FilePurged, FileTrashed)
}

func sameStates[S ~string](t *testing.T, name string, states []S, consts ...S) {
	t.Helper()
	want := slices.Sorted(slices.Values(consts))
	if !slices.Equal(states, want) {
		t.Errorf("%s table states %v, constants %v", name, states, want)
	}
}

// FileIn names the rows in the states it is given by their deleted_at, and a
// source condition is the states the table lists before the target.
func TestFileConditionsFollowTheTable(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{FileIn(FileLive), "deleted_at IS NULL"},
		{FileIn(FileTrashed), "deleted_at IS NOT NULL"},
		{FileIn(FileLive, FileTrashed), "true"},
		{FileIn(FilePurged), "false"},
		{FileSources(FileTrashed), "deleted_at IS NULL"},
		{FileSources(FileLive), "deleted_at IS NOT NULL"},
		{FileSources(FilePurged), "true"},
		{fileSourcesOf("files", FileLive), "files.deleted_at IS NOT NULL"},
	} {
		if c.got != c.want {
			t.Errorf("condition %q, want %q", c.got, c.want)
		}
	}
}
