// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
)

// body is what a POST on a path carries: exactly one of move_to,
// restore_version and copy_to, because a move, a restore and a copy are
// three operations and a request that named two named none. recursive goes
// with copy_to and with move_to and makes the path a folder (specs 028 and
// 030); a restore acts on one object.
type body struct {
	MoveTo         string `json:"move_to"`
	RestoreVersion int    `json:"restore_version"`
	CopyTo         string `json:"copy_to"`
	Recursive      bool   `json:"recursive"`
}

// post is the one verb route on a path: a move, a version brought forward,
// or a copy. They ride the file routes because the path pattern swallows any
// suffix a dedicated route would need, and a file legitimately named restore
// or copy has to stay reachable.
func (s *Service) post(w http.ResponseWriter, r *http.Request) {
	in, err := api.DecodeBody[body](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	var named []string
	if in.MoveTo != "" {
		named = append(named, "move_to")
	}
	if in.RestoreVersion != 0 {
		named = append(named, "restore_version")
	}
	if in.CopyTo != "" {
		named = append(named, "copy_to")
	}
	switch {
	case len(named) > 1:
		api.WriteError(w, r, api.Refuse(api.CodeExclusiveFields,
			"move_to renames a path, restore_version brings a version forward and copy_to copies; a request names one").About(named...))
	case in.Recursive && in.CopyTo == "" && in.MoveTo == "":
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"recursive goes with copy_to or move_to; a restore acts on one object").About("recursive"))
	case in.MoveTo != "" && in.Recursive:
		s.moveFolder(w, r, in.MoveTo)
	case in.MoveTo != "":
		s.move(w, r, in.MoveTo)
	case in.CopyTo != "" && in.Recursive:
		s.copyFolder(w, r, in.CopyTo)
	case in.CopyTo != "":
		s.copyObject(w, r, in.CopyTo)
	case in.RestoreVersion > 0:
		s.restoreVersion(w, r, in.RestoreVersion)
	case in.RestoreVersion < 0:
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"restore_version is %d; a version is a whole number from 1 upward", in.RestoreVersion).About("restore_version"))
	default:
		api.WriteError(w, r, api.Refuse(api.CodeMissingField,
			"the body names none of move_to, restore_version and copy_to").About("move_to", "restore_version", "copy_to"))
	}
}

// move changes a path and touches no bytes. A test asserts it makes zero
// calls against the bucket, which is criterion 7 of spec 001.
//
// It asks one question, with the destination as the resource's path and the
// source as its from, so the authorizer sees both ends of the move at once
// and the rung the question carries is the one the caller holds on both
// (spec 006).
func (s *Service) move(w http.ResponseWriter, r *http.Request, to string) {
	ctx := r.Context()
	from, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	dest, err := s.Target(ctx, r.PathValue("owner"), to)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if dest.Path == from.Path {
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"move_to is the path the object already has").About("move_to"))
		return
	}
	row, err := s.live(ctx, from)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.Ask(ctx, from.Owner, authorizer.ActionFileWrite, authorizer.File{
		ID: row.ID, Owner: from.Owner, Path: dest.Path, Plane: string(from.Plane),
		Size: authorizer.Bytes(row.SizeBytes), From: from.Path,
	}.Resource()); err != nil {
		api.WriteError(w, r, s.Refused(err, "there is no object at %q", from.Path))
		return
	}

	err = s.db.Tx(ctx, func(q store.Querier) error {
		moved, err := s.files.Move(ctx, q, from.Owner, from.Path, dest.Path)
		switch {
		case errors.Is(err, store.ErrConflict):
			return api.Refuse(api.CodePathTaken, "%q is taken", dest.Path)
		case err != nil:
			return fault(ctx, "move the path", err)
		case !moved:
			return api.Refuse(api.CodeNotFound, "there is no object at %q", from.Path)
		}
		// The history, the bookmarks and the grants key on the path and
		// follow it. A share on a parent prefix covers a subtree and stays
		// where it is; the one on this exact path means "this object", so it
		// follows the object.
		//
		// Leaving the grant behind would not merely lose it. The old path
		// becomes free, and the next object written there would be covered
		// by a grant its owner gave for something else, handing the grantee
		// an object nobody shared with them.
		if _, err := s.versions.Move(ctx, q, from.Owner, from.Path, dest.Path); err != nil {
			return fault(ctx, "move the history", err)
		}
		if _, err := s.stars.Move(ctx, q, from.Owner, from.Path, dest.Path); err != nil {
			return fault(ctx, "move the bookmarks", err)
		}
		if _, err := s.shares.Move(ctx, q, from.Owner, from.Path, dest.Path); err != nil {
			return fault(ctx, "move the grants", err)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, Committed(ctx, err))
		return
	}
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: from.Owner, Path: dest.Path, Action: EventMove, Actor: Caller(ctx),
		Detail: map[string]any{"from": from.Path},
	})
	row.Path = dest.Path
	api.SetETag(w, row.Checksum)
	write(w, http.StatusOK, s.Render(row))
}

// restoreVersion makes one version current. It is itself non-destructive and
// swaps identities in one transaction: the current content is captured, the
// restored version's metadata becomes the row, and the restored version
// leaves the list, because its object now backs the live file and a later
// retention pass would otherwise delete bytes that are in use.
//
// The ledger is untouched. What the space holds is unchanged: one content
// left the history and another joined it.
//
// Under an answer that carries limits.versions: false the content the
// version replaces is not captured: it is dropped, its bytes are given back
// to the space, and they leave the bucket after the commit (spec 028).
func (s *Service) restoreVersion(w http.ResponseWriter, r *http.Request, n int) {
	ctx := r.Context()
	t, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	row, err := s.live(ctx, t)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.Ask(ctx, t.Owner, authorizer.ActionFileRestore, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
	}.Resource())
	if err != nil {
		api.WriteError(w, r, s.Refused(err, "there is no object at %q", t.Path))
		return
	}
	limits, err := LimitsOf(d)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the answer's limits", err))
		return
	}

	var (
		restored store.File
		dropped  store.File
	)
	err = s.db.Tx(ctx, func(q store.Querier) error {
		current, err := s.files.GetForUpdate(ctx, q, t.Owner, t.Path)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return api.Refuse(api.CodeNotFound, "there is no object at %q", t.Path)
			}
			return fault(ctx, "read the path", err)
		}
		v, err := s.versions.Get(ctx, q, t.Owner, t.Path, n)
		if errors.Is(err, pgx.ErrNoRows) {
			return api.Refuse(api.CodeNotFound, "%q has no version %d", t.Path, n)
		}
		if err != nil {
			return fault(ctx, "read the version", err)
		}
		if limits.NoVersions {
			if _, err := s.ledger.Release(ctx, q, t.Owner, current.SizeBytes); err != nil {
				return fault(ctx, "record what the space holds", err)
			}
			dropped = current
		} else if _, err := s.versions.Capture(ctx, q, t.Owner, t.Path, ""); err != nil {
			return fault(ctx, "capture the version", err)
		}
		if err := s.files.Upsert(ctx, q, store.File{
			Owner: t.Owner, Path: t.Path, ObjectID: v.ObjectID, CreatedBy: v.CreatedBy,
			ContentType: v.ContentType, SizeBytes: v.SizeBytes,
			Checksum: v.Checksum, ChecksumKind: v.ChecksumKind,
		}); err != nil {
			return fault(ctx, "write the path", err)
		}
		if _, _, err := s.versions.Delete(ctx, q, t.Owner, t.Path, n); err != nil {
			return fault(ctx, "remove the restored version", err)
		}
		restored, err = s.files.Get(ctx, q, t.Owner, t.Path)
		if err != nil {
			return fault(ctx, "read the path back", err)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, Committed(ctx, err))
		return
	}
	if dropped.ID != "" && dropped.ObjectID != restored.ObjectID {
		s.dropUnreferenced(ctx, dropped.ObjectID)
	}
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: t.Owner, Path: t.Path, Action: EventRestore, Actor: Caller(ctx),
		Detail: map[string]any{"version_no": n},
	})
	api.SetETag(w, restored.Checksum)
	write(w, http.StatusOK, s.Render(restored))
}

// listVersions answers the history of a path, oldest first, keyset
// paginated on the version number.
func (s *Service) listVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	limit, err := api.Limit(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	after := 0
	if cursor := api.Cursor(r); cursor != "" {
		if after, err = versionCursor(cursor); err != nil {
			api.WriteError(w, r, err)
			return
		}
	}
	// A version outlives the row, so a trashed path still answers its
	// history and the question is asked with the id the row has when there
	// is one.
	row, err := s.files.Get(ctx, s.db.Querier(), t.Owner, t.Path)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		api.WriteError(w, r, fault(ctx, "read the path", err))
		return
	}
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileRead, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
	}.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	rows, err := s.versions.List(ctx, s.db.Querier(), t.Owner, t.Path, after, limit+1)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "list the versions", err))
		return
	}
	page, next := api.Paginate(rows, limit, func(v store.Version) string { return strconv.Itoa(v.VersionNo) })
	entries := make([]Version, 0, len(page))
	for _, v := range page {
		entries = append(entries, version(v))
	}
	api.WritePage(w, entries, next)
}

// versionCursor reads a cursor of the version listing, which is a version
// number.
func versionCursor(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, api.Refuse(api.CodeInvalidField,
			"cursor is %q and this listing resumes on a version number", raw).About("cursor")
	}
	return n, nil
}
