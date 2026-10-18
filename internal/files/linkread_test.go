// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/api"
	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// writeAs puts one object in both stores under the type a writer stated,
// for a case whose subject is what a read does with that type.
func writeAs(t *testing.T, h *harness, path, content, media string) store.File {
	t.Helper()
	id := object.NewID()
	if _, err := h.objects.Put(t.Context(), id.Key("arca/"), strings.NewReader(content),
		int64(len(content)), blob.PutOptions{ContentType: media}); err != nil {
		t.Fatalf("seeding the bytes of %q: %v", path, err)
	}
	if err := h.store.Upsert(t.Context(), nil, store.File{
		Owner: h.owner, Path: path, ObjectID: id, CreatedBy: h.owner,
		ContentType: media, SizeBytes: int64(len(content)),
		Checksum: digest(content), ChecksumKind: object.ChecksumSHA256,
	}); err != nil {
		t.Fatalf("seeding the row of %q: %v", path, err)
	}
	row, err := h.store.Get(t.Context(), nil, h.owner, path)
	if err != nil {
		t.Fatalf("seeding %q left no row: %v", path, err)
	}
	return row
}

// public marks a row the way a public grant marks it (spec 008).
func public(t *testing.T, h *harness, row store.File) {
	t.Helper()
	row.IsPublic = true
	h.store.files[key(h.owner, row.Path)] = row
}

// throughLink drives ServeObject the way the third link route of spec 008
// does, with no bearer, and answers what it wrote. query is the request's
// own query string, which a link holder controls.
func throughLink(t *testing.T, h *harness, path, query string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/v1/shares/links/the-token/files/" + url.PathEscape(path)
	if query != "" {
		target += "?" + query
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	if err := h.service.ServeObject(w, r, h.owner, path); err != nil {
		api.WriteError(w, r, err)
	}
	return w
}

// askedDisposition is the disposition a presigned redirect asks the bucket
// for, read off the URL the memory store signs.
func askedDisposition(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("the read answered %d, not a redirect: %s", w.Code, w.Body)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("the redirect is to %q: %v", w.Header().Get("Location"), err)
	}
	return u.Query().Get("response-content-disposition")
}

// TestALinkReadIsAnAttachmentUnderItsName is criterion 2 of spec 031: a page
// a link serves is saved under its own name, keeps the type it was written
// with, and is never sniffed, whatever ?download= says.
func TestALinkReadIsAnAttachmentUnderItsName(t *testing.T) {
	h := newHarness(t)
	// The page is at or below the inline size, so the read streams it.
	const page = "<script>alert(1)</script>"
	path := "files/reports/季度 report.html"
	writeAs(t, h, path, page, "text/html")
	want := api.Attachment("季度 report.html")

	for _, query := range []string{"", "download=0", "download=1", "inline=1"} {
		w := throughLink(t, h, path, query)
		if w.Code != http.StatusOK || w.Body.String() != page {
			t.Fatalf("?%s answered %d: %q", query, w.Code, w.Body)
		}
		if got := w.Header().Get(api.HeaderContentDisposition); got != want {
			t.Errorf("?%s answered the disposition %q, want %q", query, got, want)
		}
		if got := w.Header().Get(api.HeaderContentType); got != "text/html" {
			t.Errorf("?%s answered the type %q; a link read keeps the type the object was written with", query, got)
		}
		if got := w.Header().Get(api.HeaderContentTypeOptions); got != api.NoSniff {
			t.Errorf("?%s answered X-Content-Type-Options %q", query, got)
		}
	}
}

// TestALinkRevalidationKeepsTheName is criterion 2 of spec 031: the 304 a
// link read answers carries the disposition the 200 carried, so a cache that
// refreshes its stored headers from it keeps the file's name.
func TestALinkRevalidationKeepsTheName(t *testing.T) {
	h := newHarness(t)
	row := writeAs(t, h, "files/reports/q3.html", "<p>q3</p>", "text/html")
	w := throughLink(t, h, row.Path, "", api.HeaderIfNoneMatch, `"`+row.Checksum+`"`)
	if w.Code != http.StatusNotModified {
		t.Fatalf("a fresh link read answered %d", w.Code)
	}
	if got, want := w.Header().Get(api.HeaderContentDisposition), api.Attachment("q3.html"); got != want {
		t.Errorf("the 304 answered the disposition %q, want %q", got, want)
	}
}

// TestALinkReadThatRedirectsAsksTheBucketForTheAttachment is criterion 4 of
// spec 031: the headers of a 302 do not travel with the browser, so the
// signed URL asks the bucket for the disposition the streamed read carries,
// above the inline size and wherever the caller asks for the redirect.
func TestALinkReadThatRedirectsAsksTheBucketForTheAttachment(t *testing.T) {
	h := newHarness(t)
	large := writeAs(t, h, "files/reports/résumé.html", strings.Repeat("l", inlineBytes+1), "text/html")
	small := writeAs(t, h, "files/reports/small.html", "<p>small</p>", "text/html")

	for _, c := range []struct {
		row   store.File
		query string
		name  string
	}{
		{large, "", "résumé.html"},
		{large, "download=0", "résumé.html"},
		{small, "inline=0", "small.html"},
	} {
		w := throughLink(t, h, c.row.Path, c.query)
		if got, want := askedDisposition(t, w), api.Attachment(c.name); got != want {
			t.Errorf("a link read of %q with ?%s asks the bucket for %q, want %q", c.row.Path, c.query, got, want)
		}
	}
}

// TestALinkReadOfAPublicObjectIsPresignedAndNotSentToTheCDN is criterion 4
// of spec 031: the CDN's URL is unsigned and cannot ask for a disposition, so
// a link read of a public object is presigned with one. The owner's own read
// of the object still goes to the CDN, and the owner's download does not.
func TestALinkReadOfAPublicObjectIsPresignedAndNotSentToTheCDN(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		cfg := configOf(inlineBytes, 1<<20)
		cfg.PublicCDNURL = "https://cdn.example"
		o.Config = cfg
	})
	row := writeAs(t, h, "files/public/page.html", "<p>public</p>", "text/html")
	public(t, h, row)
	cdn := "https://cdn.example/" + row.ObjectID.Key("arca/")

	w := throughLink(t, h, row.Path, "")
	if strings.HasPrefix(w.Header().Get("Location"), "https://cdn.example") {
		t.Fatalf("a link read of a public object was sent to the CDN: %q", w.Header().Get("Location"))
	}
	if got, want := askedDisposition(t, w), api.Attachment("page.html"); got != want {
		t.Errorf("a link read of a public object asks the bucket for %q, want %q", got, want)
	}

	owner := h.call(t, http.MethodGet, h.object(row.Path), nil)
	if owner.Code != http.StatusFound || owner.Header().Get("Location") != cdn {
		t.Errorf("the owner's read of a public object answered %d to %q, want the CDN", owner.Code, owner.Header().Get("Location"))
	}
	download := h.call(t, http.MethodGet, h.object(row.Path)+"?download=1", nil)
	if got, want := askedDisposition(t, download), api.Attachment("page.html"); got != want {
		t.Errorf("the owner's download of a public object asks the bucket for %q, want %q", got, want)
	}
}

// TestAnOwnersReadIsInlineAndNeverSniffed is criterion 6 of spec 031: the
// owner's own read keeps the object inline, which a preview needs, holds a
// browser to its type, and is an attachment when the owner asks for one, at
// every size.
func TestAnOwnersReadIsInlineAndNeverSniffed(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "the plan")
	h.write(t, "files/large.md", strings.Repeat("l", inlineBytes+1))

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := h.call(t, method, h.object("files/plan.md"), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", method, w.Code, w.Body)
		}
		if got := w.Header().Get(api.HeaderContentTypeOptions); got != api.NoSniff {
			t.Errorf("%s answered X-Content-Type-Options %q", method, got)
		}
		for _, header := range []string{api.HeaderContentDisposition, api.HeaderContentSecurityPolicy} {
			if got := w.Header().Get(header); got != "" {
				t.Errorf("the owner's %s answered %s %q; it is inline unless the owner asks", method, header, got)
			}
		}
	}

	streamed := h.call(t, http.MethodGet, h.object("files/plan.md")+"?download=1", nil)
	if streamed.Code != http.StatusOK {
		t.Fatalf("?download=1 at the inline size answered %d", streamed.Code)
	}
	if got, want := streamed.Header().Get(api.HeaderContentDisposition), api.Attachment("plan.md"); got != want {
		t.Errorf("?download=1 at the inline size answered the disposition %q, want %q", got, want)
	}
	redirected := h.call(t, http.MethodGet, h.object("files/large.md")+"?download=1", nil)
	if got, want := askedDisposition(t, redirected), api.Attachment("large.md"); got != want {
		t.Errorf("?download=1 above the inline size asks the bucket for %q, want %q", got, want)
	}
	plain := h.call(t, http.MethodGet, h.object("files/large.md"), nil)
	if got := askedDisposition(t, plain); got != "" {
		t.Errorf("the owner's read above the inline size asks the bucket for %q; it is inline unless the owner asks", got)
	}
}

// TestAVersionDownloadIsAnAttachment: a version is read by the same size
// rule as the current object, so ?download=1 names it at every size too.
func TestAVersionDownloadIsAnAttachment(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "files/plan.md", "first")
	h.seed(t, "files/plan.md", "second")
	w := h.call(t, http.MethodGet, h.object("files/plan.md")+"?version=1&download=1", nil)
	if w.Code != http.StatusOK || w.Body.String() != "first" {
		t.Fatalf("the first version answered %d: %q", w.Code, w.Body)
	}
	if got, want := w.Header().Get(api.HeaderContentDisposition), api.Attachment("plan.md"); got != want {
		t.Errorf("a version download answered the disposition %q, want %q", got, want)
	}
	if got := code(t, h.call(t, http.MethodGet, h.object("files/plan.md")+"?version=1&download=maybe", nil)); got != api.CodeInvalidField {
		t.Errorf("?download=maybe on a version is %q", got)
	}
}
