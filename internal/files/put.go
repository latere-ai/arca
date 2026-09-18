// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"

	"latere.ai/x/arca/object"
)

// put writes one object at a path, at or below ARCA_INLINE_BYTES. Above that
// the bytes do not pass through a replica at all, which is invariant 4 of
// spec 001 and the session API of spec 007.
//
// The order is spec 005's contract: the current row, the question, the
// preconditions, the length, the bytes, and then one transaction that
// captures the version, applies the write and charges the space.
func (s *Service) put(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}

	current, err := s.files.Get(ctx, s.db.Querier(), t.Owner, t.Path)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		api.WriteError(w, r, fault(ctx, "read the path", err))
		return
	}

	d, err := s.Ask(ctx, t.Owner, authorizer.ActionFileWrite, authorizer.File{
		ID: current.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
		Size: declared(r.ContentLength),
	}.Resource())
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	limit, err := LimitOf(d)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the answer's limit", err))
		return
	}

	pre, err := api.Conditions(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if err := s.acceptsLength(r.ContentLength); err != nil {
		api.WriteError(w, r, err)
		return
	}

	// The bytes go first and to a fresh key, so nothing that fails after
	// this can touch an object a live row points at (spec 001, invariant 8).
	id := object.NewID()
	key := id.Key(s.cfg.BucketPrefix)
	written, err := s.bucket.Put(ctx, key, r.Body, r.ContentLength, blob.PutOptions{
		ContentType: r.Header.Get(api.HeaderContentType),
	})
	if err != nil {
		api.WriteError(w, r, fault(ctx, "store the bytes", err))
		return
	}

	out, err := s.Commit(ctx, Write{
		Owner: t.Owner, Path: t.Path, Plane: t.Plane, Actor: Caller(ctx),
		Object: id, Size: written.Size, Checksum: written.SHA256,
		ChecksumKind: object.ChecksumSHA256,
		ContentType:  contentType(r.Header.Get(api.HeaderContentType)),
		Pre:          pre, Limit: limit,
	})
	if err != nil {
		// Nothing points at the key this request wrote, and nothing else
		// can: it is an id minted a moment ago. Deleting it now is what
		// keeps a refused write from leaving the reaper anything to find.
		s.drop(ctx, key)
		api.WriteError(w, r, err)
		return
	}
	s.Settle(ctx, out)

	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: t.Owner, Path: t.Path, Action: EventPut, Actor: Caller(ctx),
		Detail: map[string]any{"size": out.File.SizeBytes},
	})
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	api.SetETag(w, out.File.Checksum)
	write(w, status, s.Render(out.File))
}

// acceptsLength is spec 005's fourth step and the two sizes of spec 007.
// A body with no declared length cannot be admitted, because the store needs
// the length up front and this server buffers no body to find it.
func (s *Service) acceptsLength(length int64) error {
	switch {
	case length < 0:
		return api.Refuse(api.CodeLengthRequired,
			"the body carries no Content-Length, and a write is admitted before its first byte is read")
	case length > s.cfg.MaxUploadBytes:
		return api.Refuse(api.CodeObjectTooLarge,
			"the body is %d bytes and this server accepts %d", length, s.cfg.MaxUploadBytes)
	case length > s.cfg.InlineBytes:
		return api.Refuse(api.CodeObjectTooLarge,
			"the body is %d bytes and this server streams %d through itself; open a session at POST /v1/uploads",
			length, s.cfg.InlineBytes)
	default:
		return nil
	}
}

// Settle drops the bytes a write superseded and kept no version of. A
// versioned overwrite keeps them, because a version row now names them.
//
// It runs after the commit, so a failure leaves bytes no row points at,
// which the reaper's first pass collects. The reference check is the one of
// spec 004, which reads every table that holds an object id.
func (s *Service) Settle(ctx context.Context, out Result) {
	if out.Versioned || out.Replaced.ID == "" || out.Replaced.ObjectID == out.File.ObjectID {
		return
	}
	s.dropUnreferenced(ctx, out.Replaced.ObjectID)
}

// dropUnreferenced removes an object's bytes unless a row still names them.
// A failure is left to the reaper: the rows are already gone, so the bytes
// are orphaned and its first pass is what they are for.
func (s *Service) dropUnreferenced(ctx context.Context, id object.ID) {
	referenced, err := s.references.Referenced(ctx, s.db.Querier(), id)
	if err != nil {
		slog.WarnContext(ctx, "files: read the references of an object; leaving the bytes for the reaper",
			"error", err)
		return
	}
	if referenced {
		return
	}
	s.drop(ctx, id.Key(s.cfg.BucketPrefix))
}

// drop removes one key, on a context that outlives the request: a client
// that disconnected cancelled its own, and a delete on a cancelled context
// would leave the orphan behind for no reason.
func (s *Service) drop(ctx context.Context, key string) {
	dropping := context.WithoutCancel(ctx)
	if err := s.bucket.Delete(dropping, key); err != nil {
		slog.WarnContext(dropping, "files: remove bytes no row points at; leaving them for the reaper",
			"error", err)
	}
}

// contentType is what a row records, the caller's or the default.
func contentType(media string) string {
	if media == "" {
		return blob.DefaultContentType
	}
	return media
}

// declared is the size a question carries. A body with no length declared
// none, and a question that has not looked a size up sends no size at all
// rather than a number nobody meant (spec 006).
func declared(length int64) *int64 {
	if length < 0 {
		return nil
	}
	return authorizer.Bytes(length)
}
