// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"strings"
	"testing"
)

// clean is a source the preflight holds over: no organization outside the
// mapping, no plane without a rule, no colliding slug, no key outside the
// prefix, and one key the manifest lists.
func clean() *fake {
	return newFake(
		answer{"SELECT DISTINCT owner_id", [][]any{{mappedOrg}}},
		answer{"split_part(path, '/', 1)", [][]any{{"files"}, {"memory"}, {"repos"}, {"workspaces"}}},
		answer{"HAVING count(*) > 1", [][]any{}},
		answer{"strpos(storage_key", [][]any{{int64(0)}}},
		answer{"true AS live", [][]any{{cleanKey, int64(10), cleanSHA, false, true}}},
	)
}

// The one object the clean source holds: a live file, its key, and the digest
// its row carries.
const (
	cleanKey = "drive/u-11111111-1111-4111-8111-111111111111/files/notes.md"
	cleanSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestAPreflightOverACleanSourceHolds(t *testing.T) {
	if err := Preflight(t.Context(), testRunPair(clean(), emptyTarget(), false)); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}

func TestAnUnmappedOrganizationRefusesBeforeAnyWrite(t *testing.T) {
	f := clean()
	unmapped := "22222222-2222-4222-8222-222222222222"
	replace(f, "SELECT DISTINCT owner_id", [][]any{{mappedOrg}, {unmapped}})
	err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false))
	if err == nil {
		t.Fatal("an organization in no mapping is a refusal")
	}
	if !strings.Contains(err.Error(), unmapped) || !strings.Contains(err.Error(), "-org-subjects") {
		t.Errorf("the refusal is %v", err)
	}
	if len(f.sent) != 0 {
		t.Errorf("the refusal wrote %d statements", len(f.sent))
	}
}

func TestAPlaneWithNoRuleRefusesBeforeAnyWrite(t *testing.T) {
	f := clean()
	replace(f, "split_part(path, '/', 1)", [][]any{{"files"}, {"sandboxes"}})
	err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false))
	if err == nil {
		t.Fatal("a plane spec 019 gives no rule for is a refusal")
	}
	if !strings.Contains(err.Error(), "sandboxes") {
		t.Errorf("the refusal names no plane: %v", err)
	}
}

// TestTheAgentsPlaneNoLongerRefusesTheRun is the maintainer's decision of
// 2026-09-19: the retired zone has a rule now, so its rows fold under files/
// rather than stopping the copy before it begins.
func TestTheAgentsPlaneNoLongerRefusesTheRun(t *testing.T) {
	f := clean()
	replace(f, "split_part(path, '/', 1)", [][]any{{"files"}, {"agents"}})
	if err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false)); err != nil {
		t.Fatalf("the agents plane refused the run: %v", err)
	}
}

func TestCollidingSlugsRefuseBeforeAnyWrite(t *testing.T) {
	f := clean()
	replace(f, "HAVING count(*) > 1", [][]any{{"org", mappedOrg, "site"}})
	err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false))
	if err == nil {
		t.Fatal("two workspaces of one name in one space is a refusal")
	}
	if !strings.Contains(err.Error(), "o-"+mappedOrg) || !strings.Contains(err.Error(), "site") {
		t.Errorf("the refusal is %v", err)
	}
}

func TestAnOwnerTypeNeitherSchemaNamesStopsTheSlugCheck(t *testing.T) {
	f := clean()
	replace(f, "HAVING count(*) > 1", [][]any{{"tenant", mappedOrg, "site"}})
	if err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false)); err == nil {
		t.Fatal("an owner type neither schema names is an error")
	}
}

func TestKeysOutsideThePrefixRefuseBeforeAnyWrite(t *testing.T) {
	f := clean()
	replace(f, "strpos(storage_key", [][]any{{int64(7)}})
	err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false))
	if err == nil {
		t.Fatal("a key outside the prefix is a refusal")
	}
	if !strings.Contains(err.Error(), "7 keys") || !strings.Contains(err.Error(), "drive/") {
		t.Errorf("the refusal is %v", err)
	}
}

func TestEveryReasonIsAnsweredAtOnce(t *testing.T) {
	// An operator who discovers the next organization on the next attempt
	// reruns the copy once per organization, so the preflight names them all.
	f := clean()
	replace(f, "SELECT DISTINCT owner_id", [][]any{
		{"22222222-2222-4222-8222-222222222222"},
		{"33333333-3333-4333-8333-333333333333"},
	})
	replace(f, "split_part(path, '/', 1)", [][]any{{"sandboxes"}})
	replace(f, "HAVING count(*) > 1", [][]any{{"principal", "9f1", "site"}})
	replace(f, "strpos(storage_key", [][]any{{int64(2)}})
	err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false))
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Preflight answered %T, want a refusal", err)
	}
	if len(refusal.Reasons) != 5 {
		t.Errorf("the refusal names %d reasons, want 5: %v", len(refusal.Reasons), refusal.Reasons)
	}
	if !strings.HasPrefix(refusal.Error(), "migrate-drive: the copy is refused and nothing was written") {
		t.Errorf("the refusal reads %q", refusal.Error())
	}
}

func TestAFailedPreflightReadStopsTheRun(t *testing.T) {
	boom := errors.New("the database went away")
	for _, text := range []string{
		"SELECT DISTINCT owner_id", "split_part(path, '/', 1)",
		"HAVING count(*) > 1", "strpos(storage_key",
	} {
		f := clean()
		f.queryErr[text] = boom
		if err := Preflight(t.Context(), testRunPair(f, emptyTarget(), false)); !errors.Is(err, boom) {
			t.Errorf("a failed read of %q answered %v", text, err)
		}
	}
}
