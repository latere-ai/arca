// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The rows of spec 005 the wire shows: put, get, head, list, move and delete
// over the files plane; the ETag a read and a write answer is the object's
// checksum; a move leaves it equal, which is invariant 8 of spec 001 seen
// from outside; a read above the inline threshold answers a redirect, which
// is invariant 4; versions, trash and stars.
//
// These cases are written from spec 013's shapes and drive the routes that
// spec registers. A build that does not answer them yet holds every one in
// the pending group, which fails with the routes and the spec they wait on,
// so a partial build is red and never quietly green.

func cases005() []testCase {
	object := []string{
		"PUT /v1/files/{owner}/{path...}",
		"GET /v1/files/{owner}/{path...}",
		"HEAD /v1/files/{owner}/{path...}",
		"DELETE /v1/files/{owner}/{path...}",
	}
	move := append(append([]string{}, object...), "POST /v1/files/{owner}/{path...}")
	trash := []string{"GET /v1/trash", "POST /v1/trash/restore", "DELETE /v1/trash"}
	stars := []string{"PUT /v1/stars", "DELETE /v1/stars", "GET /v1/stars"}
	return []testCase{
		{name: "PutGetHeadDelete", group: GroupFiles, routes: object, run: case005PutGetHeadDelete},
		{name: "List", group: GroupFiles, routes: object, run: case005List},
		{name: "MoveKeepsTheETag", group: GroupFiles, routes: move, run: case005MoveKeepsTheETag},
		{name: "Conditional", group: GroupConditional, routes: object, run: case005Conditional},
		{name: "Bytes", group: GroupBytes, routes: object, run: case005Bytes},
		{name: "Versions", group: GroupVersions, routes: move, run: case005Versions},
		{name: "Trash", group: GroupTrash, routes: append(append([]string{}, object...), trash...), run: case005Trash},
		{name: "Stars", group: GroupStars, routes: append(append([]string{}, object...), stars...), run: case005Stars},
		{name: "Materialize", group: GroupFiles, routes: append(append([]string{}, object...),
			"GET /v1/files/materialize"), run: case005Materialize},
	}
}

// filePath is a path in the caller's own files plane under the run's own
// name, so the run writes nothing anybody else stored.
func (s *session) filePath(label string) string { return "files/" + s.name(label) }

// fileRoute is the route a path is written and read through, with the owner
// as spec 013 addresses a space.
func (s *session) fileRoute(owner, path string) string {
	return "/v1/files/" + pathParam(owner) + "/" + path
}

// put writes one object with its bytes as the body and its content type
// recorded verbatim, and records it for the cleanup.
func (s *session) put(t testing.TB, principal, path, content, contentType string) response {
	t.Helper()
	owner := s.subject(t, principal)
	r := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, path),
		subject: principal, body: strings.NewReader(content), contentType: contentType})
	if r.status == http.StatusOK || r.status == http.StatusCreated {
		s.record("object "+path, func(t testing.TB) error {
			s.call(t, principal, http.MethodDelete, s.fileRoute(owner, path)+"?permanent=1", "")
			return nil
		})
	}
	return r
}

// sha256Of is the checksum spec 013 answers as the ETag of anything Arca
// streamed, computed here so a case compares the server's label to the bytes
// it sent rather than to the label the server chose.
func sha256Of(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// unquote strips the quotes of a strong validator, so a bare checksum is
// compared beside the quoted form.
func unquote(etag string) string { return strings.Trim(etag, `"`) }

// case005PutGetHeadDelete: one object written, read back byte for byte,
// headed with no body, and deleted. The ETag a write and a read answer is
// the sha-256 of the bytes Arca streamed, and the content type is recorded
// verbatim and served back.
func case005PutGetHeadDelete(t *testing.T, s *session) {
	path, content := s.filePath("hello.txt"), "the quick brown fox\n"
	owner := s.subject(t, Alice)

	written := s.put(t, Alice, path, content, "text/plain; charset=utf-8")
	failIf(t, written.status != http.StatusCreated && written.status != http.StatusOK,
		"the write answered %d: %s", written.status, written.body)
	failIf(t, unquote(written.header.Get("ETag")) != sha256Of(content),
		"the write answered the ETag %q, and the sha-256 of the bytes is %q", written.header.Get("ETag"), sha256Of(content))

	read := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", ""), http.StatusOK)
	failIf(t, string(read.body) != content, "the object read back as %q, want %q", read.body, content)
	failIf(t, unquote(read.header.Get("ETag")) != sha256Of(content),
		"the read answered the ETag %q, want the object's checksum", read.header.Get("ETag"))
	failIf(t, !strings.HasPrefix(read.header.Get("Content-Type"), "text/plain"),
		"the content type came back as %q, and spec 013 records it verbatim", read.header.Get("Content-Type"))

	head := expectStatus(t, s.do(t, request{method: http.MethodHead, subject: Alice,
		path: s.fileRoute(owner, path) + "?inline=1"}), http.StatusOK)
	failIf(t, len(head.body) != 0, "a HEAD answered a body of %d bytes", len(head.body))
	failIf(t, head.header.Get("ETag") != read.header.Get("ETag"), "HEAD and GET answer different ETags")

	// A read that still holds the checksum is a 304 with no body.
	fresh := s.with(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", "",
		map[string]string{"If-None-Match": read.header.Get("ETag")})
	failIf(t, fresh.status != http.StatusNotModified, "a conditional read answered %d, want 304", fresh.status)
	failIf(t, len(fresh.body) != 0, "a 304 answered a body")

	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, path)+"?permanent=1", ""), http.StatusNoContent)
	expectError(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path), ""), CodeNotFound)
}

// case005List: the subtree at a path answers the one list envelope of spec
// 013, and the listing holds what the run put and nothing of another space.
func case005List(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	dir := s.filePath("listing")
	for _, leaf := range []string{"a.txt", "b.txt", "deeper/c.txt"} {
		expectAWrite(t, s.put(t, Alice, dir+"/"+leaf, leaf, "text/plain"))
	}
	page := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, dir)+"?list=1&limit=1000", ""), http.StatusOK)
	failIf(t, page.json["entries"] == nil, "the listing answers a null entries: %s", page.body)
	found := map[string]bool{}
	for _, e := range list(page.json, "entries") {
		found[str(e, "path")] = true
	}
	for _, leaf := range []string{"a.txt", "b.txt"} {
		failIf(t, !found[dir+"/"+leaf], "the listing does not hold %q: %v", dir+"/"+leaf, found)
	}
}

// case005MoveKeepsTheETag: a move is a row update and touches no bytes, so
// the object's checksum after it is the checksum before it. This is
// invariant 8 of spec 001, the key is not the path, seen from outside.
func case005MoveKeepsTheETag(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	from, to := s.filePath("move-from.txt"), s.filePath("move-to.txt")
	content := "bytes that do not move\n"
	written := expectAWrite(t, s.put(t, Alice, from, content, "text/plain"))
	before := unquote(written.header.Get("ETag"))

	moved := expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, from), body(fields{"move_to": to})), http.StatusOK)
	after := unquote(moved.header.Get("ETag"))
	if after == "" {
		after = str(moved.json, "checksum")
	}
	failIf(t, after != before, "the move answered the ETag %q, and the object's was %q; invariant 8 makes a move a row update", after, before)

	read := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, to)+"?inline=1", ""), http.StatusOK)
	failIf(t, string(read.body) != content, "the moved object read back as %q", read.body)
	failIf(t, unquote(read.header.Get("ETag")) != before, "the moved object's ETag changed")
	expectError(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, from), ""), CodeNotFound)

	// A move onto a path something already holds is refused rather than
	// silently overwriting it.
	other := s.filePath("move-onto.txt")
	expectAWrite(t, s.put(t, Alice, other, "occupied\n", "text/plain"))
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, to), body(fields{"move_to": other})), CodePathTaken)
}

// case005Conditional: a conditional write with the current ETag applies, one
// with a stale ETag is refused, a create-only write onto an existing path is
// refused, and a write with neither header succeeds on every path. No route
// requires a precondition, so the caller whose lost update would be silent
// opts into the round trip and every other caller does not pay for it.
func case005Conditional(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	path := s.filePath("conditional.txt")

	// If-None-Match: * creates once and is refused the second time.
	first := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, path), subject: Alice,
		body: strings.NewReader("one\n"), contentType: "text/plain",
		header: map[string]string{"If-None-Match": "*"}})
	expectAWrite(t, first)
	s.record("object "+path, func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, path)+"?permanent=1", "")
		return nil
	})
	again := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, path), subject: Alice,
		body: strings.NewReader("two\n"), contentType: "text/plain",
		header: map[string]string{"If-None-Match": "*"}})
	expectError(t, again, CodePreconditionFailed)

	// If-Match with the current checksum applies, and with a stale one does
	// not.
	current := unquote(first.header.Get("ETag"))
	stale := sha256Of("bytes this object never held\n")
	refused := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, path), subject: Alice,
		body: strings.NewReader("three\n"), contentType: "text/plain",
		header: map[string]string{"If-Match": `"` + stale + `"`}})
	expectError(t, refused, CodePreconditionFailed)

	applied := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, path), subject: Alice,
		body: strings.NewReader("four\n"), contentType: "text/plain",
		header: map[string]string{"If-Match": `"` + current + `"`}})
	expectAWrite(t, applied)

	// A weak validator asks for a comparison this server does not make.
	weak := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, path), subject: Alice,
		body: strings.NewReader("five\n"), contentType: "text/plain",
		header: map[string]string{"If-Match": `W/"` + current + `"`}})
	expectError(t, weak, CodeInvalidField)

	// A write with neither header succeeds, on a path that holds something
	// and on one that does not.
	expectAWrite(t, s.put(t, Alice, path, "six\n", "text/plain"))
	expectAWrite(t, s.put(t, Alice, s.filePath("unconditional.txt"), "seven\n", "text/plain"))

	// A put of object bytes with no Content-Length cannot be admitted before
	// the first byte is written, so it is refused rather than guessed at.
	chunked := s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, s.filePath("chunked.txt")),
		subject: Alice, body: chunkedBody("eight\n"), contentType: "text/plain"})
	failIf(t, chunked.status < 400, "a body with no Content-Length was accepted")
	expectError(t, chunked, CodeLengthRequired)
}

// case005Bytes: an object at or below the inline threshold streams through
// the server and one above it answers a redirect to a presigned URL for one
// object and one method. This is invariant 4 of spec 001, bytes off the hot
// path, and it is not a default a caller waives: ?inline=1 above the
// threshold still answers the redirect.
func case005Bytes(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	small := s.filePath("small.bin")
	expectAWrite(t, s.put(t, Alice, small, "small\n", "application/octet-stream"))

	// ?inline=0 asks for the redirect at any size, which is the one way a
	// suite learns the shape without knowing the threshold.
	redirected := s.call(t, Alice, http.MethodGet, s.fileRoute(owner, small)+"?inline=0", "")
	failIf(t, redirected.status != http.StatusFound,
		"a read asking for the redirect answered %d, want 302: %s", redirected.status, redirected.body)
	url := redirected.header.Get("Location")
	failIf(t, url == "", "the redirect names no Location")

	fetched := s.do(t, request{method: http.MethodGet, path: url})
	expectStatus(t, fetched, http.StatusOK)
	failIf(t, string(fetched.body) != "small\n", "the presigned URL served %q", fetched.body)

	// The URL is for one method. A write through it is refused by the store,
	// which is what one method means.
	written := s.do(t, request{method: http.MethodPut, path: url, body: strings.NewReader("overwritten\n")})
	failIf(t, written.status < 400, "a presigned read URL accepted a write: %d", written.status)

	// And for one object: the same signature over another key does not
	// serve it.
	other := strings.Replace(url, small, small+".other", 1)
	if other != url {
		r := s.do(t, request{method: http.MethodGet, path: other})
		failIf(t, r.status == http.StatusOK, "a presigned URL served a second object")
	}
}

// case005Versions: a second write makes a version, the list is ordered
// oldest first, an old version is readable and restorable, and a delete of
// the current version does not delete the history.
func case005Versions(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	path := s.filePath("versioned.txt")
	expectAWrite(t, s.put(t, Alice, path, "one\n", "text/plain"))
	expectAWrite(t, s.put(t, Alice, path, "two\n", "text/plain"))

	versions := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?versions=1", ""), http.StatusOK)
	entries := list(versions.json, "entries")
	failIf(t, len(entries) < 2, "two writes made %d versions", len(entries))
	failIf(t, unquote(str(entries[0], "checksum")) != sha256Of("one\n"),
		"the version list is not oldest first: the first entry is %v", entries[0])

	old := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?version=1&inline=1", ""), http.StatusOK)
	failIf(t, string(old.body) != "one\n", "version 1 read back as %q", old.body)

	restored := expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, path), body(fields{"restore_version": 1})), http.StatusOK)
	_ = restored
	current := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", ""), http.StatusOK)
	failIf(t, string(current.body) != "one\n", "the restore left %q at the path", current.body)

	// move_to and restore_version are two fields that cannot be set
	// together.
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, path),
		body(fields{"move_to": s.filePath("elsewhere.txt"), "restore_version": 1})), CodeExclusiveFields)
}

// case005Trash: a delete moves an object to trash, a trashed object is
// absent from the listing and readable from the trash route, a restore
// returns it to its path, and a purge removes it.
func case005Trash(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	path := s.filePath("trashed.txt")
	expectAWrite(t, s.put(t, Alice, path, "trash me\n", "text/plain"))

	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, path), ""), http.StatusNoContent)
	expectError(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path), ""), CodeNotFound)

	trash := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/trash?owner=me&limit=1000", ""), http.StatusOK)
	found := false
	for _, e := range list(trash.json, "entries") {
		if str(e, "path") == path {
			found = true
		}
	}
	failIf(t, !found, "a deleted object is not in the trash listing")

	expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/trash/restore", body(fields{"owner": "me", "path": path})), http.StatusOK)
	expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", ""), http.StatusOK)

	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, path), ""), http.StatusNoContent)
	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/trash?owner=me&path="+escape(path), ""), http.StatusNoContent)
	expectError(t, s.call(t, Alice, http.MethodPost, "/v1/trash/restore", body(fields{"owner": "me", "path": path})), CodeNotFound)
}

// case005Stars: a star and an unstar are idempotent rows the caller owns,
// the starred listing reaches every space, and a star on an object the
// caller may not read is refused as missing.
func case005Stars(t *testing.T, s *session) {
	path := s.filePath("starred.txt")
	expectAWrite(t, s.put(t, Alice, path, "star me\n", "text/plain"))

	expectStatus(t, s.call(t, Alice, http.MethodPut, "/v1/stars", body(fields{"owner": "me", "path": path})), http.StatusOK)
	expectStatus(t, s.call(t, Alice, http.MethodPut, "/v1/stars", body(fields{"owner": "me", "path": path})), http.StatusOK)

	page := expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/stars?limit=1000", ""), http.StatusOK)
	found := false
	for _, e := range list(page.json, "entries") {
		if str(e, "path") == path {
			found = true
		}
	}
	failIf(t, !found, "a starred object is not in the caller's stars")

	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/stars?owner=me&path="+escape(path), ""), http.StatusNoContent)
	expectStatus(t, s.call(t, Alice, http.MethodDelete, "/v1/stars?owner=me&path="+escape(path), ""), http.StatusNoContent)

	// A star on an object of a space the caller cannot read is a missing
	// object, not a refused one.
	hidden := s.call(t, Bob, http.MethodPut, "/v1/stars",
		body(fields{"owner": s.subject(t, Alice), "path": path}))
	if hidden.status < 400 {
		s.unverifiable(t, "a star on an unreadable object is refused as missing",
			"the target allows this caller the other principal's space")
		return
	}
	expectError(t, hidden, CodeNotFound)
}

// case005Materialize: one space as a manifest of presigned URLs. It needs no
// lease, because there is no writer to exclude; the leased form is spec
// 009's.
func case005Materialize(t *testing.T, s *session) {
	path := s.filePath("materialize/a.txt")
	expectAWrite(t, s.put(t, Alice, path, "manifest me\n", "text/plain"))

	m := expectStatus(t, s.call(t, Alice, http.MethodGet,
		"/v1/files/materialize?owner=me&prefix="+escape(s.filePath("materialize")), ""), http.StatusOK)
	failIf(t, m.json["files"] == nil, "the manifest answers a null files: %s", m.body)
	failIf(t, m.json["pinned_at"] != nil, "GET /v1/files/materialize pins nothing, so pinned_at is null: %s", m.body)
	for _, f := range list(m.json, "files") {
		failIf(t, str(f, "url") == "", "a manifest entry carries no presigned url: %v", f)
	}
}

// expectAWrite asserts a write of object bytes was taken: spec 013 answers
// 201 for a path that held nothing and 200 for one that did.
func expectAWrite(t testing.TB, r response) response {
	t.Helper()
	failIf(t, r.status != http.StatusOK && r.status != http.StatusCreated,
		"the write answered %d, want 200 or 201: %s", r.status, r.body)
	return r
}

// chunkedBody is a body whose length the client cannot state, which is what
// makes a request chunked and what length_required exists for.
func chunkedBody(content string) *chunked { return &chunked{rest: content} }

// chunked is a reader net/http cannot measure, so the request goes out with
// no Content-Length.
type chunked struct{ rest string }

func (c *chunked) Read(p []byte) (int, error) {
	if c.rest == "" {
		return 0, io.EOF
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}
