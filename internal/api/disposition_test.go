// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"mime"
	"net/http"
	"strings"
	"testing"
	"unicode"
)

// downloadOnly reports what is missing from an answer that spec 031 says is
// download-only, or "" when nothing is.
func downloadOnly(h http.Header) string {
	var missing []string
	if got := h.Get(HeaderContentDisposition); got != DispositionAttachment && !strings.HasPrefix(got, DispositionAttachment+";") {
		missing = append(missing, "Content-Disposition is "+got)
	}
	if got := h.Get(HeaderContentTypeOptions); got != NoSniff {
		missing = append(missing, "X-Content-Type-Options is "+got)
	}
	if got := h.Get(HeaderContentSecurityPolicy); got != PolicySandbox {
		missing = append(missing, "Content-Security-Policy is "+got)
	}
	return strings.Join(missing, "; ")
}

// TestEveryAnswerOfTheLinkRowsIsDownloadOnly is criterion 1 of spec 031 at
// the frame: the three rows carry the headers whether the bound service
// answers, and whether a build binds a service at all.
func TestEveryAnswerOfTheLinkRowsIsDownloadOnly(t *testing.T) {
	for name, opt := range map[string]func(*Options){
		"a bound service": func(*Options) {},
		"no service":      func(o *Options) { o.Links = nil },
	} {
		h := newHarness(t, opt)
		for _, r := range routeTable {
			if !r.public {
				continue
			}
			w := h.do(t, r.method, fill(r.path), "")
			if missing := downloadOnly(w.Header()); missing != "" {
				t.Errorf("with %s, %s %s answered %d with %s", name, r.method, r.path, w.Code, missing)
			}
		}
	}
}

// TestARouteBehindTheVerifierIsNotMadeADownload: the rule is the three link
// rows', and the rest of the surface answers JSON as it always did.
func TestARouteBehindTheVerifierIsNotMadeADownload(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, http.MethodGet, "/v1/events?owner=me", h.bearer())
	if got := w.Header().Get(HeaderContentDisposition); got != "" {
		t.Errorf("the event tail answered Content-Disposition %q", got)
	}
}

// TestAnAttachmentCarriesTheNameExactlyAndNothingElse is criterion 3 of spec
// 031: a name of any script reaches a browser as itself through filename*,
// the ASCII fallback holds nothing a reader must unescape, and what a name
// must not carry into a header or into a person's reading of it is gone.
func TestAnAttachmentCarriesTheNameExactlyAndNothingElse(t *testing.T) {
	cases := []struct {
		name, plain, decoded string
	}{
		{"q3.pdf", "q3.pdf", "q3.pdf"},
		{"q3 report.pdf", "q3 report.pdf", "q3 report.pdf"},
		{"résumé.pdf", "r_sum_.pdf", "résumé.pdf"},
		{"季度报告.xlsx", "____.xlsx", "季度报告.xlsx"},
		{"تقرير.txt", "_____.txt", "تقرير.txt"},
		{`say "hi".txt`, "say _hi_.txt", `say "hi".txt`},
		{`back\slash.txt`, "back_slash.txt", `back\slash.txt`},
		{"100%25.txt", "100_25.txt", "100%25.txt"},
		{"a;b=c,d.txt", "a;b=c,d.txt", "a;b=c,d.txt"},
		// A name cannot end the header or add one: CR, LF and NUL are gone.
		{"evil.txt\r\nSet-Cookie: a=b", "evil.txtSet-Cookie: a=b", "evil.txtSet-Cookie: a=b"},
		{"nul\x00.txt", "nul.txt", "nul.txt"},
		{"tab\tname.txt", "tabname.txt", "tabname.txt"},
		{"next\u0085line.txt", "nextline.txt", "nextline.txt"},
		// A right-to-left override would make invoice\u202Efdp.exe read as a
		// PDF; it is removed, and so are the other format characters and the
		// separators.
		{"invoice\u202Efdp.exe", "invoicefdp.exe", "invoicefdp.exe"},
		{"zero\u200Bwidth\u2028line\u2029.txt", "zerowidthline.txt", "zerowidthline.txt"},
		{"bad\xffutf8.txt", "badutf8.txt", "badutf8.txt"},
		{"  padded.txt  ", "padded.txt", "padded.txt"},
		// Nothing to save under is the fallback.
		{"", FallbackFilename, FallbackFilename},
		{"..", FallbackFilename, FallbackFilename},
		{" . . ", FallbackFilename, FallbackFilename},
		{"\u202E\r\n", FallbackFilename, FallbackFilename},
		{strings.Repeat("a", MaxFilenameBytes+1), FallbackFilename, FallbackFilename},
	}
	for _, c := range cases {
		header := Attachment(c.name)
		kind, params, err := mime.ParseMediaType(header)
		if err != nil {
			t.Errorf("Attachment(%q) = %q, which does not parse: %v", c.name, header, err)
			continue
		}
		if kind != DispositionAttachment {
			t.Errorf("Attachment(%q) is a %q", c.name, kind)
		}
		// ParseMediaType answers the decoded filename* in the place of
		// filename when both are present, as RFC 6266 has a reader do.
		if params["filename"] != c.decoded {
			t.Errorf("Attachment(%q) decodes to %q, want %q", c.name, params["filename"], c.decoded)
		}
		if plain := quoted(t, header); plain != c.plain {
			t.Errorf("Attachment(%q) falls back to %q, want %q", c.name, plain, c.plain)
		}
		if strings.ContainsAny(header, "\r\n\x00") {
			t.Errorf("Attachment(%q) = %q carries a line break or a NUL", c.name, header)
		}
	}
	if got := Attachment(strings.Repeat("a", MaxFilenameBytes)); !strings.Contains(got, `"`+strings.Repeat("a", MaxFilenameBytes)+`"`) {
		t.Errorf("a name of exactly %d bytes is not kept: %q", MaxFilenameBytes, got)
	}
}

// quoted reads the filename parameter of a disposition as written, which
// mime.ParseMediaType hides behind filename* when both are present.
func quoted(t *testing.T, header string) string {
	t.Helper()
	_, rest, ok := strings.Cut(header, `filename="`)
	if !ok {
		t.Fatalf("%q has no filename parameter", header)
	}
	plain, _, ok := strings.Cut(rest, `"`)
	if !ok {
		t.Fatalf("%q does not close its filename parameter", header)
	}
	return plain
}

// FuzzAttachment is criterion 3 of spec 031 over arbitrary names: whatever a
// path holds, the header parses, decodes to the cleaned name, and carries no
// byte that could end it or confuse a reader of the name.
func FuzzAttachment(f *testing.F) {
	for _, seed := range []string{"q3.pdf", "季度报告.xlsx", "a\r\nb", `"\%`, "\u202E", "", "\xff\xfe"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		header := Attachment(name)
		_, params, err := mime.ParseMediaType(header)
		if err != nil {
			t.Fatalf("Attachment(%q) = %q does not parse: %v", name, header, err)
		}
		if want := filename(name); params["filename"] != want {
			t.Fatalf("Attachment(%q) decodes to %q, want %q", name, params["filename"], want)
		}
		for _, r := range params["filename"] {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
				t.Fatalf("Attachment(%q) keeps %U", name, r)
			}
		}
		for i := range len(header) {
			if c := header[i]; c < 0x20 || c > 0x7e {
				t.Fatalf("Attachment(%q) = %q carries the byte %#x", name, header, c)
			}
		}
	})
}
