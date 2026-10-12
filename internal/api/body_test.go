// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// star is the smallest body a route of this surface reads: what PUT
// /v1/stars takes.
type star struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
}

// starred is one well formed body of that route.
const starred = `{"owner":"me","path":"files/a.txt"}`

// bodied builds one request with a body and a content type.
func bodied(body, contentType string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/v1/stars", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

// refusalOf reads the code of a refusal, and "" from a nil error.
func refusalOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	r, ok := errors.AsType[*Refusal](err)
	if !ok {
		t.Fatalf("the failure is not a refusal of the table: %v", err)
	}
	if _, known := errorTable[r.Code]; !known {
		t.Fatalf("the refusal carries the code %q, which the table does not hold", r.Code)
	}
	return r.Code
}

func TestABodyIsReadAsJSONOrRefusedWithTheRowItsFaultNames(t *testing.T) {
	for _, c := range []struct {
		name        string
		body        string
		contentType string
		code        string
	}{
		{"one document", starred, JSONMediaType, ""},
		{"a charset", starred, "application/json; charset=utf-8", ""},
		{"a field the endpoint does not know", `{"owner":"me","path":"files/a.txt","kind":"pin"}`, JSONMediaType, CodeUnknownField},
		{"a body that is not JSON", `{`, JSONMediaType, CodeBadRequest},
		{"two documents", starred + starred, JSONMediaType, CodeBadRequest},
		{"trailing bytes", starred + ` oops`, JSONMediaType, CodeBadRequest},
		{"no content type", starred, "", CodeUnsupportedMediaType},
		{"a form", `owner=me&path=files/a.txt`, "application/x-www-form-urlencoded", CodeUnsupportedMediaType},
		{"a content type that is not a media type", `{}`, "not a media type;;;", CodeUnsupportedMediaType},
		{"no body at all", ``, JSONMediaType, CodeBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeBody[star](bodied(c.body, c.contentType))
			if code := refusalOf(t, err); code != c.code {
				t.Fatalf("the body was refused %q, want %q", code, c.code)
			}
			if err == nil && (got != star{Owner: "me", Path: "files/a.txt"}) {
				t.Errorf("the body read as %+v", got)
			}
		})
	}
}

func TestAFieldTheEndpointDoesNotKnowIsNamedBack(t *testing.T) {
	_, err := DecodeBody[star](bodied(`{"owner":"me","path":"files/a.txt","kind":"pin"}`, JSONMediaType))
	r, ok := errors.AsType[*Refusal](err)
	if !ok {
		t.Fatalf("the failure is %v", err)
	}
	// A client that misspells a field learns which one, which is the whole
	// reason the decoder refuses an unknown field rather than dropping it.
	if !slices.Contains(r.Fields, "kind") {
		t.Errorf("the refusal named %v", r.Fields)
	}
	if !strings.Contains(r.Detail, "kind") {
		t.Errorf("the developer detail reads %q", r.Detail)
	}
}

func TestABodyPastTheBoundIsRefusedRatherThanRead(t *testing.T) {
	// The bound is generous rather than tight, because the largest body this
	// surface takes is the completion of an upload session, one entry per
	// part.
	if MaxBodyBytes < 1<<20 {
		t.Fatalf("the bound is %d bytes, which no part list of a large upload fits in", MaxBodyBytes)
	}
	huge := `{"owner":"me","path":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	if code := refusalOf(t, second(DecodeBody[star](bodied(huge, JSONMediaType)))); code != CodeBodyTooLarge {
		t.Fatalf("a body past the bound = %q", code)
	}
}

// second answers the error half of a two value call, so a table above reads
// one expression.
func second[T any](_ T, err error) error { return err }

func TestACallerThatTakesNoJSONIsToldBeforeTheHandlerActs(t *testing.T) {
	for _, c := range []struct {
		accept string
		code   string
	}{
		{"", ""},
		{"*/*", ""},
		{"application/json", ""},
		{"application/*", ""},
		{"text/html, application/json;q=0.9", ""},
		{"text/html, */*;q=0.8", ""},
		{"text/csv", CodeNotAcceptable},
		{"text/html", CodeNotAcceptable},
		{"not a media type;;;", CodeNotAcceptable},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/shares", nil)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if code := refusalOf(t, Acceptable(r)); code != c.code {
			t.Errorf("Accept %q = %q, want %q", c.accept, code, c.code)
		}
	}
}

func TestAnUnknownFieldThatTheDecoderSpellsOtherwiseIsStillABadBody(t *testing.T) {
	// The field name is read out of the decoder's text, because nothing in
	// the standard library exposes it as a typed error. A refusal whose text
	// does not carry the prefix falls back to the body row rather than
	// naming a field it could not read.
	if _, ok := unknownField(errors.New("json: cannot unmarshal number")); ok {
		t.Error("a refusal that names no field read as one")
	}
	if name, ok := unknownField(errors.New(unknownFieldPrefix + `"kind"`)); !ok || name != "kind" {
		t.Errorf("the field read as %q, %v", name, ok)
	}
	if _, ok := unknownField(errors.New(unknownFieldPrefix + "kind")); ok {
		t.Error("a name the decoder did not quote read as one")
	}
}
