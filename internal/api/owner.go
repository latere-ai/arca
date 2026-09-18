// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"

	"latere.ai/x/arca/internal/auth"
)

// Me is the one alias spec 013 keeps: the caller's own space. The u-<uuid>
// and o-<uuid> forms of the service Arca replaces are gone with the claims
// that produced them, and there is no org alias, because that one was read
// from a claim (spec 019).
const Me = "me"

// Owner resolves the space a request named. A response always renders the
// subject in full, so a client that stores what it read sends it back here
// and addresses the same space.
//
// The empty name is the caller's own space too: a route whose owner is a
// query parameter answers about the caller's space when the parameter is
// absent, which is what makes ?owner= a narrowing and not a requirement.
//
// The percent-encoding of a subject in a path is the router's to undo, and
// of one in a query the URL's, so what arrives here is the subject itself.
func Owner(ctx context.Context, name string) (string, error) {
	if name != "" && name != Me {
		return name, nil
	}
	subject := auth.CallerFrom(ctx).Subject
	if subject == "" {
		// The three public link routes carry no caller, and every other
		// route runs behind the verifier, so this is a caller that reached
		// a route it cannot address a space on.
		return "", Refuse(CodeInvalidField, "%s names the caller's own space, and this request has no caller", Me).About("owner")
	}
	return subject, nil
}

// OwnerOf resolves the space a request's {owner} path wildcard or its
// ?owner= parameter names, in that order, so one call serves both spellings
// of the same rule.
func OwnerOf(r *http.Request) (string, error) {
	name := r.PathValue("owner")
	if name == "" {
		name = r.URL.Query().Get("owner")
	}
	return Owner(r.Context(), name)
}
