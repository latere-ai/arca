// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"slices"
	"strings"
)

// The conditional request headers of spec 013. Every object read and every
// object write answers an ETag whose value is the object's checksum, and no
// route requires a precondition: the caller whose lost update would be
// silent and expensive opts into the round trip, and every other caller does
// not pay for it.
const (
	HeaderETag        = "ETag"
	HeaderIfMatch     = "If-Match"
	HeaderIfNoneMatch = "If-None-Match"
)

// Wildcard is the value of If-None-Match that means create only: a write
// succeeds when nothing is at the path and is precondition_failed when
// something is.
const Wildcard = "*"

// ETag renders a checksum as the header value. It is a strong validator;
// nothing in this API is weak.
func ETag(checksum string) string {
	if checksum == "" {
		return ""
	}
	return `"` + checksum + `"`
}

// SetETag answers the object's checksum on a read or a write. It is the one
// place a validator reaches the wire, so the drift seam of spec 017 is read
// here and every route that answers an ETag is covered by it.
func SetETag(w http.ResponseWriter, checksum string) {
	if tag := ETag(driftedETag(checksum)); tag != "" {
		w.Header().Set(HeaderETag, tag)
	}
}

// Precondition is what a request asked of the object's current state, read
// off its headers. The zero value asks nothing, which is most requests.
type Precondition struct {
	// IfMatch are the checksums a write is allowed against. A write
	// proceeds when the object still carries one of them.
	IfMatch []string
	// IfNoneMatch are the checksums a read is allowed to skip. A read
	// answers 304 when the object still carries one of them.
	IfNoneMatch []string
	// CreateOnly is If-None-Match: *, a write that must find nothing at the
	// path.
	CreateOnly bool
}

// Conditions reads the preconditions of a request. Quotes are stripped
// before comparison, so a bare checksum is accepted beside the quoted form,
// and a weak validator's prefix is refused rather than silently compared:
// every validator here is strong.
func Conditions(r *http.Request) (Precondition, error) {
	var p Precondition
	if raw := r.Header.Get(HeaderIfNoneMatch); raw != "" {
		if strings.TrimSpace(raw) == Wildcard {
			p.CreateOnly = true
		} else {
			tags, err := validators(raw, HeaderIfNoneMatch)
			if err != nil {
				return Precondition{}, err
			}
			p.IfNoneMatch = tags
		}
	}
	if raw := r.Header.Get(HeaderIfMatch); raw != "" {
		if strings.TrimSpace(raw) == Wildcard {
			// If-Match: * is "anything that exists", which every write that
			// finds an object satisfies and no row of spec 013 names. It is
			// read as no condition rather than refused, since a caller that
			// sent it asked for nothing this server does not already do.
			return p, nil
		}
		tags, err := validators(raw, HeaderIfMatch)
		if err != nil {
			return Precondition{}, err
		}
		p.IfMatch = tags
	}
	return p, nil
}

// Matches reports whether the object's checksum is one the header named.
func Matches(tags []string, checksum string) bool {
	return slices.Contains(tags, checksum)
}

// Fresh reports whether a read may answer 304: the caller named the
// checksum the object still carries.
func (p Precondition) Fresh(checksum string) bool {
	return checksum != "" && Matches(p.IfNoneMatch, checksum)
}

// Allows reports whether a write may proceed against an object whose
// checksum is the one given, with "" for an object that is not there.
//
// If-None-Match refuses a write whose validator matches, which is RFC 9110
// section 13.1.2 and the reason the star form is create only rather than a
// rule of its own: * matches whatever is there, so a write finds nothing or
// is refused. Spec 013's table names the star form on a write and the named
// form on a read, and this is the same rule read on both.
func (p Precondition) Allows(checksum string) bool {
	switch {
	case p.CreateOnly:
		return checksum == ""
	case Matches(p.IfNoneMatch, checksum):
		return false
	case len(p.IfMatch) > 0:
		return Matches(p.IfMatch, checksum)
	}
	return true
}

// validators splits a header into its checksums and strips the quotes. A
// weak validator is invalid_field: this API compares byte for byte, and a
// caller that sent W/ meant something the server cannot honor.
func validators(raw, header string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "W/") {
			return nil, Refuse(CodeInvalidField,
				"%s names the weak validator %s; this server compares checksums byte for byte", header, part).About(header)
		}
		out = append(out, strings.Trim(part, `"`))
	}
	if len(out) == 0 {
		return nil, Refuse(CodeInvalidField, "%s names no validator", header).About(header)
	}
	return out, nil
}
