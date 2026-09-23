// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package uploads is the session API of spec 007: how bytes arrive when
// there are too many of them to stream through a replica.
//
// Invariant 4 of spec 001 says an upload above the boundary goes direct to
// the bucket, so this package holds a session row, a set of presigned part
// URLs the client uploads against, and a completion that assembles the parts
// and writes the row. No byte of such an upload passes through the server.
//
// The row write at completion is [files.Service.Commit], the same code path
// a put of spec 005 takes, so the two writes cannot drift apart in their
// conditional behavior, their version capture, their charge or their event.
//
// A session's key is its own object id and never the key of the object it
// will replace, so nothing a session does, and nothing an abandoned session
// leaves behind, can touch bytes a live row points at. The service Arca
// replaces built that property by hand, with a random suffix on a key
// derived from the path; here it holds by construction.
package uploads

import (
	"net/http"
	"time"

	"latere.ai/x/arca/authorizer"
	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/files"
	"latere.ai/x/arca/internal/store"
)

// The three constants of spec 007. None is configuration: the part size is
// what a client hands the bucket, the cap is a guard against a client asking
// for a million URLs, and the life of a session is long enough for a slow
// client with pauses and short enough that abandoned parts are a rounding
// error on a storage bill.
const (
	// PartSize is the size of every part but the last.
	PartSize int64 = 16 << 20
	// MaxParts is the largest number of parts one upload may take. The
	// ceiling it implies, 16 GiB, is above ARCA_MAX_UPLOAD_BYTES's default,
	// so the configured limit is the real one.
	MaxParts int64 = 1000
	// TTL is how long a session and its part URLs live.
	TTL = 24 * time.Hour
)

// Service holds the three handlers and nothing else. The bucket calls are
// spec 003's, the row work is spec 004's, and the row write at completion is
// spec 005's.
type Service struct {
	content  *files.Service
	sessions store.Sessions
	metrics  Metrics
	now      func() time.Time
}

// Metrics is where the session API's half of spec 018's table goes. That
// spec owns the registry and the names; this is the seam it binds, so this
// package registers nothing.
//
// The bytes are the assembled object's and never a transfer this process
// saw: a part goes from the client to the bucket against a presigned URL,
// which is invariant 4 of spec 001, so what is counted in is what the
// completion wrote a row for.
type Metrics interface {
	// In records bytes accepted into a space, by spec 018's vocabulary.
	In(kind string, n int64)
	// UploadSession records one session by what became of it.
	UploadSession(outcome string)
	// UploadPart records one part by what became of it.
	UploadPart(outcome string)
	// LimitRejected records one session refused against the limit the
	// authorizer's answer carried.
	LimitRejected()
}

// uncounted is the seam of a node that bound none, so every call site is one
// line rather than a branch.
type uncounted struct{}

func (uncounted) In(string, int64) {}

func (uncounted) UploadSession(string) {}

func (uncounted) UploadPart(string) {}

func (uncounted) LimitRejected() {}

// The four values of arca_upload_sessions_total's outcome and the three of
// arca_upload_parts_total's, spelled here so a call site and its series
// cannot drift (spec 018).
const (
	sessionCreated   = "created"
	sessionCompleted = "completed"
	sessionAborted   = "aborted"
	sessionExpired   = "expired"

	partPresigned = "presigned"
	partCompleted = "completed"
	partMissing   = "missing"
)

// Options adds the session query set to what spec 005's package takes, so
// the node builds both from one set of dependencies.
type Options struct {
	files.Options
	// Sessions is the query set of spec 007. Nil takes the one over
	// Postgres, which is every caller but a test.
	Sessions store.Sessions
	// Metrics is spec 018's seam for what a session becomes. It shadows the
	// field of the embedded [files.Options] deliberately: the object plane
	// records bytes and refusals, this plane records sessions and parts as
	// well, and the node binds both to the one registry.
	Metrics Metrics
}

// New builds the session surface.
func New(o Options) *Service {
	s := &Service{content: files.New(o.Options), sessions: o.Sessions, metrics: o.Metrics, now: o.Now}
	if s.sessions == nil {
		s.sessions = store.NewSessions()
	}
	if s.metrics == nil {
		s.metrics = uncounted{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Table are the three rows of spec 013's upload table, declared without their
// handlers so tools/apidoc describes the same surface the node mounts.
func Table() []api.Route {
	return []api.Route{
		{
			Method: http.MethodPost, Path: "/v1/uploads",
			Action: authorizer.ActionUploadWrite, Status: http.StatusCreated,
			Summary:     "Start upload",
			Description: "Open an upload session and answer its presigned part URLs.",
		},
		{
			Method: http.MethodPost, Path: "/v1/uploads/{id}/complete",
			Action: authorizer.ActionUploadWrite, Status: http.StatusCreated,
			Summary:     "Complete upload",
			Description: "Assemble the parts into the object at the session's path.",
		},
		{
			Method: http.MethodDelete, Path: "/v1/uploads/{id}",
			Action: authorizer.ActionUploadWrite, Status: http.StatusNoContent,
			Summary:     "Abort upload",
			Description: "Abort the session and discard its parts.",
		},
	}
}

// Routes are the same rows with this package's handlers bound.
func Routes(o Options) []api.Route { return Bind(New(o)) }

// Bind attaches one service's handlers to the declared rows, for a node that
// has already built the service because the reconciler of spec 010 reaches
// the same one.
func Bind(s *Service) []api.Route {
	handlers := []http.Handler{
		http.HandlerFunc(s.create), http.HandlerFunc(s.complete), http.HandlerFunc(s.abort),
	}
	rows := Table()
	out := make([]api.Route, len(rows))
	for i, row := range rows {
		row.Handler = handlers[i]
		out[i] = row
	}
	return out
}

// parts is how many parts an upload of this size takes. The size is
// positive, so the subtraction comes first: rounding up on the size itself
// would overflow an int64 and let an oversized request under the cap.
func parts(size int64) int64 { return 1 + (size-1)/PartSize }
