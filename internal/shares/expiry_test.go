// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package shares_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/arca/internal/blob"
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
	pass, err := shares.NewExpiry(shares.ExpiryOptions{DB: h.db, Store: h.table, Window: window})
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
	pass, _ := shares.NewExpiry(shares.ExpiryOptions{DB: h.db, Store: h.table, Window: window})
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
	pass, _ := shares.NewExpiry(shares.ExpiryOptions{DB: h.db, Store: h.table, Window: window})
	if _, err := pass.Sweep(t.Context(), nil, time.Now(), false); err == nil {
		t.Error("a failed sweep was reported as a clean run")
	}
	if _, err := pass.Sweep(t.Context(), nil, time.Now(), true); err == nil {
		t.Error("a failed dry run was reported as a clean run")
	}
	h.table.failList = nil
	h.table.failLapsed = errors.New("the connection failed")
	if _, err := pass.Sweep(t.Context(), nil, time.Now(), false); err == nil {
		t.Error("a sweep that could not read the lapsed objects was reported as a clean run")
	}
	if _, err := pass.Sweep(t.Context(), nil, time.Now(), true); err == nil {
		t.Error("a dry run that could not read the lapsed objects was reported as a clean run")
	}
	if _, err := shares.NewExpiry(shares.ExpiryOptions{DB: h.db, Window: window}); err == nil {
		t.Error("a pass with no query set was built anyway")
	}
	if _, err := shares.NewExpiry(shares.ExpiryOptions{Store: h.table, Window: window}); err == nil {
		t.Error("a pass with no database to hold a row in was built anyway")
	}
	if _, err := shares.NewExpiry(shares.ExpiryOptions{DB: h.db, Store: h.table}); err == nil {
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

// expiryOf builds pass 7 over the harness's stores, with the bucket the
// service stamps through.
func expiryOf(t *testing.T, h *harness) *shares.Expiry {
	t.Helper()
	pass, err := shares.NewExpiry(shares.ExpiryOptions{
		DB: h.db, Store: h.table, Publisher: h.bucket, BucketPrefix: "arca/", Window: window,
	})
	if err != nil {
		t.Fatalf("the pass would not build: %v", err)
	}
	return pass
}

// expire moves one grant's expiry to the given time ago, which is what the
// clock does to a link minted with one.
func expire(h *harness, id string, ago time.Duration) {
	h.table.mu.Lock()
	defer h.table.mu.Unlock()
	at := time.Now().Add(-ago)
	for i := range h.table.grants {
		if h.table.grants[i].ID == id {
			h.table.grants[i].ExpiresAt = &at
		}
	}
}

// sweep runs the pass once, live.
func sweep(t *testing.T, pass *shares.Expiry) (int, error) {
	t.Helper()
	return pass.Sweep(t.Context(), nil, time.Now(), false)
}

// TestAnExpiredPublicLinkLeavesNoMarkAndNoACL: once the last public link on
// an object has expired, the pass takes the row's mark and the bucket's ACL
// off, as the revoke of that link would have. The link itself stopped
// granting at its expiry; the ACL is what kept the object readable without a
// token after it.
func TestAnExpiredPublicLinkLeavesNoMarkAndNoACL(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	key := object.ObjectID.Key("arca/")
	folder := mint(t, h, "files/reports", "")
	link := mint(t, h, object.Path, store.GranteePublic)
	expire(h, link.ID, time.Hour)

	n, err := sweep(t, expiryOf(t, h))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep answered %d, want the one object it cleared", n)
	}
	if h.table.public(object.Path) {
		t.Error("the row is still marked after its last public link expired")
	}
	if public, _ := h.bucket.stamped(key); public {
		t.Error("the ACL is still on after the object's last public link expired")
	}
	if w := linkRead(t, h, folder, object.Path); w.Code != http.StatusOK {
		t.Errorf("a link read after the clear = %d, want the private answer", w.Code)
	}
	// An expiry inside the window is still an audit's to read.
	if _, kept := h.table.held(link.ID); !kept {
		t.Error("the clear removed the expired grant before its window")
	}
	calls := h.bucket.calls
	if n, err := sweep(t, expiryOf(t, h)); err != nil || n != 0 {
		t.Errorf("a second sweep = %d, %v, want nothing left to do", n, err)
	}
	if h.bucket.calls != calls {
		t.Error("a second sweep called the bucket for an object already cleared")
	}
}

// TestASecondLivePublicLinkKeepsTheMarkAndTheACL: an object stays public
// while any public link on it is live, so the expiry of one of two changes
// nothing and calls no bucket.
func TestASecondLivePublicLinkKeepsTheMarkAndTheACL(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	key := object.ObjectID.Key("arca/")
	first := mint(t, h, object.Path, store.GranteePublic)
	second := mint(t, h, object.Path, store.GranteePublic)
	expire(h, first.ID, time.Hour)
	calls := h.bucket.calls

	if n, err := sweep(t, expiryOf(t, h)); err != nil || n != 0 {
		t.Errorf("the sweep = %d, %v, want nothing to clear", n, err)
	}
	if !h.table.public(object.Path) {
		t.Error("the expiry of one link cleared the mark another live link holds")
	}
	if public, _ := h.bucket.stamped(key); !public {
		t.Error("the expiry of one link took the ACL off an object another live link holds")
	}
	if h.bucket.calls != calls {
		t.Errorf("the sweep called the bucket %d times over an object a live link holds", h.bucket.calls-calls)
	}
	if w := linkRead(t, h, second, object.Path); w.Code != http.StatusFound {
		t.Errorf("the live link reads %d, want what it answered before", w.Code)
	}
}

// TestARefusedClearKeepsTheObjectForTheNextRun: the bucket goes first and
// the row stays marked until it answers, so a refusal changes nothing, and
// the expired grant that names the object is kept from the purge, however
// old, until the clear has landed.
func TestARefusedClearKeepsTheObjectForTheNextRun(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	key := object.ObjectID.Key("arca/")
	link := mint(t, h, object.Path, store.GranteePublic)
	expire(h, link.ID, window+time.Hour)
	pass := expiryOf(t, h)

	h.bucket.fail = errors.New("the bucket is not answering")
	if _, err := sweep(t, pass); err == nil {
		t.Fatal("a sweep the bucket refused was reported as a clean run")
	}
	if !h.table.public(object.Path) {
		t.Error("a refused clear unmarked the row while the ACL is still on")
	}
	if public, _ := h.bucket.stamped(key); !public {
		t.Error("the ACL is off after the bucket refused to take it off")
	}
	if _, kept := h.table.held(link.ID); !kept {
		t.Fatal("the purge removed the only grant that names an object still public")
	}

	h.bucket.fail = nil
	if n, err := sweep(t, pass); err != nil || n != 1 {
		t.Fatalf("the retry = %d, %v, want the one object cleared", n, err)
	}
	if h.table.public(object.Path) {
		t.Error("the retry left the row marked")
	}
	if public, _ := h.bucket.stamped(key); public {
		t.Error("the retry left the ACL on")
	}
	// The purge runs before the clear, so the grant leaves on the run after.
	if n, err := sweep(t, pass); err != nil || n != 1 {
		t.Fatalf("the next run = %d, %v, want the one grant purged", n, err)
	}
	if _, kept := h.table.held(link.ID); kept {
		t.Error("a grant past its window was kept after its object cleared")
	}
}

// TestAnUnmarkTheDatabaseRefusesIsRetried: the bucket answered and the row
// did not, so the object is still marked and the next run takes it off.
func TestAnUnmarkTheDatabaseRefusesIsRetried(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	link := mint(t, h, object.Path, store.GranteePublic)
	expire(h, link.ID, time.Hour)
	pass := expiryOf(t, h)

	h.db.failTx, h.db.failFrom = errors.New("the connection failed"), h.db.opened+1
	if _, err := sweep(t, pass); err == nil {
		t.Fatal("a sweep whose unmark failed was reported as a clean run")
	}
	if !h.table.public(object.Path) {
		t.Fatal("the row was unmarked by a transaction that failed")
	}
	h.db.failTx = nil
	if n, err := sweep(t, pass); err != nil || n != 1 {
		t.Fatalf("the retry = %d, %v", n, err)
	}
	if h.table.public(object.Path) {
		t.Error("the retry left the row marked")
	}
}

// TestALinkMadeWhileTheObjectClearsKeepsItPublic: a public link made after
// the pass read the object found the row marked and stamped nothing, so the
// pass, holding the row, sees it, leaves the mark and puts the ACL back.
func TestALinkMadeWhileTheObjectClearsKeepsItPublic(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	key := object.ObjectID.Key("arca/")
	expired := mint(t, h, object.Path, store.GranteePublic)
	expire(h, expired.ID, time.Hour)

	var fresh shares.Link
	h.table.beforeHold = func() { fresh = mint(t, h, object.Path, store.GranteePublic) }
	if n, err := sweep(t, expiryOf(t, h)); err != nil || n != 0 {
		t.Fatalf("the sweep = %d, %v, want nothing cleared", n, err)
	}
	if !h.table.public(object.Path) {
		t.Error("the row was unmarked under a public link made during the clear")
	}
	if public, _ := h.bucket.stamped(key); !public {
		t.Error("the ACL a new public link holds was left off")
	}
	if w := linkRead(t, h, fresh, object.Path); w.Code != http.StatusFound {
		t.Errorf("the new link reads %d, want the public answer", w.Code)
	}
}

// TestAnACLTheBucketWillNotPutBackIsReported: the link made during the clear
// keeps its mark, and grants less than was asked until the last public link
// on the object goes, which is the run's failure to read and never an object
// public with no link.
func TestAnACLTheBucketWillNotPutBackIsReported(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	expired := mint(t, h, object.Path, store.GranteePublic)
	expire(h, expired.ID, time.Hour)
	h.table.beforeHold = func() {
		mint(t, h, object.Path, store.GranteePublic)
		h.bucket.fail = errors.New("the bucket is not answering")
	}
	if _, err := sweep(t, expiryOf(t, h)); err == nil {
		t.Error("an ACL the bucket would not put back was reported as a clean run")
	}
	if !h.table.public(object.Path) {
		t.Error("the row was unmarked under a live public link")
	}
}

// TestAClearWithNothingToTakeOffIsDone: a key the bucket does not hold
// carries no ACL, and a store with no object ACLs has none to take off, so
// either clears the mark rather than keeping the object for a retry that
// can never land.
func TestAClearWithNothingToTakeOffIsDone(t *testing.T) {
	for name, refusal := range map[string]error{
		"the key is not there": fmt.Errorf("blob: set public: %w", blob.ErrNotFound),
		"no object ACLs":       fmt.Errorf("blob: set public: %w", blob.ErrNotSupported),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			object := anObject(h, "files/reports/q3.pdf")
			link := mint(t, h, object.Path, store.GranteePublic)
			expire(h, link.ID, time.Hour)
			h.bucket.fail = refusal
			if n, err := sweep(t, expiryOf(t, h)); err != nil || n != 1 {
				t.Fatalf("the sweep = %d, %v, want the one object cleared", n, err)
			}
			if h.table.public(object.Path) {
				t.Error("the row is still marked")
			}
		})
	}
}

// TestADryRunCountsALapsedObjectAndClearsNothing: the objects a live run
// clears are a query, so a dry run counts them and touches neither store.
func TestADryRunCountsALapsedObjectAndClearsNothing(t *testing.T) {
	h := newHarness(t)
	object := anObject(h, "files/reports/q3.pdf")
	key := object.ObjectID.Key("arca/")
	link := mint(t, h, object.Path, store.GranteePublic)
	expire(h, link.ID, time.Hour)
	calls := h.bucket.calls

	n, err := expiryOf(t, h).Sweep(t.Context(), nil, time.Now(), true)
	if err != nil || n != 1 {
		t.Fatalf("the dry run = %d, %v, want the one object a live run clears", n, err)
	}
	if !h.table.public(object.Path) || h.bucket.calls != calls {
		t.Error("the dry run changed the row or called the bucket")
	}
	if public, _ := h.bucket.stamped(key); !public {
		t.Error("the dry run took the ACL off")
	}
}
