// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The e2e tier of specs 005 and 007: arcad as a process, against the two
// stores of the stack and the two stubs. Nothing here reaches inside the
// server: what a test sees is what an operator sees, and the parts of an
// upload go to the bucket through the presigned URLs the session answered,
// which is the path no byte of which passes through a replica.
//
// Every helper here carries a files prefix, so the generic helpers the
// harness grows for the other specs keep their own names.
package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/arca/test/stubs/issuer"
)

// filesCaller is the subject every case of this file writes as.
const filesCaller = "e2e-files"

// filesInstallation is one arcad with the stub authorizer allowing every
// question, which is the installation a case of this file drives.
func filesInstallation(t *testing.T) *installation {
	t.Helper()
	i := start(t)
	i.authorizer.Allow(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	return i
}

// filesBearer mints a token for the caller of this file.
func (i *installation) filesBearer(t *testing.T) string {
	t.Helper()
	return i.issuer.Mint(issuer.Claims{Sub: filesCaller})
}

// filesOwner is the space that caller owns, rendered as spec 006 renders a
// subject.
func (i *installation) filesOwner() string { return i.issuer.URL() + "|" + filesCaller }

// filesObject is the URL of one path in that space.
func (i *installation) filesObject(path string) string {
	return i.publicURL + "/v1/files/" + url.PathEscape(i.filesOwner()) + "/" + path
}

// filesDo drives one request against the running server, without following
// a redirect: a redirect is what a read above the inline size answers, and
// following it would hide the answer this tier is about.
func (i *installation) filesDo(t *testing.T, method, target string, body io.Reader, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+i.filesBearer(t))
	for n := 0; n+1 < len(headers); n += 2 {
		req.Header.Set(headers[n], headers[n+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return resp
}

// filesRead reads a response body and closes it.
func filesRead(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// filesJSON decodes a response body.
func filesJSON(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	body := filesRead(t, resp)
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
}

// filesDigest is the checksum a put of this content answers.
func filesDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// filesPut writes one object through the running server.
func (i *installation) filesPut(t *testing.T, path, content string, headers ...string) *http.Response {
	t.Helper()
	return i.filesDo(t, http.MethodPut, i.filesObject(path), strings.NewReader(content),
		append([]string{"Content-Type", "text/plain"}, headers...)...)
}

// filesObjectBody is the body a write and a metadata read answer.
type filesObjectBody struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum"`
	ChecksumKind string `json:"checksum_kind"`
	ContentType  string `json:"content_type"`
}

func TestE2EAPutRoundTripsAndAReadAboveTheBoundaryRedirects(t *testing.T) {
	i := filesInstallation(t)

	resp := i.filesPut(t, "files/notes/plan.md", "the first content")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the put answered %d: %s", resp.StatusCode, filesRead(t, resp))
	}
	var created filesObjectBody
	filesJSON(t, resp, &created)
	if created.Checksum != filesDigest("the first content") || created.ChecksumKind != "sha256" {
		t.Fatalf("the put answered %+v", created)
	}

	read := i.filesDo(t, http.MethodGet, i.filesObject("files/notes/plan.md"), nil)
	if read.StatusCode != http.StatusOK {
		t.Fatalf("the read answered %d", read.StatusCode)
	}
	if got := read.Header.Get("ETag"); got != `"`+created.Checksum+`"` {
		t.Errorf("the read answered the ETag %q", got)
	}
	if body := filesRead(t, read); body != "the first content" {
		t.Fatalf("the read answered %q", body)
	}

	head := i.filesDo(t, http.MethodHead, i.filesObject("files/notes/plan.md"), nil)
	if head.StatusCode != http.StatusOK || head.Header.Get("Content-Length") != "17" {
		t.Fatalf("the head answered %d with the length %q", head.StatusCode, head.Header.Get("Content-Length"))
	}
	_ = head.Body.Close()

	// A read of an object above ARCA_INLINE_BYTES is a redirect to a
	// presigned URL, and the bytes come from the bucket rather than through
	// a replica. The object is put through a session, because a put above
	// the boundary is refused.
	large := strings.Repeat("x", int(e2ePartSize)) + "tail"
	i.filesUploadInParts(t, "files/video/keynote.mp4", large)
	redirect := i.filesDo(t, http.MethodGet, i.filesObject("files/video/keynote.mp4"), nil)
	_ = redirect.Body.Close()
	if redirect.StatusCode != http.StatusFound {
		t.Fatalf("a read above the boundary answered %d", redirect.StatusCode)
	}
	signed := redirect.Header.Get("Location")
	if signed == "" || !strings.Contains(signed, "X-Amz-") {
		t.Fatalf("the redirect points at %q", signed)
	}
	fetched, err := http.Get(signed) //nolint:noctx,gosec // the URL is the server's own answer
	if err != nil {
		t.Fatalf("follow the redirect: %v", err)
	}
	bytes := filesRead(t, fetched)
	if fetched.StatusCode != http.StatusOK || len(bytes) != len(large) {
		t.Fatalf("the presigned read answered %d with %d bytes", fetched.StatusCode, len(bytes))
	}

	if w := i.filesDo(t, http.MethodDelete, i.filesObject("files/notes/plan.md"), nil); w.StatusCode != http.StatusNoContent {
		t.Fatalf("the delete answered %d: %s", w.StatusCode, filesRead(t, w))
	}
	gone := i.filesDo(t, http.MethodGet, i.filesObject("files/notes/plan.md"), nil)
	_ = gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Fatalf("a trashed path reads %d", gone.StatusCode)
	}
}

func TestE2EATrashedObjectIsRestorableAndAVersionRoundTrips(t *testing.T) {
	i := filesInstallation(t)
	for _, content := range []string{"one", "two"} {
		if resp := i.filesPut(t, "files/plan.md", content); resp.StatusCode >= 400 {
			t.Fatalf("the put answered %d: %s", resp.StatusCode, filesRead(t, resp))
		} else {
			_ = resp.Body.Close()
		}
	}

	// Two writes leave one version, and a restore brings its bytes forward.
	versions := i.filesDo(t, http.MethodGet, i.filesObject("files/plan.md")+"?versions=1", nil)
	var page struct {
		Entries []struct {
			VersionNo int    `json:"version_no"`
			Checksum  string `json:"checksum"`
		} `json:"entries"`
	}
	filesJSON(t, versions, &page)
	if len(page.Entries) != 1 || page.Entries[0].Checksum != filesDigest("one") {
		t.Fatalf("the history is %+v", page.Entries)
	}
	restored := i.filesDo(t, http.MethodPost, i.filesObject("files/plan.md"),
		strings.NewReader(`{"restore_version":1}`), "Content-Type", "application/json")
	if restored.StatusCode != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", restored.StatusCode, filesRead(t, restored))
	}
	_ = restored.Body.Close()
	read := i.filesDo(t, http.MethodGet, i.filesObject("files/plan.md"), nil)
	if body := filesRead(t, read); body != "one" {
		t.Fatalf("the restored object reads %q", body)
	}

	// A trashed object is restorable for the retention window and the bytes
	// come back with it.
	del := i.filesDo(t, http.MethodDelete, i.filesObject("files/plan.md"), nil)
	_ = del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("the trash answered %d", del.StatusCode)
	}
	listed := i.filesDo(t, http.MethodGet, i.publicURL+"/v1/trash", nil)
	var trash struct {
		Entries []struct {
			Path     string `json:"path"`
			PurgesAt string `json:"purges_at"`
		} `json:"entries"`
	}
	filesJSON(t, listed, &trash)
	if len(trash.Entries) != 1 || trash.Entries[0].Path != "files/plan.md" || trash.Entries[0].PurgesAt == "" {
		t.Fatalf("the trash is %+v", trash.Entries)
	}
	back := i.filesDo(t, http.MethodPost, i.publicURL+"/v1/trash/restore",
		strings.NewReader(`{"owner":"me","path":"files/plan.md"}`), "Content-Type", "application/json")
	if back.StatusCode != http.StatusOK {
		t.Fatalf("the restore answered %d: %s", back.StatusCode, filesRead(t, back))
	}
	_ = back.Body.Close()
	read = i.filesDo(t, http.MethodGet, i.filesObject("files/plan.md"), nil)
	if body := filesRead(t, read); body != "one" {
		t.Fatalf("the restored object reads %q", body)
	}
}

// e2ePartSize is the part size spec 007 fixes, which every part but the last
// of a multipart upload has to be.
const e2ePartSize = 16 << 20

// filesUploadInParts writes one object through a session: the create, the
// parts sent straight to the bucket, and the completion.
func (i *installation) filesUploadInParts(t *testing.T, path, content string) filesObjectBody {
	t.Helper()
	body := fmt.Sprintf(`{"owner":"me","path":%q,"size":%d,"content_type":"application/octet-stream"}`,
		path, len(content))
	resp := i.filesDo(t, http.MethodPost, i.publicURL+"/v1/uploads", strings.NewReader(body),
		"Content-Type", "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("opening a session answered %d: %s", resp.StatusCode, filesRead(t, resp))
	}
	var session struct {
		ID        string   `json:"id"`
		PartSize  int64    `json:"part_size"`
		PartCount int64    `json:"part_count"`
		PartURLs  []string `json:"part_urls"`
		ExpiresAt string   `json:"expires_at"`
	}
	filesJSON(t, resp, &session)
	if session.PartSize != e2ePartSize || int(session.PartCount) != len(session.PartURLs) {
		t.Fatalf("the session is %+v", session)
	}

	var parts []string
	for n, signed := range session.PartURLs {
		from := n * int(e2ePartSize)
		to := min(from+int(e2ePartSize), len(content))
		parts = append(parts, fmt.Sprintf(`{"n":%d,"etag":%q}`, n+1,
			filesSendPart(t, signed, content[from:to])))
	}
	completion := i.filesDo(t, http.MethodPost,
		i.publicURL+"/v1/uploads/"+session.ID+"/complete",
		strings.NewReader(`{"parts":[`+strings.Join(parts, ",")+`]}`),
		"Content-Type", "application/json")
	if completion.StatusCode != http.StatusCreated && completion.StatusCode != http.StatusOK {
		t.Fatalf("the completion answered %d: %s", completion.StatusCode, filesRead(t, completion))
	}
	var out filesObjectBody
	filesJSON(t, completion, &out)
	return out
}

// filesSendPart puts one part where the session's URL points, which is what
// a client does and what keeps the bytes off the server.
func filesSendPart(t *testing.T, signed, content string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, signed, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send the part: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("the store answered %d to a part: %s", resp.StatusCode, out)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`)
}

func TestE2EAnUploadInPartsLandsTheObjectAndAPutAboveTheBoundaryIsRefused(t *testing.T) {
	i := filesInstallation(t)
	content := strings.Repeat("a", int(e2ePartSize)) + "the tail"
	out := i.filesUploadInParts(t, "files/video/keynote.mp4", content)
	if out.Size != int64(len(content)) {
		t.Fatalf("the completion answered %+v", out)
	}
	// The object was assembled from parts the server never saw, so its
	// checksum is the store's composite label and its kind says so.
	if out.ChecksumKind != "etag" || out.Checksum == "" {
		t.Fatalf("the completion answered the checksum %q of kind %q", out.Checksum, out.ChecksumKind)
	}

	head := i.filesDo(t, http.MethodHead, i.filesObject("files/video/keynote.mp4"), nil)
	_ = head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("the head answered %d", head.StatusCode)
	}
	if got := head.Header.Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Fatalf("the head answered the length %q", got)
	}

	// A put of the same size is refused, and the refusal names the session
	// API rather than leaving a caller to guess.
	refused := i.filesPut(t, "files/video/again.mp4", content)
	body := filesRead(t, refused)
	if refused.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a put above the boundary answered %d: %s", refused.StatusCode, body)
	}
	if !strings.Contains(body, "/v1/uploads") || !strings.Contains(body, "object_too_large") {
		t.Fatalf("the refusal reads %s", body)
	}
}

func TestE2EAnObjectIsReachedByTheSubjectAResponseRenders(t *testing.T) {
	i := filesInstallation(t)
	resp := i.filesPut(t, "files/plan.md", "content")
	_ = resp.Body.Close()

	// A star names the space in full, and the subject read back addresses
	// the same space, which is criterion 5 of spec 013.
	star := i.filesDo(t, http.MethodPut, i.publicURL+"/v1/stars",
		strings.NewReader(`{"owner":"me","path":"files/plan.md"}`), "Content-Type", "application/json")
	_ = star.Body.Close()
	if star.StatusCode != http.StatusNoContent {
		t.Fatalf("the star answered %d", star.StatusCode)
	}
	listed := i.filesDo(t, http.MethodGet, i.publicURL+"/v1/stars", nil)
	var page struct {
		Entries []struct {
			Owner string `json:"owner"`
			Path  string `json:"path"`
		} `json:"entries"`
	}
	filesJSON(t, listed, &page)
	if len(page.Entries) != 1 || page.Entries[0].Owner != i.filesOwner() {
		t.Fatalf("the stars are %+v", page.Entries)
	}
	again := i.filesDo(t, http.MethodGet,
		i.publicURL+"/v1/files/"+url.PathEscape(page.Entries[0].Owner)+"/files/plan.md", nil)
	if body := filesRead(t, again); body != "content" {
		t.Fatalf("the subject read back reads %q", body)
	}
}
