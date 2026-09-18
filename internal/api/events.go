// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/arca/internal/auth"
	"latere.ai/x/arca/internal/events"
)

// This file is the binding of spec 010's event tail into the frame: the seam
// that package declared, filled with the two of spec 006, and the refusals
// it names rendered in this package's error table.
//
// The handler is internal/events'. Nothing about the log, the cursor or the
// page is decided here, and nothing about the status line, the user sentence
// or the request id is decided there.

// eventGuard fills events.Guard over the Decide seam of internal/auth and
// the caller the verifier left on the request.
type eventGuard struct{ api *API }

// Caller is the subject the verified token identifies, and "" for a request
// that carried no usable token. Every path under /v1 but the three public
// link routes runs behind the verifier, so behind the frame this is empty
// only where a middleware admitted a request it should have refused.
func (g eventGuard) Caller(r *http.Request) string {
	return auth.CallerFrom(r.Context()).Subject
}

// Ask puts one question of spec 006's vocabulary and answers the decision as
// events.Guard reads one: a deny is a decision and not an error, so the
// handler renders it as its own refusal, and an error is a call that
// produced no decision, which fails closed.
//
// The translation is the whole of this function, because auth.Decide
// collapses both into errors: a deny is an *auth.Error of CodeForbidden or
// CodeNotFound, and everything else left it undecided.
func (g eventGuard) Ask(r *http.Request, action string, res authz.Resource) (authz.Decision, error) {
	decision, err := g.api.authorizer.Decide(r.Context(), action, res)
	if err != nil {
		switch auth.CodeOf(err) {
		case auth.CodeForbidden, auth.CodeNotFound:
			return authz.Decision{Reason: auth.DetailOf(err)}, nil
		default:
			return authz.Decision{}, err
		}
	}
	return authz.Decision{
		Allow: true, TTL: decision.TTL, Limits: decision.Limits, Filter: decision.Filter,
	}, nil
}

// refuseEvent writes a refusal of the event tail in this package's envelope,
// so the row internal/events named carries the status, the one user sentence
// and the request id every other refusal of the surface carries.
func refuseEvent(w http.ResponseWriter, r *http.Request, err error) {
	WriteError(w, r, FromEvents(err))
}

// FromEvents renders a refusal of internal/events in this table. That
// package names a row and the developer detail and writes no envelope; an
// error naming no row of the table is an internal one, like any other.
func FromEvents(err error) *Refusal {
	var refusal *events.Error
	if !errors.As(err, &refusal) {
		return &Refusal{Code: CodeInternal}
	}
	if _, ok := errorTable[refusal.Code]; !ok {
		return &Refusal{Code: CodeInternal}
	}
	return &Refusal{Code: refusal.Code, Detail: refusal.Detail, Fields: refusal.Fields}
}

// events answers GET /v1/events. The tail is built once at start, over the
// log and the database the node opened, and New refuses to build without the
// log, so there is one here for every request this route reaches.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	a.eventTail.ServeHTTP(w, r)
}
