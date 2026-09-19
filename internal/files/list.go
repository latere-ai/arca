// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// manifestPage is how many rows a materialize reads per round trip. The
// route answers a whole subtree and pages through it here rather than
// handing a caller a cursor, because a manifest is a snapshot a client
// mounts and half of one is not useful.
const manifestPage = 1000

// list answers the live rows under a prefix, keyset paginated on the path,
// with the common prefixes below it synthesised so a browser can render a
// tree this server does not store.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.Subtree(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	limit, err := api.Limit(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.Ask(ctx, t.Owner, authorizer.ActionFileList, authorizer.File{
		Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
	}.Resource())
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	// The authorizer's filter narrows the page rather than refusing it, so
	// a space outside the filter answers an empty page and never a 403
	// (spec 006).
	if !within(d.Filter, t.Owner) {
		write(w, http.StatusOK, Listing{Entries: []Object{}, Prefixes: []string{}})
		return
	}

	prefix := subtree(t.Path)
	rows, _, err := s.files.ListPrefix(ctx, s.db.Querier(), t.Owner, prefix, api.Cursor(r), limit+1)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "list the subtree", err))
		return
	}
	page, next := api.Paginate(rows, limit, func(f store.File) string { return f.Path })
	listing := Listing{Entries: make([]Object, 0, len(page)), Prefixes: prefixesOf(page, prefix), NextCursor: next}
	for _, f := range page {
		listing.Entries = append(listing.Entries, s.Render(f))
	}
	// A listing of a plane root reports what the space holds, so an owner
	// reads its own usage without an administrator's action (spec 005). The
	// question is the file.list already asked: the space is what that answer
	// was about, and these are the counters of that space.
	if root(t) {
		usage, err := s.ledger.Usage(ctx, s.db.Querier(), t.Owner)
		if err != nil {
			api.WriteError(w, r, fault(ctx, "read what the space holds", err))
			return
		}
		listing.Space = &Space{Bytes: usage.Bytes, Files: usage.Files}
	}
	write(w, http.StatusOK, listing)
}

// root reports whether a listing names a plane root, which is the whole of
// what a root listing is: the path is a plane and nothing under it. Spec 005
// admits a plane root as a prefix and refuses it as a path, so this is the
// one listing that names a space rather than a subtree of one.
func root(t Target) bool { return t.Path == string(t.Plane) }

// materialize answers one space as a manifest of presigned URLs. It reads
// one space and needs no lease, because there is no writer to exclude; the
// leased form is spec 009's.
func (s *Service) materialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base := object.PlaneFiles.Prefix()
	if p := strings.Trim(r.URL.Query().Get("prefix"), "/"); p != "" {
		base = object.PlaneFiles.Prefix() + p + "/"
	}
	t, err := s.Subtree(ctx, r.URL.Query().Get("owner"), strings.TrimSuffix(base, "/"))
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	d, err := s.Ask(ctx, t.Owner, authorizer.ActionFileList, authorizer.File{
		Owner: t.Owner, Path: t.Path, Plane: string(object.PlaneFiles),
	}.Resource())
	if err != nil {
		api.WriteError(w, r, api.FromAuth(err))
		return
	}
	manifest := Manifest{Root: object.PlaneFiles.Prefix(), Files: []ManifestFile{}}
	if !within(d.Filter, t.Owner) {
		write(w, http.StatusOK, manifest)
		return
	}

	// The whole subtree, read a page at a time. The rows are the authority
	// on what exists, and each one's own object is presigned: a key
	// recomputed from a path would name nothing, because a key derives from
	// an id (spec 003).
	cursor := ""
	for {
		rows, _, err := s.files.ListPrefix(ctx, s.db.Querier(), t.Owner, base, cursor, manifestPage)
		if err != nil {
			api.WriteError(w, r, fault(ctx, "list the subtree", err))
			return
		}
		for _, f := range rows {
			url, err := s.bucket.PresignGet(ctx, f.ObjectID.Key(s.cfg.BucketPrefix), blob.PresignOptions{})
			if err != nil {
				api.WriteError(w, r, fault(ctx, "sign a read of the object", err))
				return
			}
			manifest.Files = append(manifest.Files, ManifestFile{
				Path:     strings.TrimPrefix(f.Path, object.PlaneFiles.Prefix()),
				Checksum: f.Checksum, Size: f.SizeBytes, URL: url,
			})
		}
		if len(rows) < manifestPage {
			break
		}
		cursor = rows[len(rows)-1].Path
	}
	write(w, http.StatusOK, manifest)
}

// subtree is the prefix a listing reads under: the path with one slash, so
// a listing of files/notes does not also return files/notes-archive.
func subtree(path string) string {
	if strings.HasSuffix(path, "/") {
		return path
	}
	return path + "/"
}

// prefixesOf synthesises the directories a page implies: the first segment
// below the prefix of every row that has one, in the page's own order and
// without repeats. Arca stores no directory, so this is the whole of what a
// tree view has to read.
func prefixesOf(page []store.File, prefix string) []string {
	out := []string{}
	for _, f := range page {
		rest, ok := strings.CutPrefix(f.Path, prefix)
		if !ok {
			continue
		}
		head, _, nested := strings.Cut(rest, "/")
		if !nested {
			continue
		}
		if directory := prefix + head + "/"; !slices.Contains(out, directory) {
			out = append(out, directory)
		}
	}
	return out
}

// within reports whether a space is inside the filter an answer carried. An
// answer with no filter narrows nothing, which is every answer that did not
// ask for a narrower page.
func within(filter *authz.Filter, owner string) bool {
	if filter == nil || len(filter.Owners) == 0 {
		return true
	}
	return slices.Contains(filter.Owners, owner)
}
