// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"strings"
	"testing"
)

// The rows of spec 007 the wire shows: a session above the part size, part
// URLs used directly against the bucket, completion with the part labels,
// a completion with a part missing refused, and an abort. The suite never
// assumes a limit: the part size and the part count are read off the session
// the server created.
//
// These cases are written from spec 013's shapes. A build that does not
// answer the upload routes yet holds them in the pending group, which fails
// with the routes and the spec they wait on.

func cases007() []testCase {
	uploads := []string{"POST /v1/uploads", "POST /v1/uploads/{id}/complete", "DELETE /v1/uploads/{id}"}
	return []testCase{
		{name: "Session", group: GroupUploads, routes: uploads, run: case007Session},
		{name: "Complete", group: GroupUploads, routes: append(append([]string{}, uploads...),
			"GET /v1/files/{owner}/{path...}"), run: case007Complete},
		{name: "Abort", group: GroupUploads, routes: uploads, run: case007Abort},
		{name: "TooLarge", group: GroupUploads, routes: uploads, run: case007TooLarge},
	}
}

// case007Session: a session answers the shape spec 013 names, with one
// presigned part URL per part and an expiry. The suite reads the part size
// off the answer rather than assuming one, because it is the installation's.
func case007Session(t *testing.T, s *session) {
	path := s.filePath("upload/session.bin")
	r := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/uploads",
		body(fields{"owner": "me", "path": path, "size": 32 << 20})), http.StatusCreated)
	id := str(r.json, "id")
	failIf(t, id == "", "the session carries no id: %s", r.body)
	s.record("upload "+id, func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, "/v1/uploads/"+id, "")
		return nil
	})
	failIf(t, num(r.json, "part_size") <= 0, "the session names no part size: %s", r.body)
	failIf(t, num(r.json, "part_count") <= 0, "the session names no part count: %s", r.body)
	failIf(t, str(r.json, "expires_at") == "", "the session carries no expiry: %s", r.body)
	failIf(t, str(r.json, "path") != path, "the session names the path %q, want %q", str(r.json, "path"), path)

	urls := partURLs(t, r)
	failIf(t, int64(len(urls)) != num(r.json, "part_count"),
		"the session names %d parts and carries %d part URLs", num(r.json, "part_count"), len(urls))
}

// case007Complete: the parts go straight to the bucket against the presigned
// URLs, which is invariant 4 of spec 001 on the write side, and the
// completion assembles them into the object. A completion that names a part
// nothing was uploaded for is refused with manifest_incomplete rather than
// producing a short object.
func case007Complete(t *testing.T, s *session) {
	path := s.filePath("upload/complete.bin")
	owner := s.subject(t, Alice)

	opened := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/uploads",
		body(fields{"owner": "me", "path": path, "size": 1 << 20})), http.StatusCreated)
	id := str(opened.json, "id")
	urls := partURLs(t, opened)
	failIf(t, len(urls) == 0, "the session carries no part URL: %s", opened.body)

	content := strings.Repeat("u", int(num(opened.json, "part_size")))
	if int64(len(content)) > num(opened.json, "size") && num(opened.json, "size") > 0 {
		content = content[:num(opened.json, "size")]
	}
	sent := s.do(t, request{method: http.MethodPut, path: urls[0], body: strings.NewReader(content)})
	expectStatus(t, sent, http.StatusOK)
	label := unquote(sent.header.Get("ETag"))
	failIf(t, label == "", "the bucket answered no ETag for the part")

	// A completion that leaves a part out is refused.
	if len(urls) > 1 {
		expectError(t, s.call(t, Alice, http.MethodPost, "/v1/uploads/"+id+"/complete",
			body(fields{"parts": []any{map[string]any{"number": 1, "etag": label}}})), CodeManifestIncomplete)
		return
	}

	done := s.call(t, Alice, http.MethodPost, "/v1/uploads/"+id+"/complete",
		body(fields{"parts": []any{map[string]any{"number": 1, "etag": label}}}))
	expectAWrite(t, done)
	s.record("object "+path, func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, path)+"?permanent=1", "")
		return nil
	})

	// An object assembled from parts Arca never saw carries the store's
	// composite label rather than a digest of its bytes, and spec 013 says
	// which through checksum_kind.
	read := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", ""), http.StatusOK)
	failIf(t, read.header.Get("ETag") == "", "the assembled object answers no ETag")
}

// case007Abort: an aborted session is gone, and completing it afterwards is
// a missing object rather than a second assembly.
func case007Abort(t *testing.T, s *session) {
	path := s.filePath("upload/abort.bin")
	opened := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/uploads",
		body(fields{"owner": "me", "path": path, "size": 1 << 20})), http.StatusCreated)
	id := str(opened.json, "id")

	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/uploads/"+id, ""), http.StatusNoContent)
	expectError(t, s.call(t, Alice, http.MethodPost, "/v1/uploads/"+id+"/complete", body(fields{"parts": []any{}})), CodeNotFound)
}

// case007TooLarge: a declared size above what the installation accepts is
// refused at creation, before a byte is sent, and a size that is not a size
// is a field with a value it cannot take.
func case007TooLarge(t *testing.T, s *session) {
	huge := s.call(t, Alice, http.MethodPost, "/v1/uploads",
		body(fields{"owner": "me", "path": s.filePath("upload/huge.bin"), "size": int64(1) << 62}))
	failIf(t, huge.status == http.StatusCreated, "a session was opened for an object larger than the server accepts")
	code := huge.code()
	failIf(t, code != CodeObjectTooLarge && code != CodeTooManyParts && code != CodeInvalidField,
		"an oversized declaration answered %d %s; spec 013 names object_too_large and too_many_parts", huge.status, code)
	expectError(t, huge, code)

	expectError(t, s.call(t, Alice, http.MethodPost, "/v1/uploads",
		body(fields{"owner": "me", "path": s.filePath("upload/negative.bin"), "size": -1})), CodeInvalidField)
}

// partURLs reads the presigned part URLs off a session, holding each to
// being a URL, so a case sends a part to one rather than to whatever the
// target put in the array.
func partURLs(t testing.TB, r response) []string {
	t.Helper()
	raw, _ := r.json["part_urls"].([]any)
	out := make([]string, 0, len(raw))
	for _, u := range raw {
		url, ok := u.(string)
		failIf(t, !ok || !strings.HasPrefix(url, "http"), "a part URL is not a URL: %v", u)
		out = append(out, url)
	}
	return out
}
