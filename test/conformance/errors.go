// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import "testing"

// HeaderRequestID is the header spec 013 puts on every answer, errors
// included.
const HeaderRequestID = "X-Request-Id"

// The codes of spec 013's error table. They are declared here rather than
// read from the server's own package because the suite is black box: a
// build that renamed a code has to fail a case, and a suite that imported
// the server's constants would rename with it.
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
	CodeLengthRequired        = "length_required"
	CodePreconditionFailed    = "precondition_failed"
	CodeObjectTooLarge        = "object_too_large"
	CodeBodyTooLarge          = "body_too_large"
	CodeQuotaExceeded         = "quota_exceeded"
	CodeUnsupportedMediaType  = "unsupported_media_type"
	CodeTooManyParts          = "too_many_parts"
	CodeTooManyObjects        = "too_many_objects"
	CodeLinkReadOnly          = "link_read_only"
	CodeRateLimited           = "rate_limited"
	CodeInternal              = "internal"
	CodeNotImplemented        = "not_implemented"
	CodeStorageUnavailable    = "storage_unavailable"
	CodeAuthorizerUnavailable = "authorizer_unavailable"
)

// row is one code's status and its one fixed sentence.
type row struct {
	status   int
	sentence string
}

// codeTable is spec 013's error table as the suite compares an answer to it.
// One code, one status, one sentence that never varies and never
// interpolates: a target that writes a sentence of its own fails the case
// that provoked the code, whatever the status.
//
// TestErrorTableMatchesTheSpec holds this table equal to the table in
// specs/013-api.md, so the two cannot drift.
var codeTable = map[string]row{
	CodeBadRequest:            {400, "The request could not be read."},
	CodeMissingField:          {400, "A required field is missing."},
	CodeInvalidField:          {400, "A field has a value it cannot take."},
	CodeUnknownField:          {400, "The request body has a field this endpoint does not know."},
	CodeInvalidPath:           {400, "That is not a path this server accepts."},
	CodeUnknownPlane:          {400, "That path starts with a plane this server does not serve."},
	CodeExclusiveFields:       {400, "Two fields that cannot be set together are set."},
	CodeUnauthenticated:       {401, "Sign in and send a valid token."},
	CodeForbidden:             {403, "You do not have permission to do this."},
	CodeNotFound:              {404, "There is no such object."},
	CodeNotAcceptable:         {406, "This endpoint answers in JSON."},
	CodePathTaken:             {409, "Something already exists at that path."},
	CodeLengthRequired:        {411, "Send a Content-Length with the body."},
	CodePreconditionFailed:    {412, "The object is not in the state the request required."},
	CodeObjectTooLarge:        {413, "The object is larger than this server accepts."},
	CodeBodyTooLarge:          {413, "The request body is larger than this server accepts."},
	CodeQuotaExceeded:         {413, "This space has no room left."},
	CodeUnsupportedMediaType:  {415, "Send the body as JSON."},
	CodeTooManyParts:          {422, "The object needs more parts than this server allows."},
	CodeTooManyObjects:        {422, "The folder holds more objects than one copy accepts."},
	CodeLinkReadOnly:          {422, "A link grants reading and nothing more."},
	CodeRateLimited:           {429, "Too many requests; wait and retry."},
	CodeInternal:              {500, "Something went wrong on this server."},
	CodeNotImplemented:        {501, "This server does not serve that."},
	CodeStorageUnavailable:    {503, "Storage is unavailable right now; retry shortly."},
	CodeAuthorizerUnavailable: {503, "The permission service is unavailable; retry shortly."},
}

// table answers a code's status and sentence, and fails the case on a code
// the table does not name, which is a mistake in the suite.
func table(t testing.TB, code string) (int, string) {
	t.Helper()
	r, ok := codeTable[code]
	failIf(t, !ok, "conformance: %q is not a code of spec 013's table", code)
	return r.status, r.sentence
}

// unprovoked names the codes no case makes a conforming server answer, each
// with the reason, which is what criterion 2 of spec 017 holds the suite to.
// They are in the table above for the sentence comparison alone:
//
//   - internal is a fault inside the installation, which is the e2e tier's
//     of spec 014 and not a black box suite's.
//   - rate_limited is reachable, and is not provoked: draining a subject's
//     bucket against a shared installation is a denial of service against
//     whoever else is using it, and spec 017 asserts an answer and never a
//     rate.
//   - not_implemented is a build that registers a route and does not answer
//     it. The pending group reads it off the served document rather than
//     provoking it, so a partial build is reported once and not once per
//     route.
//   - too_many_objects takes a folder above the bound of one copy, which is
//     ten thousand and one objects written into an installation somebody
//     else may be using. The bound is proved in process, beside the handler.
//
// storage_unavailable is not here. A store cut underneath a request is the
// e2e tier's, and the code is also what an answer the installation cannot
// read is refused with, which the limits cases of spec 028 provoke through
// the stub authorizer.
var unprovoked = map[string]string{
	CodeInternal:       "a fault inside the installation, which is the e2e tier's",
	CodeRateLimited:    "draining a bucket on a shared installation is a denial of service",
	CodeNotImplemented: "read off the served document by the pending group",
	CodeTooManyObjects: "ten thousand and one objects written into an installation to be refused a copy of them",
}
