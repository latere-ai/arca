// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"
)

// The writers of every lifecycle are driven through its table on Postgres:
// Exhaustive asks each writer for every state from every state, and Race
// starts two writers at once on a record in each state.

var lifecycleSeq atomic.Int64

// lifecycleName is a name no other record of the run has.
func lifecycleName(kind string) string {
	return fmt.Sprintf("%s-%d", kind, lifecycleSeq.Add(1))
}

// unchanged is the error a writer's answer of "nothing to do" becomes: the
// writer answered without error and the record did not move.
func unchanged(what string) error {
	return fmt.Errorf("%s: the record was not in a state before it: %w", what, statemachine.ErrIllegalTransition)
}

// noWriter is the answer for a state no writer takes a record to.
func noWriter(what string) error {
	return fmt.Errorf("%s: no writer takes a record there: %w", what, statemachine.ErrIllegalTransition)
}

func TestStoreGrantWriters(t *testing.T) {
	db := tier(t)
	q := db.Querier()
	d := statemachinetest.Driver[GrantStatus]{
		Enter: func(t *testing.T, s GrantStatus) string {
			g, err := NewShares().Create(t.Context(), q, subjectGrant(alice, "files/"+lifecycleName("g"), bobby, "read"))
			if err != nil {
				t.Fatalf("write the grant: %v", err)
			}
			if s == GrantRevoked {
				if err := NewShares().Revoke(t.Context(), q, g.ID); err != nil {
					t.Fatalf("revoke the grant: %v", err)
				}
			}
			return g.ID
		},
		Move: func(id string, to GrantStatus) error { return MoveGrant(t.Context(), q, id, to) },
		State: func(t *testing.T, id string) GrantStatus {
			g, err := NewShares().Get(t.Context(), q, id)
			if err != nil {
				t.Fatalf("read the grant: %v", err)
			}
			return g.Status
		},
	}
	statemachinetest.Exhaustive(t, GrantTable, d)
	statemachinetest.Race(t, GrantTable, d)
}

// A grant that is gone is not revoked: the writer refuses it as it refuses a
// grant that is not active.
func TestStoreGrantRevokeRefusesAGrantThatIsGone(t *testing.T) {
	db := tier(t)
	err := NewShares().Revoke(t.Context(), db.Querier(), "00000000-0000-7000-8000-000000000000")
	if !errors.Is(err, statemachine.ErrIllegalTransition) {
		t.Errorf("Revoke of a grant that is not there = %v, want ErrIllegalTransition", err)
	}
}

// fileWriters is one way to take a file row to each state, so every writer of
// FileTable runs under Exhaustive and Race: a writer of one step is paired
// with the writers of the others.
type fileWriters struct {
	name string
	// trash, restore and purge take the row at path to a state, and answer
	// whether the row was in a state before it.
	trash   func(t *testing.T, db *DB, path string) (bool, error)
	restore func(t *testing.T, db *DB, path string) (bool, error)
	purge   func(t *testing.T, db *DB, path string) (bool, error)
}

func fileWritersUnderTest() []fileWriters {
	files := NewFiles()
	window := func() time.Time { return time.Now().Add(-24 * time.Hour) }
	softDelete := func(t *testing.T, db *DB, path string) (bool, error) {
		return files.SoftDelete(t.Context(), db.Querier(), alice, path)
	}
	hardDelete := func(t *testing.T, db *DB, path string) (bool, error) {
		_, removed, err := files.HardDelete(t.Context(), db.Querier(), alice, path)
		return removed, err
	}
	restore := func(t *testing.T, db *DB, path string) (bool, error) {
		return files.Restore(t.Context(), db.Querier(), alice, path, window())
	}
	return []fileWriters{
		{name: "by path", trash: softDelete, restore: restore, purge: hardDelete},
		{
			name:  "by id",
			trash: softDelete,
			restore: func(t *testing.T, db *DB, path string) (bool, error) {
				f, err := files.Get(t.Context(), db.Querier(), alice, path)
				if errors.Is(err, pgx.ErrNoRows) {
					return false, nil
				} else if err != nil {
					return false, err
				}
				_, ok, err := files.RestoreByID(t.Context(), db.Querier(), alice, f.ID, window())
				return ok, err
			},
			purge: hardDelete,
		},
		{
			name: "by folder",
			trash: func(t *testing.T, db *DB, path string) (bool, error) {
				out, err := files.TrashSubtree(t.Context(), db.Querier(), alice, folderOf(path), nil)
				return out.Files == 1, err
			},
			restore: restore,
			purge: func(t *testing.T, db *DB, path string) (bool, error) {
				out, err := files.DropSubtree(t.Context(), db.Querier(), alice, folderOf(path), nil)
				return out.Files == 1, err
			},
		},
		{
			name:  "by revive",
			trash: softDelete,
			restore: func(t *testing.T, db *DB, path string) (bool, error) {
				// A write to a trashed path revives that row. A write to a
				// path with no row inserts a new record, which is not a step
				// of this one.
				if _, err := files.Get(t.Context(), db.Querier(), alice, path); errors.Is(err, pgx.ErrNoRows) {
					return false, nil
				} else if err != nil {
					return false, err
				}
				return files.CreateOnly(t.Context(), db.Querier(), aRow(alice, path))
			},
			purge: hardDelete,
		},
	}
}

// sameRecord reports whether the row now at path is the one the test entered.
// A record entered purged has no id, so any row at its path is a new one.
func sameRecord(ids *sync.Map, path, id string) bool {
	entered, ok := ids.Load(path)
	return ok && entered == id
}

// folderOf is the folder holding the one row a test stores at a path.
func folderOf(path string) string { return path[:strings.LastIndex(path, "/")] }

func TestStoreFileWriters(t *testing.T) {
	for _, w := range fileWritersUnderTest() {
		t.Run(w.name, func(t *testing.T) {
			db := tier(t)
			files := NewFiles()
			// ids is the row id each record was entered with. A write that
			// inserts at a path after the record was purged makes a new
			// record, which is not the one under test.
			var ids sync.Map
			d := statemachinetest.Driver[FileStatus]{
				Enter: func(t *testing.T, s FileStatus) string {
					// One folder per record, so a folder writer reaches
					// this row alone.
					path := "files/" + lifecycleName("f") + "/doc.md"
					if ok, err := files.Insert(t.Context(), db.Querier(), aRow(alice, path)); err != nil || !ok {
						t.Fatalf("insert the file: %v, %v", ok, err)
					}
					switch s {
					case FileTrashed:
						if ok, err := files.SoftDelete(t.Context(), db.Querier(), alice, path); err != nil || !ok {
							t.Fatalf("trash the file: %v, %v", ok, err)
						}
					case FilePurged:
						if _, ok, err := files.HardDelete(t.Context(), db.Querier(), alice, path); err != nil || !ok {
							t.Fatalf("purge the file: %v, %v", ok, err)
						}
					}
					f, err := files.Get(t.Context(), db.Querier(), alice, path)
					if err == nil {
						ids.Store(path, f.ID)
					} else if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("read the file: %v", err)
					}
					return path
				},
				Move: func(path string, to FileStatus) error {
					var (
						ok  bool
						err error
					)
					switch to {
					case FileTrashed:
						ok, err = w.trash(t, db, path)
					case FileLive:
						ok, err = w.restore(t, db, path)
					case FilePurged:
						ok, err = w.purge(t, db, path)
					default:
						return noWriter("file " + string(to))
					}
					if err != nil {
						return err
					}
					if !ok {
						return unchanged("file " + string(to))
					}
					return nil
				},
				State: func(t *testing.T, path string) FileStatus {
					f, err := files.Get(t.Context(), db.Querier(), alice, path)
					switch {
					case errors.Is(err, pgx.ErrNoRows):
						return FilePurged
					case err == nil && !sameRecord(&ids, path, f.ID):
						return FilePurged
					case err != nil:
						t.Fatalf("read the file: %v", err)
					case f.DeletedAt != nil:
						return FileTrashed
					}
					return FileLive
				},
			}
			statemachinetest.Exhaustive(t, FileTable, d)
			statemachinetest.Race(t, FileTable, d)
		})
	}
}

// Writing content to a trashed path revives the row, and the revive is the
// trashed to live step. Upsert takes the same step as CreateOnly, and a write
// to a live row keeps it live.
func TestStoreFileUpsertRevivesATrashedRow(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	q := db.Querier()
	path := "files/" + lifecycleName("f") + "/doc.md"
	if ok, err := files.Insert(t.Context(), q, aRow(alice, path)); err != nil || !ok {
		t.Fatalf("insert: %v, %v", ok, err)
	}
	if ok, err := files.CreateOnly(t.Context(), q, aRow(alice, path)); err != nil || ok {
		t.Fatalf("CreateOnly on a live row = %v, %v, want refused", ok, err)
	}
	if ok, err := files.SoftDelete(t.Context(), q, alice, path); err != nil || !ok {
		t.Fatalf("trash: %v, %v", ok, err)
	}
	if err := files.Upsert(t.Context(), q, aRow(alice, path)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	f, err := files.Get(t.Context(), q, alice, path)
	if err != nil || f.DeletedAt != nil {
		t.Fatalf("after the Upsert the row is %+v, %v, want live", f, err)
	}
	if err := files.Upsert(t.Context(), q, aRow(alice, path)); err != nil {
		t.Fatalf("Upsert on a live row: %v", err)
	}
}

// PurgeTrash and the reaper's trash pass take trashed rows only: a live row
// is not a source of their step, so it stays.
func TestStorePurgeTrashLeavesLiveRows(t *testing.T) {
	db := tier(t)
	files := NewFiles()
	q := db.Querier()
	live := "files/" + lifecycleName("f") + "/live.md"
	trashed := "files/" + lifecycleName("f") + "/trashed.md"
	for _, p := range []string{live, trashed} {
		if ok, err := files.Insert(t.Context(), q, aRow(alice, p)); err != nil || !ok {
			t.Fatalf("insert %s: %v, %v", p, ok, err)
		}
	}
	if ok, err := files.SoftDelete(t.Context(), q, alice, trashed); err != nil || !ok {
		t.Fatalf("trash: %v, %v", ok, err)
	}
	gone, err := files.PurgeTrash(t.Context(), q, alice, "", nil)
	if err != nil || len(gone) != 1 || gone[0].Path != trashed {
		t.Fatalf("PurgeTrash = %v, %v, want the trashed row alone", gone, err)
	}
	if gone, err := files.PurgeTrash(t.Context(), q, alice, live, nil); err != nil || len(gone) != 0 {
		t.Fatalf("PurgeTrash of a live path = %v, %v, want nothing", gone, err)
	}
	if _, err := files.Get(t.Context(), q, alice, live); err != nil {
		t.Errorf("the live row is gone: %v", err)
	}
}
