// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The unit half of the overview of spec 012: the statement the query sends,
// the arguments it binds, the row it scans, and the failures it wraps. What
// the SQL means over real rows is the store tier's, beside this file.

// overviewRows answers the rows of the spaces the case named, in the order
// the statement selects its columns.
func overviewRows(spaces ...SpaceOverview) *fakeRows {
	rows := &fakeRows{}
	for _, s := range spaces {
		rows.scans = append(rows.scans, func(dest ...any) error {
			return assign(dest, []any{
				s.Owner, s.Bytes, s.Files, s.TrashedBytes, s.Workspaces, s.Leases, s.LastWriteAt,
			})
		})
	}
	return rows
}

func TestOverviewPagesBySubjectAndReadsOneRowPerSpace(t *testing.T) {
	at := time.Date(2026, 9, 17, 8, 41, 2, 0, time.UTC)
	q := &fakeQuerier{rows: overviewRows(
		SpaceOverview{Owner: "https://issuer.example|9ab3", Bytes: 88213004, Files: 214,
			TrashedBytes: 402118, Workspaces: 2, Leases: 1, LastWriteAt: &at},
		SpaceOverview{Owner: "https://issuer.example|c1d0"},
	)}
	page, err := NewAdmin().Overview(t.Context(), q, "https://issuer.example|0000", 3)
	if err != nil {
		t.Fatalf("the overview failed: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("the page holds %d rows, want 2", len(page))
	}
	first := page[0]
	if first.Owner != "https://issuer.example|9ab3" || first.Bytes != 88213004 || first.Files != 214 {
		t.Errorf("the first row is %+v", first)
	}
	if first.TrashedBytes != 402118 || first.Workspaces != 2 || first.Leases != 1 {
		t.Errorf("the first row's counters are %+v", first)
	}
	if first.LastWriteAt == nil || !first.LastWriteAt.Equal(at) {
		t.Errorf("the first row's last write is %v, want %v", first.LastWriteAt, at)
	}
	if page[1].LastWriteAt != nil {
		t.Errorf("a space that holds no path answered a last write of %v", page[1].LastWriteAt)
	}
	if len(q.args) != 1 || q.args[0][0] != "https://issuer.example|0000" || q.args[0][1] != 3 {
		t.Errorf("the query bound %v, want the cursor and the limit", q.args)
	}
	if !q.rows.(*fakeRows).closed {
		t.Error("the rows were not closed")
	}
}

// TestOverviewCountsALeaseOnlyWhileItIsHeld holds the statement to the
// workspace service's own rule. A lease past its deadline is not held, and
// counting one would report a state the next attach disagrees with; the
// service Arca replaces counted every non-null holder and over-reported.
func TestOverviewCountsALeaseOnlyWhileItIsHeld(t *testing.T) {
	for _, want := range []string{
		"writer_holder IS NOT NULL",
		"writer_expires_at > now()",
	} {
		if !strings.Contains(overviewSQL, want) {
			t.Errorf("the statement does not hold a lease to %q", want)
		}
	}
}

// TestOverviewSeparatesLiveBytesFromTrashedBytes: the service Arca replaces
// summed every row into one total, so an operator could not tell what a
// reaper run would recover from what a space was actually using.
func TestOverviewSeparatesLiveBytesFromTrashedBytes(t *testing.T) {
	if !strings.Contains(overviewSQL, "SUM(size_bytes) FILTER (WHERE deleted_at IS NOT NULL)") {
		t.Error("the statement does not count the trashed bytes on their own")
	}
	if !strings.Contains(overviewSQL, "COALESCE(u.bytes, 0)") {
		t.Error("the statement does not read the usage from the ledger of spec 010")
	}
}

// TestOverviewLeavesOutASpaceThatHoldsNothing: criterion 6 of spec 012 as
// the statement carries it.
func TestOverviewLeavesOutASpaceThatHoldsNothing(t *testing.T) {
	if !strings.Contains(overviewSQL, "(bytes > 0 OR files > 0 OR trashed_bytes > 0 OR workspaces > 0)") {
		t.Error("the statement does not drop a space whose every counter is zero")
	}
	if strings.Contains(overviewSQL, "subjects") {
		t.Error("the statement reads the subject directory, which holds subjects that store nothing")
	}
}

func TestOverviewRefusesAPageOfNoRows(t *testing.T) {
	q := &fakeQuerier{}
	if _, err := NewAdmin().Overview(t.Context(), q, "", 0); err == nil {
		t.Fatal("a page of no rows was accepted")
	}
	if len(q.statements) != 0 {
		t.Errorf("a page of no rows reached the database: %v", q.statements)
	}
}

func TestOverviewWrapsWhatTheDatabaseAnswered(t *testing.T) {
	fault := errors.New("the connection is gone")
	t.Run("the query failed", func(t *testing.T) {
		_, err := NewAdmin().Overview(t.Context(), &fakeQuerier{queryErr: fault}, "", 10)
		if !errors.Is(err, fault) {
			t.Errorf("the failure is %v and does not carry the database's", err)
		}
	})
	t.Run("a row did not scan", func(t *testing.T) {
		rows := &fakeRows{scans: []func(...any) error{func(...any) error { return fault }}}
		_, err := NewAdmin().Overview(t.Context(), &fakeQuerier{rows: rows}, "", 10)
		if !errors.Is(err, fault) {
			t.Errorf("the failure is %v and does not carry the database's", err)
		}
	})
	t.Run("the page ended in a failure", func(t *testing.T) {
		rows := overviewRows(SpaceOverview{Owner: "https://issuer.example|9ab3"})
		rows.err = fault
		_, err := NewAdmin().Overview(t.Context(), &fakeQuerier{rows: rows}, "", 10)
		if !errors.Is(err, fault) {
			t.Errorf("the failure is %v and does not carry the database's", err)
		}
	})
}
