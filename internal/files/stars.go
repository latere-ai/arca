// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"encoding/base64"
	"net/http"
	"strings"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
)

// A star is a bookmark belonging to a subject, not a property of a file, so
// it lives in its own table and never appears in another reader's view.
//
// Starring asks file.write on the target, which is where spec 006 puts it in
// the vocabulary. That is stricter than the service Arca replaces, where a
// reader could star a file shared with them, and spec 005 records it as a
// decision to review rather than a detail.

// starRequest is what a star names: the space and the path.
type starRequest struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
}

// star bookmarks a path. It is idempotent: starring twice is one row.
func (s *Service) star(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := api.DecodeBody[starRequest](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	t, err := s.Target(ctx, in.Owner, in.Path)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	row, err := s.live(ctx, t)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileWrite, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
	}.Resource()); err != nil {
		api.WriteError(w, r, s.Refused(err, "there is no object at %q", t.Path))
		return
	}
	if err := s.stars.Add(ctx, s.db.Querier(), Caller(ctx), t.Owner, t.Path); err != nil {
		api.WriteError(w, r, fault(ctx, "keep the bookmark", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unstar removes a bookmark. It is idempotent and reads no row: a star whose
// target is gone is still a star the caller may drop, and refusing would
// leave one nobody could remove.
func (s *Service) unstar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.Target(ctx, r.URL.Query().Get("owner"), r.URL.Query().Get("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileWrite, authorizer.File{
		Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
	}.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if err := s.stars.Remove(ctx, s.db.Querier(), Caller(ctx), t.Owner, t.Path); err != nil {
		api.WriteError(w, r, fault(ctx, "drop the bookmark", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listStars answers the caller's stars across every space, joined with live
// rows, so a star whose target was trashed or removed drops out of the
// listing at once and its row is pruned later by the reaper.
func (s *Service) listStars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := Caller(ctx)
	limit, err := api.Limit(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	cursor, err := starCursor(api.Cursor(r))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The stars are the caller's own rows, so the question is about the
	// caller's own space and names no path: the listing crosses spaces and
	// no one prefix describes it.
	if _, err := s.Ask(ctx, caller, authorizer.ActionFileList,
		authorizer.File{Owner: caller}.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	rows, err := s.stars.List(ctx, s.db.Querier(), caller, cursor, limit+1)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "list the bookmarks", err))
		return
	}
	page, next := api.Paginate(rows, limit, renderStarCursor)
	entries := make([]Starred, 0, len(page))
	for _, row := range page {
		entries = append(entries, starred(row))
	}
	api.WritePage(w, entries, next)
}

// The star listing crosses spaces, so it orders by the space and the path
// and its cursor is that pair.
const starCursorSeparator = "\x00"

// renderStarCursor is where a star listing resumes.
func renderStarCursor(s store.Star) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s.Owner + starCursorSeparator + s.Path))
}

// starCursor reads one back.
func starCursor(raw string) (store.StarCursor, error) {
	if raw == "" {
		return store.StarCursor{}, nil
	}
	refuse := api.Refuse(api.CodeInvalidField, "cursor is not one this listing wrote").About("cursor")
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return store.StarCursor{}, refuse
	}
	owner, path, ok := strings.Cut(string(decoded), starCursorSeparator)
	if !ok {
		return store.StarCursor{}, refuse
	}
	return store.StarCursor{Owner: owner, Path: path}, nil
}
