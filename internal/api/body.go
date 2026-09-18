// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// MaxJSONBody bounds a request body that is not object bytes. Every JSON
// body of this API is a handful of fields, so a body past this is a caller
// sending something else, and the read stops rather than buffering it.
const MaxJSONBody = 1 << 20

// ContentTypeJSON is the one media type a JSON route takes and the one every
// JSON response carries.
const ContentTypeJSON = "application/json"

// Decode reads a JSON request body into a T, in the rule spec 013 sets for
// every route whose body is not object bytes.
//
// Unknown fields are refused, so a client that misspells a field learns it
// at once rather than silently writing a default. A content type that is not
// JSON is unsupported_media_type, an Accept that excludes JSON is
// not_acceptable, a body past MaxJSONBody is body_too_large, and anything
// else that will not parse is bad_request. Each is a refusal of the error
// table, so one place writes the envelope.
func Decode[T any](w http.ResponseWriter, r *http.Request) (*T, error) {
	if err := Acceptable(r); err != nil {
		return nil, err
	}
	if err := isJSON(r); err != nil {
		return nil, err
	}
	body := http.MaxBytesReader(w, r.Body, MaxJSONBody)
	v := new(T)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, bodyRefusal(err)
	}
	// A second value after the first is a caller sending a stream where the
	// route takes one object, which is as much a misunderstanding as a
	// syntax error and is answered the same way.
	var trailing any
	switch err := dec.Decode(&trailing); {
	case errors.Is(err, io.EOF):
		return v, nil
	case err != nil:
		return nil, bodyRefusal(err)
	default:
		return nil, Refuse(CodeBadRequest, "the body carries more than one JSON value")
	}
}

// bodyRefusal reads a decoder's failure as a row of the error table. The
// unknown field is named in the developer detail and in the field list,
// because a caller fixing one needs to know which.
func bodyRefusal(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return Refuse(CodeBodyTooLarge, "a JSON body of this API is at most %d bytes", MaxJSONBody)
	}
	if errors.Is(err, io.EOF) {
		return Refuse(CodeBadRequest, "the request has no body, and this route takes one")
	}
	if field, ok := unknownField(err); ok {
		return Refuse(CodeUnknownField, "the body names %s, which this endpoint does not know", field).About(field)
	}
	return Refuse(CodeBadRequest, "the body is not JSON this endpoint can read: %v", err)
}

// unknownFieldPrefix opens the one error the standard library reports for a
// field the type does not have. Reading it back is how the field reaches the
// caller by name; the decoder offers no other way.
const unknownFieldPrefix = `json: unknown field `

// unknownField reads the field name out of the decoder's message.
func unknownField(err error) (string, bool) {
	_, rest, ok := strings.Cut(err.Error(), unknownFieldPrefix)
	if !ok {
		return "", false
	}
	return strings.Trim(rest, `"`), true
}

// isJSON refuses a body sent as anything but JSON. A route that takes a body
// takes one shape, and a caller that labelled it otherwise sent something
// this endpoint would misread.
func isJSON(r *http.Request) error {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return Refuse(CodeUnsupportedMediaType, "the body carries no Content-Type, and this route takes %s", ContentTypeJSON)
	}
	media, _, err := mime.ParseMediaType(raw)
	if err != nil || media != ContentTypeJSON {
		return Refuse(CodeUnsupportedMediaType, "the body is %q, and this route takes %s", raw, ContentTypeJSON)
	}
	return nil
}

// Acceptable refuses a request whose Accept header excludes JSON, which is
// the only thing this endpoint answers with. An absent header asks for
// anything, which JSON satisfies.
func Acceptable(r *http.Request) error {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return nil
	}
	for part := range strings.SplitSeq(accept, ",") {
		media, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		switch media {
		case "*/*", "application/*", ContentTypeJSON:
			return nil
		}
	}
	return Refuse(CodeNotAcceptable, "Accept is %q, and this endpoint answers in %s", accept, ContentTypeJSON)
}
