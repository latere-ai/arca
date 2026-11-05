// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/arca/internal/config"
	"latere.ai/x/arca/internal/store"
	"latere.ai/x/arca/object"
)

// benchSink keeps only the byte count of what a handler writes.
type benchSink struct {
	header http.Header
	n      int
}

func newBenchSink() *benchSink { return &benchSink{header: http.Header{}} }

func (s *benchSink) Header() http.Header         { return s.header }
func (s *benchSink) WriteHeader(int)             {}
func (s *benchSink) Write(p []byte) (int, error) { s.n += len(p); return len(p), nil }

// BenchmarkJSONListingEncode is the listing of a folder: every row is
// rendered as the surface renders it and the page goes out as one Listing,
// the way list answers.
func BenchmarkJSONListingEncode(b *testing.B) {
	svc := New(Options{Config: config.Config{PublicCDNURL: "https://cdn.example", BucketPrefix: "arca/"}})
	updated := time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC)
	for _, c := range []struct {
		name string
		rows int
	}{
		{"small", 2},
		{"medium", 40},
		{"large", 780},
	} {
		b.Run(c.name, func(b *testing.B) {
			rows := make([]store.File, c.rows)
			for i := range rows {
				rows[i] = store.File{
					ID: fmt.Sprintf("row-%04d", i), Owner: "https://issuer.example|0f5c1d2e",
					Path:        fmt.Sprintf("files/reports/2026/q3/quarterly-%04d.pdf", i),
					ObjectID:    object.ID("0192f5c8-7a4b-7c1e-9d3a-52b6e8a1f407"),
					ContentType: "application/pdf", SizeBytes: 48213 + int64(i),
					Checksum: "9b2cf535f27731c974343645a3985328", ChecksumKind: object.ChecksumKind("md5"),
					IsPublic: i%5 == 0, UpdatedAt: updated.Add(time.Duration(i) * time.Minute),
				}
			}
			render := func(w http.ResponseWriter) {
				page := Listing{Entries: make([]Object, 0, len(rows)), Prefixes: []string{"files/reports/2026/q3/drafts/"}}
				for _, f := range rows {
					page.Entries = append(page.Entries, svc.Render(f))
				}
				page.NextCursor = rows[len(rows)-1].Path
				write(w, http.StatusOK, page)
			}
			sink := newBenchSink()
			render(sink)
			b.ReportAllocs()
			b.SetBytes(int64(sink.n))
			for b.Loop() {
				render(newBenchSink())
			}
		})
	}
}
