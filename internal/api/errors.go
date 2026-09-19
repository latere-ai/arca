// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/internal/auth"
)

// The stable error codes of spec 013's table. A caller branches on the code
// and shows the sentence; nothing else about a refusal is contractual.
const (
	CodeBadRequest            = "bad_request"
	CodeMissingField          = "missing_field"
	CodeInvalidField          = "invalid_field"
	CodeUnknownField          = "unknown_field"
	CodeInvalidPath           = "invalid_path"
	CodeUnknownPlane          = "unknown_plane"
	CodeExclusiveFields       = "exclusive_fields"
	CodeUnauthenticated       = "unauthenticated"
	CodeForbidden             = "forbidden"
	CodeNotFound              = "not_found"
	CodeNotAcceptable         = "not_acceptable"
	CodePathTaken             = "path_taken"
	CodeSlugTaken             = "slug_taken"
	CodeWriterHeld            = "writer_held"
	CodeLeaseNotHeld          = "lease_not_held"
	CodeManifestIncomplete    = "manifest_incomplete"
	CodeAttachmentGone        = "attachment_gone"
	CodeLengthRequired        = "length_required"
	CodePreconditionFailed    = "precondition_failed"
	CodeObjectTooLarge        = "object_too_large"
	CodeBodyTooLarge          = "body_too_large"
	CodeQuotaExceeded         = "quota_exceeded"
	CodeUnsupportedMediaType  = "unsupported_media_type"
	CodeTooManyParts          = "too_many_parts"
	CodeLinkReadOnly          = "link_read_only"
	CodeRateLimited           = "rate_limited"
	CodeInternal              = "internal"
	CodeNotImplemented        = "not_implemented"
	CodeStorageUnavailable    = "storage_unavailable"
	CodeAuthorizerUnavailable = "authorizer_unavailable"
)

// row is one line of spec 013's error table: the status a code is answered
// with and the one user sentence it carries. The sentence never varies,
// never interpolates and never leaks, because a caller shows it to a person
// and branches on the code. Everything that names a value, a path or a
// reason goes in the developer detail instead.
type row struct {
	status   int
	sentence string
}

// errorTable is spec 013's error table. A test reads that table out of the spec
// and holds this map equal to it, so the document a consumer codes against
// and the answers a handler writes cannot drift.
var errorTable = map[string]row{
	CodeBadRequest:            {http.StatusBadRequest, "The request could not be read."},
	CodeMissingField:          {http.StatusBadRequest, "A required field is missing."},
	CodeInvalidField:          {http.StatusBadRequest, "A field has a value it cannot take."},
	CodeUnknownField:          {http.StatusBadRequest, "The request body has a field this endpoint does not know."},
	CodeInvalidPath:           {http.StatusBadRequest, "That is not a path this server accepts."},
	CodeUnknownPlane:          {http.StatusBadRequest, "That path starts with a plane this server does not serve."},
	CodeExclusiveFields:       {http.StatusBadRequest, "Two fields that cannot be set together are set."},
	CodeUnauthenticated:       {http.StatusUnauthorized, "Sign in and send a valid token."},
	CodeForbidden:             {http.StatusForbidden, "You do not have permission to do this."},
	CodeNotFound:              {http.StatusNotFound, "There is no such object."},
	CodeNotAcceptable:         {http.StatusNotAcceptable, "This endpoint answers in JSON."},
	CodePathTaken:             {http.StatusConflict, "Something already exists at that path."},
	CodeSlugTaken:             {http.StatusConflict, "This space already has a workspace with that name."},
	CodeWriterHeld:            {http.StatusConflict, "Another writer holds this workspace."},
	CodeLeaseNotHeld:          {http.StatusConflict, "This attachment does not hold the writer lease."},
	CodeManifestIncomplete:    {http.StatusConflict, "The manifest names objects that have not been uploaded."},
	CodeAttachmentGone:        {http.StatusGone, "This attachment has ended; attach again."},
	CodeLengthRequired:        {http.StatusLengthRequired, "Send a Content-Length with the body."},
	CodePreconditionFailed:    {http.StatusPreconditionFailed, "The object is not in the state the request required."},
	CodeObjectTooLarge:        {http.StatusRequestEntityTooLarge, "The object is larger than this server accepts."},
	CodeBodyTooLarge:          {http.StatusRequestEntityTooLarge, "The request body is larger than this server accepts."},
	CodeQuotaExceeded:         {http.StatusRequestEntityTooLarge, "This space has no room left."},
	CodeUnsupportedMediaType:  {http.StatusUnsupportedMediaType, "Send the body as JSON."},
	CodeTooManyParts:          {http.StatusUnprocessableEntity, "The object needs more parts than this server allows."},
	CodeLinkReadOnly:          {http.StatusUnprocessableEntity, "A link grants reading and nothing more."},
	CodeRateLimited:           {http.StatusTooManyRequests, "Too many requests; wait and retry."},
	CodeInternal:              {http.StatusInternalServerError, "Something went wrong on this server."},
	CodeNotImplemented:        {http.StatusNotImplemented, "This server does not serve that."},
	CodeStorageUnavailable:    {http.StatusServiceUnavailable, "Storage is unavailable right now; retry shortly."},
	CodeAuthorizerUnavailable: {http.StatusServiceUnavailable, "The permission service is unavailable; retry shortly."},
}

// Sentence is the one user sentence of a code. A code the table lacks is a
// programming error: the table is the contract, so this panics rather than
// inventing text a person would read.
func Sentence(code string) string {
	r, ok := errorTable[code]
	if !ok {
		panic("api: no sentence for code " + code)
	}
	return r.sentence
}

// Status is the HTTP status of a code's row, and panics like Sentence.
func Status(code string) int {
	r, ok := errorTable[code]
	if !ok {
		panic("api: no status for code " + code)
	}
	return r.status
}

// Codes lists every code of the table, sorted, for the test that holds the
// table to the spec.
func Codes() []string {
	out := make([]string, 0, len(errorTable))
	for code := range errorTable {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// Refusal is one answer of the error table prepared away from the response
// writer: the code, the developer detail, and the field paths at fault where
// the code names fields. It is an error, so a handler returns one up through
// whatever decided it and one place writes the envelope.
type Refusal struct {
	// Code is the row of the table.
	Code string
	// Detail is the developer sentence: the only text that names values,
	// paths or reasons. It is written for a person reading a log and is
	// never parsed, and it never reaches the user sentence.
	Detail string
	// Fields are the field paths at fault, for a code that names fields.
	Fields []string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Detail }

// Refuse builds a refusal of one code with its developer detail.
func Refuse(code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// About names the field paths at fault, for the codes whose row says a
// refusal carries them.
func (r *Refusal) About(fields ...string) *Refusal {
	r.Fields = append(r.Fields, fields...)
	return r
}

// FromAuth renders a refusal of internal/auth in this table. The four codes
// of spec 006 are rows here under the same names, and the row of the reason
// table the verifier named opens the developer detail, which is where spec
// 006 puts it: a caller reads one fixed sentence and an operator reads the
// word every service of the family writes.
func FromAuth(err error) *Refusal {
	code := string(auth.CodeOf(err))
	if _, ok := errorTable[code]; !ok {
		return &Refusal{Code: CodeInternal}
	}
	detail := auth.DetailOf(err)
	if reason := auth.ReasonOf(err); reason != "" {
		detail = reason + ": " + detail
	}
	return &Refusal{Code: code, Detail: detail}
}

// Refused renders the deny of one question about a row the caller named.
//
// A deny at lookup carries the whole answer an absence carries, developer
// detail included, so a caller reading every byte of the envelope cannot
// tell a refusal from a missing row: absent is the refusal the same handler
// writes when the row is not there, and answering with it here is what keeps
// the two one answer. Any other deny keeps the reason the authorizer gave,
// because the caller may see the row the question was about.
//
// The collapse is here rather than beside one package's handlers because it
// is the sentence spec 013 makes about every 404 and criterion 2 of spec 015
// holds to: one implementation per package is how four routes were left out
// of it (spec 015, criterion 25).
func Refused(err, absent error) error {
	if auth.CodeOf(err) == auth.CodeNotFound {
		return absent
	}
	return FromAuth(err)
}

// WriteError sends a refusal in the family envelope: the code, the fixed
// user sentence of its row, and the developer detail beside the request id
// in details. An error that is not a refusal is an internal one, and its
// text is dropped rather than shown: nothing above 499 names a store, a
// query, a key or an internal type, even in the developer detail.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	refusal := asRefusal(err)
	// The code is what spec 018 labels the request with, and this is the one
	// place a refusal of this surface is written, so it is the one place the
	// label is settled.
	noting(r.Context(), refusal.Code)
	details := map[string]any{"request_id": RequestID(r.Context())}
	if refusal.Detail != "" {
		details["detail"] = refusal.Detail
	}
	if len(refusal.Fields) > 0 {
		details["fields"] = refusal.Fields
	}
	// The code goes out through the drift seam of spec 017: the refusal's
	// own in every deployment, and one row's substitute under
	// ARCA_TEST_DRIFT. It is read here rather than at each refusal site
	// because this is the one place a code reaches the wire.
	code := driftedCode(refusal.Code)
	httpjson.WriteError(w, Status(code), httpjson.Error{
		Code: code, Message: Sentence(code), Details: details,
	})
}

// asRefusal reads the refusal an error carries. Anything else is internal
// and says nothing of itself, because an error nobody wrote a row for is one
// whose text was never read for what a caller may see.
func asRefusal(err error) *Refusal {
	if r, ok := errors.AsType[*Refusal](err); ok {
		if _, known := errorTable[r.Code]; known {
			return r
		}
	}
	return &Refusal{Code: CodeInternal}
}
