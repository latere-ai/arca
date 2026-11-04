// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"strings"
	"unicode"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/files"
)

// DELETE /v1/admin/spaces/{owner}: delete everything one space holds (spec
// 032).
//
// It asks space.delete and not space.admin, so the caller that closes
// accounts is not also the caller that reads every space's overview. It
// names the space by its full subject: the alias me is refused, because a
// caller that wipes a space by an alias wipes its own by a slip.

// erased is what the route answers.
type erased struct {
	Owner  string `json:"owner"`
	Status string `json:"status"`
	Erased Erased `json:"counts"`
}

// subjectOf validates the {owner} of a deletion: a rendered subject,
// <issuer>|<sub>, of printable characters and a bounded length.
func subjectOf(raw string) error {
	switch {
	case raw == OwnerAlias:
		return api.Refuse(api.CodeInvalidField,
			"a deletion names the space by its subject, and the alias %s is not accepted", OwnerAlias).About("owner")
	case len(raw) > files.MaxSubject:
		return api.Refuse(api.CodeInvalidField,
			"the owner is %d bytes and a subject is at most %d", len(raw), files.MaxSubject).About("owner")
	case strings.IndexFunc(raw, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
		return api.Refuse(api.CodeInvalidField, "the owner holds a character a subject cannot carry").About("owner")
	}
	if issuer, sub, ok := strings.Cut(raw, "|"); !ok || issuer == "" || sub == "" {
		return api.Refuse(api.CodeInvalidField, "the owner is not a subject of the form <issuer>|<sub>").About("owner")
	}
	return nil
}

// erase answers DELETE /v1/admin/spaces/{owner}.
func (s *Service) erase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := r.PathValue("owner")
	if err := subjectOf(owner); err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The question comes before the act, so a caller the authorizer refuses
	// learns nothing about what the space holds.
	res := authorizer.Space{Owner: owner}.Resource()
	if _, err := s.authorizer.Decide(ctx, authorizer.ActionSpaceDelete, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if s.eraser == nil {
		api.WriteError(w, r, api.Refuse(api.CodeNotImplemented,
			"this build binds no space deletion"))
		return
	}
	counts, err := s.eraser.Erase(ctx, owner)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, erased{Owner: owner, Status: "deleted", Erased: counts})
}
