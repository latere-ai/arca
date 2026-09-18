// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"time"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
)

// GET /v1/admin/overview: one row per space that holds anything, with its
// usage and its counts.
//
// Seven counters per space. Bytes is the usage as the ledger of spec 010
// holds it, which is the number a platform bills and compares against
// whatever limit its authorizer hands out; Arca stores no limit, so no column
// here names one. TrashedBytes is the part of that usage trash still holds,
// because the difference is what a reaper run would recover.
//
// There is no email, no display name and no directory. The service Arca
// replaces kept a table populated from the email claim so its admin browser
// could show people instead of identifiers; that table read a claim for
// meaning and does not arrive. A console that wants names resolves the
// subject against its own identity provider.
//
// The installation's totals are not a second route either. An operator that
// wants one number sums the page or reads the aggregate from the metrics of
// spec 018, which costs no query.

// Space is one row of the overview as a caller reads it.
type Space struct {
	// Owner is the space, the subject <issuer>|<sub>.
	Owner string `json:"owner"`
	// Files is the live paths of the space.
	Files int64 `json:"files"`
	// Bytes is the usage the ledger holds for the space.
	Bytes int64 `json:"bytes"`
	// TrashedBytes is the part of that usage trash still holds.
	TrashedBytes int64 `json:"trashed_bytes"`
	// Workspaces is the live workspaces of the space.
	Workspaces int64 `json:"workspaces"`
	// Leases is the workspaces of the space a writer holds right now.
	Leases int64 `json:"leases"`
	// Links is the live token grants on the space.
	Links int64 `json:"links"`
	// LastWriteAt is when a path of the space last changed, null for a space
	// that holds no path.
	LastWriteAt *time.Time `json:"last_write_at"`
}

// overview answers GET /v1/admin/overview.
func (s *Service) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, err := api.Limit(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The overview reads across every space and names none, so the resource
	// carries no owner. A deny is the caller's own refusal and is forbidden:
	// no reference the request named was refused at lookup, and there is no
	// object to hide (spec 012).
	if _, err := s.authorizer.Decide(ctx, authorizer.ActionSpaceAdmin, authorizer.Space{}.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	// One row more than the page is read, so the presence of a further page
	// is known without a second count.
	spaces, err := s.spaces.Overview(ctx, s.querier, api.Cursor(r), limit+1)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	page, next := api.Paginate(spaces, limit, func(row store.SpaceOverview) string { return row.Owner })
	links, err := s.links.Counts(ctx, s.querier, owners(page))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	views := make([]Space, 0, len(page))
	for _, row := range page {
		views = append(views, view(row, links[row.Owner]))
	}
	api.WritePage(w, views, next)
}

// owners is the subjects of one page, for the one link count the page costs.
func owners(page []store.SpaceOverview) []string {
	out := make([]string, 0, len(page))
	for _, row := range page {
		out = append(out, row.Owner)
	}
	return out
}

// view renders one row. Every timestamp is RFC 3339 in UTC and every byte
// count a JSON number, which is the whole of what this adds to the row.
func view(row store.SpaceOverview, links int64) Space {
	s := Space{
		Owner: row.Owner, Files: row.Files, Bytes: row.Bytes,
		TrashedBytes: row.TrashedBytes, Workspaces: row.Workspaces,
		Leases: row.Leases, Links: links,
	}
	if row.LastWriteAt != nil {
		at := row.LastWriteAt.UTC()
		s.LastWriteAt = &at
	}
	return s
}
