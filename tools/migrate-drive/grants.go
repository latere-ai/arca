// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/rand"
	"encoding/base64"
)

// The names the report drops rows under. Each is a row of spec 019's "what is
// removed", counted rather than discarded silently, so the operator reads what
// the cutover cost before the routes switch.
const (
	DropRole          = "role grantees"
	DropTeam          = "team grantees"
	DropEmail         = "email grantees"
	DropShareRequests = "share requests"
	DropWritableToken = "token grants above read"
)

// DriveShare is one row of Drive's shares table, as far as the classification
// reads it.
type DriveShare struct {
	GranteeType string
	GranteeID   string // the principal or the organization; empty when the kind names none
	Permission  string
	Token       string
	Status      string
}

// Grant is the Arca row a Drive share becomes, already shaped to the three
// constraints of migration 0003: a subject grant names a grantee and carries no
// token, a token grant names no grantee and carries a token, and a token grant
// carries read and nothing more.
type Grant struct {
	Kind    string
	Grantee string
	Token   string
	Status  string

	// MintedToken is a token this copy minted because a link or public row
	// carried none. Drive's own migration 000010 minted one for the same
	// reason and left the rows it did not reach, which are the revoked ones.
	MintedToken bool
	// ClearedToken is a token this copy dropped because the row became a
	// subject grant, where the grantee is the subject and not the capability.
	ClearedToken bool
}

// Grant classifies one Drive share. It answers the Arca row, or the name the
// report drops the row under, and it drops for the first reason that applies
// in the order below: the grantee kind spec 019 removed, then the approval
// statuses the platform now owns, then a token grant above read.
func (r Rewriter) Grant(s DriveShare) (Grant, string, error) {
	switch s.GranteeType {
	case "role":
		return Grant{}, DropRole, nil
	case "team":
		return Grant{}, DropTeam, nil
	case "email":
		return Grant{}, DropEmail, nil
	}
	if s.Status == "pending" || s.Status == "denied" {
		return Grant{}, DropShareRequests, nil
	}

	switch s.GranteeType {
	case "principal", "org":
		address, err := Address(principalOrOrg(s.GranteeType), s.GranteeID)
		if err != nil {
			return Grant{}, "", err
		}
		subject, err := r.Subject(address)
		if err != nil {
			return Grant{}, "", err
		}
		return Grant{
			Kind: "subject", Grantee: subject, Status: s.Status,
			ClearedToken: s.Token != "",
		}, "", nil
	case "link", "public":
		// A token grant carries read and nothing more (spec 008). Downgrading
		// one to read would be a policy this tool does not make, so a writable
		// token is dropped and counted.
		if s.Permission != "read" {
			return Grant{}, DropWritableToken, nil
		}
		g := Grant{Kind: s.GranteeType, Token: s.Token, Status: s.Status}
		if g.Token == "" {
			g.Token, g.MintedToken = NewToken(), true
		}
		return g, "", nil
	}
	return Grant{}, "", &unknownGranteeError{kind: s.GranteeType}
}

// principalOrOrg maps a grantee kind to the owner type Address reads, so the
// grantee and the owner of a row go through one rewrite.
func principalOrOrg(granteeType string) string {
	if granteeType == "org" {
		return "org"
	}
	return "principal"
}

// unknownGranteeError is a grantee kind neither Drive's schema nor spec 019
// names. It is an error rather than a drop: a kind the tool has never seen is
// not a kind the tool may decide to discard.
type unknownGranteeError struct{ kind string }

func (e *unknownGranteeError) Error() string {
	return "migrate-drive: the grantee kind " + e.kind + " is in neither schema; spec 019 names no rule for it"
}

// TokenBytes is the entropy of a minted token: 256 bits, the same as the
// tokens internal/shares mints, so a row this copy completes is not weaker
// than a row Arca writes.
const TokenBytes = 32

// NewToken mints one capability, base64url with no padding, which is the
// rendering of spec 008.
//
// crypto/rand.Read does not fail on any supported platform, and a token short
// of its entropy is a capability that can be guessed, so a failure here is not
// something a copy may continue past.
func NewToken() string {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("migrate-drive: the system random source failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
