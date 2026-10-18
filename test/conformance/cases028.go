// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The rows of spec 028 the wire shows: a space is one tree, so a folder is
// deleted and copied in one call, an answer's limits turn the version
// history and the trash off for the write they admit and leave subtrees out
// of what a caller enumerates, a question that names two paths needs a grant
// on both, and nothing answers under the plane this server no longer serves.
//
// A folder is a prefix: every object whose path begins with the folder's
// path and a slash, and never the object at exactly that path. The cases
// below hold both operations to that, because it is the difference between
// deleting files/notes and deleting files/notes-archive with it.

func cases028() []testCase {
	const (
		put    = "PUT /v1/files/{owner}/{path...}"
		get    = "GET /v1/files/{owner}/{path...}"
		post   = "POST /v1/files/{owner}/{path...}"
		remove = "DELETE /v1/files/{owner}/{path...}"
		events = "GET /v1/events"
	)
	return []testCase{
		{name: "FolderDelete", group: GroupFiles, routes: []string{
			put, get, remove, "GET /v1/trash", "POST /v1/trash/restore", events,
			"POST /v1/shares", "GET /v1/shares/{id}",
		}, codes: []string{CodeInvalidPath, CodeExclusiveFields, CodeNotFound}, run: case028FolderDelete},
		{name: "FolderCopy", group: GroupFiles, routes: []string{put, get, post, remove, events}, codes: []string{
			CodePathTaken, CodeInvalidField, CodeExclusiveFields, CodeNotFound,
		}, run: case028FolderCopy},
		{name: "Limits", group: GroupUsage, routes: []string{
			put, get, remove, "GET /v1/trash", "POST /v1/trash/restore",
		}, codes: []string{CodeStorageUnavailable, CodeNotFound}, run: case028Limits},
		{name: "BothEnds", group: GroupAuthorizer, routes: []string{
			put, get, post, remove, "POST /v1/shares", "DELETE /v1/shares/{id}",
		}, codes: []string{CodeNotFound}, run: case028BothEnds},
		{name: "Omit", group: GroupAuthorizer, routes: []string{
			put, get, post, remove, events,
			"GET /v1/files/materialize", "GET /v1/trash",
			"PUT /v1/stars", "DELETE /v1/stars", "GET /v1/stars",
			"POST /v1/shares", "GET /v1/shares", "GET /v1/shares/{id}", "DELETE /v1/shares/{id}",
			"POST /v1/shares/links", "GET /v1/shares/links", "DELETE /v1/shares/links/{id}",
		}, codes: []string{CodeStorageUnavailable, CodeNotFound}, run: case028Omit},
		{name: "NoSecondPlane", group: GroupPaths, routes: []string{put, get}, codes: []string{
			CodeUnknownPlane, CodeNotFound, CodeUnauthenticated,
		}, run: case028NoSecondPlane},
	}
}

// folder names a folder of the principal's own space under the run's own
// name and records its removal, so whatever a case leaves under it, live or
// trashed, goes with the run. An object a case trashed is not reached by the
// delete of one path the cleanup of a put makes, and the folder delete takes
// both.
func (s *session) folder(t testing.TB, principal, label string) string {
	t.Helper()
	path, owner := s.filePath(label), s.subject(t, principal)
	s.record("folder "+path, func(t testing.TB) error {
		s.call(t, principal, http.MethodDelete, s.fileRoute(owner, path)+"?recursive=1&permanent=1", "")
		return nil
	})
	return path
}

// fill writes each object of a tree and fails the case on one the target
// did not take.
func (s *session) fill(t testing.TB, principal string, tree map[string]string) {
	t.Helper()
	for path, content := range tree {
		expectAWrite(t, s.put(t, principal, path, content, "text/plain"))
	}
}

// sizeOf is the bytes a tree holds, which is what a count of it answers.
func sizeOf(tree map[string]string) int64 {
	var total int64
	for _, content := range tree {
		total += int64(len(content))
	}
	return total
}

// within reports whether a path is at or under a prefix, on whole segments:
// files/team/pay holds files/team/pay/may.pdf and not files/team/payroll.
func within(prefix, path string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// pages reads every page of a list route as one principal and answers the
// entries, following next_cursor until it is absent. The route is given
// with whatever selects the listing and without a limit or a cursor.
func (s *session) pages(t testing.TB, principal, route string) []map[string]any {
	t.Helper()
	join := "?"
	if strings.Contains(route, "?") {
		join = "&"
	}
	var all []map[string]any
	cursor := ""
	for range 100 {
		path := route + join + "limit=1000"
		if cursor != "" {
			path += "&cursor=" + escape(cursor)
		}
		r := expectStatus(t, s.call(t, principal, http.MethodGet, path, ""), http.StatusOK)
		failIf(t, r.json["entries"] == nil, "%s answers a null entries, and spec 013 never does: %s", route, r.body)
		all = append(all, list(r.json, "entries")...)
		next := str(r.json, "next_cursor")
		if next == "" {
			return all
		}
		failIf(t, next == cursor, "%s answered the cursor it was given, so a reader never finishes", route)
		cursor = next
	}
	t.Fatalf("%s did not end after a hundred pages", route)
	return nil
}

// pathsOf is the set of values one field takes across the entries of a
// listing: the paths of a file page, the prefixes of a grant page.
func pathsOf(entries []map[string]any, field string) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[str(e, field)] = true
	}
	return out
}

// folderEvents answers the events of one action a space's log holds at one
// path, as the principal reads it.
func (s *session) folderEvents(t testing.TB, principal, owner, action, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range s.pages(t, principal, "/v1/events?owner="+escape(owner)) {
		if str(e, "action") == action && str(e, "path") == path {
			out = append(out, e)
		}
	}
	return out
}

// expectGone asserts that nothing is read at a path.
func (s *session) expectGone(t testing.TB, principal, owner, path string) {
	t.Helper()
	expectError(t, s.call(t, principal, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", ""), CodeNotFound)
}

// expectBytes asserts that a path reads back as the bytes given.
func (s *session) expectBytes(t testing.TB, principal, owner, path, content string) {
	t.Helper()
	r := expectStatus(t, s.call(t, principal, http.MethodGet, s.fileRoute(owner, path)+"?inline=1", ""), http.StatusOK)
	failIf(t, string(r.body) != content, "%q reads back as %q, want %q", path, r.body, content)
}

// case028FolderDelete: a folder is deleted in one call. Without permanent=1
// every live object under it goes to the trash, each by itself, and nothing
// beside the folder is touched: not the object at the folder's own path and
// not a sibling whose name begins with the folder's. With permanent=1 every
// row under it goes, live and trashed, with its versions and the grants on
// the folder. A folder with nothing under it is the same as one that never
// existed, so the call answers 204 either way and records nothing.
func case028FolderDelete(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	folder := s.folder(t, Alice, "folder-delete")
	under := map[string]string{
		folder + "/a.txt":        "the current content of a\n",
		folder + "/deeper/b.txt": "b, two folders down\n",
	}
	beside := map[string]string{
		folder:                    "an object at the folder's own path\n",
		folder + "-archive/x.txt": "a sibling folder whose name begins with the folder's\n",
		folder + "x.txt":          "a sibling object whose name begins with the folder's\n",
	}
	// One object is written twice, so the folder holds a version for the
	// permanent delete to take.
	const replaced = "the first content of a\n"
	expectAWrite(t, s.put(t, Alice, folder+"/a.txt", replaced, "text/plain"))
	s.fill(t, Alice, under)
	s.fill(t, Alice, beside)
	// The siblings are folders of the run's own too, and nothing else
	// removes what a failed case leaves trashed under them.
	s.record("folder "+folder+"-archive", func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, folder+"-archive")+"?recursive=1&permanent=1", "")
		return nil
	})
	granted := s.grant(t, Alice, folder+"/deeper", s.absentSubject(), "read")

	// The plane itself is not a folder, and a version is one content of one
	// path: both are refused before anything is read. The plane is asked
	// for without permanent=1, so a target that took it would have trashed
	// what it holds and removed nothing.
	expectError(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, "files")+"?recursive=1", ""), CodeInvalidPath)
	expectError(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, folder)+"?recursive=1&version=1", ""), CodeExclusiveFields)
	for path, content := range under {
		s.expectBytes(t, Alice, owner, path, content)
	}

	// Soft: the subtree is in the trash and nothing beside it is.
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, folder)+"?recursive=1", ""), http.StatusNoContent)
	for path := range under {
		s.expectGone(t, Alice, owner, path)
	}
	for path, content := range beside {
		s.expectBytes(t, Alice, owner, path, content)
	}
	trashed := pathsOf(s.pages(t, Alice, "/v1/trash?owner=me"), "path")
	for path := range under {
		failIf(t, !trashed[path], "the folder delete did not put %q in the trash", path)
	}
	for path := range beside {
		failIf(t, trashed[path], "the folder delete trashed %q, which is beside the folder and not under it", path)
	}
	soft := s.folderEvents(t, Alice, owner, "delete", folder)
	failIf(t, len(soft) != 1, "one folder delete left %d delete events at the folder, want one", len(soft))
	detail := obj(soft[0], "detail")
	failIf(t, detail["recursive"] != true || detail["trashed"] != true ||
		num(detail, "files") != int64(len(under)) || num(detail, "bytes") != sizeOf(under),
		"the event of a soft folder delete carries %v, want recursive, trashed, %d files and %d bytes",
		detail, len(under), sizeOf(under))
	// Each object is in the trash by itself, so one is restored without the
	// other.
	expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/trash/restore",
		body(fields{"owner": "me", "path": folder + "/a.txt"})), http.StatusOK)
	s.expectBytes(t, Alice, owner, folder+"/a.txt", under[folder+"/a.txt"])
	s.expectGone(t, Alice, owner, folder+"/deeper/b.txt")

	// Permanent: the live row, the trashed row, the version and the grant
	// all go, and nothing beside the folder does.
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, folder)+"?recursive=1&permanent=1", ""), http.StatusNoContent)
	for path := range under {
		s.expectGone(t, Alice, owner, path)
		expectError(t, s.call(t, Alice, http.MethodPost, "/v1/trash/restore",
			body(fields{"owner": "me", "path": path})), CodeNotFound)
	}
	listed := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, folder)+"?list=1&limit=1000", ""), http.StatusOK)
	failIf(t, len(list(listed.json, "entries")) != 0, "a permanent folder delete left %s under the folder", listed.body)
	trashed = pathsOf(s.pages(t, Alice, "/v1/trash?owner=me"), "path")
	for path := range under {
		failIf(t, trashed[path], "a permanent folder delete left %q in the trash", path)
	}
	history := s.call(t, Alice, http.MethodGet, s.fileRoute(owner, folder+"/a.txt")+"?versions=1", "")
	failIf(t, history.status == http.StatusOK && len(list(history.json, "entries")) != 0,
		"a permanent folder delete left a version of an object it removed: %s", history.body)
	expectError(t, s.call(t, Alice, http.MethodGet, "/v1/shares/"+str(granted, "id"), ""), CodeNotFound)
	for path, content := range beside {
		s.expectBytes(t, Alice, owner, path, content)
	}
	both := s.folderEvents(t, Alice, owner, "delete", folder)
	failIf(t, len(both) != 2, "a soft and a permanent folder delete left %d delete events at the folder, want two", len(both))
	detail = obj(both[1], "detail")
	gone := sizeOf(under) + int64(len(replaced))
	failIf(t, detail["recursive"] != true || detail["trashed"] != false ||
		num(detail, "files") != int64(len(under)) || num(detail, "bytes") != gone,
		"the event of a permanent folder delete carries %v, want recursive, not trashed, %d files and the %d bytes the space got back",
		detail, len(under), gone)

	// Idempotent: the folder holds nothing now, and a folder that never
	// existed holds nothing either. Each answers 204, twice, in both forms,
	// and a call that found nothing appends nothing.
	for _, empty := range []string{folder, s.filePath("folder-that-never-was")} {
		for _, form := range []string{"?recursive=1", "?recursive=1&permanent=1"} {
			for range 2 {
				expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, empty)+form, ""), http.StatusNoContent)
			}
		}
	}
	failIf(t, len(s.folderEvents(t, Alice, owner, "delete", folder)) != 2,
		"a folder delete that found nothing under the folder appended an event")
}

// case028FolderCopy: a folder is copied on the server in one call, and each
// copy is an object of its own with the same bytes. A destination a live
// object holds refuses the whole copy as path_taken and nothing is written,
// a destination inside the source is refused, and a copy carries neither a
// version nor a trashed object. Without recursive the source is one object
// and the answer is the object, as a move answers it.
func case028FolderCopy(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	source := s.folder(t, Alice, "copy-source")
	copied := s.folder(t, Alice, "copy-destination")
	taken := s.folder(t, Alice, "copy-taken")
	tree := map[string]string{
		"/a.txt":        "the current content of a\n",
		"/deeper/b.txt": "b, two folders down\n",
	}
	// A version, a trashed object and an object at the folder's own path,
	// none of which is under the folder as a copy reads it.
	expectAWrite(t, s.put(t, Alice, source+"/a.txt", "the first content of a\n", "text/plain"))
	for leaf, content := range tree {
		expectAWrite(t, s.put(t, Alice, source+leaf, content, "text/plain"))
	}
	expectAWrite(t, s.put(t, Alice, source+"/trashed.txt", "in the trash\n", "text/plain"))
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, source+"/trashed.txt"), ""), http.StatusNoContent)
	expectAWrite(t, s.put(t, Alice, source, "an object at the folder's own path\n", "text/plain"))
	var bytes int64
	for _, content := range tree {
		bytes += int64(len(content))
	}

	// One call, and the answer says where the copy is and what it holds.
	done := expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source),
		body(fields{"copy_to": copied, "recursive": true})), http.StatusOK)
	failIf(t, str(done.json, "path") != copied || num(done.json, "files") != int64(len(tree)) || num(done.json, "bytes") != bytes,
		"the folder copy answered %s, want the path %q, %d files and %d bytes", done.body, copied, len(tree), bytes)
	for leaf, content := range tree {
		s.expectBytes(t, Alice, owner, source+leaf, content)
		s.expectBytes(t, Alice, owner, copied+leaf, content)
	}
	s.expectGone(t, Alice, owner, copied+"/trashed.txt")
	s.expectGone(t, Alice, owner, copied)
	history := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, copied+"/a.txt")+"?versions=1", ""), http.StatusOK)
	failIf(t, len(list(history.json, "entries")) != 0, "a copy carried the versions of its source: %s", history.body)

	// The log shows one copy, at the destination, naming the source.
	logged := s.folderEvents(t, Alice, owner, "copy", copied)
	failIf(t, len(logged) != 1, "one folder copy left %d copy events at the destination, want one", len(logged))
	detail := obj(logged[0], "detail")
	failIf(t, str(detail, "from") != source || detail["recursive"] != true ||
		num(detail, "files") != int64(len(tree)) || num(detail, "bytes") != bytes,
		"the copy event carries %v, want the source %q, recursive, %d files and %d bytes", detail, source, len(tree), bytes)

	// Each copy is an object of its own: the source leaves for good and the
	// copy still reads.
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, source+"/a.txt")+"?permanent=1", ""), http.StatusNoContent)
	s.expectBytes(t, Alice, owner, copied+"/a.txt", tree["/a.txt"])
	expectAWrite(t, s.put(t, Alice, source+"/a.txt", tree["/a.txt"], "text/plain"))

	// One destination a live object holds refuses the whole copy, and no
	// object of it is written.
	const held = "what the destination already held\n"
	expectAWrite(t, s.put(t, Alice, taken+"/deeper/b.txt", held, "text/plain"))
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source),
		body(fields{"copy_to": taken, "recursive": true})), CodePathTaken)
	s.expectGone(t, Alice, owner, taken+"/a.txt")
	s.expectBytes(t, Alice, owner, taken+"/deeper/b.txt", held)

	// The destination is not the source and not inside it, and a folder
	// with nothing under it is not there to copy.
	for _, inside := range []string{source, source + "/deeper", source + "/copy-of-itself"} {
		details := expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source),
			body(fields{"copy_to": inside, "recursive": true})), CodeInvalidField)
		named := expectFields(t, details)
		failIf(t, !strings.Contains(strings.Join(named, ","), "copy_to"),
			"the refusal of a copy into its own source names the fields %v and not copy_to", named)
	}
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, s.filePath("copy-of-nothing")),
		body(fields{"copy_to": s.filePath("copy-of-nothing-destination"), "recursive": true})), CodeNotFound)

	// Without recursive the source is one object, and the answer is that
	// object at its new path.
	single := s.filePath("copy-single.txt")
	s.record("object "+single, func(t testing.TB) error {
		s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, single)+"?permanent=1", "")
		return nil
	})
	one := expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source+"/a.txt"),
		body(fields{"copy_to": single})), http.StatusOK)
	failIf(t, str(one.json, "path") != single || unquote(str(one.json, "checksum")) != sha256Of(tree["/a.txt"]),
		"the copy of one object answered %s, want the object at %q with the checksum of its bytes", one.body, single)
	s.expectBytes(t, Alice, owner, single, tree["/a.txt"])
	s.expectBytes(t, Alice, owner, source+"/a.txt", tree["/a.txt"])
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source+"/a.txt"),
		body(fields{"copy_to": single})), CodePathTaken)

	// recursive goes with copy_to or move_to and with nothing else, and a
	// request names one of the three operations. With move_to it makes the
	// path a folder (spec 030), and an object is not one: nothing is under
	// it to move.
	elsewhere := s.filePath("copy-elsewhere.txt")
	for _, alone := range []fields{
		{"recursive": true},
		{"restore_version": 1, "recursive": true},
	} {
		expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source+"/a.txt"), body(alone)), CodeInvalidField)
	}
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source+"/a.txt"),
		body(fields{"move_to": elsewhere, "recursive": true})), CodeNotFound)
	for _, two := range []fields{
		{"move_to": elsewhere, "copy_to": single},
		{"restore_version": 1, "copy_to": elsewhere},
		{"move_to": elsewhere, "restore_version": 1, "copy_to": single},
	} {
		expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, source+"/a.txt"), body(two)), CodeExclusiveFields)
	}
	s.expectGone(t, Alice, owner, elsewhere)
	s.expectBytes(t, Alice, owner, source+"/a.txt", tree["/a.txt"])
}

// limit puts one limits object on every allow the stub answers, which is the
// one way a limit reaches the target: it stores none. The ttl is the
// shortest the contract has, so an answer that carried one limit is not the
// answer the next question gets.
func (s *session) limit(t testing.TB, limits map[string]any) {
	t.Helper()
	s.setRules(t, stubRule{Subject: "*", Action: "*", Resource: "*", Allow: true, TTL: 1, Limits: limits})
}

// case028Limits: an answer's limits shape the write it admits. An overwrite
// under limits.versions: false keeps no version of what it replaces and
// leaves the versions the path already has alone, a delete under
// limits.trash: false is permanent, and a member that is not a boolean is an
// answer this server cannot read, which is no decision: a 503 with nothing
// written.
func case028Limits(t *testing.T, s *session) {
	// A limit reaches the target on its authorizer's answer, so a run with
	// no stub to put one there has no way to make the target apply one.
	if s.options.AuthorizerControl == "" {
		s.unverifiable(t, "an answer's limits.versions and limits.trash shape the write and the delete it admits",
			"Options.AuthorizerControl is not set, so no answer of this target's authorizer carries a limit the suite chose")
		return
	}
	defer s.setRules(t)
	owner := s.subject(t, Alice)
	versions := func(path string) []map[string]any {
		t.Helper()
		r := expectStatus(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, path)+"?versions=1", ""), http.StatusOK)
		return list(r.json, "entries")
	}

	// Every content has a length of its own. The question an overwrite asks
	// carries the size, so no two of them are one question and none is
	// answered from an answer held for another.
	kept := s.filePath("limits-versions.txt")
	const first, second, third, fourth = "one\n", "the second\n", "the third, longer\n", "the fourth, the longest of them\n"

	s.limit(t, map[string]any{"versions": false})
	expectAWrite(t, s.put(t, Alice, kept, first, "text/plain"))
	expectAWrite(t, s.put(t, Alice, kept, second, "text/plain"))
	failIf(t, len(versions(kept)) != 0,
		"an overwrite under limits.versions: false kept %d versions, want none", len(versions(kept)))
	s.expectBytes(t, Alice, owner, kept, second)

	// With the member true the write keeps a version, as it does with the
	// member absent.
	s.limit(t, map[string]any{"versions": true})
	expectAWrite(t, s.put(t, Alice, kept, third, "text/plain"))
	history := versions(kept)
	failIf(t, len(history) != 1 || unquote(str(history[0], "checksum")) != sha256Of(second),
		"an overwrite under limits.versions: true left the history %v, want the one content it replaced", history)

	// The limit is about the write and not about the path: the version the
	// path already has stays, and the content this write replaces is not
	// added to it.
	s.limit(t, map[string]any{"versions": false})
	expectAWrite(t, s.put(t, Alice, kept, fourth, "text/plain"))
	history = versions(kept)
	failIf(t, len(history) != 1 || unquote(str(history[0], "checksum")) != sha256Of(second),
		"an overwrite under limits.versions: false left the history %v, want the one version the path already had", history)
	s.expectBytes(t, Alice, owner, kept, fourth)

	// A plain delete under limits.trash: false is permanent: the object is
	// not in the trash and there is nothing to restore.
	removed := s.filePath("limits-trash.txt")
	s.limit(t, map[string]any{"trash": false})
	expectAWrite(t, s.put(t, Alice, removed, "removed, not trashed\n", "text/plain"))
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, removed), ""), http.StatusNoContent)
	s.expectGone(t, Alice, owner, removed)
	failIf(t, pathsOf(s.pages(t, Alice, "/v1/trash?owner=me"), "path")[removed],
		"a delete under limits.trash: false put the object in the trash")
	expectError(t, s.call(t, Alice, http.MethodPost, "/v1/trash/restore",
		body(fields{"owner": "me", "path": removed})), CodeNotFound)

	// A member that is not a boolean is not a limit this server can apply,
	// and applying the rest of the answer would ignore one the endpoint set.
	// The write and the delete are refused and neither happens.
	unwritten := s.filePath("limits-unreadable.txt")
	s.limit(t, map[string]any{"versions": "no"})
	expectError(t, s.do(t, request{method: http.MethodPut, path: s.fileRoute(owner, unwritten), subject: Alice,
		body: strings.NewReader("never written\n"), contentType: "text/plain"}), CodeStorageUnavailable)
	s.limit(t, map[string]any{"trash": 1})
	expectError(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, kept), ""), CodeStorageUnavailable)
	s.setRules(t)
	s.expectGone(t, Alice, owner, unwritten)
	s.expectBytes(t, Alice, owner, kept, fourth)
}

// case028BothEnds: a move and a copy name two paths, and the question each
// asks carries the lower of the rungs the caller holds on the two. A grantee
// whose grant covers only the source, or only the destination, holds nothing
// on the other end and is refused; one whose grant covers both is admitted.
//
// It runs under the stub's grants mode, as case006Grantee does: the rule
// table allows the space's owner and nobody else, so the grant on the
// resource is the whole of what admits the grantee.
func case028BothEnds(t *testing.T, s *session) {
	if !s.grantsMode(t, true) {
		s.unverifiable(t, "a grantee whose grant covers one end of a move or a copy is refused",
			"the target's authorizer does not serve the grants mode of spec 014, so no endpoint here decides by the resource's grant")
		return
	}
	defer func() { s.grantsMode(t, false) }()

	owner, grantee := s.subject(t, Alice), s.subject(t, Bob)
	covered := s.folder(t, Alice, "both-ends-granted")
	beside := s.folder(t, Alice, "both-ends-beside")
	const inside, outside, nested = "inside the granted folder\n", "outside the granted folder\n", "nested in the granted folder\n"
	s.fill(t, Alice, map[string]string{
		covered + "/a.txt":      inside,
		covered + "/sub/c.txt":  nested,
		beside + "/b.txt":       outside,
		beside + "/sub/d.txt":   outside,
		covered + "/keep/e.txt": inside,
	})
	s.grant(t, Alice, covered, grantee, "write")

	// The table is read from the last row back, so the deny goes first and
	// the space's owner after it. The grantee matches neither and is left to
	// the grant on the resource.
	s.setRules(t,
		stubRule{Subject: "*", Action: "*", Resource: "*", Allow: false, Reason: "denied by the conformance suite"},
		stubRule{Subject: owner, Action: "*", Resource: "*", Allow: true, TTL: 1},
	)
	defer s.setRules(t)

	act := func(from string, operation fields) response {
		return s.call(t, Bob, http.MethodPost, s.fileRoute(owner, from), body(operation))
	}
	// One end covered, in each direction, for a move, a copy of one object
	// and a copy of a folder. Each is a refusal at lookup, which is a
	// missing object (invariant 6), and none writes.
	for _, refused := range []struct {
		why, from string
		operation fields
	}{
		{"a move out of the granted folder", covered + "/a.txt", fields{"move_to": beside + "/moved.txt"}},
		{"a copy out of the granted folder", covered + "/a.txt", fields{"copy_to": beside + "/copied.txt"}},
		{"a folder copy out of the granted folder", covered + "/sub", fields{"copy_to": beside + "/sub-copied", "recursive": true}},
		{"a move into the granted folder", beside + "/b.txt", fields{"move_to": covered + "/moved-in.txt"}},
		{"a copy into the granted folder", beside + "/b.txt", fields{"copy_to": covered + "/copied-in.txt"}},
		{"a folder copy into the granted folder", beside + "/sub", fields{"copy_to": covered + "/sub-copied-in", "recursive": true}},
	} {
		r := act(refused.from, refused.operation)
		failIf(t, r.status < 400, "%s was admitted with a grant on one of its two paths: %d %s", refused.why, r.status, r.body)
		expectError(t, r, CodeNotFound)
	}
	s.expectBytes(t, Alice, owner, covered+"/a.txt", inside)
	s.expectBytes(t, Alice, owner, beside+"/b.txt", outside)
	for _, unwritten := range []string{
		beside + "/moved.txt", beside + "/copied.txt", beside + "/sub-copied/c.txt",
		covered + "/moved-in.txt", covered + "/copied-in.txt", covered + "/sub-copied-in/d.txt",
	} {
		s.expectGone(t, Alice, owner, unwritten)
	}

	// Both ends covered: the same grantee copies and moves inside the
	// folder the grant names.
	for _, admitted := range []struct {
		why, from string
		operation fields
	}{
		{"a copy inside the granted folder", covered + "/a.txt", fields{"copy_to": covered + "/copied.txt"}},
		{"a folder copy inside the granted folder", covered + "/sub", fields{"copy_to": covered + "/sub-copied", "recursive": true}},
		{"a move inside the granted folder", covered + "/a.txt", fields{"move_to": covered + "/moved.txt"}},
	} {
		r := act(admitted.from, admitted.operation)
		failIf(t, r.status != http.StatusOK,
			"%s answered %d %s; the grant covers both of its paths and the question carries the rung", admitted.why, r.status, r.code())
	}
	s.expectBytes(t, Alice, owner, covered+"/copied.txt", inside)
	s.expectBytes(t, Alice, owner, covered+"/sub-copied/c.txt", nested)
	s.expectBytes(t, Alice, owner, covered+"/moved.txt", inside)
	s.expectGone(t, Alice, owner, covered+"/a.txt")
}

// omitting puts limits.omit on every allow the stub answers one subject and
// leaves every other subject an allow with no limit, so the same space is
// read with the omission by one principal and without it by another.
func (s *session) omitting(t testing.TB, subject string, omit any) {
	t.Helper()
	s.setRules(t,
		stubRule{Subject: "*", Action: "*", Resource: "*", Allow: true, TTL: 1},
		stubRule{Subject: subject, Action: "*", Resource: "*", Allow: true, TTL: 1, Limits: map[string]any{"omit": omit}},
	)
}

// case028Omit: an answer to a question that enumerates may leave subtrees
// out, and every route that answers the path, a count or the bytes of
// anything but the one object asked for honors it: the listing, the
// manifest, the trash, the stars, the log, the grants, the links, and the
// two folder operations. A question about one object is not shaped by it,
// a second principal whose answers carry no omission reads the whole space,
// and an entry that is not a path under the plane is an answer this server
// cannot read.
func case028Omit(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	base := s.folder(t, Alice, "omit")
	hidden := base + "/hidden"
	// What the omission leaves in, and what it leaves out. The sibling of
	// the omitted folder begins with its name and is not under it: an entry
	// covers whole segments.
	seen := map[string]string{
		base + "/b.txt":            "beside the omitted folder\n",
		base + "/seen/a.txt":       "in a folder the omission does not name\n",
		base + "/hidden-not/d.txt": "in a folder whose name begins with the omitted one's\n",
	}
	unseen := map[string]string{
		hidden + "/secret.txt":   "at the top of the omitted folder\n",
		hidden + "/deeper/c.txt": "two folders into the omitted folder\n",
	}
	s.fill(t, Alice, seen)
	s.fill(t, Alice, unseen)
	leftIn := func(path string) bool { return !within(hidden, path) }

	// One trashed object on each side.
	for _, path := range []string{base + "/seen/trashed.txt", hidden + "/trashed.txt"} {
		expectAWrite(t, s.put(t, Alice, path, "in the trash\n", "text/plain"))
		expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, path), ""), http.StatusNoContent)
	}
	// One object moved out of the omitted folder: the event is at a path the
	// omission leaves in and names a source it leaves out.
	const left = "moved out of the omitted folder\n"
	arrived := base + "/seen/arrived.txt"
	expectAWrite(t, s.put(t, Alice, hidden+"/leaving.txt", left, "text/plain"))
	expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, hidden+"/leaving.txt"),
		body(fields{"move_to": arrived})), http.StatusOK)
	seen[arrived] = left

	// Stars on both sides for the space's owner, and one on the omitted
	// side for the second principal.
	star := func(principal, path string) {
		t.Helper()
		expectStatus(t, s.call(t, principal, http.MethodPut, "/v1/stars",
			body(fields{"owner": owner, "path": path})), http.StatusNoContent)
		s.record("star "+path, func(t testing.TB) error {
			s.call(t, principal, http.MethodDelete, "/v1/stars?owner="+escape(owner)+"&path="+escape(path), "")
			return nil
		})
	}
	star(Alice, base+"/seen/a.txt")
	star(Alice, hidden+"/secret.txt")
	star(Bob, hidden+"/secret.txt")

	// Grants and links at the omitted folder, under it, above it and beside
	// it. The grantee is a subject no principal has, so nothing here changes
	// what either principal may do.
	grants := map[string]string{}
	for _, prefix := range []string{hidden, hidden + "/deeper", base, base + "/seen"} {
		grants[prefix] = str(s.grant(t, Alice, prefix, s.absentSubject(), "read"), "id")
	}
	links := map[string]string{}
	for _, prefix := range []string{hidden, base + "/seen"} {
		made := expectStatus(t, s.call(t, Alice, http.MethodPost, "/v1/shares/links",
			body(fields{"owner": "me", "path_prefix": prefix, "permission": "read"})), http.StatusCreated)
		id := str(made.json, "id")
		failIf(t, id == "", "a minted link carries no id: %s", made.body)
		s.record("link "+id, func(t testing.TB) error {
			s.call(t, Alice, http.MethodDelete, "/v1/shares/links/"+id, "")
			return nil
		})
		links[prefix] = id
	}

	s.omitting(t, owner, []string{hidden})
	defer s.setRules(t)
	theirs := "owner=" + escape(owner)

	// The listing: no entry under the omitted folder and no folder
	// synthesized for it, with everything else on the page. The second
	// principal reads the same folder whole.
	listing := func(principal string) (map[string]bool, map[string]bool) {
		t.Helper()
		r := expectStatus(t, s.call(t, principal, http.MethodGet, s.fileRoute(owner, base)+"?list=1&limit=1000", ""), http.StatusOK)
		folders := map[string]bool{}
		raw, _ := r.json["prefixes"].([]any)
		for _, p := range raw {
			name, _ := p.(string)
			folders[strings.TrimSuffix(name, "/")] = true
		}
		return pathsOf(list(r.json, "entries"), "path"), folders
	}
	mine, folders := listing(Alice)
	for path := range seen {
		failIf(t, !mine[path], "the listing leaves out %q, which no entry of limits.omit covers: %v", path, mine)
	}
	for path := range mine {
		failIf(t, !leftIn(path), "the listing holds %q, which limits.omit leaves out", path)
	}
	failIf(t, folders[hidden], "the listing synthesizes the folder limits.omit leaves out: %v", folders)
	failIf(t, !folders[base+"/seen"] || !folders[base+"/hidden-not"],
		"the listing synthesizes the folders %v and not the two the omission does not name", folders)
	all, everyFolder := listing(Bob)
	for path := range unseen {
		failIf(t, !all[path], "a principal whose answer carries no omission does not list %q", path)
	}
	failIf(t, !everyFolder[hidden], "a principal whose answer carries no omission is not shown the folder %q", hidden)

	// The manifest.
	manifest := manifestPaths(s.manifest(t, Alice, "me", base))
	for path := range seen {
		failIf(t, !manifest[path], "the manifest leaves out %q, which no entry of limits.omit covers", path)
	}
	for path := range manifest {
		failIf(t, !leftIn(path), "the manifest holds %q, which limits.omit leaves out", path)
	}
	whole := manifestPaths(s.manifest(t, Bob, owner, base))
	for path := range unseen {
		failIf(t, !whole[path], "a principal whose answer carries no omission is given a manifest without %q", path)
	}

	// The trash.
	trash := pathsOf(s.pages(t, Alice, "/v1/trash?owner=me"), "path")
	failIf(t, !trash[base+"/seen/trashed.txt"], "the trash listing leaves out an object no entry of limits.omit covers")
	failIf(t, trash[hidden+"/trashed.txt"], "the trash listing holds an object limits.omit leaves out")
	failIf(t, !pathsOf(s.pages(t, Bob, "/v1/trash?"+theirs), "path")[hidden+"/trashed.txt"],
		"a principal whose answer carries no omission does not see the trashed object under the folder")

	// The stars, which are the caller's own rows across every space.
	starred := pathsOf(s.pages(t, Alice, "/v1/stars"), "path")
	failIf(t, !starred[base+"/seen/a.txt"], "the star listing leaves out a star no entry of limits.omit covers")
	failIf(t, starred[hidden+"/secret.txt"], "the star listing holds a star limits.omit leaves out")
	failIf(t, !pathsOf(s.pages(t, Bob, "/v1/stars"), "path")[hidden+"/secret.txt"],
		"a principal whose answer carries no omission does not see its star under the folder")

	// The grants: one at the omitted folder and one under it are left out,
	// and one above it and one beside it are listed.
	shared := pathsOf(s.pages(t, Alice, "/v1/shares?owner=me"), "id")
	failIf(t, !shared[grants[base]] || !shared[grants[base+"/seen"]],
		"the grant listing leaves out a grant above or beside the folder limits.omit names")
	failIf(t, shared[grants[hidden]] || shared[grants[hidden+"/deeper"]],
		"the grant listing holds a grant at or under the folder limits.omit leaves out")
	everyGrant := pathsOf(s.pages(t, Bob, "/v1/shares?"+theirs), "id")
	for prefix, id := range grants {
		failIf(t, !everyGrant[id], "a principal whose answer carries no omission does not list the grant on %q", prefix)
	}

	// The links.
	linked := pathsOf(s.pages(t, Alice, "/v1/shares/links?owner=me"), "id")
	failIf(t, !linked[links[base+"/seen"]], "the link listing leaves out a link beside the folder limits.omit names")
	failIf(t, linked[links[hidden]], "the link listing holds a link on the folder limits.omit leaves out")
	failIf(t, !pathsOf(s.pages(t, Bob, "/v1/shares/links?"+theirs), "id")[links[hidden]],
		"a principal whose answer carries no omission does not list the link on the folder")

	// The log: an event is left out when its path is covered and when the
	// source it names is, so neither end of a move out of the folder shows.
	for _, e := range s.pages(t, Alice, "/v1/events?owner=me") {
		failIf(t, !leftIn(str(e, "path")), "the log holds an event at %q, which limits.omit leaves out: %v", str(e, "path"), e)
		from := str(obj(e, "detail"), "from")
		failIf(t, from != "" && !leftIn(from), "the log holds an event whose source is %q, which limits.omit leaves out: %v", from, e)
	}
	whoseLog := s.pages(t, Bob, "/v1/events?"+theirs)
	under, moved := false, false
	for _, e := range whoseLog {
		under = under || str(e, "path") == hidden+"/secret.txt"
		moved = moved || (str(e, "path") == arrived && str(obj(e, "detail"), "from") == hidden+"/leaving.txt")
	}
	failIf(t, !under || !moved,
		"a principal whose answer carries no omission does not read the events of the folder: the write under it %v, the move out of it %v", under, moved)

	// Paging moves past an omitted event. Three writes go to the end of the
	// log, the middle one under the omitted folder, and a page of one read
	// from just before them answers the first and then the third. The
	// cursor is the server's: the page that ends just before the three is
	// read again at its own length once they exist, and names the next.
	resume, held := s.logEnd(t, Alice)
	first, between, last := base+"/seen/page-1.txt", hidden+"/page-2.txt", base+"/seen/page-3.txt"
	for _, path := range []string{first, between, last} {
		expectAWrite(t, s.put(t, Alice, path, "one of three\n", "text/plain"))
	}
	seen[first], unseen[between], seen[last] = "one of three\n", "one of three\n", "one of three\n"
	page := func(cursor string, limit int) response {
		t.Helper()
		path := "/v1/events?owner=me&limit=" + strconv.Itoa(limit)
		if cursor != "" {
			path += "&cursor=" + escape(cursor)
		}
		return expectStatus(t, s.call(t, Alice, http.MethodGet, path, ""), http.StatusOK)
	}
	if held > 0 {
		again := page(resume, held)
		resume = str(again.json, "next_cursor")
		failIf(t, len(list(again.json, "entries")) != held || resume == "",
			"a page of %d that two readable events follow answered %d entries and the next cursor %q",
			held, len(list(again.json, "entries")), resume)
	}
	one := page(resume, 1)
	entries := list(one.json, "entries")
	failIf(t, len(entries) != 1 || str(entries[0], "path") != first,
		"the page after the cursor answered %v, want the one event at %q", entries, first)
	next := str(one.json, "next_cursor")
	failIf(t, next == "", "a page that a readable event follows names no next cursor: %s", one.body)
	two := page(next, 1)
	entries = list(two.json, "entries")
	failIf(t, len(entries) != 1 || str(entries[0], "path") != last,
		"the page after an omitted event answered %v, want the one event at %q: a page is as full as the rows the caller may read allow",
		entries, last)
	failIf(t, str(two.json, "next_cursor") != "", "the last page of the log names a next cursor: %s", two.body)

	// One object is one question, and the omission does not shape it.
	for path, content := range unseen {
		s.expectBytes(t, Alice, owner, path, content)
	}

	// A folder copy leaves out an object whose source is covered and one
	// whose destination is, and counts neither.
	expectError(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, base+"/seen"),
		body(fields{"copy_to": hidden + "/into", "recursive": true})), CodeNotFound)
	s.expectGone(t, Alice, owner, hidden+"/into/a.txt")
	copied := s.folder(t, Alice, "omit-copy")
	done := expectStatus(t, s.call(t, Alice, http.MethodPost, s.fileRoute(owner, base),
		body(fields{"copy_to": copied, "recursive": true})), http.StatusOK)
	failIf(t, num(done.json, "files") != int64(len(seen)) || num(done.json, "bytes") != sizeOf(seen),
		"the folder copy answered %s, want the %d files and %d bytes limits.omit leaves in", done.body, len(seen), sizeOf(seen))
	for path, content := range seen {
		s.expectBytes(t, Alice, owner, copied+strings.TrimPrefix(path, base), content)
	}
	for path := range unseen {
		s.expectGone(t, Alice, owner, copied+strings.TrimPrefix(path, base))
	}

	// An entry that is not a path under the plane, and an omit that is not
	// a list, are an answer this server cannot read. It is no decision:
	// nothing is listed and nothing is removed.
	for _, unreadable := range []any{
		[]string{"../x"}, []string{"files"}, []string{hidden, "files/"}, []string{"/files/x"}, "files/x",
	} {
		s.omitting(t, owner, unreadable)
		expectError(t, s.call(t, Alice, http.MethodGet, s.fileRoute(owner, base)+"?list=1", ""), CodeStorageUnavailable)
		expectError(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, base)+"?recursive=1", ""), CodeStorageUnavailable)
	}
	s.omitting(t, owner, []string{hidden})
	for path, content := range seen {
		s.expectBytes(t, Alice, owner, path, content)
	}

	// A folder delete leaves the omitted folder where it is, in both forms,
	// and its event counts what it took. The permanent form takes the grants
	// and the links under the folder and leaves the ones at, under or above
	// the omitted subtree: a grant on the folder itself is above it.
	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, base)+"?recursive=1", ""), http.StatusNoContent)
	for path := range seen {
		s.expectGone(t, Alice, owner, path)
	}
	for path, content := range unseen {
		s.expectBytes(t, Alice, owner, path, content)
	}
	soft := s.folderEvents(t, Alice, owner, "delete", base)
	failIf(t, len(soft) != 1, "one folder delete left %d delete events at the folder, want one", len(soft))
	detail := obj(soft[0], "detail")
	failIf(t, num(detail, "files") != int64(len(seen)) || num(detail, "bytes") != sizeOf(seen),
		"the event of the folder delete carries %v, want the %d files and %d bytes limits.omit leaves in",
		detail, len(seen), sizeOf(seen))

	expectStatus(t, s.call(t, Alice, http.MethodDelete, s.fileRoute(owner, base)+"?recursive=1&permanent=1", ""), http.StatusNoContent)
	for path, content := range unseen {
		s.expectBytes(t, Alice, owner, path, content)
	}
	failIf(t, !pathsOf(s.pages(t, Bob, "/v1/trash?"+theirs), "path")[hidden+"/trashed.txt"],
		"a permanent folder delete took a trashed object under the folder limits.omit leaves out")
	for _, prefix := range []string{hidden, hidden + "/deeper", base} {
		expectStatus(t, s.call(t, Alice, http.MethodGet, "/v1/shares/"+grants[prefix], ""), http.StatusOK)
	}
	expectError(t, s.call(t, Alice, http.MethodGet, "/v1/shares/"+grants[base+"/seen"], ""), CodeNotFound)
	remaining := pathsOf(s.pages(t, Bob, "/v1/shares/links?"+theirs), "id")
	failIf(t, !remaining[links[hidden]], "a permanent folder delete took the link on the folder limits.omit leaves out")
	failIf(t, remaining[links[base+"/seen"]], "a permanent folder delete left a link on a folder under it that limits.omit does not name")
}

// logEnd walks a space's own log to its last page as the principal reads it
// and answers the cursor that page was read from and how many events it
// held. A cursor is the server's and the last page names none, so a case
// that wants to resume from the end of the log reads that page again, at
// that length, once something follows it.
func (s *session) logEnd(t testing.TB, principal string) (cursor string, held int) {
	t.Helper()
	for range 100 {
		path := "/v1/events?owner=me&limit=1000"
		if cursor != "" {
			path += "&cursor=" + escape(cursor)
		}
		r := expectStatus(t, s.call(t, principal, http.MethodGet, path, ""), http.StatusOK)
		next := str(r.json, "next_cursor")
		if next == "" {
			return cursor, len(list(r.json, "entries"))
		}
		failIf(t, next == cursor, "the log answered the cursor it was given, so a reader never finishes")
		cursor = next
	}
	t.Fatalf("the log did not end after a hundred pages")
	return "", 0
}

// case028NoSecondPlane: a space is one tree, and every path of it begins
// with files/. The plane this server served beside it until spec 028 is
// gone: a path under workspaces/ is in no plane, as any path outside files/
// is, and nothing is registered under /v1/workspaces, so a caller with a
// token reads a missing object there and a caller without one is refused
// before any route is looked for.
func case028NoSecondPlane(t *testing.T, s *session) {
	owner := s.subject(t, Alice)
	removed := "workspaces/" + s.name("plane")
	object := s.fileRoute(owner, removed+"/y.txt")

	written := s.do(t, request{method: http.MethodPut, path: object, subject: Alice,
		body: strings.NewReader("in no plane\n"), contentType: "text/plain"})
	if written.status < 300 {
		s.record("object "+removed+"/y.txt", func(t testing.TB) error {
			s.call(t, Alice, http.MethodDelete, object+"?permanent=1", "")
			return nil
		})
	}
	expectError(t, written, CodeUnknownPlane)
	expectError(t, s.call(t, Alice, http.MethodGet, object, ""), CodeUnknownPlane)

	// The routes are gone with the plane. A path under the base that no
	// row registers is a missing object for a caller the verifier admitted,
	// and the verifier answers first for one it did not.
	expectError(t, s.call(t, Alice, http.MethodGet, "/v1/workspaces", ""), CodeNotFound)
	expectError(t, s.do(t, request{method: http.MethodGet, path: "/v1/workspaces"}), CodeUnauthenticated)
}
