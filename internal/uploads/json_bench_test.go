// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package uploads

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/arca/internal/api"
)

// BenchmarkJSONCompleteRequestDecode is the completion of a multipart upload
// (spec 007): one entry per part, read by the same strict decode complete
// uses.
func BenchmarkJSONCompleteRequestDecode(b *testing.B) {
	for _, c := range []struct {
		name  string
		parts int
	}{
		{"small", 10},
		{"medium", 200},
		{"large", 4000},
	} {
		b.Run(c.name, func(b *testing.B) {
			var req completeRequest
			req.Parts = make([]struct {
				N    int32  `json:"n"`
				ETag string `json:"etag"`
			}, c.parts)
			for i := range req.Parts {
				req.Parts[i].N = int32(i + 1)
				req.Parts[i].ETag = fmt.Sprintf("\"9b2cf535f27731c974343645a3985328-%05d\"", i)
			}
			payload, err := json.Marshal(req)
			if err != nil {
				b.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/uploads/u/complete", nil)
			r.Header.Set(api.HeaderContentType, api.JSONMediaType)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				r.Body = io.NopCloser(bytes.NewReader(payload))
				got, err := api.DecodeBody[completeRequest](r)
				if err != nil {
					b.Fatal(err)
				}
				if len(got.Parts) != c.parts {
					b.Fatalf("decoded %d parts", len(got.Parts))
				}
			}
		})
	}
}
