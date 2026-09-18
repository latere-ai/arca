// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/object"
)

// aSession is one open upload as the queries read and write it.
func aSession() Session {
	return Session{
		ID:           "7f1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e",
		Owner:        "https://issuer.example|9ab3",
		Path:         "files/video/keynote.mp4",
		ObjectID:     object.NewID(),
		UploadID:     "2~rrrKqLQ",
		DeclaredSize: 700 << 20,
		ContentType:  "video/mp4",
		CreatedBy:    "https://issuer.example|9ab3",
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
}

// sessionScan answers the scan of one session row, in the order of
// sessionColumns.
func sessionScan(s Session) func(dest ...any) error {
	return func(dest ...any) error {
		return assign(dest, []any{
			s.ID, s.Owner, s.Path, s.ObjectID, s.UploadID, s.DeclaredSize,
			s.ContentType, s.CreatedBy, s.CreatedAt, s.ExpiresAt,
		})
	}
}

// notAUUID is what Postgres answers when a path parameter is not an
// identifier at all.
var notAUUID = &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "nonsense"`}

func TestOpeningASessionAnswersTheRowAClientSendsBack(t *testing.T) {
	s := aSession()
	opened := &fakeQuerier{row: fakeRow{scan: sessionScan(s)}}
	got, err := NewSessions().Insert(t.Context(), opened, s)
	if err != nil || got.ID != s.ID || got.ObjectID != s.ObjectID {
		t.Fatalf("Insert = %+v, %v", got, err)
	}
	if !strings.Contains(opened.statements[0], "expires_at") {
		t.Fatalf("the deadline is not written with the row:\n%s", opened.statements[0])
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, err := NewSessions().Insert(t.Context(), broken, s); err == nil {
		t.Fatal("a failed insert reported success")
	}
}

func TestASessionIdThatIsNotAnIdentifierNamesNothingRatherThanFailing(t *testing.T) {
	s := aSession()
	found := &fakeQuerier{row: fakeRow{scan: sessionScan(s)}}
	if got, err := NewSessions().Get(t.Context(), found, s.ID); err != nil || got.UploadID != s.UploadID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	absent := &fakeQuerier{row: failing(pgx.ErrNoRows)}
	if _, err := NewSessions().Get(t.Context(), absent, s.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a session that is not there = %v", err)
	}
	nonsense := &fakeQuerier{row: failing(notAUUID)}
	if _, err := NewSessions().Get(t.Context(), nonsense, "nonsense"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an id that is not an identifier = %v", err)
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, err := NewSessions().Get(t.Context(), broken, s.ID); errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a fault answered as a missing session: %v", err)
	}
}

func TestClosingASessionIsIdempotentAndTakesNonsenseAsNothing(t *testing.T) {
	closed := &fakeQuerier{tag: pgconn.NewCommandTag("DELETE 1")}
	if ok, err := NewSessions().Delete(t.Context(), closed, "7f1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e"); err != nil || !ok {
		t.Fatalf("Delete = %t, %v", ok, err)
	}
	already := &fakeQuerier{tag: pgconn.NewCommandTag("DELETE 0")}
	if ok, err := NewSessions().Delete(t.Context(), already, "7f1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e"); err != nil || ok {
		t.Fatalf("Delete of a session already gone = %t, %v", ok, err)
	}
	nonsense := &fakeQuerier{execErr: notAUUID}
	if ok, err := NewSessions().Delete(t.Context(), nonsense, "nonsense"); err != nil || ok {
		t.Fatalf("Delete of an id that is not an identifier = %t, %v", ok, err)
	}
	broken := &fakeQuerier{execErr: errFault}
	if _, err := NewSessions().Delete(t.Context(), broken, "7f1f0e2a-77f3-4f0e-9a0c-0e2a77f34f0e"); err == nil {
		t.Fatal("a failed close reported success")
	}
}

// TestTheOpenSessionsAreCountedByTheSweepsOwnPredicate:
// arca_upload_sessions_open of spec 018 reads one number, and it is the
// complement of the sweep below, so the statement reads the same column and
// compares it the other way.
func TestTheOpenSessionsAreCountedByTheSweepsOwnPredicate(t *testing.T) {
	q := &fakeQuerier{row: values(int64(2))}
	open, err := NewSessions().CountOpen(t.Context(), q, time.Now())
	if err != nil || open != 2 {
		t.Fatalf("CountOpen = %d, %v", open, err)
	}
	if !strings.Contains(q.statements[0], "expires_at > $1") {
		t.Fatalf("the count does not read the deadline:\n%s", q.statements[0])
	}
	broken := &fakeQuerier{row: failing(errFault)}
	if _, err := NewSessions().CountOpen(t.Context(), broken, time.Now()); err == nil {
		t.Fatal("a connection failure counted as no session open")
	}
}

func TestTheExpiredSessionsAreTheReapersQueryAndAreOldestFirst(t *testing.T) {
	rows := &fakeRows{scans: []func(...any) error{sessionScan(aSession())}}
	listed := &fakeQuerier{rows: rows}
	page, err := NewSessions().Expired(t.Context(), listed, time.Now(), 100)
	if err != nil || len(page) != 1 {
		t.Fatalf("Expired = %d rows, %v", len(page), err)
	}
	if !strings.Contains(listed.statements[0], "expires_at <= $1") ||
		!strings.Contains(listed.statements[0], "ORDER BY expires_at") {
		t.Fatalf("the sweep does not read the deadline:\n%s", listed.statements[0])
	}
	if _, err := NewSessions().Expired(t.Context(), listed, time.Now(), 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	broken := &fakeQuerier{queryErr: errFault}
	if _, err := NewSessions().Expired(t.Context(), broken, time.Now(), 10); err == nil {
		t.Fatal("a failed listing reported success")
	}
	torn := &fakeQuerier{rows: &fakeRows{scans: []func(...any) error{failing(errFault).scan}}}
	if _, err := NewSessions().Expired(t.Context(), torn, time.Now(), 10); err == nil {
		t.Fatal("a row that would not scan reported success")
	}
	cut := &fakeQuerier{rows: &fakeRows{err: errFault}}
	if _, err := NewSessions().Expired(t.Context(), cut, time.Now(), 10); !errors.Is(err, errFault) {
		t.Fatalf("a listing cut short answered %v", err)
	}
}
