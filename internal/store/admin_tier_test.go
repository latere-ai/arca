// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 012: the administrative overview against a real
// Postgres. What it proves is what a fake cannot, and what criteria 5 and 6
// of that spec ask for by name: that each counter equals a direct count of
// the fixtures, that the usage comes from the ledger rather than from a sum
// this statement made up, and that a space holding nothing is not a row.
package store

import (
	"fmt"
	"testing"
	"time"

	"latere.ai/x/arca/object"
)

// adminSpace is a subject of this run's own.
func adminSpace(t *testing.T, name string) string {
	t.Helper()
	return fmt.Sprintf("https://issuer.example|%s-%s-%d", t.Name(), name, time.Now().UnixNano())
}

// adminFile writes one live path of a space.
func adminFile(t *testing.T, db *DB, owner, path string, size int64) {
	t.Helper()
	created, err := NewFiles().Insert(t.Context(), db.Querier(), File{
		Owner: owner, Path: path, ObjectID: object.NewID(), CreatedBy: owner,
		SizeBytes: size, Checksum: "9f2c", ChecksumKind: object.ChecksumSHA256,
	})
	if err != nil || !created {
		t.Fatalf("write %q: %v, %v", path, created, err)
	}
}

// adminTrash moves a live path into the trash, which spec 005 owns and this
// tier writes directly because that spec's own statement has not landed.
func adminTrash(t *testing.T, db *DB, owner, path string) {
	t.Helper()
	tag, err := db.Querier().Exec(t.Context(),
		`UPDATE files SET deleted_at = now() WHERE owner = $1 AND path = $2`, owner, path)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("trash %q: %v, %d rows", path, err, tag.RowsAffected())
	}
}

// adminUsage writes the ledger row of a space, which spec 010 owns.
func adminUsage(t *testing.T, db *DB, owner string, bytes int64) {
	t.Helper()
	if _, err := db.Querier().Exec(t.Context(),
		`INSERT INTO space_usage (owner, bytes) VALUES ($1, $2)`, owner, bytes); err != nil {
		t.Fatalf("write the ledger of %q: %v", owner, err)
	}
}

// TestStoreOverviewCountsWhatEachSpaceHolds is criterion 5 of spec 012: each
// counter equals a direct count of the fixtures.
func TestStoreOverviewCountsWhatEachSpaceHolds(t *testing.T) {
	db := tier(t)
	full := adminSpace(t, "full")

	adminFile(t, db, full, "files/reports/q3.pdf", 100)
	adminFile(t, db, full, "files/reports/q4.pdf", 200)
	adminFile(t, db, full, "files/notes/old.txt", 50)
	adminTrash(t, db, full, "files/notes/old.txt")
	adminUsage(t, db, full, 350)

	held := wsRow(t, db, full, "build")
	wsRow(t, db, full, "free")
	deleted := wsRow(t, db, full, "gone")
	now := time.Now()
	took, err := NewWorkspaces().TakeLease(t.Context(), db.Querier(), held.ID, "sbx_1", now, now.Add(time.Hour))
	if err != nil || !took {
		t.Fatalf("take the lease: %v, %v", took, err)
	}
	if _, err := db.Querier().Exec(t.Context(),
		`UPDATE workspaces SET deleted_at = now() WHERE id = $1`, deleted.ID); err != nil {
		t.Fatalf("soft delete the workspace: %v", err)
	}

	page, err := NewAdmin().Overview(t.Context(), db.Querier(), "", 10)
	if err != nil {
		t.Fatalf("the overview failed: %v", err)
	}
	row, ok := overviewOf(page, full)
	if !ok {
		t.Fatalf("the overview holds %v and not the space that holds everything", page)
	}
	if row.Files != 2 {
		t.Errorf("the space holds %d live paths, want 2", row.Files)
	}
	if row.Bytes != 350 {
		t.Errorf("the space's usage is %d, want the ledger's 350", row.Bytes)
	}
	if row.TrashedBytes != 50 {
		t.Errorf("the space's trashed bytes are %d, want 50", row.TrashedBytes)
	}
	if row.Workspaces != 2 {
		t.Errorf("the space holds %d live workspaces, want 2", row.Workspaces)
	}
	if row.Leases != 1 {
		t.Errorf("the space holds %d leases, want 1", row.Leases)
	}
	if row.LastWriteAt == nil {
		t.Error("the space names no last write and it holds three paths")
	}
}

// TestStoreOverviewCountsNoLapsedLease: a lease past its deadline is not
// held, so the next attach takes it and the overview may not report one.
func TestStoreOverviewCountsNoLapsedLease(t *testing.T) {
	db := tier(t)
	owner := adminSpace(t, "lapsed")
	adminFile(t, db, owner, "files/a.txt", 1)
	ws := wsRow(t, db, owner, "stale")
	past := time.Now().Add(-2 * time.Hour)
	took, err := NewWorkspaces().TakeLease(t.Context(), db.Querier(), ws.ID, "sbx_1", past, past.Add(time.Minute))
	if err != nil || !took {
		t.Fatalf("take the lease: %v, %v", took, err)
	}

	page, err := NewAdmin().Overview(t.Context(), db.Querier(), "", 10)
	if err != nil {
		t.Fatalf("the overview failed: %v", err)
	}
	row, ok := overviewOf(page, owner)
	if !ok {
		t.Fatalf("the overview holds %v and not the space", page)
	}
	if row.Leases != 0 {
		t.Errorf("a lapsed lease was counted as held: %d", row.Leases)
	}
	if row.Workspaces != 1 {
		t.Errorf("the space holds %d live workspaces, want 1", row.Workspaces)
	}
}

// TestStoreOverviewLeavesOutASpaceThatHoldsNothing is criterion 6's second
// half: a ledger row at zero and a purged space are not rows of the page.
func TestStoreOverviewLeavesOutASpaceThatHoldsNothing(t *testing.T) {
	db := tier(t)
	empty := adminSpace(t, "empty")
	holding := adminSpace(t, "holding")
	adminUsage(t, db, empty, 0)
	adminFile(t, db, holding, "files/a.txt", 7)

	page, err := NewAdmin().Overview(t.Context(), db.Querier(), "", 10)
	if err != nil {
		t.Fatalf("the overview failed: %v", err)
	}
	if _, ok := overviewOf(page, empty); ok {
		t.Errorf("a space that holds nothing is a row of the page: %v", page)
	}
	if _, ok := overviewOf(page, holding); !ok {
		t.Errorf("the space that holds a path is not a row of the page: %v", page)
	}
}

// TestStoreOverviewPagesBySubject is criterion 6's first half: the page is
// keyset paginated on the subject, so every space is read once.
func TestStoreOverviewPagesBySubject(t *testing.T) {
	db := tier(t)
	stamp := time.Now().UnixNano()
	var owners []string
	for _, name := range []string{"a", "b", "c"} {
		owner := fmt.Sprintf("https://issuer.example|page-%d-%s", stamp, name)
		owners = append(owners, owner)
		adminFile(t, db, owner, "files/a.txt", 1)
	}

	var seen []string
	cursor := ""
	for range len(owners) + 1 {
		page, err := NewAdmin().Overview(t.Context(), db.Querier(), cursor, 2)
		if err != nil {
			t.Fatalf("the overview failed: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			seen = append(seen, row.Owner)
		}
		cursor = page[len(page)-1].Owner
	}
	for _, owner := range owners {
		if count := countOf(seen, owner); count != 1 {
			t.Errorf("%q appeared %d times across the pages", owner, count)
		}
	}
}

// overviewOf answers the row of one space.
func overviewOf(page []SpaceOverview, owner string) (SpaceOverview, bool) {
	for _, row := range page {
		if row.Owner == owner {
			return row, true
		}
	}
	return SpaceOverview{}, false
}

// countOf answers how many times a subject appeared.
func countOf(seen []string, owner string) int {
	n := 0
	for _, s := range seen {
		if s == owner {
			n++
		}
	}
	return n
}
