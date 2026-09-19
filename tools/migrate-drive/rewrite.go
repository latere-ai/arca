// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"strings"
)

// Rewriter turns Drive's spelling of a space into Arca's. It holds the issuer
// every personal subject is prefixed with and the mapping the platform
// exported for its organizations, and it decides nothing else.
type Rewriter struct {
	issuer string
	orgs   map[string]string
}

// NewRewriter answers a rewriter over one issuer and one mapping. A nil
// mapping is an empty one: every organization then refuses in the preflight,
// which is the answer an operator who forgot -org-subjects needs.
func NewRewriter(issuer string, orgs map[string]string) Rewriter {
	if orgs == nil {
		orgs = map[string]string{}
	}
	return Rewriter{issuer: strings.TrimSuffix(issuer, "/"), orgs: orgs}
}

// Address is Drive's spelling of a space, built from the pair its schema
// holds: u-<id> for a principal and o-<id> for an organization. The rewrite
// below reads the spelling rather than the pair, because the spelling is what
// spec 019 names and what an operator reads in a refusal.
func Address(ownerType, ownerID string) (string, error) {
	switch ownerType {
	case "principal":
		return "u-" + ownerID, nil
	case "org":
		return "o-" + ownerID, nil
	}
	return "", fmt.Errorf("migrate-drive: the owner type %q is neither principal nor org", ownerType)
}

// Organization reads back the organization a Drive address names, and reports false
// for a personal address. The preflight collects these to check the mapping.
func Organization(address string) (string, bool) { return strings.CutPrefix(address, "o-") }

// Subject answers the subject a Drive address becomes. A personal address
// takes the issuer; an organization takes the subject the platform's identity
// provider assigns it, which only the mapping knows, so an unmapped one is an
// error and never a guess.
func (r Rewriter) Subject(address string) (string, error) {
	if id, ok := strings.CutPrefix(address, "u-"); ok {
		return r.issuer + "|" + id, nil
	}
	if id, ok := Organization(address); ok {
		subject, ok := r.orgs[id]
		if !ok {
			return "", fmt.Errorf("migrate-drive: no subject is mapped to the organization %s", id)
		}
		return subject, nil
	}
	return "", fmt.Errorf("migrate-drive: %q is not a Drive address; an address is u-<id> or o-<id>", address)
}

// Owner answers the subject a Drive owner pair becomes, which is the two steps
// above taken together and what every owner column of the copy goes through.
func (r Rewriter) Owner(ownerType, ownerID string) (string, error) {
	address, err := Address(ownerType, ownerID)
	if err != nil {
		return "", err
	}
	return r.Subject(address)
}

// Principal answers the subject a principal id addresses. Drive's created_by,
// actor_id and principal_id columns hold a principal and never an
// organization, so they reach no mapping.
func (r Rewriter) Principal(id string) string { return r.issuer + "|" + id }

// planes maps the leading segment of a Drive path to the plane spec 019
// leaves it in. Four planes become two: files/ and workspaces/ stay, memory/
// folds into files/ as a convention prefix rather than a plane of its own, and
// repos/ becomes workspaces/, because a checked-out tree is a workspace like
// any other and a repository's history lives on a git host.
var planes = map[string]string{
	"files":      "files/",
	"memory":     "files/memory/",
	"repos":      "workspaces/",
	"workspaces": "workspaces/",
}

// Path answers where a Drive path lands. A path whose plane spec 019 gives no
// rule for is an error rather than a guess: Arca has two planes, a row outside
// them is a row no route can reach, and which plane such a path belongs in is
// the maintainer's decision and not this tool's.
//
// The rewrite reads the leading segment and nothing else, so it serves a file
// path, a share's subtree prefix and an event's path alike, with or without a
// trailing slash.
func Path(path string) (string, error) {
	plane, rest, ok := strings.Cut(path, "/")
	if !ok {
		return "", fmt.Errorf("migrate-drive: the path %q names no plane", path)
	}
	prefix, ok := planes[plane]
	if !ok {
		return "", fmt.Errorf("migrate-drive: the path %q is in the plane %q, which spec 019 gives no rule for", path, plane)
	}
	return prefix + rest, nil
}

// PlaneKnown reports whether the leading segment of a path has a rule. The
// preflight asks it of every distinct segment in the source, so a plane with
// no rule is one refusal before the copy rather than a failure part way in.
func PlaneKnown(plane string) bool {
	_, ok := planes[plane]
	return ok
}

// ChecksumKind reads which kind of checksum Drive stored. Drive kept one
// column for two things: the sha256 of the body on a single put, and the
// store's composite ETag on a multipart completion, which is a digest of
// digests and does not have a sha256's shape. Arca holds the two apart in
// checksum_kind, so the shape is what the copy reads them apart by.
func ChecksumKind(checksum string) string {
	if len(checksum) != sha256HexLen {
		return "etag"
	}
	for _, c := range checksum {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "etag"
		}
	}
	return "sha256"
}

// sha256HexLen is the length of a sha256 written in lowercase hexadecimal.
const sha256HexLen = 64
