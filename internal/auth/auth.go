// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package auth is arcad's side of spec 006: who a caller is, and who
// decides what that caller may do.
//
// Who is [Verifier]. A bearer is verified against one of the issuers
// ARCA_OIDC_ISSUERS lists, through latere.ai/x/pkg/authkit/jwt, and becomes
// a [Caller]: the rendered subject "<iss>|<sub>", its two halves apart, and
// every claim of the token verbatim. No claim is read for meaning here. An
// issuer's membership claim means something to the authorizer that reads it
// and nothing to Arca, which is invariant 5 of spec 001.
//
// What is the authorizer. arcad asks one endpoint per request through
// latere.ai/x/pkg/authz's client, with the vocabulary of
// latere.ai/x/arca/authorizer, and caches, retries and fails closed by that
// package's rules. With no endpoint configured the [OwnerPolicy] of this
// package answers instead, built on authz.Policy: an owner acts on its own
// space, an administrator on every space, a grantee within its grant, and a
// link that resolves reads what it names.
//
// [Authorizer.Decide] is the seam every handler calls. The caller and what
// is known about the request ride on the context, put there by
// [Verifier.Middleware], so a handler names an action and a resource and
// nothing else, and cannot tell an operator's endpoint from the owner
// policy.
//
// Nothing here calls an issuer while a request is served. The discovery
// documents and the key sets are read at start by [Verifier] warming the
// shared validator, and refreshed by that validator on its own schedule. A
// warm that fails is not a start-up failure: it is one line in the developer
// register, a retry in the background, and [Verifier.Check] failing until an
// issuer answers, so a replica whose issuer is late stays out of rotation
// rather than exiting.
package auth

import (
	"errors"
	"fmt"
)

// Code is why a request was refused, in the vocabulary spec 006 and spec 013
// share. The HTTP envelope is spec 013's; this package names the reason and
// nothing about the status line.
type Code string

// The refusals of spec 006.
const (
	// CodeUnauthenticated is a request with no bearer, or one no listed
	// issuer could have signed. 401.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeForbidden is a deny on the request's own action. 403.
	CodeForbidden Code = "forbidden"
	// CodeNotFound is a deny on an object reached through a lookup, so a
	// refused object and a missing one are one answer. 404, invariant 6.
	CodeNotFound Code = "not_found"
	// CodeAuthorizerUnavailable is a call that produced no decision. 503,
	// and never an allow.
	CodeAuthorizerUnavailable Code = "authorizer_unavailable"
)

// ReasonMissing is the one row Arca adds to the shared reason table: a
// request that carries no bearer at all. Every other row is
// latere.ai/x/pkg/authkit/jwt's, whose table starts at a token that exists,
// and is rendered with the same word every service of the family renders.
const ReasonMissing = "missing"

// Error is a refusal: the code a caller is answered with, the row of the
// reason table it belongs to, and the detail a developer reads. The detail
// never reaches the user sentence, so an authorizer's reason and an issuer's
// refusal are a log line and not a page.
type Error struct {
	Code   Code
	Reason string
	Detail string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Detail }

// refuse builds a refusal that names no row of the reason table, which is
// every refusal but the verifier's.
func refuse(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// unauthenticated builds the 401 of one row of the reason table.
func unauthenticated(reason, format string, args ...any) *Error {
	return &Error{Code: CodeUnauthenticated, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// CodeOf reads the refusal code of an error, and "" for an error that is not
// one of this package's.
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}

// ReasonOf reads the reason table row of a refusal, and "" for an error that
// names none.
func ReasonOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Reason
	}
	return ""
}

// DetailOf reads the developer detail of a refusal, and the error's own text
// for an error that is not one of this package's.
func DetailOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Detail
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
