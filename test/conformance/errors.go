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
	CodeSlugTaken:             {409, "This space already has a workspace with that name."},
	CodeWriterHeld:            {409, "Another writer holds this workspace."},
	CodeLeaseNotHeld:          {409, "This attachment does not hold the writer lease."},
	CodeManifestIncomplete:    {409, "The manifest names objects that have not been uploaded."},
	CodeAttachmentGone:        {410, "This attachment has ended; attach again."},
	CodeLengthRequired:        {411, "Send a Content-Length with the body."},
	CodePreconditionFailed:    {412, "The object is not in the state the request required."},
	CodeObjectTooLarge:        {413, "The object is larger than this server accepts."},
	CodeBodyTooLarge:          {413, "The request body is larger than this server accepts."},
	CodeQuotaExceeded:         {413, "This space has no room left."},
	CodeUnsupportedMediaType:  {415, "Send the body as JSON."},
	CodeTooManyParts:          {422, "The object needs more parts than this server allows."},
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

// provoked names the codes a caller outside the installation can make a
// conforming server answer, which is what criterion 2 of spec 017 holds the
// suite to. The rest are here for the sentence comparison alone:
//
//   - internal and storage_unavailable are a fault inside the installation,
//     which is the e2e tier's of spec 014 and not a black box suite's.
//   - rate_limited is reachable, and is not provoked: draining a subject's
//     bucket against a shared installation is a denial of service against
//     whoever else is using it, and spec 017 asserts an answer and never a
//     rate.
//   - not_implemented is a build that registers a route and does not answer
//     it. The pending group reads it off the served document rather than
//     provoking it, so a partial build is reported once and not once per
//     route.
var unprovoked = map[string]string{
	CodeInternal:           "a fault inside the installation, which is the e2e tier's",
	CodeStorageUnavailable: "a store cut underneath a request, which is the e2e tier's",
	CodeRateLimited:        "draining a bucket on a shared installation is a denial of service",
	CodeNotImplemented:     "read off the served document by the pending group",
}
