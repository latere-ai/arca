// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/object"
)

// The unit tier of spec 032's statements: what the Go half does with the
// answers, and that each statement names the one owner and binds nothing
// wider. What the SQL means is the store tier's, against Postgres.

// erasing answers each statement of an erasure from a script: the rows of
// the three statements that return them, and the rows affected of the rest.
// failAt fails the nth statement, counting from one.
type erasing struct {
	files, versions [][]any
	grants          []string
	affected        map[string]int64
	failAt          int
	scanErr, rowErr error

	n          int
	statements []string
	args       [][]any
}

func (q *erasing) next(sql string, args []any) error {
	q.n++
	q.statements = append(q.statements, sql)
	q.args = append(q.args, args)
	if q.n == q.failAt {
		return errors.New("the statement failed")
	}
	return nil
}

func (q *erasing) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := q.next(sql, args); err != nil {
		return pgconn.CommandTag{}, err
	}
	for part, n := range q.affected {
		if strings.Contains(sql, part) {
			return pgconn.NewCommandTag("DELETE " + itoa(n)), nil
		}
	}
	return pgconn.NewCommandTag("DELETE 0"), nil
}

func (q *erasing) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := q.next(sql, args); err != nil {
		return nil, err
	}
	rows := &fakeRows{err: q.rowErr}
	switch {
	case strings.Contains(sql, "DELETE FROM files"):
		for _, r := range q.files {
			rows.scans = append(rows.scans, scanOf(r, q.scanErr))
		}
	case strings.Contains(sql, "DELETE FROM file_versions"):
		for _, r := range q.versions {
			rows.scans = append(rows.scans, scanOf(r, q.scanErr))
		}
	case strings.Contains(sql, "DELETE FROM shares"):
		for _, kind := range q.grants {
			rows.scans = append(rows.scans, scanOf([]any{kind}, q.scanErr))
		}
	case strings.Contains(sql, "FROM upload_sessions"):
		for _, r := range q.files {
			rows.scans = append(rows.scans, scanOf(r, q.scanErr))
		}
	}
	return rows, nil
}

func (q *erasing) QueryRow(context.Context, string, ...any) pgx.Row {
	return failing(errors.New("unused"))
}

func scanOf(vs []any, fail error) func(...any) error {
	return func(dest ...any) error {
		if fail != nil {
			return fail
		}
		return assign(dest, vs)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

const erased = "https://issuer.example|9ab3"

func scripted() (*erasing, object.ID, object.ID) {
	shared, own := object.NewID(), object.NewID()
	return &erasing{
		files:    [][]any{{shared, false}, {own, true}, {shared, false}},
		versions: [][]any{{shared}},
		grants:   []string{"subject", "link", "public"},
		affected: map[string]int64{
			"FROM stars WHERE owner": 2, "FROM stars WHERE subject": 1, "grantee": 4,
			"FROM upload_sessions": 1, "space_usage": 1, "subjects": 1, "FROM events": 9,
		},
	}, shared, own
}

func TestEraseCountsEveryKindAndNamesEachObjectOnce(t *testing.T) {
	q, shared, own := scripted()
	got, err := NewErasure().Erase(t.Context(), q, erased, "space_delete")
	if err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	want := SpaceErased{
		Files: 2, Trashed: 1, Versions: 1, Stars: 2, Shares: 1, Links: 2,
		StarsHeld: 1, SharesHeld: 4, Uploads: 1, Events: 9, Usage: 1, Subject: 1,
		Objects: []object.ID{shared, own},
	}
	if got.Files != want.Files || got.Trashed != want.Trashed || got.Versions != want.Versions ||
		got.Stars != want.Stars || got.Shares != want.Shares || got.Links != want.Links ||
		got.StarsHeld != want.StarsHeld || got.SharesHeld != want.SharesHeld || got.Uploads != want.Uploads ||
		got.Events != want.Events || got.Usage != want.Usage || got.Subject != want.Subject {
		t.Errorf("the erasure counted %+v, want %+v", got, want)
	}
	if len(got.Objects) != 2 || got.Objects[0] != shared || got.Objects[1] != own {
		t.Errorf("the erasure named the objects %v, want %v once each", got.Objects, want.Objects)
	}
	if got.Total() != 26 {
		t.Errorf("the total is %d, want 26", got.Total())
	}
}

// TestEveryStatementOfAnErasureNamesTheOneOwner: the only value bound to any
// statement is the owner, and the log's is bound beside the action it keeps.
func TestEveryStatementOfAnErasureNamesTheOneOwner(t *testing.T) {
	q, _, _ := scripted()
	if _, err := NewErasure().Erase(t.Context(), q, erased, "space_delete"); err != nil {
		t.Fatalf("the erasure failed: %v", err)
	}
	if len(q.statements) != 10 {
		t.Fatalf("the erasure sent %d statements, want 10", len(q.statements))
	}
	for i, sql := range q.statements {
		if !strings.Contains(sql, "WHERE") || !strings.Contains(sql, "$1") {
			t.Errorf("statement %d has no predicate on the owner: %s", i+1, sql)
		}
		if len(q.args[i]) == 0 || q.args[i][0] != erased {
			t.Errorf("statement %d binds %v, want the owner first", i+1, q.args[i])
		}
	}
	if last := q.args[len(q.args)-1]; len(last) != 2 || last[1] != "space_delete" {
		t.Errorf("the log's delete binds %v, want the action it keeps", last)
	}
}

func TestAFailureAnywhereInAnErasureFailsItAndCountsNothing(t *testing.T) {
	for at := 1; at <= 10; at++ {
		q, _, _ := scripted()
		q.failAt = at
		got, err := NewErasure().Erase(t.Context(), q, erased, "space_delete")
		if err == nil {
			t.Errorf("statement %d failed and the erasure answered success", at)
		}
		if got.Total() != 0 || got.Objects != nil {
			t.Errorf("statement %d failed and the erasure still counted %+v", at, got)
		}
	}
}

func TestARowThatDoesNotScanOrEndsInAFailureFailsTheErasure(t *testing.T) {
	for name, set := range map[string]func(*erasing){
		"scan": func(q *erasing) { q.scanErr = errors.New("the row is not an object id") },
		"rows": func(q *erasing) { q.rowErr = errors.New("the connection dropped") },
	} {
		q, _, _ := scripted()
		set(q)
		if _, err := NewErasure().Erase(t.Context(), q, erased, "space_delete"); err == nil {
			t.Errorf("%s: the erasure answered success", name)
		}
	}
}

func TestOpenUploadsReadsTheSessionsOfOneOwner(t *testing.T) {
	id := object.NewID()
	q := &erasing{files: [][]any{{id, "u-1"}}}
	got, err := NewErasure().OpenUploads(t.Context(), q, erased)
	if err != nil {
		t.Fatalf("read the open uploads: %v", err)
	}
	if len(got) != 1 || got[0].ObjectID != id || got[0].UploadID != "u-1" {
		t.Errorf("the open uploads are %+v", got)
	}
	if len(q.args) != 1 || q.args[0][0] != erased || !strings.Contains(q.statements[0], "WHERE owner = $1") {
		t.Errorf("the read was %q with %v, want one owner", q.statements, q.args)
	}
	for name, bad := range map[string]*erasing{
		"query": {failAt: 1},
		"scan":  {files: [][]any{{id, "u-1"}}, scanErr: errors.New("bad row")},
		"rows":  {rowErr: errors.New("dropped")},
	} {
		if _, err := NewErasure().OpenUploads(t.Context(), bad, erased); err == nil {
			t.Errorf("%s: the read answered success", name)
		}
	}
}
