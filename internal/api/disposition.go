// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"strings"
	"unicode"
)

// The headers that keep a read from being rendered (spec 031). A link token
// is a URL anyone may open in a browser, so every answer of the three link
// routes is saved rather than shown, read only as the type it states, and
// sandboxed should a browser render it anyway.
const (
	HeaderContentDisposition    = "Content-Disposition"
	HeaderContentTypeOptions    = "X-Content-Type-Options"
	HeaderContentSecurityPolicy = "Content-Security-Policy"

	// DispositionAttachment is the disposition with no name, which the
	// answers that are not one file carry.
	DispositionAttachment = "attachment"
	// NoSniff holds a browser to the Content-Type the answer states.
	NoSniff = "nosniff"
	// PolicySandbox is the most restrictive sandbox: a document made of the
	// answer runs no script, submits no form, and is of a unique opaque
	// origin rather than the server's.
	PolicySandbox = "sandbox"
)

// MaxFilenameBytes is the longest name a download is saved under, the longest
// file name the common file systems take. A longer name is saved as
// FallbackFilename rather than cut, because a cut name loses its extension.
const MaxFilenameBytes = 255

// FallbackFilename is what a download is saved as when its name has nothing
// left to save under once it is cleaned.
const FallbackFilename = "download"

// DownloadOnly sets the three headers of spec 031 on an answer that has not
// been written yet: an attachment with no name, nosniff, and the sandbox. A
// handler that serves one file replaces the disposition with Attachment's.
func DownloadOnly(w http.ResponseWriter) {
	h := w.Header()
	h.Set(HeaderContentDisposition, DispositionAttachment)
	h.Set(HeaderContentTypeOptions, NoSniff)
	h.Set(HeaderContentSecurityPolicy, PolicySandbox)
}

// Attachment is the Content-Disposition of a download saved as name, in the
// two forms RFC 6266 section 4.3 describes:
//
//   - filename*, the cleaned name in UTF-8, percent-encoded per RFC 8187
//     (formerly RFC 5987), which a browser that reads it prefers;
//   - filename, a quoted string for a reader that takes no other form, in
//     printable ASCII with every other character, and every quote, backslash
//     and percent sign, written as an underscore. RFC 6266 appendix D advises
//     against the backslash, which some readers do not unescape, and the
//     percent sign, which some decode, so the string holds nothing to escape.
//
// Neither form can end the parameter or the header: filename holds no quote
// and no control character, and filename* holds only attr-char and %XX.
func Attachment(name string) string {
	name = filename(name)
	var plain, extended strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '%' {
			plain.WriteByte('_')
		} else {
			plain.WriteRune(r)
		}
	}
	for i := range len(name) {
		c := name[i]
		if attrChar(c) {
			extended.WriteByte(c)
		} else {
			extended.WriteByte('%')
			extended.WriteByte(upperHex[c>>4])
			extended.WriteByte(upperHex[c&0x0f])
		}
	}
	return DispositionAttachment + `; filename="` + plain.String() + `"; filename*=UTF-8''` + extended.String()
}

// filename is the name a download is saved under, cleaned of what a name may
// hold and a header or a person reading the name must not: invalid UTF-8,
// every control character (CR, LF and NUL among them), every format
// character, such as U+202E, which reverses how the rest of a name reads and
// can hide its real extension, and every line or paragraph separator. Space
// around the name is trimmed. A name with nothing left but dots and spaces,
// or longer than MaxFilenameBytes, is FallbackFilename.
func filename(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	name = strings.TrimSpace(name)
	if strings.Trim(name, ". ") == "" || len(name) > MaxFilenameBytes {
		return FallbackFilename
	}
	return name
}

// upperHex is the alphabet of a percent-encoded byte, upper case as RFC 3986
// section 2.1 recommends.
const upperHex = "0123456789ABCDEF"

// attrChar reports whether RFC 8187 lets an extended parameter's value carry
// the byte as itself; every other byte is percent-encoded.
func attrChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	default:
		return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
	}
}
