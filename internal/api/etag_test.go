// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestETagIsTheChecksumQuoted: the value of the header is the object's
// checksum, strongly quoted. Nothing here is a weak validator.
func TestETagIsTheChecksumQuoted(t *testing.T) {
	if got := ETag("9f2c"); got != `"9f2c"` {
		t.Errorf("ETag(%q) = %q", "9f2c", got)
	}
	if got := ETag(""); got != "" {
		t.Errorf("an object with no checksum answers the tag %q", got)
	}
	w := httptest.NewRecorder()
	SetETag(w, "9f2c")
	if got := w.Header().Get(HeaderETag); got != `"9f2c"` {
		t.Errorf("the header is %q", got)
	}
	w = httptest.NewRecorder()
	SetETag(w, "")
	if w.Header().Get(HeaderETag) != "" {
		t.Error("an object with no checksum answered an ETag")
	}
}

// TestConditionalRequests is criterion 9 of spec 013: If-None-Match on a
// read is a 304 when the object still has that checksum, If-None-Match: * on
// a write is create only, a stale If-Match is refused, and a request with
// neither succeeds.
func TestConditionalRequests(t *testing.T) {
	cases := []struct {
		name     string
		headers  map[string]string
		checksum string
		fresh    bool
		allows   bool
	}{
		{name: "no precondition at all", checksum: "9f2c", allows: true},
		{name: "no precondition, nothing at the path", allows: true},
		{
			name:    "If-None-Match naming the checksum the object has",
			headers: map[string]string{HeaderIfNoneMatch: `"9f2c"`}, checksum: "9f2c", fresh: true,
		},
		{
			name:    "If-None-Match naming a checksum the object had",
			headers: map[string]string{HeaderIfNoneMatch: `"4d81"`}, checksum: "9f2c", allows: true,
		},
		{
			name:    "If-None-Match unquoted, which is accepted beside the quoted form",
			headers: map[string]string{HeaderIfNoneMatch: "9f2c"}, checksum: "9f2c", fresh: true,
		},
		{
			name:    "If-None-Match naming several, one of which matches",
			headers: map[string]string{HeaderIfNoneMatch: `"4d81", "9f2c"`}, checksum: "9f2c", fresh: true,
		},
		{
			name:    "If-None-Match star with nothing at the path",
			headers: map[string]string{HeaderIfNoneMatch: Wildcard}, allows: true,
		},
		{
			name:    "If-None-Match star with something at the path",
			headers: map[string]string{HeaderIfNoneMatch: Wildcard}, checksum: "9f2c",
		},
		{
			name:    "If-Match naming the checksum the object has",
			headers: map[string]string{HeaderIfMatch: `"9f2c"`}, checksum: "9f2c", allows: true,
		},
		{
			name:    "If-Match naming a checksum the object no longer has",
			headers: map[string]string{HeaderIfMatch: `"4d81"`}, checksum: "9f2c",
		},
		{
			name:    "If-Match against nothing at the path",
			headers: map[string]string{HeaderIfMatch: `"9f2c"`},
		},
		{
			name:    "If-Match star, which asks for anything that exists",
			headers: map[string]string{HeaderIfMatch: Wildcard}, checksum: "9f2c", allows: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/v1/files/me/files/x", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			p, err := Conditions(r)
			if err != nil {
				t.Fatalf("the preconditions were refused: %v", err)
			}
			if got := p.Fresh(c.checksum); got != c.fresh {
				t.Errorf("Fresh(%q) = %v, want %v", c.checksum, got, c.fresh)
			}
			if got := p.Allows(c.checksum); got != c.allows {
				t.Errorf("Allows(%q) = %v, want %v", c.checksum, got, c.allows)
			}
		})
	}
}

// TestAWeakValidatorIsRefused: this API compares checksums byte for byte, so
// a caller that sent a weak validator asked for something the server cannot
// honour, and is told rather than quietly given a strong comparison.
func TestAWeakValidatorIsRefused(t *testing.T) {
	for _, header := range []string{HeaderIfMatch, HeaderIfNoneMatch} {
		t.Run(header, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/v1/files/me/files/x", nil)
			r.Header.Set(header, `W/"9f2c"`)
			_, err := Conditions(r)
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("a weak validator was accepted: %v", err)
			}
			if refusal.Code != CodeInvalidField {
				t.Errorf("the code is %q", refusal.Code)
			}
			if len(refusal.Fields) != 1 || refusal.Fields[0] != header {
				t.Errorf("the refusal names the fields %v", refusal.Fields)
			}
		})
	}
}

// TestAHeaderWithNoValidatorIsRefused: a header a caller sent and left empty
// is a request nobody can answer, not a request with no precondition.
func TestAHeaderWithNoValidatorIsRefused(t *testing.T) {
	for _, header := range []string{HeaderIfMatch, HeaderIfNoneMatch} {
		r := httptest.NewRequest(http.MethodPut, "/v1/files/me/files/x", nil)
		r.Header.Set(header, " , ")
		if _, err := Conditions(r); err == nil {
			t.Errorf("%s naming no validator was accepted", header)
		}
	}
}

// TestNoRouteRequiresAPrecondition: a request with neither header asks
// nothing of the object's state, so a caller that does not need the round
// trip does not pay for it.
func TestNoRouteRequiresAPrecondition(t *testing.T) {
	p, err := Conditions(httptest.NewRequest(http.MethodPut, "/v1/files/me/files/x", nil))
	if err != nil {
		t.Fatalf("a request with no precondition was refused: %v", err)
	}
	if p.CreateOnly || len(p.IfMatch) != 0 || len(p.IfNoneMatch) != 0 {
		t.Errorf("a request with no precondition read as %+v", p)
	}
	if !p.Allows("9f2c") || !p.Allows("") {
		t.Error("a request with no precondition was not allowed to write")
	}
	if p.Fresh("9f2c") {
		t.Error("a request with no precondition read as fresh")
	}
}
