// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// steps is a querier that answers one outcome per call, so a test drives a
// query function that runs more than one statement and fails the second.
type steps struct {
	fakeQuerier
	tags []pgconn.CommandTag
	errs []error
	at   int
}

func (s *steps) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.record(sql, args)
	i := s.at
	s.at++
	var tag pgconn.CommandTag
	if i < len(s.tags) {
		tag = s.tags[i]
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return tag, err
}

// errFault is a database that answered something other than a verdict. Every
// query below has to surface it as itself rather than as a missing row or a
// conflict.
var errFault = errors.New("the connection failed")

func TestHoldingARowNamesTheLockAndTellsAFaultFromAnAbsence(t *testing.T) {
	f := aFile()
	held := &fakeQuerier{row: fakeRow{scan: fileScan(f)}}
	got, err := NewFiles().GetForUpdate(t.Context(), held, f.Owner, f.Path)
	if err != nil || got.Path != f.Path {
		t.Fatalf("GetForUpdate = %+v, %v", got, err)
	}
	if !strings.Contains(held.statements[0], "FOR UPDATE") {
		t.Fatalf("the read holds nothing:\n%s", held.statements[0])
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewFiles().GetForUpdate(t.Context(), absent, f.Owner, f.Path); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a path with no row = %v", err)
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, err := NewFiles().GetForUpdate(t.Context(), broken, f.Owner, f.Path); errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a fault answered as a missing row: %v", err)
	}
}

func TestAnUnconditionalWriteRevivesATrashedPathAndKeepsItsCreatorAndItsPublicity(t *testing.T) {
	f := aFile()
	written := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 1")}
	if err := NewFiles().Upsert(t.Context(), written, f); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	statement := written.statements[0]
	if !strings.Contains(statement, "deleted_at = NULL") {
		t.Errorf("the write does not revive a trashed path:\n%s", statement)
	}
	update, _, _ := strings.Cut(statement, "WHERE")
	_, update, _ = strings.Cut(update, "DO UPDATE")
	for _, column := range []string{"created_by", "is_public"} {
		if strings.Contains(update, column+" =") {
			t.Errorf("an overwrite rewrites %s, which is not the bytes it wrote:\n%s", column, update)
		}
	}
	refused := &fakeQuerier{execErr: errFault}
	if err := NewFiles().Upsert(t.Context(), refused, f); err == nil {
		t.Fatal("a failed write reported success")
	}
}

func TestACreateOnlyWriteLosesToALiveRowAndTakesATrashedOne(t *testing.T) {
	f := aFile()
	created := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 1")}
	if ok, err := NewFiles().CreateOnly(t.Context(), created, f); err != nil || !ok {
		t.Fatalf("CreateOnly = %t, %v", ok, err)
	}
	if !strings.Contains(created.statements[0], "WHERE files.deleted_at IS NOT NULL") {
		t.Fatalf("the conflict arm is not scoped to trashed rows:\n%s", created.statements[0])
	}
	occupied := &fakeQuerier{tag: pgconn.NewCommandTag("INSERT 0 0")}
	if ok, err := NewFiles().CreateOnly(t.Context(), occupied, f); err != nil || ok {
		t.Fatalf("CreateOnly against a live row = %t, %v", ok, err)
	}
	refused := &fakeQuerier{execErr: conflictError}
	if _, err := NewFiles().CreateOnly(t.Context(), refused, f); !errors.Is(err, ErrConflict) {
		t.Fatalf("a unique violation surfaced as %v", err)
	}
}

func TestTrashingAPathTouchesOnlyALiveRow(t *testing.T) {
	trashed := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 1")}
	ok, err := NewFiles().SoftDelete(t.Context(), trashed, "space", "files/a.md")
	if err != nil || !ok {
		t.Fatalf("SoftDelete = %t, %v", ok, err)
	}
	if !strings.Contains(trashed.statements[0], "deleted_at IS NULL") {
		t.Fatalf("the trash arm is not scoped to live rows:\n%s", trashed.statements[0])
	}
	already := &fakeQuerier{tag: pgconn.NewCommandTag("UPDATE 0")}
	if ok, err := NewFiles().SoftDelete(t.Context(), already, "space", "files/a.md"); err != nil || ok {
		t.Fatalf("SoftDelete of a path with no live row = %t, %v", ok, err)
	}
	broken := &fakeQuerier{execErr: errFault}
	if _, err := NewFiles().SoftDelete(t.Context(), broken, "space", "files/a.md"); err == nil {
		t.Fatal("a failed trash reported success")
	}
}

func TestRemovingARowAnswersWhatItRemovedSoTheBytesCanFollow(t *testing.T) {
	f := aFile()
	removed := &fakeQuerier{row: fakeRow{scan: fileScan(f)}}
	got, ok, err := NewFiles().HardDelete(t.Context(), removed, f.Owner, f.Path)
	if err != nil || !ok || got.ObjectID != f.ObjectID {
		t.Fatalf("HardDelete = %+v, %t, %v", got, ok, err)
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, ok, err := NewFiles().HardDelete(t.Context(), absent, f.Owner, f.Path); err != nil || ok {
		t.Fatalf("HardDelete of a path with no row = %t, %v", ok, err)
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, _, err := NewFiles().HardDelete(t.Context(), broken, f.Owner, f.Path); err == nil {
		t.Fatal("a failed delete reported success")
	}
}
