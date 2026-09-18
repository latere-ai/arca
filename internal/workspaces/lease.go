// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/store"
)

// The writer lease: attach, renew, release, and the pass that expires one
// whose holder crashed.
//
// A workspace has at most one live lease. A rw attach is a conditional update
// on the workspace row in the same transaction as the attachment insert, so
// a second rw attach matches no row and is a conflict. A ro attach never
// touches the lease and there is no limit on how many exist at once.
//
// The mode of an attach picks the action, so the permission ladder of spec
// 008 falls out with no second rule: a read grantee mounts read-only, and
// only a write grantee can take the lease.

// SweepLimit is how many rows one reaper pass reads. A pass is a batch and
// not a queue: what it does not reach this time it reaches on the next one,
// and a bounded pass keeps one sweep from holding a connection while a large
// backlog drains.
const SweepLimit = 1000

// attach answers POST /v1/workspaces/{id}/attach.
func (s *Service) attach(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := api.DecodeBody[struct {
		SandboxID  string `json:"sandbox_id"`
		Mode       string `json:"mode"`
		TTLSeconds int    `json:"ttl_seconds"`
	}](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if body.SandboxID == "" {
		api.WriteError(w, r, api.Refuse(api.CodeMissingField,
			"an attachment names the sandbox that holds it").About("sandbox_id"))
		return
	}
	mode := store.Mode(body.Mode)
	if !mode.Valid() {
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"the mode is %q; an attachment is %q or %q", body.Mode, store.ModeRead, store.ModeWrite).About("mode"))
		return
	}
	lease, err := ttl(body.TTLSeconds)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The mode picks the action, which is the whole of the ladder: a read
	// grantee reaches workspace.read and mounts read-only, and only a write
	// grantee reaches workspace.attach and can take the lease.
	ws, err := s.lookup(r, actionOf(mode), live)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}

	now := s.now()
	expires := now.Add(lease)
	var opened store.Attachment
	var pinned []Entry
	err = s.db.Tx(ctx, func(q store.Querier) error {
		if mode == store.ModeWrite {
			taken, err := s.workspaces.TakeLease(ctx, q, ws.ID, body.SandboxID, now, expires)
			if err != nil {
				return err
			}
			if !taken {
				// The condition matched no row, so another writer holds the
				// lease. The holder is named in the developer detail, so a
				// runtime fails a sandbox create with something a person can
				// act on rather than a bare conflict.
				return api.Refuse(api.CodeWriterHeld, "the workspace is held by %s", holderOf(ctx, s, q, ws.ID))
			}
		}
		// The snapshot is pinned inside the transaction that took the lease,
		// so what the attachment holds is the state at the moment it became
		// the writer and not a state read a moment before.
		rows, err := s.objects.Manifest(ctx, q, ws.Owner, Root(ws.Slug))
		if err != nil {
			return err
		}
		pinned = entries(Root(ws.Slug), rows)
		manifest, err := json.Marshal(pinned)
		if err != nil {
			return err
		}
		opened, err = s.attachments.Insert(ctx, q, store.Attachment{
			WorkspaceID: ws.ID, Holder: body.SandboxID,
			Subject: auth.CallerFrom(ctx).Subject,
			Mode:    mode, Manifest: manifest, ExpiresAt: expires,
		})
		if err != nil {
			return err
		}
		return s.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: Root(ws.Slug), Action: ActionAttach,
			Actor: auth.CallerFrom(ctx).Subject,
			Detail: map[string]any{
				"attachment_id": opened.ID, "sandbox_id": body.SandboxID, "mode": string(mode),
			},
		})
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, Attachment{
		ID: opened.ID, WorkspaceID: ws.ID, Mode: string(mode),
		ExpiresAt: opened.ExpiresAt.UTC(), Manifest: pinned,
	})
}

// renew answers POST /v1/workspaces/{id}/attach/{aid}/renew: a new deadline
// and nothing else.
func (s *Service) renew(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := api.DecodeOptionalBody[struct {
		TTLSeconds int `json:"ttl_seconds"`
	}](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	lease, err := ttl(body.TTLSeconds)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	ws, attachment, err := s.attachment(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if attachment.Status != store.StatusActive {
		api.WriteError(w, r, ended(attachment))
		return
	}

	expires := s.now().Add(lease)
	err = s.db.Tx(ctx, func(q store.Querier) error {
		ok, err := s.attachments.SetExpiry(ctx, q, attachment.ID, expires)
		if err != nil {
			return err
		}
		if !ok {
			return ended(attachment)
		}
		if attachment.Mode != store.ModeWrite {
			return nil
		}
		// A renew of a writer moves the lease with the attachment. The write
		// is scoped to the holder, so an attachment whose lease was taken by
		// the next writer renews nothing and is told it no longer holds it.
		held, err := s.workspaces.RenewLease(ctx, q, ws.ID, attachment.Holder, expires)
		if err != nil {
			return err
		}
		if !held {
			return api.Refuse(api.CodeLeaseNotHeld,
				"the attachment %q no longer holds the writer lease of the workspace", attachment.ID)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, Renewal{ID: attachment.ID, ExpiresAt: expires.UTC()})
}

// release answers DELETE /v1/workspaces/{id}/attach/{aid}. It is idempotent:
// an attachment that has already ended is the state the caller asked for, so
// it is answered rather than refused.
func (s *Service) release(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, attachment, err := s.attachment(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if attachment.Status != store.StatusActive {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err = s.db.Tx(ctx, func(q store.Querier) error {
		ok, err := s.attachments.Release(ctx, q, attachment.ID)
		if err != nil {
			return err
		}
		if !ok {
			// Something else ended it between the read and the write, which
			// is the state the caller asked for.
			return nil
		}
		if attachment.Mode == store.ModeWrite {
			// Scoped to the holder, so a release arriving after the lease
			// moved on frees the next writer's lease from nobody.
			if _, err := s.workspaces.ReleaseLease(ctx, q, ws.ID, attachment.Holder); err != nil {
				return err
			}
		}
		return s.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: Root(ws.Slug), Action: ActionRelease,
			Actor:  auth.CallerFrom(ctx).Subject,
			Detail: map[string]any{"attachment_id": attachment.ID},
		})
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExpireLeases is the pass the reaper of spec 010 runs on its own schedule.
// The loop and the interval are that spec's; the statements are this one's,
// because what a lapsed lease means is the lease's own spec.
//
// It marks every active attachment past now reaped, clears the lease each
// reaped writer held, and frees a lease whose deadline passed with no
// attachment behind it, which is what an attach that took the lease directly
// leaves when its holder crashes. A reaped writer's unsynced work is lost by
// design: Arca holds no copy of what a sandbox has not sent it, so the only
// alternative is holding a lease forever, and the reap row in the log is
// what makes the loss visible rather than silent.
//
// It answers how many rows it ended, so a caller logs a number and a pass
// that ended nothing says nothing.
func (s *Service) ExpireLeases(ctx context.Context, now time.Time) (int, error) {
	stale, err := s.attachments.Expired(ctx, s.db.Querier(), now, SweepLimit)
	if err != nil {
		return 0, err
	}
	ended := 0
	for _, a := range stale {
		reaped, err := s.expire(ctx, a)
		if err != nil {
			return ended, err
		}
		if reaped {
			ended++
		}
	}
	lapsed, err := s.workspaces.ExpiredLeases(ctx, s.db.Querier(), now, SweepLimit)
	if err != nil {
		return ended, err
	}
	for _, ws := range lapsed {
		freed, err := s.workspaces.ReleaseLease(ctx, s.db.Querier(), ws.ID, *ws.WriterHolder)
		if err != nil {
			return ended, err
		}
		if freed {
			ended++
		}
	}
	return ended, nil
}

// expire ends one attachment whose time ran out, with the lease it held and
// the row in the log, in one transaction.
func (s *Service) expire(ctx context.Context, a store.Attachment) (bool, error) {
	reaped := false
	err := s.db.Tx(ctx, func(q store.Querier) error {
		ok, err := s.attachments.Reap(ctx, q, a.ID)
		if err != nil || !ok {
			return err
		}
		reaped = true
		ws, err := s.workspaces.Get(ctx, q, a.WorkspaceID)
		if err != nil {
			return err
		}
		if a.Mode == store.ModeWrite {
			if _, err := s.workspaces.ReleaseLease(ctx, q, ws.ID, a.Holder); err != nil {
				return err
			}
		}
		return s.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: Root(ws.Slug), Action: ActionReap,
			Actor:  a.Subject,
			Detail: map[string]any{"attachment_id": a.ID, "sandbox_id": a.Holder, "mode": string(a.Mode)},
		})
	})
	if err != nil {
		return false, err
	}
	return reaped, nil
}

// attachment reads the workspace and the attachment a route names, and asks
// the one question the attachment's mode says.
//
// The order matters and is the one invariant 6 asks for. The workspace is
// read first, the attachment second, and the question is put third, so a
// caller that may not reach the workspace learns nothing about whether the
// attachment exists: every refusal along the way is the same not-found.
func (s *Service) attachment(r *http.Request) (store.Workspace, store.Attachment, error) {
	ctx := r.Context()
	ws, err := s.find(r, live)
	if err != nil {
		return store.Workspace{}, store.Attachment{}, err
	}
	aid := r.PathValue("aid")
	a, err := s.attachments.Get(ctx, s.db.Querier(), ws.ID, aid)
	if err != nil {
		return store.Workspace{}, store.Attachment{}, attachmentGone(err, aid)
	}
	if err := s.ask(r, actionOf(a.Mode), ws); err != nil {
		return store.Workspace{}, store.Attachment{}, err
	}
	return ws, a, nil
}

// actionOf is the action an attach in that mode asks, and the action the
// renew and the release that follow it ask in turn.
func actionOf(mode store.Mode) string {
	if mode == store.ModeWrite {
		return authorizer.ActionWorkspaceAttach
	}
	return authorizer.ActionWorkspaceRead
}

// holderOf reads who holds the lease, for the developer detail of a refused
// attach. A read that fails says nothing rather than failing the refusal: the
// conflict is already decided, and the name is a courtesy.
//
// It reads through the querier it is given and never through the pool. The
// caller is inside a transaction, which holds one connection; taking a
// second one for a courtesy read means every contended attach holds one
// connection while it waits for another, and a pool at its default size
// serves a handful of those before the rest block until the acquire
// deadline. The querier is also the transaction's own view, which is the
// right one to report.
func holderOf(ctx context.Context, s *Service, q store.Querier, id string) string {
	ws, err := s.workspaces.Get(ctx, q, id)
	if err != nil || ws.WriterHolder == nil {
		return "another writer"
	}
	return *ws.WriterHolder
}

// ended is the answer to a request against an attachment that is no longer
// active. It is gone and not forbidden: the sandbox must attach again and
// materialize again rather than continue against a snapshot it no longer
// holds a claim to.
func ended(a store.Attachment) error {
	return api.Refuse(api.CodeAttachmentGone, "the attachment %q is %s; attach again", a.ID, a.Status)
}

// attachmentGone renders a read that found no attachment as not-found and
// leaves every other failure a fault.
func attachmentGone(err error, id string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Refuse(api.CodeNotFound, "there is no attachment %q on this workspace", id)
	}
	return err
}
