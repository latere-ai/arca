// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"latere.ai/x/pkg/httpjson"
	"latere.ai/x/pkg/relpath"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// Materialize and sync: the two ends of a run.
//
// Bytes never pass through arcad, which is invariant 4 of spec 001.
// Materialize answers the manifest pinned at attach plus one presigned GET
// per file, and the sandbox fetches each URL from the bucket itself. Sync
// takes back a declaration of the post-state; the changed bytes arrived
// through the ordinary write path of spec 005 before the call.
//
// Two properties come from pinning to the attachment's manifest. A reader
// never sees a torn mix of a writer's half-finished sync, because the
// manifest was taken at attach and a sync rewrites it only once it has
// completed. And a URL is signed against the object id the row carries, not
// against a key recomputed from the path, because an object assembled from
// parts (spec 007) carries a key the path does not predict.

// Manifest is what either materialize route answers.
type Manifest struct {
	// Root is the subtree the paths below are relative to.
	Root string `json:"root"`
	// PinnedAt is when the snapshot was taken, null for a manifest that
	// pins nothing.
	PinnedAt *time.Time `json:"pinned_at"`
	// Files are the objects of the snapshot, each with a presigned read.
	Files []File `json:"files"`
}

// File is one entry of a manifest with the URL its bytes are fetched from.
type File struct {
	Entry
	// URL is a presigned GET, valid for the window spec 003 sets.
	URL string `json:"url"`
}

// SyncResult is what a completed write-back answers.
type SyncResult struct {
	SyncedFiles  int       `json:"synced_files"`
	DeletedFiles int       `json:"deleted_files"`
	LastSync     time.Time `json:"last_sync"`
}

// materialize answers GET /v1/workspaces/{id}/materialize.
func (s *Service) materialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.lookup(r, authorizer.ActionWorkspaceRead, live)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	aid := r.URL.Query().Get("attachment")
	if aid == "" {
		api.WriteError(w, r, api.Refuse(api.CodeMissingField,
			"a materialize names the attachment whose snapshot it serves").About("attachment"))
		return
	}
	a, err := s.attachments.Get(ctx, s.db.Querier(), ws.ID, aid)
	if err != nil {
		api.WriteError(w, r, attachmentGone(err, aid))
		return
	}
	if a.Status != store.StatusActive {
		api.WriteError(w, r, ended(a))
		return
	}
	pinned, err := decode(a)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The pinned manifest fixes which paths are served; the bytes are
	// fetched by the key each path's row names now.
	rows, err := s.objects.Manifest(ctx, s.db.Querier(), ws.Owner, Root(ws.Slug))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	keys := make(map[string]object.ID, len(rows))
	for _, f := range rows {
		keys[relative(Root(ws.Slug), f.Path)] = f.ObjectID
	}

	files := make([]File, 0, len(pinned))
	for _, e := range pinned {
		id, held := keys[e.Path]
		if !held {
			// The path was in the snapshot and has since lost its row. It is
			// omitted rather than served as a URL that answers nothing.
			continue
		}
		url, err := s.bucket.PresignGet(ctx, id.Key(s.prefix), blob.PresignOptions{})
		if err != nil {
			api.WriteError(w, r, err)
			return
		}
		files = append(files, File{Entry: e, URL: url})
	}
	// The bytes of spec 018: what the manifest hands out, which is what the
	// client will fetch from the bucket. Nothing of it passes through this
	// process, so this is the only place the number exists.
	var out int64
	for _, f := range files {
		out += f.Size
	}
	s.metrics.Out("materialize", out)
	at := pinnedAt(ws, a)
	httpjson.Write(w, http.StatusOK, Manifest{Root: Root(ws.Slug), PinnedAt: &at, Files: files})
}

// sync answers POST /v1/workspaces/{id}/sync: the write-back boundary.
//
// The writer declares the full post-state of the subtree and the server
// reconciles. A row under the root the manifest does not name is deleted,
// rows first and keys after, which is invariant 1 of spec 001; a path the
// manifest names that has no row is a conflict naming every missing path,
// and the whole sync writes nothing. That last rule is why sync declares
// rather than diffs: Arca never records a boundary that lies about what it
// holds.
func (s *Service) sync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := api.DecodeBody[struct {
		AttachmentID string  `json:"attachment_id"`
		Files        []Entry `json:"files"`
	}](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if body.AttachmentID == "" {
		api.WriteError(w, r, api.Refuse(api.CodeMissingField,
			"a sync names the attachment that holds the writer lease").About("attachment_id"))
		return
	}
	ws, err := s.lookup(r, authorizer.ActionWorkspaceSync, live)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	root := Root(ws.Slug)
	declared, err := declare(root, body.Files)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	a, err := s.attachments.Get(ctx, s.db.Querier(), ws.ID, body.AttachmentID)
	if err != nil {
		api.WriteError(w, r, attachmentGone(err, body.AttachmentID))
		return
	}
	if err := s.writer(ws, a); err != nil {
		api.WriteError(w, r, err)
		return
	}

	var freed []object.ID
	var result SyncResult
	err = s.db.Tx(ctx, func(q store.Querier) error {
		current, err := s.objects.Manifest(ctx, q, ws.Owner, root)
		if err != nil {
			return err
		}
		held := make(map[string]bool, len(current))
		var drop []string
		var kept []store.WorkspaceFile
		for _, f := range current {
			held[f.Path] = true
			if _, named := declared[f.Path]; named {
				kept = append(kept, f)
				continue
			}
			drop = append(drop, f.Path)
		}
		// A client that announced a file it never uploaded is told so, and
		// the refusal comes before the delete: a sync that wrote nothing is
		// a sync the client can repeat once the puts are finished.
		var missing []string
		for path := range declared {
			if !held[path] {
				missing = append(missing, relative(root, path))
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return api.Refuse(api.CodeManifestIncomplete,
				"the manifest names %d object(s) this space does not hold", len(missing)).About(missing...)
		}
		dropped, bytes, err := s.objects.Drop(ctx, q, ws.Owner, drop)
		if err != nil {
			return err
		}
		// Rows first, keys after. What the reference check answers here is
		// what may go: bytes a version or another path still names stay.
		freed, err = s.objects.Unreferenced(ctx, q, dropped)
		if err != nil {
			return err
		}
		if err := s.ledger.Release(ctx, q, ws.Owner, bytes); err != nil {
			return err
		}
		at, err := s.workspaces.StampSync(ctx, q, ws.ID)
		if err != nil {
			return err
		}
		// The attachment is re-pinned to what the rows hold, not to what the
		// client declared, so the same attachment materializes the state the
		// server has rather than the state the client believes it sent.
		post, err := json.Marshal(entries(root, kept))
		if err != nil {
			return err
		}
		if _, err := s.attachments.SetManifest(ctx, q, a.ID, post); err != nil {
			return err
		}
		result = SyncResult{SyncedFiles: len(body.Files), DeletedFiles: len(drop), LastSync: at.UTC()}
		return s.ledger.Append(ctx, q, Event{
			Owner: ws.Owner, Path: root, Action: ActionSync,
			Actor: auth.CallerFrom(ctx).Subject,
			Detail: map[string]any{
				"attachment_id": a.ID, "files": len(body.Files), "deleted": len(drop),
			},
		})
	})
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The bytes of spec 018, once the boundary committed: what the writer
	// declared it had put under the root, which is what the space now holds
	// of this sync.
	var in int64
	for _, e := range body.Files {
		in += e.Size
	}
	s.metrics.In("sync", in)
	s.sweep(ctx, freed)
	httpjson.Write(w, http.StatusOK, result)
}

// sweep removes the bytes the rows no longer name. It runs after the commit,
// so a failure leaves an object with no row, which is what the reaper of
// spec 010 finds and what invariant 1 of spec 001 asks for. Failing the
// request instead would tell a writer its sync did not happen when it did.
//
// The keys go in one call per thousand, so dropping a large tree costs a
// constant number of round trips and not one per file.
//
// The request's cancellation is dropped first. The rows are already
// committed, so a client that hangs up on the response must not be able to
// leave the bytes behind by doing it.
func (s *Service) sweep(ctx context.Context, freed []object.ID) {
	removeBytes(ctx, s.bucket, s.prefix, freed)
}

// removeBytes drops the keys of objects no row names any more. It is shared
// by the sync above and by the tombstone purge of spec 010's pass 6, because
// both reach it having already committed the rows and both owe the same
// promise: the request's cancellation is dropped, so a client that hangs up
// cannot leave the bytes behind, and a failure is a warning rather than a
// refusal to a caller whose work is done.
func removeBytes(ctx context.Context, bucket blob.Store, prefix string, freed []object.ID) {
	if len(freed) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	keys := make([]string, 0, len(freed))
	for _, id := range freed {
		keys = append(keys, id.Key(prefix))
	}
	if err := bucket.DeleteMany(ctx, keys); err != nil {
		slog.WarnContext(ctx, "the bytes the rows no longer name are still in the bucket",
			"keys", len(keys), "err", err)
	}
}

// writer reports whether the attachment may write back: it is still active,
// its mode is rw, and the workspace's lease is still its own. A sync from
// anything else is refused before a row is read.
func (s *Service) writer(ws store.Workspace, a store.Attachment) error {
	if a.Status != store.StatusActive {
		return ended(a)
	}
	if a.Mode != store.ModeWrite {
		return api.Refuse(api.CodeLeaseNotHeld,
			"the attachment %q is %s and a sync is the writer's", a.ID, a.Mode)
	}
	held := s.lease(ws)
	if held == nil || held.Holder != a.Holder {
		return api.Refuse(api.CodeLeaseNotHeld,
			"the attachment %q no longer holds the writer lease of the workspace", a.ID)
	}
	return nil
}

// declare reads a declared manifest as the set of plane-rooted paths it
// names. A path that escapes the root, one that is named twice, and one that
// is not a path at all are refused before anything is read: a manifest is
// acted on whole or not at all.
func declare(root string, files []Entry) (map[string]Entry, error) {
	out := make(map[string]Entry, len(files))
	for _, e := range files {
		clean, err := relpath.Clean(e.Path)
		if err != nil {
			return nil, api.Refuse(api.CodeInvalidPath,
				"the manifest names %q, which is not a path inside the workspace", e.Path).About("files")
		}
		if e.Size < 0 {
			return nil, api.Refuse(api.CodeInvalidField,
				"the manifest gives %q a size of %d bytes", e.Path, e.Size).About("files")
		}
		full := root + clean
		if _, twice := out[full]; twice {
			return nil, api.Refuse(api.CodeInvalidField,
				"the manifest names %q twice", clean).About("files")
		}
		out[full] = e
	}
	return out, nil
}

// decode reads the manifest an attachment pinned.
func decode(a store.Attachment) ([]Entry, error) {
	var pinned []Entry
	if err := json.Unmarshal(a.Manifest, &pinned); err != nil {
		return nil, err
	}
	return pinned, nil
}

// pinnedAt is when the manifest the attachment holds was taken: at attach,
// or at the sync that rewrote it.
//
// Only the lease holder syncs, so a boundary recorded since this attachment
// was opened, while the workspace's lease is still this attachment's, is
// this attachment's own sync. Anything else leaves the attach, which is when
// a read-only snapshot and a writer that has not synced were pinned.
func pinnedAt(ws store.Workspace, a store.Attachment) time.Time {
	if a.Mode == store.ModeWrite && ws.WriterHolder != nil && *ws.WriterHolder == a.Holder &&
		ws.LastSync != nil && ws.LastSync.After(a.CreatedAt) {
		return ws.LastSync.UTC()
	}
	return a.CreatedAt.UTC()
}
