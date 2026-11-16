// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"fmt"
	"strings"

	"latere.ai/x/pkg/statemachine"
)

// The lifecycles the store keeps, each as a statemachine table. Every writer
// of a lifecycle takes its guard from the table: a write that would take a
// record along a step the table lacks changes nothing. The tests drive the
// real writers through each table on Postgres, and formal/ renders the tables
// into the Lean and TLA+ models the lean and tla gates check.

// GrantStatus is the state of a grant, as shares.status holds it.
type GrantStatus string

// The states of a grant.
const (
	GrantActive  GrantStatus = "active"
	GrantRevoked GrantStatus = "revoked"
)

// GrantTable is the lifecycle of a row of shares, held in shares.status.
// MoveGrant is the one writer of the column, and Revoke is its one step. A
// revoked row is kept so an audit can see the grant existed; the reaper
// removes a grant that expired long ago with a delete, which ends the row
// and is not a state. The expiry itself is a clock overlay on an active
// grant, read from expires_at, and no write makes it.
var GrantTable = statemachine.Must(statemachine.New("Grant", []GrantStatus{GrantActive}, map[GrantStatus][]GrantStatus{
	GrantActive:  {GrantRevoked},
	GrantRevoked: nil,
}))

// FileStatus is the state of a row of files.
type FileStatus string

// The states of a file row. Live and trashed are told apart by deleted_at;
// purged is not a stored value: the row is deleted, so the state is the
// absence of the row.
const (
	FileLive    FileStatus = "live"
	FilePurged  FileStatus = "purged"
	FileTrashed FileStatus = "trashed"
)

// FileTable is the lifecycle of a row of files, derived from
// files.deleted_at: null is live, set is trashed. A path holds at most one
// row, so a write to a trashed path revives that row rather than inserting
// beside it.
//
// The writers of the steps are
//
//   - live to trashed: SoftDelete and TrashSubtree;
//   - trashed to live: Restore, RestoreByID, and the revive arm of Upsert and
//     CreateOnly;
//   - live or trashed to purged: HardDelete, DropSubtree, PurgeTrash, the
//     reaper's trash pass and the erase of a space.
//
// A write of content to a live row keeps it live and is not a step. Each
// writer binds its source states to FileSources, so the table is the one
// place that says which states a step may start from.
var FileTable = statemachine.Must(statemachine.New("File", []FileStatus{FileLive}, map[FileStatus][]FileStatus{
	FileLive:    {FileTrashed, FilePurged},
	FileTrashed: {FileLive, FilePurged},
	FilePurged:  nil,
}))

// FileSources is the condition on files.deleted_at that holds for the rows a
// step to the state to may start from, which a writer puts in its WHERE
// clause.
func FileSources(to FileStatus) string {
	return FileIn(FileTable.Sources(to)...)
}

// fileSourcesOf is FileSources for a statement where deleted_at needs its
// table named, such as the arm of an upsert that sees both the row and the
// one proposed.
func fileSourcesOf(table string, to FileStatus) string {
	return strings.ReplaceAll(FileSources(to), "deleted_at", table+".deleted_at")
}

// FileIn is the condition on files.deleted_at that holds for the rows in any
// of the states, for a writer whose step starts from fewer states than the
// table allows, such as emptying the trash. A purged row has no deleted_at
// to read, so purged adds nothing here: a row that is gone cannot be
// selected.
func FileIn(states ...FileStatus) string {
	var live, trashed bool
	for _, from := range states {
		switch from {
		case FileLive:
			live = true
		case FileTrashed:
			trashed = true
		case FilePurged:
			// The row is gone: nothing to select.
		}
	}
	switch {
	case live && trashed:
		return "true"
	case live:
		return "deleted_at IS NULL"
	case trashed:
		return "deleted_at IS NOT NULL"
	default:
		return "false"
	}
}

// startsLive checks that a file row may be inserted in the live state, which
// is the one start the table has. The three inserts of the store call it.
func startsLive() error {
	if err := FileTable.CheckStart(FileLive); err != nil {
		return fmt.Errorf("store: insert a file row: %w", err)
	}
	return nil
}

// sources renders a table's sources of a state as the text values a
// status = ANY($n) condition binds.
func sources[S ~string](states []S) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

// activeGrant is the condition of a grant that has not been revoked, as the
// reads of a live grant spell it.
const activeGrant = "status = '" + string(GrantActive) + "'"
