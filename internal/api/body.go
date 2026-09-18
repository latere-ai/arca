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

// MaxBodyBytes bounds a JSON request body. Every JSON body of this API is a
// handful of fields, and the largest is the part list of a completion, which
// is a thousand small objects. A megabyte is above that and far below what a
// replica would notice, so a body past it is a caller doing something else.
const MaxBodyBytes = 1 << 20

// The two headers a JSON route reads before it reads a body.
const (
	HeaderContentType = "Content-Type"
	HeaderAccept      = "Accept"
	// MediaJSON is the one media type this API takes and the one it
	// answers.
	MediaJSON = "application/json"
)

// Decode reads a JSON request body into a new T, in the one shape spec 013
// fixes: the media type is JSON, the caller takes JSON back, the body is
// bounded, and a field the endpoint does not know is refused rather than
// silently dropped, so a client that misspells one learns it at once instead
// of writing a default.
//
// A refusal is one of this package's, so the caller writes it with
// WriteError and the envelope is the same envelope every other refusal uses.
func Decode[T any](r *http.Request) (*T, error) {
	if err := Acceptable(r); err != nil {
		return nil, err
	}
	if err := jsonMedia(r); err != nil {
		return nil, err
	}
	v := new(T)
	// The bound is the reader's, so a body past it stops being read rather
	// than being buffered and measured afterwards. The writer is nil: this
	// package writes the refusal itself, in the envelope of spec 013.
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, decodeRefusal(err)
	}
	// A second value after the first is a body this endpoint did not ask
	// for, and reading it is also what proves the first one ended.
	var extra any
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return v, nil
	case err != nil:
		return nil, decodeRefusal(err)
	default:
		return nil, Refuse(CodeBadRequest, "the body carries a second value after the first")
	}
}

// Acceptable refuses a caller that asked for something this API does not
// answer. An absent Accept asks for anything.
func Acceptable(r *http.Request) error {
	raw := r.Header.Get(HeaderAccept)
	if raw == "" {
		return nil
	}
	for part := range strings.SplitSeq(raw, ",") {
		media, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		switch strings.TrimSpace(media) {
		case MediaJSON, "application/*", "*/*":
			return nil
		}
	}
	return Refuse(CodeNotAcceptable, "Accept is %q and this endpoint answers %s", raw, MediaJSON).About(HeaderAccept)
}

// jsonMedia refuses a body declared as something other than JSON. A body
// with no Content-Type at all declared nothing, which is not a wrong
// declaration, so it is read and the decoder is what refuses bytes that are
// not JSON.
func jsonMedia(r *http.Request) error {
	raw := r.Header.Get(HeaderContentType)
	if raw == "" {
		return nil
	}
	media, _, err := mime.ParseMediaType(raw)
	if err != nil || media != MediaJSON {
		return Refuse(CodeUnsupportedMediaType,
			"Content-Type is %q and this endpoint takes %s", raw, MediaJSON).About(HeaderContentType)
	}
	return nil
}

// decodeRefusal renders what the decoder refused in the table of spec 013: a
// body past the bound, a field the endpoint does not know with the field
// named, and everything else as a body that could not be read.
func decodeRefusal(err error) error {
	if errors.Is(err, io.EOF) {
		return Refuse(CodeBadRequest, "the body is empty and this endpoint takes a JSON object")
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return Refuse(CodeBodyTooLarge, "the body is longer than the %d bytes this endpoint reads", MaxBodyBytes)
	}
	if name, ok := unknownField(err.Error()); ok {
		return Refuse(CodeUnknownField, "the body names the field %s, which this endpoint does not know", name).About(name)
	}
	var unexpected *json.UnmarshalTypeError
	if errors.As(err, &unexpected) {
		return Refuse(CodeInvalidField, "the field %s is a %s and this endpoint reads a %s",
			unexpected.Field, unexpected.Value, unexpected.Type).About(unexpected.Field)
	}
	return Refuse(CodeBadRequest, "the body is not a JSON object this endpoint can read")
}

// unknownFieldPrefix opens the one error the standard library writes for a
// field an endpoint does not declare. It is matched on the text because the
// package gives that refusal no type of its own.
const unknownFieldPrefix = `json: unknown field `

// unknownField reads the field name out of that error.
func unknownField(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, unknownFieldPrefix)
	if !ok {
		return "", false
	}
	return strings.Trim(rest, `"`), true
}
