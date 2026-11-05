// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"fmt"
	"net/http"
	"testing"
	"time"
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

// benchPageSizes are the page lengths that give a tail answer of about
// 0.5 KB, 10 KB and 200 KB.
var benchPageSizes = []struct {
	name string
	rows int
}{
	{"small", 1},
	{"medium", 28},
	{"large", 560},
}

// benchEvent is one row as the writers of spec 010 leave it: a put with its
// size and a share with its kind.
func benchEvent(i int) Event {
	e := Event{
		ID: int64(41000 + i), Owner: "https://issuer.example|0f5c1d2e-7a4b-4c1e-9d3a-52b6e8a1f407",
		Path: fmt.Sprintf("files/reports/2026/q3/quarterly-%04d.pdf", i), Action: ActionPut,
		Actor: "https://issuer.example|0f5c1d2e-7a4b-4c1e-9d3a-52b6e8a1f407",
		At:    time.Date(2026, 9, 18, 10, 2, 11, 0, time.UTC).Add(time.Duration(i) * time.Second),
		Detail: map[string]any{
			"size": 48213 + i, "content_type": "application/pdf", "version": 3,
			"checksum": "9b2cf535f27731c974343645a3985328",
		},
	}
	return e
}

// BenchmarkJSONEventPageEncode is the tail of the event log: writePage
// renders a page of rows, each with its detail passed through as raw JSON.
func BenchmarkJSONEventPageEncode(b *testing.B) {
	for _, size := range benchPageSizes {
		b.Run(size.name, func(b *testing.B) {
			page := Page{Entries: make([]Event, size.rows), NextCursor: 41999}
			for i := range page.Entries {
				page.Entries[i] = benchEvent(i)
			}
			sink := newBenchSink()
			writePage(sink, page)
			b.ReportAllocs()
			b.SetBytes(int64(sink.n))
			for b.Loop() {
				writePage(newBenchSink(), page)
			}
		})
	}
}

// BenchmarkJSONEventDetailMarshal is the encode every append of the log does
// to its detail before the row is written.
func BenchmarkJSONEventDetailMarshal(b *testing.B) {
	detail := benchEvent(1).Detail
	encoded, err := marshalDetail(detail)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(encoded)))
	for b.Loop() {
		if _, err := marshalDetail(detail); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONEventScanUnmarshal is the read of one stored row, including
// the decode of its detail column, as every tail and every export does it.
func BenchmarkJSONEventScanUnmarshal(b *testing.B) {
	e := benchEvent(1)
	encoded, err := marshalDetail(e.Detail)
	if err != nil {
		b.Fatal(err)
	}
	row := values(e.ID, e.Owner, nullable(e.Path), e.Action, nullable(e.Actor), encoded, e.At)
	b.ReportAllocs()
	b.SetBytes(int64(len(encoded)))
	for b.Loop() {
		got, err := scanEvent(row)
		if err != nil {
			b.Fatal(err)
		}
		if len(got.Detail) != len(e.Detail) {
			b.Fatalf("the detail read back with %d members", len(got.Detail))
		}
	}
}
