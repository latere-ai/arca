// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// benchSink is a response writer that keeps nothing but the byte count, so
// a benchmark of an encoder measures the encoder and not a growing buffer.
type benchSink struct {
	header http.Header
	n      int
}

func newBenchSink() *benchSink { return &benchSink{header: http.Header{}} }

func (s *benchSink) Header() http.Header         { return s.header }
func (s *benchSink) WriteHeader(int)             {}
func (s *benchSink) Write(p []byte) (int, error) { s.n += len(p); return len(p), nil }

// benchPart is one part of an upload completion body, the largest request
// body this surface takes (spec 013).
type benchPart struct {
	N    int32  `json:"n"`
	ETag string `json:"etag"`
}

type benchParts struct {
	Parts []benchPart `json:"parts"`
}

// benchObject is one entry of a listing page, shaped like a file object.
type benchObject struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum"`
	ChecksumKind string `json:"checksum_kind"`
	ContentType  string `json:"content_type"`
	IsPublic     bool   `json:"is_public"`
	Modified     string `json:"modified"`
}

// benchSizes are the part and entry counts that give a body of about 0.5 KB,
// 10 KB and 200 KB.
var benchSizes = []struct {
	name    string
	parts   int
	entries int
}{
	{"small", 10, 2},
	{"medium", 200, 48},
	{"large", 4000, 930},
}

func benchBody(parts int) []byte {
	v := benchParts{Parts: make([]benchPart, parts)}
	for i := range v.Parts {
		v.Parts[i] = benchPart{N: int32(i + 1), ETag: fmt.Sprintf("\"9b2cf535f27731c974343645a3985328-%05d\"", i)}
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// BenchmarkJSONDecodeBodyDecode is every JSON request body of the surface
// passing through DecodeBody: the strict decode with unknown fields refused
// and the trailing-value check.
func BenchmarkJSONDecodeBodyDecode(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			payload := benchBody(size.parts)
			r := httptest.NewRequest(http.MethodPost, "/v1/uploads/u/complete", nil)
			r.Header.Set(HeaderContentType, JSONMediaType)
			r.Body = io.NopCloser(bytes.NewReader(payload))
			if got, err := DecodeBody[benchParts](r); err != nil || len(got.Parts) != size.parts {
				b.Fatalf("decoded %d parts, err %v", len(got.Parts), err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				r.Body = io.NopCloser(bytes.NewReader(payload))
				got, err := DecodeBody[benchParts](r)
				if err != nil {
					b.Fatal(err)
				}
				if len(got.Parts) != size.parts {
					b.Fatalf("decoded %d parts", len(got.Parts))
				}
			}
		})
	}
}

// BenchmarkJSONWritePageEncode is the envelope every list route answers,
// written through WritePage.
func BenchmarkJSONWritePageEncode(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			entries := make([]benchObject, size.entries)
			for i := range entries {
				entries[i] = benchObject{
					Path: fmt.Sprintf("files/reports/2026/q3/quarterly-%04d.pdf", i), Size: 48213 + int64(i),
					Checksum: "9b2cf535f27731c974343645a3985328", ChecksumKind: "md5",
					ContentType: "application/pdf", Modified: "2026-09-18T10:02:11Z",
				}
			}
			sink := newBenchSink()
			WritePage(sink, entries, "files/reports/2026/q3/quarterly-9999.pdf")
			b.ReportAllocs()
			b.SetBytes(int64(sink.n))
			for b.Loop() {
				WritePage(newBenchSink(), entries, "files/reports/2026/q3/quarterly-9999.pdf")
			}
		})
	}
}

// BenchmarkJSONWriteErrorEncode is the error envelope of spec 013 that every
// refused request answers.
func BenchmarkJSONWriteErrorEncode(b *testing.B) {
	r := httptest.NewRequest(http.MethodPost, "/v1/files/x", nil)
	refusal := Refuse(CodeUnknownField, "the body carries the field %s, which this endpoint does not know", "colour").About("colour")
	sink := newBenchSink()
	WriteError(sink, r, refusal)
	size := sink.n
	b.ReportAllocs()
	b.SetBytes(int64(size))
	for b.Loop() {
		WriteError(newBenchSink(), r, refusal)
	}
}
