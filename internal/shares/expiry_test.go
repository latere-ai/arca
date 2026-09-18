// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"errors"
	"testing"
	"time"

	"latere.ai/x/arca/internal/shares"
	"latere.ai/x/arca/internal/store"
)

// window is what the node hands both seams below: the trash retention, which
// is the one retention setting the tree has.
const window = 720 * time.Hour

// expired is one grant whose expiry passed the given time ago.
func expired(owner string, ago time.Duration) store.Grant {
	at := time.Now().Add(-ago)
	return store.Grant{
		ID: "01J8GRANT", Owner: owner, PathPrefix: "files/reports",
		GranteeKind: store.GranteeSubject, Grantee: "someone", Permission: "read",
		Status: store.GrantActive, ExpiresAt: &at, CreatedBy: owner,
	}
}

func TestTheGrantSweepRemovesWhatExpiredAWholeWindowAgo(t *testing.T) {
	h := newHarness(t)
	h.table.grants = []store.Grant{
		expired(h.subject("9ab3"), window+time.Hour),
		expired(h.subject("9ab3"), time.Hour),
		{ID: "01J8FOREVER", Owner: h.subject("9ab3"), PathPrefix: "files", GranteeKind: store.GranteeSubject,
			Grantee: "someone", Permission: "read", Status: store.GrantActive},
	}
	pass, err := shares.NewExpiry(shares.ExpiryOptions{Store: h.table, Window: window})
	if err != nil {
		t.Fatalf("the pass would not build: %v", err)
	}
	n, err := pass.Sweep(t.Context(), nil, time.Now(), false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("the sweep removed %d grants, want the one past the window", n)
	}
	// A grant that expired an hour ago is still an audit's to read, and one
	// with no expiry grants until somebody revokes it (spec 008).
	if len(h.table.grants) != 2 {
		t.Fatalf("the sweep left %d grants, want 2", len(h.table.grants))
	}
}

// A dry run reports what a live run would remove and takes nothing: the
// condition the delete carries is the condition the count reads.
func TestADryGrantSweepCountsAndRemovesNothing(t *testing.T) {
	h := newHarness(t)
	h.table.grants = []store.Grant{expired(h.subject("9ab3"), window+time.Hour)}
	pass, _ := shares.NewExpiry(shares.ExpiryOptions{Store: h.table, Window: window})
	n, err := pass.Sweep(t.Context(), nil, time.Now(), true)
	if err != nil || n != 1 {
		t.Fatalf("the dry run = %d, %v", n, err)
	}
	if len(h.table.grants) != 1 {
		t.Fatal("the dry run removed a grant")
	}
}

func TestTheGrantSweepCarriesAStoreFailureAndRefusesAWindowOfNoTime(t *testing.T) {
	h := newHarness(t)
	h.table.failList = errors.New("the connection failed")
	pass, _ := shares.NewExpiry(shares.ExpiryOptions{Store: h.table, Window: window})
	if _, err := pass.Sweep(t.Context(), nil, time.Now(), false); err == nil {
		t.Error("a failed sweep was reported as a clean run")
	}
	if _, err := pass.Sweep(t.Context(), nil, time.Now(), true); err == nil {
		t.Error("a failed dry run was reported as a clean run")
	}
	if _, err := shares.NewExpiry(shares.ExpiryOptions{Window: window}); err == nil {
		t.Error("a pass with no query set was built anyway")
	}
	if _, err := shares.NewExpiry(shares.ExpiryOptions{Store: h.table}); err == nil {
		t.Error("a pass that removes a grant in the second it expires was built anyway")
	}
}

// The link counter of spec 012's overview, bound here because the token
// grants are this spec's table.
func TestTheLinkCounterAnswersTheSpacesThePageNamed(t *testing.T) {
	h := newHarness(t)
	h.table.grants = []store.Grant{{
		ID: "01J8LINK", Owner: h.subject("9ab3"), PathPrefix: "files/reports",
		GranteeKind: store.GranteeLink, Permission: "read", Token: "t0k3n",
		Status: store.GrantActive, CreatedBy: h.subject("9ab3"),
	}}
	counts, err := shares.LinkCounts(h.table).Counts(t.Context(), nil, []string{h.subject("9ab3"), "https://issuer.example|c1d0"})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts[h.subject("9ab3")] != 1 {
		t.Fatalf("the space holds %d links, want 1: %v", counts[h.subject("9ab3")], counts)
	}
	if _, named := counts["https://issuer.example|c1d0"]; named {
		t.Errorf("a space that holds no link is a row of the answer: %v", counts)
	}
}
