// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const aSpace = "https://issuer.example|0f5c1d2e"

func TestAppendWritesOneRowAndAnswersItsCursor(t *testing.T) {
	q := &fakeQuerier{row: values(int64(41822))}
	e := anEvent(aSpace, ActionPut)
	e.Detail = map[string]any{"size": 48213}

	id, err := NewLog().Append(t.Context(), q, e)
	if err != nil {
		t.Fatal(err)
	}
	if id != 41822 {
		t.Fatalf("the append answered %d and the row is 41822", id)
	}
	if !strings.Contains(q.statements[0], "INSERT INTO events") {
		t.Fatalf("the append sent %q", q.statements[0])
	}
	args := q.args[0]
	if args[0] != aSpace || *(args[1].(*string)) != "files/reports/q3.pdf" || args[2] != "put" {
		t.Fatalf("the append bound %v", args[:3])
	}
	if string(args[4].([]byte)) != `{"size":48213}` {
		t.Fatalf("the detail was bound as %s", args[4])
	}
}

func TestAppendBindsAnAbsentPathAndActorAsNull(t *testing.T) {
	q := &fakeQuerier{row: values(int64(1))}
	if _, err := NewLog().Append(t.Context(), q, Event{Owner: aSpace, Action: ActionReap}); err != nil {
		t.Fatal(err)
	}
	if q.args[0][1] != (*string)(nil) || q.args[0][3] != (*string)(nil) {
		t.Fatalf("an absent path and actor were bound as %v and %v", q.args[0][1], q.args[0][3])
	}
	if detail, _ := q.args[0][4].([]byte); detail != nil {
		t.Fatalf("an absent detail was bound as %v", detail)
	}
}

func TestAppendRefusesAnActionOutsideTheTable(t *testing.T) {
	q := &fakeQuerier{row: values(int64(1))}
	_, err := NewLog().Append(t.Context(), q, anEvent(aSpace, Action("share_resolved")))
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("an action outside the table answered %v", err)
	}
	if len(q.statements) != 0 {
		t.Fatal("an action outside the table reached the database")
	}
}

func TestAppendRefusesADetailItCannotEncode(t *testing.T) {
	q := &fakeQuerier{row: values(int64(1))}
	e := anEvent(aSpace, ActionPut)
	e.Detail = map[string]any{"size": make(chan int)}
	if _, err := NewLog().Append(t.Context(), q, e); err == nil {
		t.Fatal("a detail that will not encode was written anyway")
	}
	if len(q.statements) != 0 {
		t.Fatal("a detail that will not encode reached the database")
	}
}

func TestAppendSurfacesTheDatabaseFailure(t *testing.T) {
	boom := errors.New("the connection went away")
	_, err := NewLog().Append(t.Context(), &fakeQuerier{row: failing(boom)}, anEvent(aSpace, ActionPut))
	if !errors.Is(err, boom) {
		t.Fatalf("the append answered %v", err)
	}
}

func TestNoteSwallowsAFailureAndAppendsOtherwise(t *testing.T) {
	ok := &fakeQuerier{row: values(int64(7))}
	NewLog().Note(t.Context(), ok, anEvent(aSpace, ActionPut))
	if len(ok.statements) != 1 {
		t.Fatal("the note wrote no row")
	}
	// A failed insert never fails the operation it describes: the operation
	// already happened, and an event is a notification about it.
	NewLog().Note(t.Context(), &fakeQuerier{row: failing(errors.New("gone"))}, anEvent(aSpace, ActionPut))
}

func TestTailReadsOneRowBeyondTheLimitAndAnswersTheCursorOnlyWhenThereIsMore(t *testing.T) {
	for _, c := range []struct {
		name  string
		held  []Event
		limit int
		want  int
		next  int64
	}{
		{"a full page with more behind it", pageOf(3), 2, 2, 2},
		{"a page that is the end of the log", pageOf(2), 2, 2, 0},
		{"a short page", pageOf(1), 2, 1, 0},
		{"an empty page", nil, 2, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows := eventRows(c.held...)
			q := &fakeQuerier{rows: rows}
			page, err := NewLog().Tail(t.Context(), q, Query{Owner: aSpace, Cursor: 0, Limit: c.limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Entries) != c.want {
				t.Fatalf("the page holds %d entries and the case wants %d", len(page.Entries), c.want)
			}
			if page.NextCursor != c.next {
				t.Fatalf("the cursor is %d and the case wants %d", page.NextCursor, c.next)
			}
			if !rows.closed {
				t.Fatal("the rows were not closed")
			}
			if q.args[0][2] != c.limit+1 {
				t.Fatalf("the tail asked for %v rows and the limit is %d", q.args[0][2], c.limit)
			}
		})
	}
}

func TestTailReadsBackWhatAnAppendWrote(t *testing.T) {
	held := Event{ID: 41822, Owner: aSpace, Path: "files/reports/q3.pdf", Action: ActionPut,
		Actor: aSpace, Detail: map[string]any{"size": float64(48213)}, At: aMoment}
	q := &fakeQuerier{rows: eventRows(held)}
	page, err := NewLog().Tail(t.Context(), q, Query{Owner: aSpace, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := page.Entries[0]
	if got.ID != held.ID || got.Path != held.Path || got.Action != held.Action || got.Actor != held.Actor {
		t.Fatalf("the row read back as %+v", got)
	}
	if got.Detail["size"] != float64(48213) {
		t.Fatalf("the detail read back as %v", got.Detail)
	}
}

func TestTailReadsAnAbsentPathAndActorAsEmpty(t *testing.T) {
	q := &fakeQuerier{rows: eventRows(Event{ID: 1, Owner: aSpace, Action: ActionReap, At: aMoment})}
	page, err := NewLog().Tail(t.Context(), q, Query{Owner: aSpace, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Entries[0].Path != "" || page.Entries[0].Actor != "" || page.Entries[0].Detail != nil {
		t.Fatalf("an absent path, actor and detail read back as %+v", page.Entries[0])
	}
}

func TestTailRefusesAnEmptyPageSizeAndSurfacesAFailure(t *testing.T) {
	if _, err := NewLog().Tail(t.Context(), &fakeQuerier{}, Query{Owner: aSpace, Limit: 0}); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	boom := errors.New("the connection went away")
	if _, err := NewLog().Tail(t.Context(), &fakeQuerier{queryErr: boom}, Query{Owner: aSpace, Limit: 1}); !errors.Is(err, boom) {
		t.Fatalf("a failed query answered %v", err)
	}
	rows := eventRows(pageOf(1)...)
	rows.err = boom
	if _, err := NewLog().Tail(t.Context(), &fakeQuerier{rows: rows}, Query{Owner: aSpace, Limit: 5}); !errors.Is(err, boom) {
		t.Fatalf("a failed walk answered %v", err)
	}
	bad := &fakeRows{scans: []func(...any) error{func(...any) error { return boom }}}
	if _, err := NewLog().Tail(t.Context(), &fakeQuerier{rows: bad}, Query{Owner: aSpace, Limit: 5}); !errors.Is(err, boom) {
		t.Fatalf("a failed scan answered %v", err)
	}
}

func TestTailRefusesARowWhoseDetailWillNotRead(t *testing.T) {
	bad := &fakeRows{scans: []func(...any) error{func(dest ...any) error {
		return assign(dest, []any{int64(1), aSpace, (*string)(nil), ActionPut, (*string)(nil), []byte("{"), aMoment})
	}}}
	if _, err := NewLog().Tail(t.Context(), &fakeQuerier{rows: bad}, Query{Owner: aSpace, Limit: 5}); err == nil {
		t.Fatal("a row whose detail is not JSON read back as an entry")
	}
}

func TestPruneRemovesTheRowsOlderThanTheWindow(t *testing.T) {
	q := &fakeQuerier{tag: pgconn.NewCommandTag("DELETE 12")}
	before := aMoment.Add(-30 * 24 * time.Hour)
	gone, err := NewLog().Prune(t.Context(), q, before)
	if err != nil {
		t.Fatal(err)
	}
	if gone != 12 {
		t.Fatalf("the prune removed %d rows and the tag says twelve", gone)
	}
	if !strings.Contains(q.statements[0], "DELETE FROM events WHERE created_at <") {
		t.Fatalf("the prune sent %q", q.statements[0])
	}
	if q.args[0][0] != before {
		t.Fatalf("the prune bound %v", q.args[0][0])
	}
	boom := errors.New("the connection went away")
	if _, err := NewLog().Prune(t.Context(), &fakeQuerier{execErr: boom}, before); !errors.Is(err, boom) {
		t.Fatalf("a failed prune answered %v", err)
	}
}

// pageOf answers n appendable rows, ids from one upward.
func pageOf(n int) []Event {
	es := make([]Event, 0, n)
	for i := 1; i <= n; i++ {
		es = append(es, Event{ID: int64(i), Owner: aSpace, Action: ActionPut, At: aMoment})
	}
	return es
}
