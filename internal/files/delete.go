// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"net/http"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// remove answers DELETE on an object: one version with ?version=, the whole
// object otherwise.
//
// Under files/ a delete is soft: the row leaves every listing, read, share
// and link, and the bytes stay, restorable for ARCA_TRASH_RETENTION. Under
// workspaces/ it is hard, because the sync protocol of spec 009 owns those
// trees and a soft row would come back as a phantom file at the next
// materialize. ?permanent=1 is hard anywhere.
func (s *Service) remove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.URL.Query().Has("version") {
		s.removeVersion(w, r)
		return
	}
	t, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	permanent, err := flag(r, "permanent")
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	row, err := s.live(ctx, t)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileDelete, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
		Size: authorizer.Bytes(row.SizeBytes),
	}.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}

	if t.Plane == object.PlaneFiles && !permanent {
		if err := s.trash(ctx, t); err != nil {
			api.WriteError(w, r, err)
			return
		}
		s.ledger.Append(ctx, s.db.Querier(), Event{
			Owner: t.Owner, Path: t.Path, Action: EventDelete, Actor: Caller(ctx),
			Detail: map[string]any{"trashed": true},
		})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.purge(ctx, t); err != nil {
		api.WriteError(w, r, err)
		return
	}
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: t.Owner, Path: t.Path, Action: EventDelete, Actor: Caller(ctx),
	})
	w.WriteHeader(http.StatusNoContent)
}

// trash sets deleted_at and charges nothing: trashed bytes count towards the
// space for the whole retention window, so a space that wants them back
// empties its trash and a reader of the figure is not surprised later.
func (s *Service) trash(ctx context.Context, t Target) error {
	trashed, err := s.files.SoftDelete(ctx, s.db.Querier(), t.Owner, t.Path)
	switch {
	case err != nil:
		return fault(ctx, "trash the path", err)
	case !trashed:
		return api.Refuse(api.CodeNotFound, "there is no object at %q", t.Path)
	default:
		return nil
	}
}

// purge removes the row and its history, then the bytes of both. Rows go
// first, so a failure between the two leaves bytes the reaper finds and
// never a row whose object is gone (spec 001, invariant 1).
func (s *Service) purge(ctx context.Context, t Target) error {
	var gone []object.ID
	err := s.db.Tx(ctx, func(q store.Querier) error {
		row, removed, err := s.files.HardDelete(ctx, q, t.Owner, t.Path)
		switch {
		case err != nil:
			return fault(ctx, "remove the path", err)
		case !removed:
			return api.Refuse(api.CodeNotFound, "there is no object at %q", t.Path)
		}
		history, err := s.versions.DeletePath(ctx, q, t.Owner, t.Path)
		if err != nil {
			return fault(ctx, "remove the history", err)
		}
		freed := row.SizeBytes
		gone = append(gone, row.ObjectID)
		for _, v := range history {
			freed += v.SizeBytes
			gone = append(gone, v.ObjectID)
		}
		if _, err := s.ledger.Release(ctx, q, t.Owner, freed); err != nil {
			return fault(ctx, "record what the space holds", err)
		}
		return nil
	})
	if err != nil {
		return Committed(ctx, err)
	}
	for _, id := range gone {
		s.dropUnreferenced(ctx, id)
	}
	return nil
}

// removeVersion prunes one version: the row first, then the bytes, and only
// when nothing else names them, because a restore moves an object back onto
// the live row.
func (s *Service) removeVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, n, err := s.versionTarget(ctx, r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	row, err := s.live(ctx, t)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileDelete, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
	}.Resource()); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}

	var pruned store.Version
	err = s.db.Tx(ctx, func(q store.Querier) error {
		v, removed, err := s.versions.Delete(ctx, q, t.Owner, t.Path, n)
		switch {
		case err != nil:
			return fault(ctx, "remove the version", err)
		case !removed:
			return api.Refuse(api.CodeNotFound, "%q has no version %d", t.Path, n)
		}
		if _, err := s.ledger.Release(ctx, q, t.Owner, v.SizeBytes); err != nil {
			return fault(ctx, "record what the space holds", err)
		}
		pruned = v
		return nil
	})
	if err != nil {
		api.WriteError(w, r, Committed(ctx, err))
		return
	}
	s.dropUnreferenced(ctx, pruned.ObjectID)
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: t.Owner, Path: t.Path, Action: EventDelete, Actor: Caller(ctx),
		Detail: map[string]any{"version_no": n},
	})
	w.WriteHeader(http.StatusNoContent)
}
