// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

const mappedOrg = "11111111-1111-4111-8111-111111111111"

func TestAGranteeSpec019RemovedIsDroppedAndCounted(t *testing.T) {
	r := testRewriter()
	for _, c := range []struct {
		granteeType, want string
	}{
		{"role", DropRole},
		{"team", DropTeam},
		{"email", DropEmail},
	} {
		grant, dropped, err := r.Grant(DriveShare{GranteeType: c.granteeType, Permission: "read", Status: "active"})
		if err != nil {
			t.Fatalf("Grant(%q): %v", c.granteeType, err)
		}
		if dropped != c.want {
			t.Errorf("Grant(%q) dropped under %q, want %q", c.granteeType, dropped, c.want)
		}
		if grant.Kind != "" {
			t.Errorf("Grant(%q) answered a kind for a dropped row: %q", c.granteeType, grant.Kind)
		}
	}
}

func TestTheApprovalStatusesAreTheShareRequestsTheCoreDoesNotKeep(t *testing.T) {
	r := testRewriter()
	for _, status := range []string{"pending", "denied"} {
		_, dropped, err := r.Grant(DriveShare{
			GranteeType: "principal", GranteeID: "9f1", Permission: "read", Status: status,
		})
		if err != nil {
			t.Fatalf("Grant(%q): %v", status, err)
		}
		if dropped != DropShareRequests {
			t.Errorf("a %s grant dropped under %q, want %q", status, dropped, DropShareRequests)
		}
	}
}

func TestAGranteeKindIsReadBeforeAStatus(t *testing.T) {
	// A pending role grant is dropped once, under the grantee it names, so no
	// row is counted twice and the reasons sum to the rows.
	_, dropped, err := testRewriter().Grant(DriveShare{
		GranteeType: "role", Permission: "read", Status: "pending",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if dropped != DropRole {
		t.Errorf("dropped under %q, want %q", dropped, DropRole)
	}
}

func TestAPrincipalGranteeBecomesASubject(t *testing.T) {
	grant, dropped, err := testRewriter().Grant(DriveShare{
		GranteeType: "principal", GranteeID: "9f1", Permission: "write", Status: "active",
	})
	if err != nil || dropped != "" {
		t.Fatalf("Grant: %v, dropped %q", err, dropped)
	}
	if grant.Kind != "subject" {
		t.Errorf("kind = %q, want subject", grant.Kind)
	}
	if want := "https://issuer.example|9f1"; grant.Grantee != want {
		t.Errorf("grantee = %q, want %q", grant.Grantee, want)
	}
	if grant.Token != "" {
		t.Errorf("a subject grant carries a token: %q", grant.Token)
	}
}

func TestAnOrganizationGranteeBecomesTheMappedSubject(t *testing.T) {
	grant, dropped, err := testRewriter().Grant(DriveShare{
		GranteeType: "org", GranteeID: mappedOrg, Permission: "manage", Status: "active",
	})
	if err != nil || dropped != "" {
		t.Fatalf("Grant: %v, dropped %q", err, dropped)
	}
	if want := "https://issuer.example|org-acme"; grant.Grantee != want {
		t.Errorf("grantee = %q, want %q", grant.Grantee, want)
	}
}

func TestAnUnmappedOrganizationGranteeIsAnError(t *testing.T) {
	_, _, err := testRewriter().Grant(DriveShare{
		GranteeType: "org", GranteeID: "22222222-2222-4222-8222-222222222222",
		Permission: "read", Status: "active",
	})
	if err == nil {
		t.Fatal("an unmapped organization is an error and not a drop")
	}
}

func TestAnInviteTokenIsClearedOnASubjectGrant(t *testing.T) {
	grant, _, err := testRewriter().Grant(DriveShare{
		GranteeType: "principal", GranteeID: "9f1", Permission: "read",
		Token: "accept-me", Status: "active",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if grant.Token != "" || !grant.ClearedToken {
		t.Errorf("token = %q, cleared = %v; a subject grant holds no token", grant.Token, grant.ClearedToken)
	}
}

func TestATokenGrantKeepsItsTokenAndNamesNoGrantee(t *testing.T) {
	for _, kind := range []string{"link", "public"} {
		grant, dropped, err := testRewriter().Grant(DriveShare{
			GranteeType: kind, Permission: "read", Token: "abc", Status: "active",
		})
		if err != nil || dropped != "" {
			t.Fatalf("Grant(%q): %v, dropped %q", kind, err, dropped)
		}
		if grant.Kind != kind || grant.Token != "abc" || grant.Grantee != "" {
			t.Errorf("Grant(%q) = %+v", kind, grant)
		}
		if grant.MintedToken {
			t.Errorf("Grant(%q) minted a token over the one the row carried", kind)
		}
	}
}

func TestATokenGrantWithoutOneIsMintedTheWayDriveMintedIt(t *testing.T) {
	grant, _, err := testRewriter().Grant(DriveShare{
		GranteeType: "public", Permission: "read", Status: "revoked",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !grant.MintedToken {
		t.Fatal("a token grant carrying none is minted one")
	}
	if len(grant.Token) != len(NewToken()) {
		t.Errorf("the minted token is %d characters, want %d", len(grant.Token), len(NewToken()))
	}
}

func TestATokenGrantAboveReadIsDroppedAndNeverDowngraded(t *testing.T) {
	_, dropped, err := testRewriter().Grant(DriveShare{
		GranteeType: "link", Permission: "write", Token: "abc", Status: "active",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if dropped != DropWritableToken {
		t.Errorf("dropped under %q, want %q", dropped, DropWritableToken)
	}
}

func TestAGranteeKindInNeitherSchemaIsAnError(t *testing.T) {
	_, _, err := testRewriter().Grant(DriveShare{
		GranteeType: "unicorn", Permission: "read", Status: "active",
	})
	if err == nil {
		t.Fatal("a kind the tool has never seen is not a kind it may discard")
	}
	if !strings.Contains(err.Error(), "unicorn") {
		t.Errorf("the error names no kind: %v", err)
	}
}

func TestAMintedTokenCarriesItsEntropy(t *testing.T) {
	first, second := NewToken(), NewToken()
	if first == second {
		t.Fatal("two minted tokens are the same")
	}
	if want := (TokenBytes*8 + 5) / 6; len(first) != want {
		t.Errorf("the token is %d characters, want %d", len(first), want)
	}
}
