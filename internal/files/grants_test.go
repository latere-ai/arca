// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// grantsOf is the grants table of the harness's space as the owner policy
// reads it: the rung one grantee holds on each prefix, matched on whole
// segments as spec 008 matches a prefix.
type grantsOf struct {
	grantee string
	held    map[string]auth.Permission
}

func (g *grantsOf) Permission(_ context.Context, _, subject, path string) (auth.Permission, error) {
	if subject != g.grantee {
		return auth.PermissionNone, nil
	}
	best := auth.PermissionNone
	for _, prefix := range store.PrefixesOf(path) {
		if held := g.held[prefix]; !auth.Admits(best, held) {
			best = held
		}
	}
	return best, nil
}

// underTheOwnerPolicy mounts the surface on the built-in owner policy over a
// grants table, which is an installation that runs no authorizer endpoint.
func underTheOwnerPolicy(grants auth.GrantLookup) func(*Options) {
	return func(o *Options) {
		o.Decide = auth.NewAuthorizer(&auth.OwnerPolicy{Grants: grants}).WithGrants(grants)
	}
}

// TestAMoveNeedsAGrantOnBothEnds: under the owner policy a grantee moves an
// object of another space only when the grant it holds admits a write on the
// source and on the destination.
func TestAMoveNeedsAGrantOnBothEnds(t *testing.T) {
	const source, destination = "files/reports/q3.md", "files/inbox/q3.md"
	for _, c := range []struct {
		name  string
		held  map[string]auth.Permission
		moves bool
	}{
		{"write on both", map[string]auth.Permission{
			"files/reports": auth.PermissionWrite, "files/inbox": auth.PermissionWrite}, true},
		{"write on the destination alone", map[string]auth.Permission{
			"files/inbox": auth.PermissionWrite}, false},
		{"write on the source alone", map[string]auth.Permission{
			"files/reports": auth.PermissionWrite}, false},
		{"read on the source and write on the destination", map[string]auth.Permission{
			"files/reports": auth.PermissionRead, "files/inbox": auth.PermissionWrite}, false},
		{"write on the source and read on the destination", map[string]auth.Permission{
			"files/reports": auth.PermissionWrite, "files/inbox": auth.PermissionRead}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			table := &grantsOf{held: c.held}
			h := newHarness(t, underTheOwnerPolicy(table))
			table.grantee = h.issuer.URL() + "|carol"
			h.seed(t, source, "third quarter")

			r := h.request(http.MethodPost, h.object(source),
				strings.NewReader(`{"move_to":"`+destination+`"}`),
				api.HeaderContentType, api.JSONMediaType)
			r.Header.Set("Authorization", "Bearer "+h.issuer.Mint(issuertest.Claims{Sub: "carol"}))
			w := h.send(r)

			_, atSource := h.read(t, source)
			_, atDestination := h.read(t, destination)
			if c.moves {
				if w.Code != http.StatusOK || atSource || !atDestination {
					t.Fatalf("the move answered %d and left source=%t destination=%t: %s",
						w.Code, atSource, atDestination, w.Body)
				}
				return
			}
			// A refusal on a space that is not the caller's own is the
			// answer a missing object gives.
			if got := code(t, w); got != api.CodeNotFound {
				t.Errorf("the move answered %q, want %q", got, api.CodeNotFound)
			}
			if !atSource || atDestination {
				t.Errorf("a refused move left source=%t destination=%t", atSource, atDestination)
			}
			if moved := h.grants.moved; !slices.Equal(moved, nil) {
				t.Errorf("a refused move carried the grants %v", moved)
			}
		})
	}
}
