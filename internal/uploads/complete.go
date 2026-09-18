// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// completeRequest is the part list a completion carries: every part's number
// and the label the store answered when the client uploaded it.
type completeRequest struct {
	Parts []struct {
		N    int32  `json:"n"`
		ETag string `json:"etag"`
	} `json:"parts"`
}

// complete assembles the parts and writes the row.
//
// It asks upload.write again rather than file.write: the session was
// authorized against the path when it opened, and one action over the whole
// flow keeps a client from passing a gate at create that it would fail at
// completion.
//
// In order: read the session, refuse a doomed completion before anything is
// assembled, assemble, head the assembled object for its true size and
// label, and write the row in one transaction that also captures the
// version, charges the difference and deletes the session.
func (s *Service) complete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := api.DecodeBody[completeRequest](r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	held, limit, err := s.session(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	assembled, err := parcels(in)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	pre, err := api.Conditions(r)
	if err != nil {
		api.WriteError(w, r, err)
		return
	}
	// The conditional-write contract of spec 005, checked against the row
	// before assembly so a doomed completion costs nothing. The write repeats
	// it in SQL, which is what decides a race.
	if err := s.doomed(ctx, held, pre); err != nil {
		s.give(ctx, held)
		api.WriteError(w, r, err)
		return
	}

	key := held.ObjectID.Key(s.content.Prefix())
	if _, err := s.content.Bucket().CompleteMultipart(ctx, key, held.UploadID, assembled); err != nil {
		// A retry whose upload id the store no longer knows may be a second
		// attempt after the first assembled the object and failed at the
		// database. If the object is there the completion resumes from the
		// row write; if it is not, the parts really were wrong.
		if _, head := s.content.Bucket().Head(ctx, key); head != nil {
			// The store does not say which part it could not find, so the
			// completion is counted as one missing part rather than as a
			// number this package would be inventing (spec 018).
			s.metrics.UploadPart(partMissing)
			slog.ErrorContext(ctx, "uploads: the parts did not assemble", "error", err)
			api.WriteError(w, r, api.Refuse(api.CodeBadRequest,
				"the parts did not assemble; check every part's number and the label the store answered for it"))
			return
		}
	}
	stored, err := s.content.Bucket().Head(ctx, key)
	if err != nil {
		api.WriteError(w, r, fault(ctx, "read the assembled object", err))
		return
	}

	// The declared size was a promise and the head is the fact, so the space
	// is charged the difference and the row records what the store holds. The
	// checksum is the store's composite label and not a digest of the bytes,
	// which the row says with its kind.
	out, err := s.content.Commit(ctx, files.Write{
		Owner: held.Owner, Path: held.Path, Plane: plane(held.Path), Actor: files.Caller(ctx),
		Object: held.ObjectID, Size: stored.Size,
		Checksum: stored.ETag, ChecksumKind: object.ChecksumETag,
		ContentType: held.ContentType, Pre: pre, Limit: limit,
		Charged: held.DeclaredSize,
		Also: func(ctx context.Context, q store.Querier) error {
			if _, err := s.sessions.Delete(ctx, q, held.ID); err != nil {
				return fault(ctx, "close the upload", err)
			}
			return nil
		},
	})
	if err != nil {
		// A completion the caller's own request refused will not come back:
		// the assembled object is on the session's own key and no live row
		// names it, so it goes now and the session goes with it. A store
		// that failed will come back, so both stay and the retry resumes
		// from the row write rather than from a part.
		if caused(err) {
			s.drop(ctx, key)
			s.give(ctx, held)
		}
		api.WriteError(w, r, err)
		return
	}
	s.content.Settle(ctx, out)

	// The row is written, so the parts are an object in the space. The bytes
	// are the head's and not the declaration's, which is the same number the
	// charge above was corrected to (spec 018).
	s.metrics.UploadSession(sessionCompleted)
	for range assembled {
		s.metrics.UploadPart(partCompleted)
	}
	s.metrics.In("part", out.File.SizeBytes)

	s.content.Ledger().Append(ctx, s.content.Querier(), files.Event{
		Owner: held.Owner, Path: held.Path, Action: files.EventPut, Actor: files.Caller(ctx),
		Detail: map[string]any{"size": out.File.SizeBytes, "multipart": true},
	})
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	api.SetETag(w, out.File.Checksum)
	httpjson.Write(w, status, s.content.Render(out.File))
}

// parcels reads the part list. Every part needs a number from one upward and
// the label the store answered, because the store recomputes the composite
// over them and a wrong one fails the assembly there rather than here.
func parcels(in completeRequest) ([]blob.Part, error) {
	if len(in.Parts) == 0 {
		return nil, api.Refuse(api.CodeMissingField,
			"the body names no part, and a completion assembles the parts it names").About("parts")
	}
	out := make([]blob.Part, 0, len(in.Parts))
	for _, p := range in.Parts {
		if p.N < 1 || p.ETag == "" {
			return nil, api.Refuse(api.CodeInvalidField,
				"a part is numbered %d with the label %q; a part is numbered from 1 upward and carries "+
					"the label the store answered", p.N, p.ETag).About("parts")
		}
		out = append(out, blob.Part{Number: p.N, ETag: strings.Trim(p.ETag, `"`)})
	}
	slices.SortFunc(out, func(a, b blob.Part) int { return int(a.Number) - int(b.Number) })
	return out, nil
}

// doomed reports the precondition a completion would fail on, read against
// the row before the parts are assembled.
func (s *Service) doomed(ctx context.Context, held store.Session, pre api.Precondition) error {
	row, err := s.content.Files().Get(ctx, s.content.Querier(), held.Owner, held.Path)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fault(ctx, "read the path", err)
	}
	stored := ""
	if err == nil && row.DeletedAt == nil {
		stored = row.Checksum
	}
	if pre.Allows(stored) {
		return nil
	}
	return api.Refuse(api.CodePreconditionFailed,
		"the path holds %s and the request required another state", what(stored))
}

// caused reports whether the caller's own request is why a write failed,
// which is every refusal the error table of spec 013 answers below 500. A
// store that failed is not one of them, and what it owes the caller is a
// retry that works rather than an upload to do again.
func caused(err error) bool {
	refusal, named := errors.AsType[*api.Refusal](err)
	return named && api.Status(refusal.Code) < http.StatusInternalServerError
}

// what names what a path holds, with a word for the path that holds nothing.
func what(checksum string) string {
	if checksum == "" {
		return "nothing"
	}
	return checksum
}

// give closes a session a completion is refusing. The caller still owes its
// own refusal, so a store that will not take the parts back leaves the row
// for the reaper and changes nothing about the answer.
func (s *Service) give(ctx context.Context, held store.Session) {
	if err := s.close(ctx, held); err != nil {
		slog.WarnContext(ctx, "uploads: a refused completion left its session for the reaper", "error", err)
	}
}

// drop removes an assembled object no row names, on a context that outlives
// the request.
func (s *Service) drop(ctx context.Context, key string) {
	dropping := context.WithoutCancel(ctx)
	if err := s.content.Bucket().Delete(dropping, key); err != nil {
		slog.WarnContext(dropping, "uploads: remove an assembled object no row names", "error", err)
	}
}

// plane is the plane a session's path is rooted in, which the path rule
// settled when the session opened.
func plane(path string) object.Plane {
	p, _, _ := object.SplitPath(path)
	return p
}
