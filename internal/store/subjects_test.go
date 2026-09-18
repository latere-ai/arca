// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/object"
)

func TestTouchingASubjectKeepsTheDisplayItAlreadyHad(t *testing.T) {
	q := &fakeQuerier{}
	if err := NewSubjects().Touch(t.Context(), q, "https://issuer.example|sub", "person@example.test"); err != nil {
		t.Fatal(err)
	}
	statement := q.statements[0]
	if !strings.Contains(statement, "ON CONFLICT (subject) DO UPDATE") || !strings.Contains(statement, "last_seen = now()") {
		t.Fatalf("the directory is not kept current:\n%s", statement)
	}
	if !strings.Contains(statement, "EXCLUDED.display <> ''") {
		t.Fatalf("a request with no display would erase the one on record:\n%s", statement)
	}
	refused := &fakeQuerier{execErr: errors.New("the connection failed")}
	if err := NewSubjects().Touch(t.Context(), refused, "subject", ""); err == nil {
		t.Error("a connection failure was swallowed")
	}
}

func TestReadingASubjectAnswersTheRowOrNoRowAtAll(t *testing.T) {
	seen := time.Now()
	q := &fakeQuerier{row: values("https://issuer.example|sub", "person@example.test", seen)}
	got, err := NewSubjects().Get(t.Context(), q, "https://issuer.example|sub")
	if err != nil || got.Display != "person@example.test" || !got.LastSeen.Equal(seen) {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewSubjects().Get(t.Context(), absent, "subject"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a subject that has not been seen = %v", err)
	}
}

func TestTheReferenceCheckReadsEveryTableThatHoldsAnObject(t *testing.T) {
	id := object.NewID()
	held := &fakeQuerier{row: values(true)}
	referenced, err := ObjectReferenced(t.Context(), held, id)
	if err != nil || !referenced {
		t.Fatalf("ObjectReferenced = %t, %v", referenced, err)
	}
	statement := held.statements[0]
	for _, table := range []string{"files", "file_versions"} {
		if !strings.Contains(statement, "FROM "+table+" WHERE object_id") {
			t.Errorf("the union does not read %s:\n%s", table, statement)
		}
	}
	free := &fakeQuerier{row: values(false)}
	if referenced, err := ObjectReferenced(t.Context(), free, id); err != nil || referenced {
		t.Fatalf("an object nothing names = %t, %v", referenced, err)
	}
	broken := &fakeQuerier{row: failing(errors.New("the connection failed"))}
	if _, err := ObjectReferenced(t.Context(), broken, id); err == nil {
		t.Fatal("a connection failure answered that the object is free")
	}
}
