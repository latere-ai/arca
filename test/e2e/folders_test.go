// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The e2e tier of spec 028's two folder operations: arcad as a process, the
// bytes in MinIO and the rows in Postgres. A folder is deleted and copied in
// one call each, and what this tier reads that no fake can show is the
// bucket: a copy made inside the store is readable at both ends through the
// presigned URL a read answers, under a key of its own, and a permanent
// delete leaves no key behind and gives the space its bytes back.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// foldersUsage reads what the caller's space holds off the root of the plane,
// which is the usage a write is charged against (spec 005).
func (i *installation) foldersUsage(t *testing.T) (bytes, files int64) {
	t.Helper()
	resp := i.filesDo(t, http.MethodGet, i.filesObject("files")+"?list=1&limit=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the root listing answered %d: %s", resp.StatusCode, filesRead(t, resp))
	}
	var page struct {
		Space *struct {
			Bytes int64 `json:"bytes"`
			Files int64 `json:"files"`
		} `json:"space"`
	}
	filesJSON(t, resp, &page)
	if page.Space == nil {
		t.Fatal("the root listing carries no space")
	}
	return page.Space.Bytes, page.Space.Files
}

// foldersObjectID reads the object a live path names, which is the key its
// bytes sit under.
func (i *installation) foldersObjectID(t *testing.T, db *store.DB, path string) object.ID {
	t.Helper()
	var id object.ID
	if err := db.Querier().QueryRow(t.Context(),
		`SELECT object_id FROM files WHERE owner = $1 AND path = $2 AND deleted_at IS NULL`,
		i.filesOwner(), path).Scan(&id); err != nil {
		t.Fatalf("read the object of %q: %v", path, err)
	}
	return id
}

// foldersObjectIDs reads every object a set of paths names, live or trashed,
// with the objects of their versions, which is every key a permanent delete
// of them has to remove.
func (i *installation) foldersObjectIDs(t *testing.T, db *store.DB, paths ...string) []object.ID {
	t.Helper()
	rows, err := db.Querier().Query(t.Context(),
		`SELECT object_id FROM files WHERE owner = $1 AND path = ANY($2)
		 UNION ALL
		 SELECT object_id FROM file_versions WHERE owner = $1 AND path = ANY($2)`,
		i.filesOwner(), paths)
	if err != nil {
		t.Fatalf("read the objects of %v: %v", paths, err)
	}
	defer rows.Close()
	var out []object.ID
	for rows.Next() {
		var id object.ID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read the objects of %v: %v", paths, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the objects of %v: %v", paths, err)
	}
	return out
}

// foldersFetch reads a path the way a client reads an object it may be
// redirected for: the server answers a presigned URL and the bytes come from
// the bucket.
func (i *installation) foldersFetch(t *testing.T, path string) []byte {
	t.Helper()
	resp := i.filesDo(t, http.MethodGet, i.filesObject(path)+"?inline=0", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a read of %q asking for the redirect answered %d", path, resp.StatusCode)
	}
	status, body := fetch(t, resp.Header.Get("Location"))
	if status != http.StatusOK {
		t.Fatalf("the presigned read of %q answered %d: %s", path, status, body)
	}
	return body
}

// foldersWrite puts one object and fails on a write the server did not take.
func (i *installation) foldersWrite(t *testing.T, path, content string) {
	t.Helper()
	resp := i.filesPut(t, path, content)
	body := filesRead(t, resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("the put of %q answered %d: %s", path, resp.StatusCode, body)
	}
}

// foldersCall drives one request on a path of the caller's space, with a
// JSON body when one is given, and answers the status and the body.
func (i *installation) foldersCall(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	var headers []string
	if body != "" {
		reader, headers = strings.NewReader(body), []string{"Content-Type", "application/json"}
	}
	resp := i.filesDo(t, method, i.filesObject(path), reader, headers...)
	return resp.StatusCode, filesRead(t, resp)
}

// TestE2EAFolderIsCopiedInsideTheBucketAndReadAtBothEnds is spec 028's folder
// copy against MinIO: one call, every object under the folder written below
// the destination under a key of its own, the bytes read back at both ends
// through the bucket, and the space charged for what the copy holds. One of
// the objects is assembled from parts, so the store copies an object whose
// label is not a digest of its bytes.
func TestE2EAFolderIsCopiedInsideTheBucketAndReadAtBothEnds(t *testing.T) {
	i := filesInstallation(t)
	small := map[string]string{
		"/notes/plan.md":      "the plan\n",
		"/notes/deeper/a.txt": "two folders down\n",
	}
	for leaf, content := range small {
		i.foldersWrite(t, "files/src"+leaf, content)
	}
	large := strings.Repeat("v", int(e2ePartSize)) + "the tail"
	i.filesUploadInParts(t, "files/src/video/keynote.mp4", large)
	tree := map[string]string{"/video/keynote.mp4": large}
	var total int64
	for leaf, content := range small {
		tree[leaf] = content
	}
	for _, content := range tree {
		total += int64(len(content))
	}
	beforeBytes, beforeFiles := i.foldersUsage(t)

	code, body := i.foldersCall(t, http.MethodPost, "files/src", `{"copy_to":"files/dst","recursive":true}`)
	if code != http.StatusOK {
		t.Fatalf("the folder copy answered %d: %s", code, body)
	}
	var answer struct {
		Path  string `json:"path"`
		Files int    `json:"files"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the folder copy answered %s: %v", body, err)
	}
	if answer.Path != "files/dst" || answer.Files != len(tree) || answer.Bytes != total {
		t.Fatalf("the folder copy answered %+v, want files/dst, %d files and %d bytes", answer, len(tree), total)
	}

	bucket, db := i.bucket(t), i.database(t)
	for leaf, content := range tree {
		source, copied := "files/src"+leaf, "files/dst"+leaf
		for _, path := range []string{source, copied} {
			if got := i.foldersFetch(t, path); !bytes.Equal(got, []byte(content)) {
				t.Errorf("%q fetched %d bytes from the bucket, want the %d the source holds", path, len(got), len(content))
			}
		}
		// Each copy is an object of its own: a key the store wrote for it,
		// beside the source's.
		from, to := i.foldersObjectID(t, db, source), i.foldersObjectID(t, db, copied)
		if from == to {
			t.Errorf("%q and its copy name one object, %s", source, from)
		}
		for _, id := range []object.ID{from, to} {
			if _, err := bucket.Head(t.Context(), id.Key(i.prefix)); err != nil {
				t.Errorf("the key of %s is not in the bucket: %v", id, err)
			}
		}
	}

	// The copy of a digest carries the digest, because the bytes are the
	// same.
	plan := i.filesDo(t, http.MethodGet, i.filesObject("files/dst/notes/plan.md"), nil)
	_ = plan.Body.Close()
	if got := plan.Header.Get("ETag"); got != `"`+filesDigest(small["/notes/plan.md"])+`"` {
		t.Errorf("the copy of a written object answers the ETag %q, want the digest of its bytes", got)
	}

	afterBytes, afterFiles := i.foldersUsage(t)
	if afterBytes-beforeBytes != total || afterFiles-beforeFiles != int64(len(tree)) {
		t.Errorf("the copy moved the usage from %d bytes in %d files to %d in %d, want %d more bytes in %d more files",
			beforeBytes, beforeFiles, afterBytes, afterFiles, total, len(tree))
	}

	// A second copy onto the same destination is refused whole, and the
	// usage does not move.
	code, body = i.foldersCall(t, http.MethodPost, "files/src", `{"copy_to":"files/dst","recursive":true}`)
	if code != http.StatusConflict || errorCode(t, []byte(body)) != "path_taken" {
		t.Fatalf("a copy onto a destination the first copy holds answered %d: %s", code, body)
	}
	if again, _ := i.foldersUsage(t); again != afterBytes {
		t.Errorf("a refused copy moved the usage from %d to %d", afterBytes, again)
	}
}

// TestE2EAFolderDeleteTrashesWithoutChargingAndAPermanentOneGivesTheBytesBack
// is spec 028's folder delete against both stores. A soft delete puts every
// object under the folder in the trash, keeps its bytes in the bucket and
// leaves the usage where it was, because trashed bytes count towards the
// space. A permanent delete removes the rows, the versions and the keys, and
// gives the space back every byte they held. Nothing beside the folder moves.
func TestE2EAFolderDeleteTrashesWithoutChargingAndAPermanentOneGivesTheBytesBack(t *testing.T) {
	i := filesInstallation(t)
	under := map[string]string{
		"files/proj/a.txt":     "the current content of a\n",
		"files/proj/sub/b.txt": "b, one folder down\n",
	}
	beside := map[string]string{
		"files/proj":           "an object at the folder's own path\n",
		"files/proj-old/c.txt": "a sibling whose name begins with the folder's\n",
	}
	const replaced = "the first content of a\n"
	i.foldersWrite(t, "files/proj/a.txt", replaced)
	for path, content := range under {
		i.foldersWrite(t, path, content)
	}
	for path, content := range beside {
		i.foldersWrite(t, path, content)
	}
	held := int64(len(replaced))
	for _, content := range under {
		held += int64(len(content))
	}
	paths := make([]string, 0, len(under))
	for path := range under {
		paths = append(paths, path)
	}
	db := i.database(t)
	keys := i.foldersObjectIDs(t, db, paths...)
	if len(keys) != len(under)+1 {
		t.Fatalf("the folder names %d objects, want its %d paths and the one version", len(keys), len(under))
	}
	bucket := i.bucket(t)
	beforeBytes, beforeFiles := i.foldersUsage(t)

	// Soft: the trash holds the subtree, the bucket keeps every key, and the
	// usage is unchanged.
	if code, body := i.foldersCall(t, http.MethodDelete, "files/proj?recursive=1", ""); code != http.StatusNoContent {
		t.Fatalf("the folder delete answered %d: %s", code, body)
	}
	for path := range under {
		gone := i.filesDo(t, http.MethodGet, i.filesObject(path), nil)
		_ = gone.Body.Close()
		if gone.StatusCode != http.StatusNotFound {
			t.Errorf("%q reads %d after the folder went to the trash", path, gone.StatusCode)
		}
	}
	listed := i.filesDo(t, http.MethodGet, i.publicURL+"/v1/trash", nil)
	var trash struct {
		Entries []struct {
			Path string `json:"path"`
		} `json:"entries"`
	}
	filesJSON(t, listed, &trash)
	trashed := map[string]bool{}
	for _, e := range trash.Entries {
		trashed[e.Path] = true
	}
	for path := range under {
		if !trashed[path] {
			t.Errorf("the trash does not hold %q: %+v", path, trash.Entries)
		}
	}
	for path := range beside {
		if trashed[path] {
			t.Errorf("the folder delete trashed %q, which is beside the folder", path)
		}
	}
	for _, id := range keys {
		if _, err := bucket.Head(t.Context(), id.Key(i.prefix)); err != nil {
			t.Errorf("a soft folder delete removed the key of %s: %v", id, err)
		}
	}
	softBytes, softFiles := i.foldersUsage(t)
	if softBytes != beforeBytes {
		t.Errorf("a soft folder delete moved the usage from %d bytes to %d; trashed bytes count towards the space", beforeBytes, softBytes)
	}
	if softFiles != beforeFiles-int64(len(under)) {
		t.Errorf("a soft folder delete left %d live files of %d, want %d fewer", softFiles, beforeFiles, len(under))
	}

	// Permanent: the rows, the version and the keys go, and the usage is
	// given back every byte they held.
	if code, body := i.foldersCall(t, http.MethodDelete, "files/proj?recursive=1&permanent=1", ""); code != http.StatusNoContent {
		t.Fatalf("the permanent folder delete answered %d: %s", code, body)
	}
	if left := i.foldersObjectIDs(t, db, paths...); len(left) != 0 {
		t.Errorf("a permanent folder delete left %d rows naming an object under the folder", len(left))
	}
	for _, id := range keys {
		if _, err := bucket.Head(t.Context(), id.Key(i.prefix)); err == nil {
			t.Errorf("a permanent folder delete left the key of %s in the bucket", id)
		}
	}
	afterBytes, afterFiles := i.foldersUsage(t)
	if beforeBytes-afterBytes != held {
		t.Errorf("a permanent folder delete gave back %d bytes, want the %d the paths and the version held",
			beforeBytes-afterBytes, held)
	}
	if afterFiles != softFiles {
		t.Errorf("a permanent delete of trashed objects moved the live files from %d to %d", softFiles, afterFiles)
	}
	for path, content := range beside {
		if got := filesRead(t, i.filesDo(t, http.MethodGet, i.filesObject(path), nil)); got != content {
			t.Errorf("%q, beside the folder, reads %q after the folder delete", path, got)
		}
	}

	// The folder holds nothing now, and a delete of it is the answer a
	// delete of a folder that never existed gets.
	for _, form := range []string{"?recursive=1", "?recursive=1&permanent=1"} {
		if code, body := i.foldersCall(t, http.MethodDelete, "files/proj"+form, ""); code != http.StatusNoContent {
			t.Errorf("a delete of an empty folder with %s answered %d: %s", form, code, body)
		}
	}
	if final, _ := i.foldersUsage(t); final != afterBytes {
		t.Errorf("a delete of an empty folder moved the usage from %d to %d", afterBytes, final)
	}
}
