// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// listTrash answers what a space has trashed and can still restore, newest
// first. A row past ARCA_TRASH_RETENTION is the reaper's and is not offered
// to a caller that could not restore it.
func (s *Service) listTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner, err := s.owner(ctx, r.URL.Query().Get("owner"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	limit, err := api.Limit(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	cursor, err := trashCursor(api.Cursor(r))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.Ask(ctx, owner, authorizer.ActionFileList, authorizer.File{Owner: owner}.Resource())
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	if !within(d.Filter, owner) {
		api.WritePage(w, []Trashed{}, "")
		return
	}
	rows, err := s.files.ListTrash(ctx, s.db.Querier(), owner, cursor, limit+1, s.window())
	if err != nil {
		api.WriteError(w, r, fault(ctx, "list the trash", err))
		return
	}
	page, next := api.Paginate(rows, limit, func(f store.File) string { return renderTrashCursor(f) })
	entries := make([]Trashed, 0, len(page))
	for _, f := range page {
		entries = append(entries, trashed(f, s.cfg.TrashRetention))
	}
	api.WritePage(w, entries, next)
}

// restoreRequest is what a restore names: the space and the path.
type restoreRequest struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
}

// restoreTrash returns one object to its path. A path a live row has taken
// back is a conflict and not a missing object: the caller can see that
// something is there, and what it needs is to move it or to name another
// path.
func (s *Service) restoreTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := api.DecodeBody[restoreRequest](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	t, err := s.Target(ctx, in.Owner, in.Path)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	row, err := s.files.Get(ctx, s.db.Querier(), t.Owner, t.Path)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		api.WriteError(w, r, api.Refuse(api.CodeNotFound, "%q is not in the trash", t.Path))
		return
	case err != nil:
		api.WriteError(w, r, fault(ctx, "read the path", err))
		return
	case row.DeletedAt == nil:
		api.WriteError(w, r, api.Refuse(api.CodePathTaken, "a live object holds %q", t.Path))
		return
	}
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileRestore, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
		Size: authorizer.Bytes(row.SizeBytes),
	}.Resource()); err != nil {
		api.WriteError(w, r, s.Refused(err, "%q is not in the trash", t.Path))
		return
	}
	restored, err := s.files.Restore(ctx, s.db.Querier(), t.Owner, t.Path, s.window())
	switch {
	case err != nil:
		api.WriteError(w, r, fault(ctx, "restore the path", err))
		return
	case !restored:
		// The row was read a moment ago and is not restorable now: it is
		// past the window, or another restore of the same path won.
		api.WriteError(w, r, api.Refuse(api.CodeNotFound, "%q is not in the trash", t.Path))
		return
	}
	row.DeletedAt = nil
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: t.Owner, Path: t.Path, Action: EventRestore, Actor: Caller(ctx),
	})
	api.SetETag(w, row.Checksum)
	write(w, http.StatusOK, s.Render(row))
}

// ErrNotTrashed is an id that names no trashed object of the space still
// inside the retention window: a typo, an id of another space, or one the
// reaper has already purged.
var ErrNotTrashed = errors.New("files: no trashed object of that space carries the id")

// RestoreTrashed returns the trashed object an id names to its path, and is
// the arm the restore across owners of spec 012 reaches for [KindFile].
//
// It takes an id where the route above takes a path, because the
// administrative restore takes one id that names either a trashed object or
// a soft deleted workspace and answers which it was. The condition is the
// trash's own: the row is the space's, it is trashed, and it is inside
// ARCA_TRASH_RETENTION.
//
// It asks nothing. The caller asked space.admin before it reached here, and
// asking file.restore as well would ask an administrator for a permission on
// a space it does not own, which is the one thing the administrative route
// exists to act without.
func (s *Service) RestoreTrashed(ctx context.Context, owner, id string) (store.File, error) {
	row, restored, err := s.files.RestoreByID(ctx, s.db.Querier(), owner, id, s.window())
	switch {
	case err != nil:
		return store.File{}, fault(ctx, "restore the object", err)
	case !restored:
		return store.File{}, ErrNotTrashed
	}
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: owner, Path: row.Path, Action: EventRestore, Actor: Caller(ctx),
	})
	return row, nil
}

// purged is what emptying a trash answers: how many entries left for good.
type purged struct {
	Purged int `json:"purged"`
}

// purgeTrash removes trashed entries now, one path with ?path= and the whole
// trash without one. Rows go before bytes, and a version of a purged path
// goes with it: a purge is the end of the line.
func (s *Service) purgeTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner, err := s.owner(ctx, r.URL.Query().Get("owner"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	path := r.URL.Query().Get("path")
	resource := authorizer.File{Owner: owner}
	if path != "" {
		plane, err := ValidatePath(path)
		if err != nil {
			api.WriteError(w, r, err)
			return
		}
		resource = authorizer.File{Owner: owner, Path: path, Plane: string(plane)}
	}
	if _, err := s.Ask(ctx, owner, authorizer.ActionFileDelete, resource.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}

	var gone []object.ID
	var count int
	err = s.db.Tx(ctx, func(q store.Querier) error {
		rows, err := s.files.PurgeTrash(ctx, q, owner, path)
		if err != nil {
			return fault(ctx, "empty the trash", err)
		}
		if path != "" && len(rows) == 0 {
			return api.Refuse(api.CodeNotFound, "%q is not in the trash", path)
		}
		freed := int64(0)
		for _, f := range rows {
			freed += f.SizeBytes
			gone = append(gone, f.ObjectID)
			history, err := s.versions.DeletePath(ctx, q, owner, f.Path)
			if err != nil {
				return fault(ctx, "remove the history", err)
			}
			for _, v := range history {
				freed += v.SizeBytes
				gone = append(gone, v.ObjectID)
			}
		}
		if _, err := s.ledger.Release(ctx, q, owner, freed); err != nil {
			return fault(ctx, "record what the space holds", err)
		}
		count = len(rows)
		return nil
	})
	if err != nil {
		api.WriteError(w, r, Committed(ctx, err))
		return
	}
	for _, id := range gone {
		s.dropUnreferenced(ctx, id)
	}
	write(w, http.StatusOK, purged{Purged: count})
}

// window is the moment a trashed row stops being restorable.
func (s *Service) window() time.Time { return s.now().Add(-s.cfg.TrashRetention) }

// The trash listing is newest first, so its cursor is the pair it orders by.
// It is opaque to a caller, which is what lets it be a pair at all.
const trashCursorSeparator = "\x00"

// renderTrashCursor is where a trash listing resumes.
func renderTrashCursor(f store.File) string {
	at := time.Time{}
	if f.DeletedAt != nil {
		at = *f.DeletedAt
	}
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + trashCursorSeparator + f.Path))
}

// trashCursor reads one back. A cursor this server did not write is a field
// with a value it cannot take, and never a page starting somewhere else.
func trashCursor(raw string) (store.TrashCursor, error) {
	if raw == "" {
		return store.TrashCursor{}, nil
	}
	refuse := api.Refuse(api.CodeInvalidField, "cursor is not one this listing wrote").About("cursor")
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return store.TrashCursor{}, refuse
	}
	stamp, path, ok := strings.Cut(string(decoded), trashCursorSeparator)
	if !ok {
		return store.TrashCursor{}, refuse
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return store.TrashCursor{}, refuse
	}
	return store.TrashCursor{DeletedAt: at, Path: path}, nil
}
