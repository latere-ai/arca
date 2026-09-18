// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package workspaces

import (
	"time"

	"latere.ai/x/arca/internal/store"
)

// The shapes spec 013 renders. Every timestamp is RFC 3339 in UTC and every
// byte count a JSON number, which is the whole of what this file adds to the
// rows behind it.

// Workspace is one workspace as a caller reads it.
//
// The service Arca replaces carried locked, writer_sandbox_id and a separate
// expiry across two differently shaped responses for one resource. One
// nested object says the same thing once, and it is null when no writer
// holds the lease.
type Workspace struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Slug  string `json:"slug"`
	// RootPrefix is the subtree of the workspaces plane this workspace
	// owns. It derives from the slug and is never stored.
	RootPrefix string `json:"root_prefix"`
	// Files and Bytes are the counters of the subtree, carried by the read
	// of one workspace and left out of a listing, which counts nothing.
	Files *int64 `json:"files,omitempty"`
	Bytes *int64 `json:"bytes,omitempty"`
	// Lease is the writer that holds the workspace, null when none does.
	Lease *Lease `json:"lease"`
	// LastSync is the boundary the newest snapshot was taken at, null for a
	// workspace nothing has written back yet.
	LastSync  *time.Time `json:"last_sync"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	// DeletedAt is when the workspace was soft deleted, carried only by the
	// listing of what is still restorable.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Lease is the writer lease a workspace carries.
type Lease struct {
	// Holder is the sandbox or process that holds it, opaque to Arca.
	Holder string `json:"holder"`
	// Mode is the mode the attach asked for, which for a lease is rw.
	Mode string `json:"mode"`
	// ExpiresAt is when the lease lapses.
	ExpiresAt time.Time `json:"expires_at"`
}

// Attachment is one session against one workspace, as an attach answers it.
type Attachment struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Mode        string `json:"mode"`
	// ExpiresAt is when the attachment lapses. A renew pushes it forward.
	ExpiresAt time.Time `json:"expires_at"`
	// Manifest is the snapshot pinned at attach, which materialize serves
	// and a sync declares the successor of.
	Manifest []Entry `json:"manifest"`
}

// Renewal is what a renew answers: the attachment and its new deadline.
type Renewal struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Entry is one file of a manifest: a path relative to the workspace root,
// the checksum of its bytes, and its size. It is the unit both directions,
// so what a sandbox declares and what it was handed are one shape.
type Entry struct {
	Path     string `json:"path"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

// view renders one workspace row.
func (s *Service) view(w store.Workspace) Workspace {
	v := Workspace{
		ID: w.ID, Owner: w.Owner, Slug: w.Slug, RootPrefix: Root(w.Slug),
		Lease: s.lease(w), CreatedBy: w.CreatedBy, CreatedAt: w.CreatedAt.UTC(),
	}
	if w.LastSync != nil {
		at := w.LastSync.UTC()
		v.LastSync = &at
	}
	if w.DeletedAt != nil {
		at := w.DeletedAt.UTC()
		v.DeletedAt = &at
	}
	return v
}

// counted renders one workspace with the counters of its subtree.
func (s *Service) counted(w store.Workspace, files, bytes int64) Workspace {
	v := s.view(w)
	v.Files, v.Bytes = &files, &bytes
	return v
}

// entries renders the rows under a root as a manifest, with each path
// relative to that root. A row whose path is not under the root cannot
// appear: the query that read them asked for the prefix.
func entries(root string, rows []store.WorkspaceFile) []Entry {
	out := make([]Entry, 0, len(rows))
	for _, f := range rows {
		out = append(out, Entry{Path: relative(root, f.Path), Checksum: f.Checksum, Size: f.Size})
	}
	return out
}

// relative trims the root off a plane-rooted path.
func relative(root, path string) string {
	if len(path) <= len(root) {
		return path
	}
	return path[len(root):]
}
