// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"strings"
	"testing"
)

// The cases of spec 030: a folder moved in one call.
func cases030() []testCase {
	const (
		put    = "PUT /v1/files/{owner}/{path...}"
		get    = "GET /v1/files/{owner}/{path...}"
		post   = "POST /v1/files/{owner}/{path...}"
		remove = "DELETE /v1/files/{owner}/{path...}"
	)
	return []testCase{
		{name: "FolderMove", group: GroupFiles, routes: []string{
			put, get, post, remove, "GET /v1/trash", "GET /v1/events",
		}, codes: []string{CodePathTaken, CodeInvalidField, CodeNotFound}, run: case030FolderMove},
	}
}

// case030FolderMove: a folder is moved in one call, and each object under it
// reads at the same path below the destination with the validator it had,
// which is invariant 8 seen from outside: a move renames and copies nothing.
// The history moves with each object, a trashed object and the object at the
// folder's own path stay, and the log records one move with the counts. One
// destination any object holds refuses the whole move, a destination inside
// the folder is refused, and a folder with nothing under it is not there.
func case030FolderMove(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	source := s.folder(t, Alice, "move-source")
	moved := s.folder(t, Alice, "move-destination")
	other := s.folder(t, Alice, "move-other")
	tree := map[string]string{
		"/a.txt":        "the current content of a\n",
		"/deeper/b.txt": "b, two folders down\n",
	}
	expectAWrite(t, s.put(t, Alice, source+"/a.txt", "the first content of a\n", "text/plain"))
	for leaf, content := range tree {
		expectAWrite(t, s.put(t, Alice, source+leaf, content, "text/plain"))
	}
	expectAWrite(t, s.put(t, Alice, source+"/trashed.txt", "in the trash\n", "text/plain"))
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, source+"/trashed.txt"), ""), http.StatusNoContent)
	expectAWrite(t, s.put(t, Alice, source, "an object at the folder's own path\n", "text/plain"))
	validators := map[string]string{}
	for leaf := range tree {
		r := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, source+leaf)+"?inline=1", ""), http.StatusOK)
		validators[leaf] = r.header.Get("ETag")
	}

	done := expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source),
		body(fields{"move_to": moved, "recursive": true})), http.StatusOK)
	failIf(t, str(done.json, "path") != moved || num(done.json, "files") != int64(len(tree)) || num(done.json, "bytes") != sizeOf(tree),
		"the folder move answered %s, want the path %q, %d files and %d bytes", done.body, moved, len(tree), sizeOf(tree))
	for leaf, content := range tree {
		s.expectBytes(t, Alice, owner, moved+leaf, content)
		s.expectGone(t, Alice, owner, source+leaf)
		r := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, moved+leaf)+"?inline=1", ""), http.StatusOK)
		failIf(t, r.header.Get("ETag") != validators[leaf],
			"%s answers the validator %q after the move and %q before it; a move renames and copies nothing",
			leaf, r.header.Get("ETag"), validators[leaf])
	}
	history := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, moved+"/a.txt")+"?versions=1", ""), http.StatusOK)
	failIf(t, len(list(history.json, "entries")) != 1, "the history did not move with the object: %s", history.body)
	s.expectBytes(t, Alice, owner, source, "an object at the folder's own path\n")
	s.expectGone(t, Alice, owner, moved+"/trashed.txt")
	failIf(t, !pathsOf(s.pages(t, Alice, "/v1/trash?owner=me"), "path")[source+"/trashed.txt"],
		"the trashed object under the folder left its path; it stays where it was trashed from")

	logged := s.folderEvents(t, Alice, owner, "move", moved)
	failIf(t, len(logged) != 1, "one folder move left %d move events at the destination, want one", len(logged))
	detail := obj(logged[0], "detail")
	failIf(t, str(detail, "from") != source || detail["recursive"] != true ||
		num(detail, "files") != int64(len(tree)) || num(detail, "bytes") != sizeOf(tree),
		"the move event carries %v, want the source %q, recursive, %d files and %d bytes", detail, source, len(tree), sizeOf(tree))

	// One destination an object holds refuses the whole move, and nothing
	// of the folder moves.
	const fresh = "nothing holds this path at the destination\n"
	expectAWrite(t, s.put(t, Alice, other+"/a.txt", "another a\n", "text/plain"))
	expectAWrite(t, s.put(t, Alice, other+"/fresh.txt", fresh, "text/plain"))
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, other),
		body(fields{"move_to": moved, "recursive": true})), CodePathTaken)
	s.expectBytes(t, Alice, owner, other+"/fresh.txt", fresh)
	s.expectGone(t, Alice, owner, moved+"/fresh.txt")
	s.expectBytes(t, Alice, owner, moved+"/a.txt", tree["/a.txt"])

	// The destination is not the folder and not inside it, and a folder
	// with nothing under it is not there to move.
	for _, inside := range []string{moved, moved + "/deeper", moved + "/into/itself"} {
		details := expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, moved),
			body(fields{"move_to": inside, "recursive": true})), CodeInvalidField)
		named := expectFields(t, details)
		failIf(t, !strings.Contains(strings.Join(named, ","), "move_to"),
			"the refusal of a move into its own folder names the fields %v and not move_to", named)
	}
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, s.filePath("move-of-nothing")),
		body(fields{"move_to": s.filePath("move-of-nothing-destination"), "recursive": true})), CodeNotFound)
}
