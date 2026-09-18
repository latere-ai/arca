// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// body is the shape a JSON route of specs 005 and 007 reads.
type body struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
}

// request builds a JSON request with the headers the case named.
func request(json string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/probe", strings.NewReader(json))
	r.Header.Set(HeaderContentType, MediaJSON)
	for name, value := range headers {
		if value == "" {
			r.Header.Del(name)
			continue
		}
		r.Header.Set(name, value)
	}
	return r
}

// codeOf reads the error table row a refusal names.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal is %v, which names no row of the error table", err)
	}
	return refusal.Code
}

func TestDecodeReadsABodyAndNamesTheFieldItCannotRead(t *testing.T) {
	got, err := Decode[body](request(`{"owner":"me","path":"files/a.md","size":11}`, nil))
	if err != nil || got.Owner != "me" || got.Size != 11 {
		t.Fatalf("Decode = %+v, %v", got, err)
	}

	for name, c := range map[string]struct {
		json    string
		headers map[string]string
		want    string
		field   string
	}{
		"a field this endpoint does not know": {
			json: `{"owner":"me","zone":"agents"}`, want: CodeUnknownField, field: "zone",
		},
		"a field of the wrong kind": {
			json: `{"size":"eleven"}`, want: CodeInvalidField, field: "size",
		},
		"bytes that are not JSON": {
			json: `not json`, want: CodeBadRequest,
		},
		"no body at all": {
			json: ``, want: CodeBadRequest,
		},
		"a second value after the first": {
			json: `{"owner":"me"} {"owner":"you"}`, want: CodeBadRequest,
		},
		"a body past the bound": {
			json: `{"owner":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, want: CodeBodyTooLarge,
		},
		"a media type this endpoint does not take": {
			json: `{"owner":"me"}`, headers: map[string]string{HeaderContentType: "text/plain"},
			want: CodeUnsupportedMediaType,
		},
		"a media type that is not one": {
			json: `{"owner":"me"}`, headers: map[string]string{HeaderContentType: "application/json; charset"},
			want: CodeUnsupportedMediaType,
		},
		"an Accept that excludes JSON": {
			json: `{"owner":"me"}`, headers: map[string]string{HeaderAccept: "text/html, image/png"},
			want: CodeNotAcceptable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode[body](request(c.json, c.headers))
			if err == nil {
				t.Fatalf("Decode accepted %s", name)
			}
			if got := codeOf(t, err); got != c.want {
				t.Fatalf("Decode answered %q and the table says %q", got, c.want)
			}
			if c.field == "" {
				return
			}
			var refusal *Refusal
			_ = errors.As(err, &refusal)
			if len(refusal.Fields) != 1 || refusal.Fields[0] != c.field {
				t.Fatalf("the refusal names the fields %v and the body's fault is %q", refusal.Fields, c.field)
			}
		})
	}
}

func TestABodyThatDeclaresNoMediaTypeIsReadAndOneThatDeclaresJSONIsToo(t *testing.T) {
	for _, declared := range []string{"", MediaJSON, "application/json; charset=utf-8"} {
		got, err := Decode[body](request(`{"owner":"me"}`, map[string]string{HeaderContentType: declared}))
		if err != nil || got.Owner != "me" {
			t.Errorf("a body declared as %q read as %+v, %v", declared, got, err)
		}
	}
}

func TestEveryAcceptThatAdmitsJSONIsAccepted(t *testing.T) {
	for _, accept := range []string{"", "*/*", "application/*", "application/json;q=0.9", "text/html, application/json"} {
		if err := Acceptable(request(`{}`, map[string]string{HeaderAccept: accept})); err != nil {
			t.Errorf("an Accept of %q was refused: %v", accept, err)
		}
	}
}
