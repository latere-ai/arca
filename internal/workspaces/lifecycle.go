// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// The record and its life: create, list, read, rename, soft delete, the
// listing of what is still restorable, and restore.
//
// Every route that names a workspace by id reads the row first and then asks
// one question about it, because the question carries the space and the slug
// the row holds. A deny is not-found and not forbidden: a reference the
// request named was refused at lookup, which is invariant 6 of spec 001, so
// a refused workspace and a missing one are byte for byte one answer.

// create answers POST /v1/workspaces.
func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := api.DecodeBody[struct {
		Owner string `json:"owner"`
		Slug  string `json:"slug"`
	}](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	owner, err := space(r, body.Owner)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if err := checkSlug(body.Slug); err != nil {
		api.WriteError(w, r, err)
		return
	}
	// There is no row to hide yet, so the question is about the caller's own
	// action and a deny is forbidden rather than not-found.
	res := authorizer.Workspace{Owner: owner, Slug: body.Slug}.Resource()
	if _, err := s.authorizer.Decide(ctx, authorizer.ActionWorkspaceCreate, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	created, err := s.workspaces.Create(ctx, s.db.Querier(), store.Workspace{
		Owner: owner, Slug: body.Slug, CreatedBy: auth.CallerFrom(ctx).Subject,
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		// A tombstone keeps its slug reserved until the reaper purges it, so
		// a deleted workspace collides with a create exactly as a live one
		// does, and the answer says so once.
		api.WriteError(w, r, api.Refuse(api.CodeSlugTaken,
			"the space already holds a workspace called %q, live or deleted", body.Slug).About("slug"))
		return
	case err != nil:
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, s.view(created))
}

// list answers GET /v1/workspaces.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, s.workspaces.List)
}

// listDeleted answers GET /v1/workspaces/deleted, which is what is inside
// the restore window. The word is a literal beside a wildcard and the router
// prefers the literal, so deleted is not a workspace id.
func (s *Service) listDeleted(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, s.workspaces.ListDeleted)
}

// lister is one of the two listings, which differ in the rows they read and
// in nothing else.
type lister func(ctx context.Context, q store.Querier, owner, cursor string, limit int) ([]store.Workspace, error)

// page answers one page of either listing.
func (s *Service) page(w http.ResponseWriter, r *http.Request, read lister) {
	ctx := r.Context()
	owner, err := space(r, r.URL.Query().Get("owner"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	limit, err := api.Limit(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// A list action names the prefix it lists and carries the authorizer's
	// filter when one comes back. There is no row to hide, so a deny is the
	// caller's own refusal.
	res := authorizer.Workspace{Owner: owner}.Resource()
	if _, err := s.authorizer.Decide(ctx, authorizer.ActionWorkspaceList, res); err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	// One row more than the page is read, so the presence of a further page
	// is known without a second count.
	rows, err := read(ctx, s.db.Querier(), owner, api.Cursor(r), limit+1)
	if errors.Is(err, store.ErrBadCursor) {
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"the cursor is not one this listing answered").About("cursor"))
		return
	}
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	page, next := api.Paginate(rows, limit, func(row store.Workspace) string { return row.ID })
	views := make([]Workspace, 0, len(page))
	for _, row := range page {
		views = append(views, s.view(row))
	}
	api.WritePage(w, views, next)
}

// read answers GET /v1/workspaces/{id}: the record, the counters of its
// subtree, and the lease.
func (s *Service) read(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.lookup(r, authorizer.ActionWorkspaceRead, live)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	files, bytes, err := s.objects.Stat(ctx, s.db.Querier(), ws.Owner, Root(ws.Slug))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.counted(ws, files, bytes))
}

// rename answers PATCH /v1/workspaces/{id}.
//
// A rename moves rows and issues no bucket call, which is invariant 8 of
// spec 001: the key derives from the object id the row carries, so renaming
// a workspace of a hundred thousand files is one statement and zero calls to
// the bucket. It is refused while the lease is held, because the writer's
// view of its own paths would change underneath it.
func (s *Service) rename(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.lookup(r, authorizer.ActionWorkspaceWrite, live)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	body, err := api.DecodeBody[struct {
		Slug string `json:"slug"`
	}](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if err := checkSlug(body.Slug); err != nil {
		api.WriteError(w, r, err)
		return
	}
	if body.Slug == ws.Slug {
		httpjson.Write(w, http.StatusOK, s.view(ws))
		return
	}
	renamed := ws
	renamed.Slug = body.Slug
	err = s.db.Tx(ctx, func(q store.Querier) error {
		// The row is held for the rest of the transaction, so the lease read
		// here and the write that follows it are one decision.
		current, err := s.workspaces.GetForUpdate(ctx, q, ws.ID)
		if err != nil {
			return gone(err, ws.ID)
		}
		if current.DeletedAt != nil {
			return notFound(ws.ID)
		}
		if s.held(current) {
			return api.Refuse(api.CodeWriterHeld,
				"the workspace is held by %s until %s", *current.WriterHolder, current.WriterExpiresAt.UTC())
		}
		ok, err := s.workspaces.Rename(ctx, q, ws.ID, body.Slug, s.now())
		switch {
		case errors.Is(err, store.ErrConflict):
			return api.Refuse(api.CodeSlugTaken,
				"the space already holds a workspace called %q, live or deleted", body.Slug).About("slug")
		case err != nil:
			return err
		case !ok:
			return notFound(ws.ID)
		}
		if _, err := s.objects.MoveSubtree(ctx, q, ws.Owner, Root(ws.Slug), Root(body.Slug)); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return api.Refuse(api.CodeSlugTaken,
					"the space already holds objects under %s", Root(body.Slug)).About("slug")
			}
			return err
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.view(renamed))
}

// remove answers DELETE /v1/workspaces/{id}: the soft delete. The subtree
// stays restorable by the owner for the trash window of spec 005, after
// which the reaper of spec 010 purges the rows, the bytes, and the record.
func (s *Service) remove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.lookup(r, authorizer.ActionWorkspaceDelete, live)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	err = s.db.Tx(ctx, func(q store.Querier) error {
		current, err := s.workspaces.GetForUpdate(ctx, q, ws.ID)
		if err != nil {
			return gone(err, ws.ID)
		}
		if current.DeletedAt != nil {
			return notFound(ws.ID)
		}
		if s.held(current) {
			return api.Refuse(api.CodeWriterHeld,
				"the workspace is held by %s until %s", *current.WriterHolder, current.WriterExpiresAt.UTC())
		}
		ok, err := s.workspaces.SoftDelete(ctx, q, ws.ID, s.now())
		if err != nil {
			return err
		}
		if !ok {
			return notFound(ws.ID)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restore answers POST /v1/workspaces/{id}/restore.
//
// It needs no collision guard, and the reason is the uniqueness constraint:
// (owner, slug) is unique regardless of deleted_at, so the tombstone kept
// its slug reserved and no live workspace can have taken the name.
func (s *Service) restore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// This is the one route that reads a deleted workspace, so it looks past
	// the soft delete rather than through it. A workspace the reaper has
	// already purged is gone, which is not-found and not a conflict.
	ws, err := s.lookup(r, authorizer.ActionWorkspaceRestore, deleted)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if ws.DeletedAt == nil {
		api.WriteError(w, r, api.Refuse(api.CodeSlugTaken,
			"the workspace %q is live; a restore brings back a deleted one", ws.Slug))
		return
	}
	restored := ws
	restored.DeletedAt = nil
	err = s.db.Tx(ctx, func(q store.Querier) error {
		ok, err := s.workspaces.Restore(ctx, q, ws.ID)
		if err != nil {
			return err
		}
		if !ok {
			return notFound(ws.ID)
		}
		return s.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: Root(ws.Slug), Action: ActionRestore,
			Actor:  auth.CallerFrom(ctx).Subject,
			Detail: map[string]any{"slug": ws.Slug},
		})
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.view(restored))
}

// ErrNotDeleted is an id that names no soft deleted workspace of the space:
// a typo, an id of another space, a workspace that is live, or one the
// reaper has already purged.
var ErrNotDeleted = errors.New("workspaces: no deleted workspace of that space carries the id")

// RestoreDeleted returns the soft deleted workspace an id names to its slug,
// and is the arm the restore across owners of spec 012 reaches for
// [KindWorkspace].
//
// The space is checked against the row and not taken from it. The
// administrative route asked space.admin on the space its path named, so a
// row of another owner that happens to carry the id is a workspace nothing
// authorized this caller to touch, and it answers as if the id named
// nothing.
//
// It asks nothing further, for the reason the route above asks
// workspace.restore: that question is the owner's, and an administrator
// acting on somebody else's space would be refused it.
func (s *Service) RestoreDeleted(ctx context.Context, owner, id string) (store.Workspace, error) {
	var back store.Workspace
	err := s.db.Tx(ctx, func(q store.Querier) error {
		// The row is held for the rest of the transaction, so the check that
		// it is this space's tombstone and the statement that brings it back
		// cannot be interleaved with a purge or a second restore.
		ws, err := s.workspaces.GetForUpdate(ctx, q, id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotDeleted
		case err != nil:
			return err
		case ws.Owner != owner, ws.DeletedAt == nil:
			return ErrNotDeleted
		}
		ok, err := s.workspaces.Restore(ctx, q, ws.ID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotDeleted
		}
		ws.DeletedAt = nil
		back = ws
		return s.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: Root(ws.Slug), Action: ActionRestore,
			Actor:  auth.CallerFrom(ctx).Subject,
			Detail: map[string]any{"slug": ws.Slug},
		})
	})
	if err != nil {
		return store.Workspace{}, err
	}
	return back, nil
}

// visibility says which workspaces a route reads.
type visibility bool

const (
	// live hides a soft deleted workspace behind the same not-found a
	// missing one gets, which is every route but the restore.
	live visibility = false
	// deleted reads a workspace whether or not it is soft deleted.
	deleted visibility = true
)

// lookup reads the workspace a route names and asks the one question its row
// says, in that order: the question carries the space and the slug the row
// holds, so the row has to be read to ask it.
//
// Every refusal here is the same not-found. A workspace that is not there, a
// workspace this caller may not reach, and a workspace that was soft deleted
// are one answer, which is what keeps a request from being written to
// enumerate what somebody else owns.
func (s *Service) lookup(r *http.Request, action string, see visibility) (store.Workspace, error) {
	ws, err := s.find(r, see)
	if err != nil {
		return store.Workspace{}, err
	}
	if err := s.ask(r, action, ws); err != nil {
		return store.Workspace{}, err
	}
	return ws, nil
}

// find reads the workspace a route names and asks nothing. It is for the two
// routes whose action the attachment they name decides: the mode of that
// attachment has to be read before there is a question to put, and reading a
// row is not acting on it.
func (s *Service) find(r *http.Request, see visibility) (store.Workspace, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	ws, err := s.workspaces.Get(ctx, s.db.Querier(), id)
	if err != nil {
		return store.Workspace{}, gone(err, id)
	}
	if ws.DeletedAt != nil && see == live {
		return store.Workspace{}, notFound(id)
	}
	return ws, nil
}

// ask puts the one question a route asks about a workspace it has read. A
// deny is the not-found every other refusal here is.
func (s *Service) ask(r *http.Request, action string, ws store.Workspace) error {
	ctx := r.Context()
	if _, err := s.authorizer.Lookup(ctx, action, resource(ws).Resource()); err != nil {
		return api.FromAuth(err)
	}
	return nil
}

// gone renders a read that found no row as not-found and leaves every other
// failure a fault: a transient error must never reach a caller as a missing
// workspace.
func gone(err error, id string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(id)
	}
	return err
}

// notFound is the one answer a missing workspace, a refused workspace, and a
// deleted one share.
func notFound(id string) error {
	return api.Refuse(api.CodeNotFound, "there is no workspace %q", id)
}
