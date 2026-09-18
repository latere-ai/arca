// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/arca/internal/auth"
)

// The two token buckets of spec 015, whose values spec 002's table owns.
//
// One is per subject and is charged once a request has a verified caller;
// it is the ordinary quota of a client that is behaving. The other is per
// client address and is charged only where there is no verified caller: a
// public link route, and a request whose bearer the verifier refused. That
// is what bounds a caller guessing link tokens and a caller replaying bad
// tokens, neither of which has a subject to charge.
//
// A request that authenticates therefore pays one bucket and not two, and a
// request that does not pays the address bucket alone.

// buckets builds one limiter. A rate of zero is the row's own value for a
// limiter that is off, and a nil Buckets is exactly that: the shared package
// treats a nil receiver as unlimited, so the off case costs no branch on the
// request path.
func buckets(perMinute int) *ratelimit.Buckets {
	if perMinute <= 0 {
		return nil
	}
	return ratelimit.New(ratelimit.Config{PerMinute: perMinute})
}

// allowed reports whether one request may proceed against a bucket. A nil
// limiter allows everything.
func allowed(b *ratelimit.Buckets, key string) bool {
	if b == nil || key == "" {
		return true
	}
	return b.Allow(key).OK
}

// limitSubject charges the per-subject bucket. It runs after the verifier,
// so the key is the rendered subject and every replica counts the same
// caller under the same name.
func (a *API) limitSubject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := auth.CallerFrom(r.Context()).Subject
		// The verifier has just settled who this is, and the line of spec
		// 018 names the caller by that subject, so it is written where the
		// observation outside can read it.
		knowing(r.Context(), subject)
		if !allowed(a.perSubject, subject) {
			WriteError(w, r, Refuse(CodeRateLimited, "the subject %s is over its rate for this minute", subject))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitAddress charges the per-address bucket, for the routes that carry no
// bearer at all.
func (a *API) limitAddress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.chargeAddress(r) {
			WriteError(w, r, Refuse(CodeRateLimited, "this address is over its rate for this minute"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// chargeAddress takes one token from the address bucket and reports whether
// there was one. The key is the peer address the authorizer would be told
// about, so one client is one key whatever it sends in a header.
func (a *API) chargeAddress(r *http.Request) bool {
	return allowed(a.perAddress, auth.RequestInfo(r, "").IP)
}

// refuseVerification is the verifier's [auth.Refuser]: the 401 of spec 006
// in the envelope of spec 013, with the row of the reason table opening the
// developer detail.
//
// A refused request has no subject to charge, so this is where the address
// bucket is charged on the guarded routes. A caller replaying bad tokens
// meets the same limit as one guessing link tokens, and a caller whose token
// verifies pays neither.
func (a *API) refuseVerification(w http.ResponseWriter, r *http.Request, err error) {
	// The row of the reason table is the label of spec 018's counter, and
	// the verifier's refusal is the one thing that carries it.
	a.metrics.TokenRejected(auth.ReasonOf(err))
	if !a.chargeAddress(r) {
		WriteError(w, r, Refuse(CodeRateLimited, "this address is over its rate for this minute"))
		return
	}
	WriteError(w, r, FromAuth(err))
}
