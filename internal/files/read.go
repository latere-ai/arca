// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// The three parameters that shape a read without changing what it names
// (spec 013). Any value but 0 and 1 is a field with a value it cannot take.
const (
	paramInline   = "inline"
	paramDownload = "download"
)

// get answers one object, or one of the three representations a query
// parameter selects. They are checked in the order spec 013 fixes, and they
// are the one place a parameter changes what a GET returns.
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	switch {
	case query.Has("list"):
		s.list(w, r)
	case query.Has("versions"):
		s.listVersions(w, r)
	case query.Has("version"):
		s.readVersion(w, r)
	default:
		s.read(w, r, false)
	}
}

// head runs the same gate as a read and answers its headers with no body,
// including for a public object, where a read redirects.
func (s *Service) head(w http.ResponseWriter, r *http.Request) { s.read(w, r, true) }

// read answers one object by the size rule of spec 005: the bytes at or
// below ARCA_INLINE_BYTES, a redirect above it.
func (s *Service) read(w http.ResponseWriter, r *http.Request, headOnly bool) {
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
	if _, err := s.Ask(ctx, t.Owner, authorizer.ActionFileRead, authorizer.File{
		ID: row.ID, Owner: t.Owner, Path: t.Path, Plane: string(t.Plane),
		Size: authorizer.Bytes(row.SizeBytes),
	}.Resource()); err != nil {
		api.WriteError(w, r, s.Refused(err, "there is no object at %q", t.Path))
		return
	}

	if err := s.serve(w, r, t, row, headOnly); err != nil {
		api.WriteError(w, r, err)
	}
}

// ServeObject answers one object of a space to a caller a link token has
// already authorized, which is the seam spec 008's third link route reaches
// (shares.ObjectReader).
//
// No question is put here. The link route resolved the token, confined the
// path to the grant's prefix and asked link.read with an anonymous subject,
// and asking file.read after it would ask a second question about a caller
// that carries no claim at all: every answer would be a deny and the route
// would serve nothing. What follows the question is this spec's, and it is
// the same half an owner's own read runs, so the two cannot answer one
// object differently.
//
// A path that is not one, and a path that names nothing live, are refused as
// a missing object, which is byte for byte what the route answers for a
// token that names nothing.
func (s *Service) ServeObject(w http.ResponseWriter, r *http.Request, owner, path string) error {
	ctx := r.Context()
	t, err := s.Target(ctx, owner, path)
	if err != nil {
		return err
	}
	row, err := s.live(ctx, t)
	if err != nil {
		return err
	}
	return s.serve(w, r, t, row, false)
}

// serve is the half of a read that follows the question: the conditional
// headers of spec 013 and the size rule of spec 005. It writes its own
// refusals through the handlers it calls and answers an error only where the
// caller has not been written to yet.
func (s *Service) serve(w http.ResponseWriter, r *http.Request, t Target, row store.File, headOnly bool) error {
	pre, err := api.Conditions(r)
	if err != nil {
		return err
	}
	api.SetETag(w, row.Checksum)
	if pre.Fresh(row.Checksum) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if headOnly {
		s.headers(w, row.ContentType, row.SizeBytes, row.UpdatedAt)
		w.WriteHeader(http.StatusOK)
		return nil
	}
	// A public object is readable without this server in the path, so a read
	// of one is a redirect whatever its size: the URL is the point of making
	// it public. With ARCA_PUBLIC_CDN_URL set the redirect is to a base that
	// caches and carries no expiry; without it, to the ordinary presigned
	// URL, which is the fallback spec 003 names for a store that offers
	// bucket policies instead of object ACLs.
	if row.IsPublic {
		if s.cfg.PublicCDNURL != "" {
			http.Redirect(w, r, s.cfg.PublicCDNURL+"/"+row.ObjectID.Key(s.cfg.BucketPrefix), http.StatusFound)
			return nil
		}
		s.presign(w, r, row.ObjectID, t.Path, false)
		return nil
	}
	s.answer(w, r, row.ObjectID, row.SizeBytes, row.ContentType, t.Path)
	return nil
}

// readVersion answers one version of a path, by the same size rule as a
// current read. A version outlives the row, so a trashed path still answers
// its history.
func (s *Service) readVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, n, err := s.versionTarget(ctx, r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
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
	v, err := s.version(ctx, t, n)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	api.SetETag(w, v.Checksum)
	s.answer(w, r, v.ObjectID, v.SizeBytes, v.ContentType, t.Path)
}

// answer is the size rule of spec 005 applied to one object: the bytes at or
// below the inline size, a presigned redirect above it.
//
// ?inline=1 asks for the bytes and is honoured only at or below the
// boundary, because invariant 4 of spec 001 is not a default a caller
// waives. ?inline=0 asks for the redirect at any size, for a client that
// would rather not hold a connection open.
func (s *Service) answer(w http.ResponseWriter, r *http.Request, id object.ID, size int64, media, path string) {
	wants, err := mode(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	download, err := flag(r, paramDownload)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	if size <= s.cfg.InlineBytes && wants != asRedirect {
		s.stream(w, r, id.Key(s.cfg.BucketPrefix), media, size)
		return
	}
	s.presign(w, r, id, path, download)
}

// presign answers the redirect half of a read: one signed URL for one key,
// one method and five minutes, with the path's last segment as the file name
// when the caller asked for a download.
func (s *Service) presign(w http.ResponseWriter, r *http.Request, id object.ID, path string, download bool) {
	ctx := r.Context()
	options := blob.PresignOptions{}
	if download {
		options.Filename = lastSegment(path)
	}
	url, err := s.bucket.PresignGet(ctx, id.Key(s.cfg.BucketPrefix), options)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "sign a read of the object", err))
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// stream copies the bytes through this server, which is what a read at or
// below the inline size answers.
//
// A row whose bytes the bucket does not hold is a lie the database is
// telling, and it answers 500 and never 404: invariant 2 of spec 001 says
// the database decides existence, so an object that exists and cannot be
// read is a fault and a finding for the reaper's second pass.
func (s *Service) stream(w http.ResponseWriter, r *http.Request, key, media string, size int64) {
	ctx := r.Context()
	body, held, err := s.bucket.Get(ctx, key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			slog.ErrorContext(ctx, "files: a row names bytes the bucket does not hold", "error", err)
			api.WriteError(w, r, api.Refuse(api.CodeInternal, ""))
			return
		}
		api.WriteError(w, r, fault(ctx, "read the bytes", err))
		return
	}
	defer func() { _ = body.Close() }()

	if media == "" {
		media = held.ContentType
	}
	s.headers(w, media, size, time.Time{})
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// The status is committed, so the client reads a truncated body
		// under a 200 and this is the only place the truncation is
		// recorded.
		slog.ErrorContext(ctx, "files: a read ended early", "error", err)
	}
}

// headers are what a read and a head both answer about an object.
func (s *Service) headers(w http.ResponseWriter, media string, size int64, modified time.Time) {
	if media == "" {
		media = blob.DefaultContentType
	}
	w.Header().Set(api.HeaderContentType, media)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if !modified.IsZero() {
		w.Header().Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
}

// live reads the row a read names, which is a live one: a trashed path is a
// path nobody can read, and it answers as missing.
func (s *Service) live(ctx context.Context, t Target) (store.File, error) {
	row, err := s.files.Get(ctx, s.db.Querier(), t.Owner, t.Path)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return store.File{}, api.Refuse(api.CodeNotFound, "there is no object at %q", t.Path)
	case err != nil:
		return store.File{}, fault(ctx, "read the path", err)
	case row.DeletedAt != nil:
		return store.File{}, api.Refuse(api.CodeNotFound, "there is no object at %q", t.Path)
	default:
		return row, nil
	}
}

// version reads one version of a path.
func (s *Service) version(ctx context.Context, t Target, n int) (store.Version, error) {
	v, err := s.versions.Get(ctx, s.db.Querier(), t.Owner, t.Path, n)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return store.Version{}, api.Refuse(api.CodeNotFound, "%q has no version %d", t.Path, n)
	case err != nil:
		return store.Version{}, fault(ctx, "read the version", err)
	default:
		return v, nil
	}
}

// versionTarget resolves the path and the version number a request named.
func (s *Service) versionTarget(ctx context.Context, r *http.Request) (Target, int, error) {
	t, err := s.Target(ctx, r.PathValue("owner"), r.PathValue("path"))
	if err != nil {
		return Target{}, 0, err
	}
	n, err := versionNumber(r.URL.Query().Get("version"))
	if err != nil {
		return Target{}, 0, err
	}
	return t, n, nil
}

// versionNumber reads the version a parameter names. Versions count from one
// upward, so anything else is a field with a value it cannot take.
func versionNumber(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, api.Refuse(api.CodeInvalidField,
			"version is %q; a version is a whole number from 1 upward", raw).About("version")
	}
	return n, nil
}

// readMode is what a caller asked for beyond the size rule.
type readMode int

const (
	bySize readMode = iota
	asInline
	asRedirect
)

// mode reads ?inline=. Absent leaves the size rule alone.
func mode(r *http.Request) (readMode, error) {
	if !r.URL.Query().Has(paramInline) {
		return bySize, nil
	}
	switch raw := r.URL.Query().Get(paramInline); raw {
	case "1", "":
		return asInline, nil
	case "0":
		return asRedirect, nil
	default:
		return bySize, api.Refuse(api.CodeInvalidField,
			"inline is %q; it is 0 or 1", raw).About(paramInline)
	}
}

// flag reads a parameter that is 0 or 1 and absent.
func flag(r *http.Request, name string) (bool, error) {
	if !r.URL.Query().Has(name) {
		return false, nil
	}
	switch raw := r.URL.Query().Get(name); raw {
	case "1", "":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, api.Refuse(api.CodeInvalidField, "%s is %q; it is 0 or 1", name, raw).About(name)
	}
}

// lastSegment is the name a browser saves a download under.
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
