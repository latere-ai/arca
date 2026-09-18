// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// The body rules of spec 013, in the error table's envelope.
//
// A request body that is not object bytes is JSON, decoded with unknown
// fields refused, so a client that misspells a field learns it at once
// rather than silently writing a default. The shared decoder of
// latere.ai/x/pkg/httpjson writes plain text on a refusal, which is the one
// thing this surface cannot do: every refusal here is one code of the error
// table with one fixed sentence.

// MaxBodyBytes bounds a JSON request body. The largest one this surface
// takes is a workspace sync manifest, one entry per file of the subtree, so
// the bound is generous rather than tight: a tree of a few hundred thousand
// files declares its post-state in one call, and a body past this is a
// client that meant something else.
const MaxBodyBytes = 64 << 20

// JSONMediaType is the one content type a body route accepts.
const JSONMediaType = "application/json"

// The two request headers this surface negotiates on, named rather than
// spelled at each site that reads one, so a handler and the test that
// drives it cannot disagree by a letter.
const (
	HeaderContentType = "Content-Type"
	HeaderAccept      = "Accept"
)

// DecodeBody reads a JSON request body into a T. A body route refuses a
// content type that is not JSON, a field the endpoint does not know, a body
// past the bound, and anything that is not JSON at all, each with the code
// of spec 013's table its row names.
func DecodeBody[T any](r *http.Request) (T, error) {
	var v T
	if err := JSONRequest(r); err != nil {
		return v, err
	}
	body := http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		var zero T
		return zero, decodeRefusal(err)
	}
	// A second value after the first is a client sending two documents where
	// the route reads one, and reading the first alone would act on half of
	// what it meant.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		var zero T
		if err == nil {
			return zero, Refuse(CodeBadRequest, "the body carries more than one JSON value")
		}
		return zero, decodeRefusal(err)
	}
	return v, nil
}

// DecodeOptionalBody reads a JSON body a route does not require. An absent
// or empty body is the zero value, which is what a renew that asks for the
// default lease sends. A body that is there is held to every rule above.
func DecodeOptionalBody[T any](r *http.Request) (T, error) {
	var v T
	if r.Body == nil || r.ContentLength == 0 {
		return v, nil
	}
	return DecodeBody[T](r)
}

// JSONRequest holds a request to the two content negotiation rules of spec
// 013: a body that is not JSON is unsupported_media_type, and an Accept that
// excludes JSON is not_acceptable.
func JSONRequest(r *http.Request) error {
	if err := Acceptable(r); err != nil {
		return err
	}
	raw := r.Header.Get(HeaderContentType)
	if raw == "" {
		return Refuse(CodeUnsupportedMediaType, "the body carries no content type; this endpoint reads %s", JSONMediaType)
	}
	media, _, err := mime.ParseMediaType(raw)
	if err != nil || media != JSONMediaType {
		return Refuse(CodeUnsupportedMediaType, "the body is %q; this endpoint reads %s", raw, JSONMediaType)
	}
	return nil
}

// Acceptable reports whether the caller takes a JSON answer. A request that
// sends no Accept takes anything, which is most of them.
func Acceptable(r *http.Request) error {
	raw := r.Header.Get(HeaderAccept)
	if raw == "" {
		return nil
	}
	for field := range strings.SplitSeq(raw, ",") {
		media, _, err := mime.ParseMediaType(strings.TrimSpace(field))
		if err != nil {
			continue
		}
		if media == "*/*" || media == "application/*" || media == JSONMediaType {
			return nil
		}
	}
	return Refuse(CodeNotAcceptable, "the caller accepts %q, and this endpoint answers %s", raw, JSONMediaType)
}

// decodeRefusal renders what the decoder refused as one row of the error
// table. The three cases a caller acts on differently are apart: a field the
// endpoint does not know names the field, a body past the bound says so, and
// everything else is a body that could not be read.
func decodeRefusal(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return Refuse(CodeBodyTooLarge, "the body is larger than %d bytes", MaxBodyBytes)
	}
	if field, ok := unknownField(err); ok {
		return Refuse(CodeUnknownField, "the body carries the field %s, which this endpoint does not know", field).About(field)
	}
	return Refuse(CodeBadRequest, "the body is not JSON this endpoint can read: %v", err)
}

// unknownFieldPrefix is what the standard library's decoder writes for a
// field the type does not declare. Nothing in the standard library exposes
// it as a typed error, so the prefix is read rather than the type, and the
// test below holds this to what the decoder actually writes.
const unknownFieldPrefix = "json: unknown field "

// unknownField reads the field name out of the decoder's refusal.
func unknownField(err error) (string, bool) {
	text := err.Error()
	rest, found := strings.CutPrefix(text, unknownFieldPrefix)
	if !found {
		return "", false
	}
	var name string
	if _, err := fmt.Sscanf(rest, "%q", &name); err != nil {
		return "", false
	}
	return name, true
}
