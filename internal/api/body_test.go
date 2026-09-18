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

// grant is a body of the shape a JSON route takes.
type grant struct {
	Owner      string `json:"owner"`
	Permission string `json:"permission"`
}

// post builds one request with a body and a content type.
func post(contentType, body string) (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, "/v1/shares", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return httptest.NewRecorder(), r
}

// codeOf reads the code of the refusal an error carries.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the failure is not a refusal: %v", err)
	}
	return refusal.Code
}

// TestABodyIsReadStrictly is criterion 11 of spec 013 at the decoder: an
// unknown field, a wrong content type, and an Accept that excludes JSON are
// each their own row of the error table.
func TestABodyIsReadStrictly(t *testing.T) {
	w, r := post(ContentTypeJSON, `{"owner":"me","permission":"read"}`)
	body, err := Decode[grant](w, r)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if body.Owner != Me || body.Permission != "read" {
		t.Errorf("Decode read %+v", body)
	}

	for _, c := range []struct{ name, contentType, body, code string }{
		{"an unknown field", ContentTypeJSON, `{"grantee_type":"principal"}`, CodeUnknownField},
		{"a body that is not JSON", ContentTypeJSON, `{`, CodeBadRequest},
		{"no body", ContentTypeJSON, ``, CodeBadRequest},
		{"a second value", ContentTypeJSON, `{}{}`, CodeBadRequest},
		{"a trailing token", ContentTypeJSON, `{} nope`, CodeBadRequest},
		{"no content type", "", `{}`, CodeUnsupportedMediaType},
		{"another content type", "application/xml", `{}`, CodeUnsupportedMediaType},
		{"a content type that will not parse", "application/json; charset", `{}`, CodeUnsupportedMediaType},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, r := post(c.contentType, c.body)
			if _, err := Decode[grant](w, r); codeOf(t, err) != c.code {
				t.Errorf("the refusal is %q, want %q", codeOf(t, err), c.code)
			}
		})
	}

	// The charset a browser adds is a parameter of the media type, not
	// another type.
	w, r = post("application/json; charset=utf-8", `{"owner":"me"}`)
	if _, err := Decode[grant](w, r); err != nil {
		t.Errorf("a JSON body with a charset: %v", err)
	}
}

// TestABodyPastTheBoundIsRefusedRatherThanBuffered: a JSON body of this API
// is a handful of fields, so the read stops rather than holding what a
// caller sent instead.
func TestABodyPastTheBoundIsRefusedRatherThanBuffered(t *testing.T) {
	huge := `{"owner":"` + strings.Repeat("a", MaxJSONBody+1) + `"}`
	w, r := post(ContentTypeJSON, huge)
	if _, err := Decode[grant](w, r); codeOf(t, err) != CodeBodyTooLarge {
		t.Errorf("a body of %d bytes = %q", len(huge), codeOf(t, err))
	}
}

// TestAnAcceptThatExcludesJSONIsNotAcceptable: this endpoint answers in JSON
// and nothing else, so a caller that asked for something else is told before
// the body is read.
func TestAnAcceptThatExcludesJSONIsNotAcceptable(t *testing.T) {
	for _, c := range []struct {
		accept string
		ok     bool
	}{
		{"", true},
		{"*/*", true},
		{"application/json", true},
		{"application/*", true},
		{"text/html, application/json;q=0.9", true},
		{"text/html", false},
		{"application/xml, text/html", false},
		{"not a media type", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/shares", nil)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		err := Acceptable(r)
		if (err == nil) != c.ok {
			t.Errorf("Accept %q = %v, want ok=%t", c.accept, err, c.ok)
		}
		if err != nil && codeOf(t, err) != CodeNotAcceptable {
			t.Errorf("Accept %q is refused as %q", c.accept, codeOf(t, err))
		}
	}
	w, r := post(ContentTypeJSON, `{}`)
	r.Header.Set("Accept", "text/html")
	if _, err := Decode[grant](w, r); codeOf(t, err) != CodeNotAcceptable {
		t.Errorf("a decode with an Accept that excludes JSON = %q", codeOf(t, err))
	}
}

// TestAnUnknownFieldIsNamed: the field a caller misspelled reaches the
// caller by name, which is the point of refusing it rather than ignoring it.
func TestAnUnknownFieldIsNamed(t *testing.T) {
	w, r := post(ContentTypeJSON, `{"grantee_type":"principal"}`)
	_, err := Decode[grant](w, r)
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the failure is not a refusal: %v", err)
	}
	if len(refusal.Fields) != 1 || refusal.Fields[0] != "grantee_type" {
		t.Errorf("the refusal names %v", refusal.Fields)
	}
	if !strings.Contains(refusal.Detail, "grantee_type") {
		t.Errorf("the detail is %q", refusal.Detail)
	}
}
