// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"latere.ai/x/pkg/authz"
)

// Criterion 5 of spec 010: the admission table, one row per case. Every
// write path routes its charge through Delta, so the two overwrite cases
// cannot diverge.
func TestDeltaChargesWhatAWriteAdds(t *testing.T) {
	for _, c := range []struct {
		name      string
		size      int64
		replaced  int64
		versioned bool
		want      int64
	}{
		{"a new object", 48213, 0, false, 48213},
		{"a new object on a versioned path", 48213, 0, true, 48213},
		{"a versioned overwrite", 100, 80, true, 100},
		{"a non-versioned overwrite that grows", 100, 80, false, 20},
		{"a non-versioned overwrite that shrinks", 80, 100, false, -20},
		{"a non-versioned overwrite of the same size", 80, 80, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Delta(c.size, c.replaced, c.versioned); got != c.want {
				t.Fatalf("the charge is %d and the table says %d", got, c.want)
			}
		})
	}
}

// Criteria 1, 3 and 4 of spec 010 at the seam: with no limit in the answer
// no write is refused at any usage, a write that lands exactly on the limit
// is admitted, the next byte over is refused with the two figures, and a
// delete is admitted over the limit.
func TestChargeHonoursTheLimitTheAnswerCarried(t *testing.T) {
	for _, c := range []struct {
		name    string
		delta   int64
		total   int64
		limit   Limit
		refused bool
	}{
		{"no limit at all, at any usage", 1 << 40, 1 << 41, Unlimited(), false},
		{"under the limit", 10, 90, LimitBytes(100), false},
		{"exactly on the limit", 10, 100, LimitBytes(100), false},
		{"one byte over", 11, 101, LimitBytes(100), true},
		{"a delete on a space over the limit", -10, 500, LimitBytes(100), false},
		{"a write of nothing on a space over the limit", 0, 500, LimitBytes(100), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQuerier{row: values(c.total)}
			total, err := NewLedger().Charge(t.Context(), q, aSpace, c.delta, c.limit)
			if total != c.total {
				t.Fatalf("the charge answered %d and the row holds %d", total, c.total)
			}
			over, refused := AsOverLimit(err)
			if refused != c.refused {
				t.Fatalf("the charge answered %v and the case wants refused=%v", err, c.refused)
			}
			if !refused {
				return
			}
			if over.Used != c.total-c.delta || over.Limit != c.limit.Bytes() || over.Delta != c.delta {
				t.Fatalf("the refusal reads %+v", over)
			}
			if !strings.Contains(over.Error(), "bytes") {
				t.Fatalf("the developer sentence is %q", over.Error())
			}
		})
	}
}

func TestChargeAppliesTheDeltaInOneStatement(t *testing.T) {
	q := &fakeQuerier{row: values(int64(48213))}
	if _, err := NewLedger().Charge(t.Context(), q, aSpace, 48213, Unlimited()); err != nil {
		t.Fatal(err)
	}
	if len(q.statements) != 1 {
		t.Fatalf("the charge sent %d statements", len(q.statements))
	}
	sent := q.statements[0]
	for _, want := range []string{"INSERT INTO space_usage", "ON CONFLICT (owner) DO UPDATE", "GREATEST(0,", "RETURNING bytes"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("the charge sent %q, which does not hold %q", sent, want)
		}
	}
	if q.args[0][0] != aSpace || q.args[0][1] != int64(48213) {
		t.Fatalf("the charge bound %v", q.args[0])
	}
}

// TestUsageReadsTheCounterAndTheRowsInOneStatement: the bytes come off the
// ledger's own row and the paths off the file rows, which is spec 012's
// split, and a root listing of spec 005 pays one round trip for both.
func TestUsageReadsTheCounterAndTheRowsInOneStatement(t *testing.T) {
	q := &fakeQuerier{row: values(int64(48213), int64(7))}
	held, err := NewLedger().Usage(t.Context(), q, aSpace)
	if err != nil {
		t.Fatal(err)
	}
	if held.Bytes != 48213 || held.Files != 7 {
		t.Fatalf("the read answered %+v", held)
	}
	if len(q.statements) != 1 {
		t.Fatalf("the read sent %d statements", len(q.statements))
	}
	sent := q.statements[0]
	for _, want := range []string{"FROM space_usage", "COUNT(*) FROM files", "deleted_at IS NULL"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("the read sent %q, which does not hold %q", sent, want)
		}
	}
	if q.args[0][0] != aSpace {
		t.Fatalf("the read bound %v", q.args[0])
	}
}

// TestUsageThatCannotBeReadIsAFailure: the number is part of a listing's
// answer, so a counter that cannot be read is an error and never a zero a
// caller would read as an empty space.
func TestUsageThatCannotBeReadIsAFailure(t *testing.T) {
	boom := errors.New("the connection went away")
	held, err := NewLedger().Usage(t.Context(), &fakeQuerier{row: failing(boom)}, aSpace)
	if err == nil {
		t.Fatalf("a counter that cannot be read answered %+v", held)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the failure is %v, and it does not carry the store's own", err)
	}
}

// Criterion 6 of spec 010 at the seam: a ledger write that fails is a
// failure the caller hears about, so the write it was charging for fails
// with it. Nothing is admitted on a number that could not be written.
func TestUsageFailsClosed(t *testing.T) {
	boom := errors.New("the connection went away")
	for _, c := range []struct {
		name string
		call func(Ledger) error
	}{
		{"a charge", func(l Ledger) error {
			_, err := l.Charge(t.Context(), &fakeQuerier{row: failing(boom)}, aSpace, 10, LimitBytes(100))
			return err
		}},
		{"a release", func(l Ledger) error {
			_, err := l.Release(t.Context(), &fakeQuerier{row: failing(boom)}, aSpace, 10)
			return err
		}},
		{"a read", func(l Ledger) error {
			_, err := l.Read(t.Context(), &fakeQuerier{row: failing(boom)}, aSpace)
			return err
		}},
		{"a recomputation", func(l Ledger) error {
			_, err := l.Recompute(t.Context(), &fakeQuerier{row: failing(boom)}, aSpace)
			return err
		}},
		{"a correction", func(l Ledger) error {
			_, err := l.Correct(t.Context(), &fakeQuerier{execErr: boom}, aSpace, 1, 2)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(NewLedger())
			if !errors.Is(err, boom) {
				t.Fatalf("the ledger answered %v", err)
			}
			if _, refused := AsOverLimit(err); refused {
				t.Fatal("a fault reached the caller as a refusal against the limit")
			}
		})
	}
}

func TestReleaseGivesBytesBackAndIsNeverRefused(t *testing.T) {
	q := &fakeQuerier{row: values(int64(90))}
	total, err := NewLedger().Release(t.Context(), q, aSpace, 10)
	if err != nil || total != 90 {
		t.Fatalf("the release answered %d, %v", total, err)
	}
	if q.args[0][1] != int64(-10) {
		t.Fatalf("the release bound %v, and it gives ten bytes back", q.args[0][1])
	}
}

func TestReadAnswersNothingForASpaceWithNoRow(t *testing.T) {
	q := &fakeQuerier{row: values(int64(0))}
	bytes, err := NewLedger().Read(t.Context(), q, aSpace)
	if err != nil || bytes != 0 {
		t.Fatalf("the read answered %d, %v", bytes, err)
	}
	if !strings.Contains(q.statements[0], "COALESCE") {
		t.Fatalf("the read sent %q, and a space with no row holds nothing", q.statements[0])
	}
}

func TestRecomputeSumsTheRowsThatHoldTheBytes(t *testing.T) {
	q := &fakeQuerier{row: values(int64(48213))}
	bytes, err := NewLedger().Recompute(t.Context(), q, aSpace)
	if err != nil || bytes != 48213 {
		t.Fatalf("the recomputation answered %d, %v", bytes, err)
	}
	// upload_sessions is the third table and not an optional one. A session
	// is charged its declared bytes the moment it opens, so a recomputation
	// that leaves the table out answers less than the ledger holds, and the
	// reaper's correction then writes that lower number over a live charge.
	// Repeat it and a space's usage never reflects what its open sessions
	// hold.
	for _, table := range []string{"files", "file_versions", "upload_sessions"} {
		if !strings.Contains(q.statements[0], "FROM "+table) {
			t.Fatalf("the recomputation reads %q and leaves out %s", q.statements[0], table)
		}
	}
}

func TestCorrectWritesOnlyWhereTheRowStillHoldsWhatWasRead(t *testing.T) {
	for _, c := range []struct {
		name string
		tag  pgconn.CommandTag
		want bool
	}{
		{"the row is untouched", pgconn.NewCommandTag("UPDATE 1"), true},
		{"a charge landed since the read", pgconn.NewCommandTag("UPDATE 0"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQuerier{tag: c.tag}
			corrected, err := NewLedger().Correct(t.Context(), q, aSpace, 100, 90)
			if err != nil {
				t.Fatal(err)
			}
			if corrected != c.want {
				t.Fatalf("the correction answered %v and the case wants %v", corrected, c.want)
			}
			if !strings.Contains(q.statements[0], "WHERE owner = $1 AND bytes = $2") {
				t.Fatalf("the correction sent %q, which is not conditional on what it read", q.statements[0])
			}
		})
	}
}

func TestSpacesWalksTheLedgerInOwnerOrder(t *testing.T) {
	rows := spaceRows(Space{Owner: "a", Bytes: 1}, Space{Owner: "b", Bytes: 2})
	q := &fakeQuerier{rows: rows}
	page, err := NewLedger().Spaces(t.Context(), q, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Owner != "a" || page[1].Bytes != 2 {
		t.Fatalf("the page reads %+v", page)
	}
	if !rows.closed {
		t.Fatal("the rows were not closed")
	}
	if _, err := NewLedger().Spaces(t.Context(), q, "", 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	boom := errors.New("the connection went away")
	if _, err := NewLedger().Spaces(t.Context(), &fakeQuerier{queryErr: boom}, "", 10); !errors.Is(err, boom) {
		t.Fatalf("a failed query answered %v", err)
	}
	broken := &fakeRows{scans: []func(...any) error{func(...any) error { return boom }}}
	if _, err := NewLedger().Spaces(t.Context(), &fakeQuerier{rows: broken}, "", 10); !errors.Is(err, boom) {
		t.Fatalf("a failed scan answered %v", err)
	}
	walked := spaceRows(Space{Owner: "a"})
	walked.err = boom
	if _, err := NewLedger().Spaces(t.Context(), &fakeQuerier{rows: walked}, "", 10); !errors.Is(err, boom) {
		t.Fatalf("a failed walk answered %v", err)
	}
}

func TestLimitOfReadsWhatTheAnswerCarried(t *testing.T) {
	for _, c := range []struct {
		name    string
		limits  string
		want    Limit
		refused bool
	}{
		{"an answer with no limits object", "", Unlimited(), false},
		{"an answer with limits and no quota", `{"requests_per_minute":600}`, Unlimited(), false},
		{"an answer with a limit", `{"quota_bytes":53687091200}`, LimitBytes(53687091200), false},
		{"an answer with a limit of nothing", `{"quota_bytes":0}`, LimitBytes(0), false},
		{"an answer whose limits will not read", `{"quota_bytes":"a lot"}`, Unlimited(), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := authz.Decision{Allow: true}
			if c.limits != "" {
				d.Limits = json.RawMessage(c.limits)
			}
			got, err := LimitOf(d)
			if (err != nil) != c.refused {
				t.Fatalf("the answer read as %v", err)
			}
			if got != c.want {
				t.Fatalf("the limit reads %+v and the case wants %+v", got, c.want)
			}
		})
	}
}

// Criterion 2 of spec 010: the limit an answer carried is honoured for that
// answer's ttl and no longer. The ttl belongs to the decision cache of spec
// 006, so the proof runs the shared client over a stub authorizer on a clock
// the test moves.
func TestTheLimitLivesAsLongAsTheAnswerAndNoLonger(t *testing.T) {
	answers := []string{
		`{"allow":true,"ttl":60,"limits":{"quota_bytes":100}}`,
		`{"allow":true,"ttl":60}`,
	}
	asked := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answers[min(asked, len(answers)-1)]))
		asked++
	}))
	defer endpoint.Close()

	now := aMoment
	client, err := authz.NewClient(authz.Options{
		URL: endpoint.URL, HTTP: endpoint.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ask := func() Limit {
		t.Helper()
		d, err := client.Authorize(t.Context(), authz.Request{
			Subject: aSpace, Action: ActionEventRead,
			Resource: authz.NewResource("File", "an-object", map[string]any{"owner": aSpace}),
		})
		if err != nil {
			t.Fatal(err)
		}
		limit, err := LimitOf(d)
		if err != nil {
			t.Fatal(err)
		}
		return limit
	}

	if limit := ask(); limit != LimitBytes(100) {
		t.Fatalf("the first answer's limit reads %+v", limit)
	}
	now = now.Add(59 * time.Second)
	if limit := ask(); limit != LimitBytes(100) {
		t.Fatalf("inside the ttl the limit reads %+v", limit)
	}
	if asked != 1 {
		t.Fatalf("the authorizer was asked %d times inside one ttl", asked)
	}
	// Past the ttl the cached answer is gone, and the answer that replaces
	// it carries no limit, so the space has none: nothing was stored.
	now = now.Add(2 * time.Second)
	if limit := ask(); limit != Unlimited() {
		t.Fatalf("past the ttl the limit reads %+v", limit)
	}
}
