// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// createRequest is what opens a session.
type createRequest struct {
	Owner       string `json:"owner"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// Session is what a create answers: the id a client sends back, the part
// size and count it uploads against, one presigned URL per part, and the
// deadline the whole session lives to.
type Session struct {
	ID        string   `json:"id"`
	Owner     string   `json:"owner"`
	Path      string   `json:"path"`
	PartSize  int64    `json:"part_size"`
	PartCount int64    `json:"part_count"`
	PartURLs  []string `json:"part_urls"`
	ExpiresAt string   `json:"expires_at"`
}

// create opens a session: the same gate as a put, then a multipart upload on
// a fresh object id and one presigned URL per part.
//
// A session for a declared size at or below ARCA_INLINE_BYTES is accepted. A
// client that already knows it wants resumable parts should not be argued
// with, and the boundary is what a put refuses above, not what a session
// refuses below.
func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := api.DecodeBody[createRequest](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	t, err := s.content.Target(ctx, in.Owner, in.Path)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if err := s.accepts(in.Size); err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.content.Ask(ctx, t.Owner, authorizer.ActionUploadWrite, authorizer.Upload{
		Owner: t.Owner, Path: t.Path, Size: authorizer.Bytes(in.Size),
	}.Resource())
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	limit, err := files.LimitOf(d)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the answer's limit", err))
		return
	}

	// The key is the session's own object id, so nothing this session does
	// can touch bytes a live row points at (spec 001, invariant 8).
	id := object.NewID()
	key := id.Key(s.content.Prefix())
	media := in.ContentType
	if media == "" {
		media = blob.DefaultContentType
	}
	uploadID, err := s.content.Bucket().CreateMultipart(ctx, key, blob.PutOptions{ContentType: media})
	if err != nil {
		api.WriteError(w, r, fault(ctx, "open the upload", err))
		return
	}

	var written store.Session
	err = s.content.DB().Tx(ctx, func(q store.Querier) error {
		// The declared bytes count from the moment the session opens, so a
		// caller cannot hold a thousand sessions open and fit them all under
		// one limit (spec 010).
		if _, err := s.content.Ledger().Charge(ctx, q, t.Owner, in.Size, limit); err != nil {
			return charged(ctx, err)
		}
		written, err = s.sessions.Insert(ctx, q, store.Session{
			Owner: t.Owner, Path: t.Path, ObjectID: id, UploadID: uploadID,
			DeclaredSize: in.Size, ContentType: media, CreatedBy: files.Caller(ctx),
			ExpiresAt: s.now().Add(TTL),
		})
		if err != nil {
			return fault(ctx, "open the upload", err)
		}
		return nil
	})
	if err != nil {
		// The row never committed, so there is nothing durable pointing at
		// the parts and nothing for the reaper to hold: the multipart is
		// discarded here.
		s.discard(ctx, key, uploadID)
		api.WriteError(w, r, files.Committed(ctx, err))
		return
	}

	urls := make([]string, 0, parts(in.Size))
	for n := range parts(in.Size) {
		url, err := s.content.Bucket().PresignPart(ctx, key, uploadID, int32(n+1))
		if err != nil {
			s.give(ctx, written)
			api.WriteError(w, r, fault(ctx, "sign a part of the upload", err))
			return
		}
		urls = append(urls, url)
	}
	httpjson.Write(w, http.StatusCreated, Session{
		ID: written.ID, Owner: written.Owner, Path: written.Path,
		PartSize: PartSize, PartCount: parts(in.Size), PartURLs: urls,
		ExpiresAt: written.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// accepts is the size rule of spec 007, read before anything opens: a
// declared size over the largest object this server takes, and one over the
// part cap, are refused and open no multipart.
func (s *Service) accepts(size int64) error {
	cfg := s.content.Config()
	switch {
	case size <= 0:
		return api.Refuse(api.CodeInvalidField,
			"size is %d, and a session uploads bytes", size).About("size")
	case size > cfg.MaxUploadBytes:
		return api.Refuse(api.CodeObjectTooLarge,
			"the object is %d bytes and this server accepts %d", size, cfg.MaxUploadBytes)
	case parts(size) > MaxParts:
		return api.Refuse(api.CodeTooManyParts,
			"the object needs %d parts of %d bytes and this server signs %d",
			parts(size), PartSize, MaxParts)
	default:
		return nil
	}
}

// session reads the session a request names and asks the one question every
// route of this package asks. A session nobody may act on is a session that
// is not there.
func (s *Service) session(r *http.Request) (store.Session, files.Limit, error) {
	ctx := r.Context()
	held, err := s.sessions.Get(ctx, s.content.Querier(), r.PathValue("id"))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return store.Session{}, files.Limit{}, api.Refuse(api.CodeNotFound, "there is no upload with that id")
	case err != nil:
		return store.Session{}, files.Limit{}, fault(ctx, "read the upload", err)
	}
	d, err := s.content.Ask(ctx, held.Owner, authorizer.ActionUploadWrite, authorizer.Upload{
		Owner: held.Owner, Path: held.Path, Size: authorizer.Bytes(held.DeclaredSize),
	}.Resource())
	if err != nil {
		return store.Session{}, files.Limit{}, s.content.Refused(err, "there is no upload with that id")
	}
	limit, err := files.LimitOf(d)
	if err != nil {
		return store.Session{}, files.Limit{}, fault(ctx, "read the answer's limit", err)
	}
	return held, limit, nil
}

// close discards a session: the multipart first, and the row only once the
// store has taken the parts.
//
// The row is the sole durable pointer to an upload's parts, because object
// listing does not show them. Deleting it after a failed abort strands them
// for the life of the bucket, billed and unreachable; keeping it costs the
// reaper one retry. It answers whether the store took the parts.
func (s *Service) close(ctx context.Context, held store.Session) error {
	key := held.ObjectID.Key(s.content.Prefix())
	closing := context.WithoutCancel(ctx)
	if err := s.content.Bucket().AbortMultipart(closing, key, held.UploadID); err != nil {
		slog.WarnContext(closing, "uploads: the store would not discard the parts; the row is kept for the reaper",
			"error", err)
		return fault(ctx, "discard the parts of the upload", err)
	}
	return files.Committed(closing, s.content.DB().Tx(closing, func(q store.Querier) error {
		if _, err := s.sessions.Delete(closing, q, held.ID); err != nil {
			return fault(closing, "close the upload", err)
		}
		// The declared bytes were charged when the session opened, and a
		// session that is gone holds none of them.
		if _, err := s.content.Ledger().Release(closing, q, held.Owner, held.DeclaredSize); err != nil {
			return fault(closing, "record what the space holds", err)
		}
		return nil
	}))
}

// discard drops a multipart no row points at, which is one whose session row
// never committed.
func (s *Service) discard(ctx context.Context, key, uploadID string) {
	dropping := context.WithoutCancel(ctx)
	if err := s.content.Bucket().AbortMultipart(dropping, key, uploadID); err != nil {
		slog.WarnContext(dropping, "uploads: discard a multipart no row points at", "error", err)
	}
}

// abort discards a session on the caller's word.
//
// A store that will not take the parts back is told to the caller as the
// outage it is, and the row stays: spec 007 names 502 for it, and the error
// table of spec 013 has no row at 502, so it is the row for a store that
// could not do what was asked. Either way the reaper retries.
func (s *Service) abort(w http.ResponseWriter, r *http.Request) {
	held, _, err := s.session(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if err := s.close(r.Context(), held); err != nil {
		api.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fault renders a failure of either store, in the envelope of spec 013.
func fault(ctx context.Context, what string, err error) error {
	slog.ErrorContext(ctx, "uploads: "+what, "error", err)
	return api.Refuse(api.CodeStorageUnavailable, "could not %s", what)
}

// charged renders a charge the answer's limit did not admit.
func charged(ctx context.Context, err error) error {
	var over *files.OverLimit
	if errors.As(err, &over) {
		return api.Refuse(api.CodeQuotaExceeded, "%s", over.Error())
	}
	if _, ok := errors.AsType[*api.Refusal](err); ok {
		return err
	}
	return fault(ctx, "record what the space holds", err)
}
