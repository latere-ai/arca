// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"errors"
	"net/http"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
)

// POST /v1/admin/spaces/{owner}/restore: return one trashed object to the
// space the path names.
//
// It restores across owners, which is the whole reason it exists beside the
// owner's own restore of spec 005, and it takes an id rather than a path.
// The answer's kind is file.
//
// An administrator finds what a space has trashed in the owner's own
// listing, GET /v1/trash?owner=, which carries the purge time derived from
// ARCA_TRASH_RETENTION.

// restored is what the route answers.
type restored struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// restore answers POST /v1/admin/spaces/{owner}/restore.
func (s *Service) restore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := api.DecodeBody[struct {
		ID string `json:"id"`
	}](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	owner, err := space(r, r.PathValue("owner"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if body.ID == "" {
		api.WriteError(w, r, api.Refuse(api.CodeMissingField,
			"a restore names the id of the object to bring back").About("id"))
		return
	}
	// The question comes before the act and before any lookup, so a caller
	// the authorizer refuses learns nothing about what the space holds. A
	// deny is the caller's own refusal and is forbidden: a space's own owner
	// with no space.admin is refused on its own space the same way.
	res := authorizer.Space{Owner: owner}.Resource()
	if _, err := s.authorizer.Decide(ctx, authorizer.ActionSpaceAdmin, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if s.restorer == nil {
		api.WriteError(w, r, api.Refuse(api.CodeNotImplemented,
			"this build binds no restore; the trash of spec 005 is what it returns rows to"))
		return
	}
	back, err := s.restorer.Restore(ctx, owner, body.ID)
	switch {
	case errors.Is(err, ErrNotRestorable):
		// A typo and an expiry are one answer on the wire, and the developer
		// detail names the window, so an administrator is not left guessing
		// between them.
		api.WriteError(w, r, api.Refuse(api.CodeNotFound,
			"no trashed object of that space carries the id %q; "+
				"one past ARCA_TRASH_RETENTION has been purged", body.ID))
		return
	case err != nil:
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, restored{ID: back.ID, Kind: back.Kind, Status: "restored"})
}
