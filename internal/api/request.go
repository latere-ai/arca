// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/arca/internal/auth"
)

// Header is the request id a client may send and every response carries
// back, errors included.
const Header = "X-Request-Id"

// Prefix opens every request id arcad mints, so an id in a log is read at a
// glance as this server's rather than a caller's.
const Prefix = "req_"

// MaxClientRequestID is the bound on a client's own id: spec 013 keeps one
// of at most this many printable ASCII characters and replaces anything
// else, so a header cannot carry a log injection or a megabyte.
const MaxClientRequestID = 128

type requestIDKey struct{}

// WithRequestID carries the request id of one request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID is the id of the request being served, and "" outside one.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestID is the first middleware of every route under /v1 and of
// GET /openapi.json. It settles the id spec 013 names, puts it on the
// context, answers it back in the header, hands it to the authorizer as the
// request block, and records it on the request's span so a trace and a log
// line name one request.
//
// It runs before the verifier, so a refused request carries an id too and an
// operator can find the 401 a caller is asking about.
func (a *API) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := a.identify(r)
		w.Header().Set(Header, id)
		ctx := auth.WithRequest(WithRequestID(r.Context(), id), auth.RequestInfo(r, id))
		otel.SetAttributes(ctx, attribute.String("arca.request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// identify is spec 013's rule: a client's own id within the bound is kept,
// so a caller correlates its own logs with the server's, and anything else
// is replaced with one this server minted.
func (a *API) identify(r *http.Request) string {
	if given := r.Header.Get(Header); usableID(given) {
		return given
	}
	return Prefix + a.newID()
}

// usableID reports whether a client's id may be kept: at most the bound, and
// printable ASCII throughout, so nothing in it can break a log line.
func usableID(id string) bool {
	if id == "" || len(id) > MaxClientRequestID {
		return false
	}
	for i := range len(id) {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// crockford is the alphabet of a ULID: base32 without the letters a reader
// confuses, and ordered so that sorting the text sorts the bytes.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var ulid = base32.NewEncoding(crockford).WithPadding(base32.NoPadding)

// newID mints a ULID: forty-eight bits of milliseconds, most significant
// first, and eighty bits from the system's random source, rendered in
// Crockford base32. The time leads, so ids sort by the order they were
// minted; the rest is random, so two replicas minting in the same
// millisecond do not collide. Nothing parses one back, so the encoder's
// alignment is its own and no id is ever read for its time.
func (a *API) newID() string {
	var b [16]byte
	ms := uint64(a.now().UnixMilli())
	for i := range 6 {
		b[i] = byte(ms >> (40 - 8*i))
	}
	_, _ = rand.Read(b[6:])
	return ulid.EncodeToString(b[:])
}

// now is the clock the request ids are minted on, time.Now unless a test
// gave one.
func (a *API) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}
