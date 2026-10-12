// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The two operations of spec 028 on a folder, and the copy of one object.
//
// A folder is a prefix. Arca stores no directory, so a folder is every
// object whose path begins with the folder's path and a slash, and never the
// object at exactly that path: deleting files/notes as a folder touches
// neither files/notes-archive nor a file named files/notes.
//
// Both operations enumerate, so both read limits.omit off the answer that
// admitted them: a subtree it names is neither removed nor copied, and the
// counts the answer and the event carry leave it out.

const (
	// MaxCopyObjects bounds one copy. A copy holds its request open while
	// the bucket works, so a folder above the bound is refused before any
	// bucket call and the caller copies its subfolders.
	MaxCopyObjects = 10_000
	// CopyConcurrency is how many bucket copies one call keeps in flight.
	CopyConcurrency = 8
	// deleteBatch is how many keys one bucket call removes, which is the
	// most the S3 API takes in one.
	deleteBatch = 1000
)

// folder resolves the folder a request names. The plane on its own is a
// prefix a listing may name and is not a folder: emptying or copying a whole
// space is not a folder operation.
func (s *Service) folder(ctx context.Context, owner, path, field string) (Target, error) {
	t, err := s.Subtree(ctx, owner, path)
	if err != nil {
		return Target{}, err
	}
	if root(t) {
		return Target{}, api.Refuse(api.CodeInvalidPath,
			"%q is the plane and not a folder in it", path).About(field)
	}
	return t, nil
}

// removeFolder answers DELETE on a path with ?recursive=1: every object
// under the folder goes to the trash in one statement, or leaves for good
// with ?permanent=1 or under an answer that carries limits.trash: false.
//
// It asks once, about the folder, with recursive on the question and no id
// and no size. A folder with nothing under it and a folder that never
// existed are the same thing, so the call answers 204 either way and is
// idempotent.
func (s *Service) removeFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.folder(ctx, r.PathValue("owner"), r.PathValue("path"), "path")
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	permanent, err := flag(r, "permanent")
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.Ask(ctx, t.Owner, authorizer.ActionFileDelete, authorizer.File{
		Owner: t.Owner, Path: t.Path, Plane: string(t.Plane), Recursive: true,
	}.Resource())
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	limits, err := LimitsOf(d)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the answer's limits", err))
		return
	}

	if !permanent && !limits.NoTrash {
		trashed, err := s.files.TrashSubtree(ctx, s.db.Querier(), t.Owner, t.Path, limits.Omit)
		if err != nil {
			api.WriteError(w, r, fault(ctx, "trash the folder", err))
			return
		}
		s.noteFolderDelete(ctx, t, trashed, true)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Rows first and bytes second (spec 001, invariant 1). The grants and
	// the links under the folder go in the same transaction, for the reason
	// a move carries an exact-path grant: the next object written there must
	// not inherit one its owner gave for something else.
	var gone store.Subtree
	err = s.db.Tx(ctx, func(q store.Querier) error {
		var err error
		if gone, err = s.files.DropSubtree(ctx, q, t.Owner, t.Path, limits.Omit); err != nil {
			return fault(ctx, "remove the folder", err)
		}
		if _, err := s.shares.DeleteUnder(ctx, q, t.Owner, t.Path, limits.Omit); err != nil {
			return fault(ctx, "remove the grants under the folder", err)
		}
		if _, err := s.ledger.Release(ctx, q, t.Owner, gone.Bytes); err != nil {
			return fault(ctx, "record what the space holds", err)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, Committed(ctx, err))
		return
	}
	s.dropMany(ctx, gone.Objects)
	s.noteFolderDelete(ctx, t, gone, false)
	w.WriteHeader(http.StatusNoContent)
}

// noteFolderDelete appends the one event of a folder delete, with the
// counts. A call that found nothing under the folder changed nothing and
// records nothing, so a retry of a delete that already succeeded leaves the
// log as it was.
func (s *Service) noteFolderDelete(ctx context.Context, t Target, sub store.Subtree, trashed bool) {
	if sub.Files == 0 && len(sub.Objects) == 0 {
		return
	}
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: t.Owner, Path: t.Path, Action: EventDelete, Actor: Caller(ctx),
		Detail: map[string]any{
			"recursive": true, "files": sub.Files, "bytes": sub.Bytes, "trashed": trashed,
		},
	})
}

// dropMany removes the bytes of many objects whose rows are already gone: a
// thousand keys to a bucket call, each batch through the reference check of
// spec 004 first.
//
// A batch runs on a context that outlives the request, so a client that
// disconnected does not tear one in half. A request that has ended stops
// before the next batch, and so does a store that fails: the rows are gone,
// so what is left is bytes nothing points at, which the reaper's first pass
// collects.
func (s *Service) dropMany(ctx context.Context, ids []object.ID) {
	dropping := context.WithoutCancel(ctx)
	for batch := range slices.Chunk(ids, deleteBatch) {
		if err := ctx.Err(); err != nil {
			slog.WarnContext(dropping, "files: the request ended before the bytes were removed; leaving the rest for the reaper",
				"error", err)
			return
		}
		free, err := s.files.Unreferenced(dropping, s.db.Querier(), batch)
		if err != nil {
			slog.WarnContext(dropping, "files: read the references of a batch; leaving the bytes for the reaper",
				"error", err)
			return
		}
		if len(free) == 0 {
			continue
		}
		keys := make([]string, len(free))
		for i, id := range free {
			keys[i] = id.Key(s.cfg.BucketPrefix)
		}
		if err := s.bucket.DeleteMany(dropping, keys); err != nil {
			slog.WarnContext(dropping, "files: remove bytes no row points at; leaving them for the reaper",
				"error", err)
			return
		}
	}
}

// copied is what a folder copy answers: where the copy is, and what it
// holds.
type copied struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// copyItem is one object of a copy: the row it reads, the path it writes,
// and what the bucket answered for the copy.
type copyItem struct {
	source store.File
	path   string
	id     object.ID
	stored blob.Object
}

// copyObject copies one object to another path of the same space. The
// bucket copies: the bytes go from one key to another inside the store and
// none passes through this server.
//
// It asks one question, with the destination as the resource's path, the
// source as its from, and copy set, so the rung the question carries is the
// one the caller holds on both paths (spec 006).
func (s *Service) copyObject(w http.ResponseWriter, r *http.Request, to string) {
	ctx := r.Context()
	from, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	dest, err := s.destination(ctx, from.Owner, to)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if dest.Path == from.Path {
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"copy_to is the path the object already has").About("copy_to"))
		return
	}
	row, err := s.live(ctx, from)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.Ask(ctx, from.Owner, authorizer.ActionFileWrite, authorizer.File{
		Owner: from.Owner, Path: dest.Path, Plane: string(dest.Plane),
		Size: authorizer.Bytes(row.SizeBytes), From: from.Path, Copy: true,
	}.Resource())
	if err != nil {
		api.WriteError(w, r, s.Refused(err, "there is no object at %q", from.Path))
		return
	}
	limits, err := LimitsOf(d)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the answer's limits", err))
		return
	}

	items := []copyItem{{source: row, path: dest.Path}}
	written, err := s.copyAll(ctx, from.Owner, items, limits, func(ctx context.Context, q store.Querier) error {
		current, err := s.files.Get(ctx, q, from.Owner, from.Path)
		switch {
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fault(ctx, "read the source", err)
		case err != nil || current.DeletedAt != nil || current.ObjectID != row.ObjectID:
			return api.Refuse(api.CodePreconditionFailed, "%q changed while it was copied", from.Path)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	out := written[0].File
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: from.Owner, Path: dest.Path, Action: EventCopy, Actor: Caller(ctx),
		Detail: map[string]any{"from": from.Path, "recursive": false, "files": 1, "bytes": out.SizeBytes},
	})
	api.SetETag(w, out.Checksum)
	write(w, http.StatusOK, s.Render(out))
}

// destination resolves copy_to, which names a path of the space the source
// is in: a copy does not cross spaces.
func (s *Service) destination(ctx context.Context, owner, to string) (Target, error) {
	plane, err := ValidatePath(to)
	if err != nil {
		if refusal, ok := errors.AsType[*api.Refusal](err); ok {
			refusal.Fields = []string{"copy_to"}
		}
		return Target{}, err
	}
	return Target{Owner: owner, Path: to, Plane: plane}, nil
}

// copyFolder copies every object under a folder to the same path below
// copy_to, in one call.
//
// In order: count what the folder holds, ask once with both ends and the
// sum, read the rows the answer leaves in, copy each object inside the
// bucket to a key of a new id, and write every destination row create-only
// in one transaction that charges the space. A caller sees every object of
// the copy or none.
func (s *Service) copyFolder(w http.ResponseWriter, r *http.Request, to string) {
	ctx := r.Context()
	from, err := s.Subtree(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	dest, err := s.destination(ctx, from.Owner, to)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if store.Omitted([]string{from.Path}, dest.Path) {
		api.WriteError(w, r, api.Refuse(api.CodeInvalidField,
			"copy_to is the folder or inside it").About("copy_to"))
		return
	}
	nothing := api.Refuse(api.CodeNotFound, "there is nothing under %q", from.Path)
	held, err := s.files.StatSubtree(ctx, s.db.Querier(), from.Owner, from.Path, nil)
	switch {
	case err != nil:
		api.WriteError(w, r, fault(ctx, "read the folder", err))
		return
	case held.Files == 0:
		api.WriteError(w, r, nothing)
		return
	}
	d, err := s.Ask(ctx, from.Owner, authorizer.ActionFileWrite, authorizer.File{
		Owner: from.Owner, Path: dest.Path, Plane: string(dest.Plane),
		Size: authorizer.Bytes(held.Bytes), From: from.Path, Copy: true, Recursive: true,
	}.Resource())
	if err != nil {
		api.WriteError(w, r, api.Refused(err, nothing))
		return
	}
	limits, err := LimitsOf(d)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the answer's limits", err))
		return
	}

	items, err := s.copyPlan(ctx, s.db.Querier(), from, dest, limits.Omit)
	switch {
	case err != nil:
		api.WriteError(w, r, err)
		return
	case len(items) == 0:
		api.WriteError(w, r, nothing)
		return
	case len(items) > MaxCopyObjects:
		api.WriteError(w, r, api.Refuse(api.CodeTooManyObjects,
			"the folder holds more than %d objects; copy its subfolders", MaxCopyObjects))
		return
	}
	written, err := s.copyAll(ctx, from.Owner, items, limits, func(ctx context.Context, q store.Querier) error {
		// The rows the bucket copied are the rows the folder still holds:
		// the same paths on the same objects. A write, a delete or a move
		// under the folder since then is a source that changed.
		again, err := s.copyPlan(ctx, q, from, dest, limits.Omit)
		if err != nil {
			return err
		}
		same := slices.EqualFunc(items, again, func(a, b copyItem) bool {
			return a.source.Path == b.source.Path && a.source.ObjectID == b.source.ObjectID
		})
		if !same {
			return api.Refuse(api.CodePreconditionFailed, "the folder %q changed while it was copied", from.Path)
		}
		return nil
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	out := copied{Path: dest.Path, Files: len(written)}
	for _, result := range written {
		out.Bytes += result.File.SizeBytes
	}
	s.ledger.Append(ctx, s.db.Querier(), Event{
		Owner: from.Owner, Path: dest.Path, Action: EventCopy, Actor: Caller(ctx),
		Detail: map[string]any{"from": from.Path, "recursive": true, "files": out.Files, "bytes": out.Bytes},
	})
	write(w, http.StatusOK, out)
}

// copyPlan reads the live rows under a folder that a copy takes, each with
// the path it lands on. A row at or under an omitted subtree is left out,
// and so is a row whose destination is: an answer that leaves a subtree out
// of what the caller enumerates leaves it out at both ends.
//
// It reads one row past the bound, so the caller tells a folder at the bound
// from one above it.
func (s *Service) copyPlan(ctx context.Context, q store.Querier, from, dest Target, omit []string) ([]copyItem, error) {
	prefix := subtree(from.Path)
	rows, _, err := s.files.ListPrefix(ctx, q, from.Owner, prefix, "", MaxCopyObjects+1, omit)
	if err != nil {
		return nil, fault(ctx, "read the folder", err)
	}
	items := make([]copyItem, 0, len(rows))
	for _, row := range rows {
		path := dest.Path + "/" + strings.TrimPrefix(row.Path, prefix)
		if store.Omitted(omit, path) {
			continue
		}
		items = append(items, copyItem{source: row, path: path})
	}
	return items, nil
}

// copyAll copies the bytes of every item inside the bucket and then writes
// every destination row in one transaction, after unchanged has confirmed
// inside it that the sources are what was copied.
//
// The order is invariant 1's: the bucket first and the rows second. Each
// row is written by the write of spec 005 with the create-only precondition,
// so a destination a live object holds refuses the whole copy as path_taken
// and a charge past the answer's limit refuses it as quota_exceeded. A
// refusal leaves no row, and the keys this call wrote are deleted at once:
// each is an id minted here, so nothing else can name it.
func (s *Service) copyAll(ctx context.Context, owner string, items []copyItem, limits Limits,
	unchanged func(context.Context, store.Querier) error) ([]Result, error) {
	if err := s.copyBytes(ctx, items); err != nil {
		s.dropCopies(ctx, items)
		return nil, fault(ctx, "copy the bytes", err)
	}
	written := make([]Result, 0, len(items))
	err := s.db.Tx(ctx, func(q store.Querier) error {
		if err := unchanged(ctx, q); err != nil {
			return err
		}
		for _, item := range items {
			out, err := s.commit(ctx, q, Write{
				Owner: owner, Path: item.path, Actor: Caller(ctx),
				Object: item.id, Size: item.stored.Size,
				Checksum: checksumOf(item), ChecksumKind: kindOf(item),
				ContentType: contentType(item.source.ContentType),
				Pre:         api.Precondition{CreateOnly: true},
				Limit:       limits.Quota, NoVersion: limits.NoVersions,
			})
			if refusal, ok := errors.AsType[*api.Refusal](err); ok && refusal.Code == api.CodePreconditionFailed {
				return api.Refuse(api.CodePathTaken, "%q is taken", item.path)
			}
			if err != nil {
				return err
			}
			written = append(written, out)
		}
		return nil
	})
	if err != nil {
		s.dropCopies(ctx, items)
		return nil, Committed(ctx, err)
	}
	// A destination that revived a trashed path under limits.versions:
	// false superseded bytes no row names any more.
	for _, out := range written {
		s.Settle(ctx, out)
	}
	return written, nil
}

// copyBytes copies each item's object to a key of a new id, CopyConcurrency
// at a time, and records what the store answered. The first failure stops
// the copies not yet started and is the one answered.
func (s *Service) copyBytes(ctx context.Context, items []copyItem) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	next := make(chan int)
	for range min(CopyConcurrency, len(items)) {
		wg.Go(func() {
			for i := range next {
				item := &items[i]
				id := object.NewID()
				stored, err := s.bucket.Copy(ctx, item.source.ObjectID.Key(s.cfg.BucketPrefix),
					id.Key(s.cfg.BucketPrefix), blob.PutOptions{})
				if err != nil {
					cancel(err)
					continue
				}
				// The id is recorded only for a key that holds bytes, so a
				// refused copy is cleaned up by exactly what it wrote.
				item.id, item.stored = id, stored
			}
		})
	}
feed:
	for i := range items {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

// dropCopies removes the keys a refused copy wrote, on a context that
// outlives the request. A failure leaves bytes no row points at, which the
// reaper's first pass collects.
func (s *Service) dropCopies(ctx context.Context, items []copyItem) {
	var keys []string
	for _, item := range items {
		if item.id != "" {
			keys = append(keys, item.id.Key(s.cfg.BucketPrefix))
		}
	}
	dropping := context.WithoutCancel(ctx)
	for batch := range slices.Chunk(keys, deleteBatch) {
		if err := s.bucket.DeleteMany(dropping, batch); err != nil {
			slog.WarnContext(dropping, "files: remove the bytes of a refused copy; leaving them for the reaper",
				"error", err)
		}
	}
}

// checksumOf is the checksum a copy's row carries. A digest of the bytes is
// carried over, since the bytes are the same; a store's label is the one the
// store answered for the copy, because a label names an object and not its
// content.
func checksumOf(item copyItem) string {
	if item.source.ChecksumKind == object.ChecksumSHA256 || item.stored.ETag == "" {
		return item.source.Checksum
	}
	return item.stored.ETag
}

// kindOf is the kind of that checksum, which is the source's.
func kindOf(item copyItem) object.ChecksumKind { return item.source.ChecksumKind }
