// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

func TestAddressIsDrivesSpellingOfASpace(t *testing.T) {
	for _, c := range []struct {
		ownerType, ownerID, want string
	}{
		{"principal", "abc", "u-abc"},
		{"org", "abc", "o-abc"},
	} {
		got, err := Address(c.ownerType, c.ownerID)
		if err != nil {
			t.Fatalf("Address(%q, %q): %v", c.ownerType, c.ownerID, err)
		}
		if got != c.want {
			t.Errorf("Address(%q, %q) = %q, want %q", c.ownerType, c.ownerID, got, c.want)
		}
	}
	if _, err := Address("team", "abc"); err == nil {
		t.Fatal("an owner type neither schema names is an error")
	}
}

func TestAPersonalAddressTakesTheIssuer(t *testing.T) {
	r := testRewriter()
	got, err := r.Subject("u-9f1")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if want := "https://issuer.example|9f1"; got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestTheIssuerLosesItsTrailingSlash(t *testing.T) {
	r := NewRewriter("https://issuer.example/", "", nil)
	got, err := r.Subject("u-9f1")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if want := "https://issuer.example|9f1"; got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestAnOrganizationTakesTheMappedSubject(t *testing.T) {
	r := testRewriter()
	got, err := r.Subject("o-11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}
	if want := "https://issuer.example|org-acme"; got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
}

func TestAnUnmappedOrganizationIsAnErrorAndNeverAGuess(t *testing.T) {
	r := testRewriter()
	_, err := r.Subject("o-22222222-2222-4222-8222-222222222222")
	if err == nil {
		t.Fatal("an organization in no mapping has no subject")
	}
	if !strings.Contains(err.Error(), "22222222") {
		t.Errorf("the error names no organization: %v", err)
	}
}

func TestAnAddressInNeitherFormIsAnError(t *testing.T) {
	if _, err := testRewriter().Subject("9f1"); err == nil {
		t.Fatal("a bare id is not a Drive address")
	}
}

func TestOwnerTakesThePairTheSchemaHolds(t *testing.T) {
	r := testRewriter()
	got, err := r.Owner("org", "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}
	if want := "https://issuer.example|org-acme"; got != want {
		t.Errorf("Owner = %q, want %q", got, want)
	}
	if _, err := r.Owner("team", "x"); err == nil {
		t.Fatal("an owner type neither schema names is an error")
	}
	if _, err := r.Owner("org", "nope"); err == nil {
		t.Fatal("an unmapped organization is an error")
	}
}

func TestAPrincipalColumnReachesNoMapping(t *testing.T) {
	if got, want := testRewriter().Principal("9f1"), "https://issuer.example|9f1"; got != want {
		t.Errorf("Principal = %q, want %q", got, want)
	}
}

func TestOrganizationReadsBackTheOrganization(t *testing.T) {
	if id, ok := Organization("o-abc"); !ok || id != "abc" {
		t.Errorf("Organization(\"o-abc\") = %q, %v", id, ok)
	}
	if _, ok := Organization("u-abc"); ok {
		t.Error("a personal address names no organization")
	}
}

func TestFourPlanesBecomeTwo(t *testing.T) {
	for _, c := range []struct{ from, want string }{
		{"files/notes.md", "files/notes.md"},
		{"files/", "files/"},
		{"memory/agent.md", "files/memory/agent.md"},
		{"memory/", "files/memory/"},
		{"repos/site/README.md", "workspaces/site/README.md"},
		{"repos/site", "workspaces/site"},
		{"repos/site/", "workspaces/site/"},
		{"workspaces/build/out.txt", "workspaces/build/out.txt"},
	} {
		got, err := Path(c.from)
		if err != nil {
			t.Fatalf("Path(%q): %v", c.from, err)
		}
		if got != c.want {
			t.Errorf("Path(%q) = %q, want %q", c.from, got, c.want)
		}
	}
}

// TestAnOrganizationIssuerDerivesEverySubject is the alternative to the
// mapping file: the platform assigns an organization the subject
// authz.Subject(issuer, <drive organization id>), which is a rule, so there is
// no table for an operator to export and keep in step.
func TestAnOrganizationIssuerDerivesEverySubject(t *testing.T) {
	r := NewRewriter("https://issuer.example", "https://orgs.example", nil)
	for _, id := range []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	} {
		got, err := r.Subject("o-" + id)
		if err != nil {
			t.Fatalf("Subject of an organization: %v", err)
		}
		if want := "https://orgs.example|" + id; got != want {
			t.Errorf("Subject = %q, want %q", got, want)
		}
	}
	// A personal subject still takes its own issuer, which is a different
	// value and the reason the two are two flags.
	if got, _ := r.Subject("u-9f1"); got != "https://issuer.example|9f1" {
		t.Errorf("a personal subject is %q", got)
	}
	// The organization issuer loses its trailing slash the way the personal
	// one does, because a subject is rendered once, by authz.Subject.
	trailing := NewRewriter("https://issuer.example", "https://orgs.example/", nil)
	if got, _ := trailing.Subject("o-9f1"); got != "https://orgs.example|9f1" {
		t.Errorf("the organization issuer kept its slash: %q", got)
	}
}

// TestTheMappingStillWinsWhereNoOrganizationIssuerIsGiven holds the fallback:
// the flag is an alternative to the file, and a run with neither refuses.
func TestTheMappingStillWinsWhereNoOrganizationIssuerIsGiven(t *testing.T) {
	mapped := testRewriter()
	got, err := mapped.Subject("o-11111111-1111-4111-8111-111111111111")
	if err != nil || got != "https://issuer.example|org-acme" {
		t.Fatalf("the mapped subject is %q, %v", got, err)
	}
	if _, err := mapped.Subject("o-22222222-2222-4222-8222-222222222222"); err == nil {
		t.Error("an unmapped organization is not a guess")
	}
}

func TestAPlaneWithNoRuleIsAnErrorAndNeverAGuess(t *testing.T) {
	// A plane spec 019 gives no rule for. The tool refuses rather than
	// reading another plane's rule onto it: which plane such a path belongs
	// in is the maintainer's decision.
	_, err := Path("sandboxes/notes.md")
	if err == nil {
		t.Fatal("a plane spec 019 gives no rule for has no rewrite")
	}
	if !strings.Contains(err.Error(), "sandboxes") {
		t.Errorf("the error names no plane: %v", err)
	}
	if _, err := Path("files"); err == nil {
		t.Fatal("a path with no slash names no plane")
	}
}

// TestTheAgentsZoneFoldsUnderFiles is the maintainer's decision of
// 2026-09-19: the zone rule is gone, so its rows are files like any other
// file, under a prefix that says who wrote them.
func TestTheAgentsZoneFoldsUnderFiles(t *testing.T) {
	for from, want := range map[string]string{
		"agents/notes.md":       "files/agents/notes.md",
		"agents/":               "files/agents/",
		"agents/a/b/c.txt":      "files/agents/a/b/c.txt",
		"agents/memory/note.md": "files/agents/memory/note.md",
	} {
		got, err := Path(from)
		if err != nil {
			t.Fatalf("Path(%q): %v", from, err)
		}
		if got != want {
			t.Errorf("Path(%q) = %q, want %q", from, got, want)
		}
		if !Folded(from) {
			t.Errorf("Folded(%q) = false", from)
		}
	}
	for _, kept := range []string{"files/notes.md", "memory/a.md", "workspaces/site/x", "agentsfoo/x"} {
		if Folded(kept) {
			t.Errorf("Folded(%q) = true", kept)
		}
	}
}

func TestPlaneKnownAnswersTheRules(t *testing.T) {
	for _, plane := range []string{"files", "memory", "agents", "repos", "workspaces"} {
		if !PlaneKnown(plane) {
			t.Errorf("PlaneKnown(%q) = false", plane)
		}
	}
	if PlaneKnown("sandboxes") {
		t.Error("PlaneKnown(\"sandboxes\") = true")
	}
}

func TestChecksumKindReadsTheShapeDriveStored(t *testing.T) {
	for _, c := range []struct{ checksum, want string }{
		{strings.Repeat("a", 64), "sha256"},
		{strings.Repeat("0", 64), "sha256"},
		{strings.Repeat("a", 32) + "-3", "etag"},
		{strings.Repeat("A", 64), "etag"},
		{strings.Repeat("z", 64), "etag"},
		{"", "etag"},
	} {
		if got := ChecksumKind(c.checksum); got != c.want {
			t.Errorf("ChecksumKind(%q) = %q, want %q", c.checksum, got, c.want)
		}
	}
}
