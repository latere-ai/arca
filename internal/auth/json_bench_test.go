// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth_test

import (
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/arca/internal/auth"
)

// BenchmarkJSONReadLimitsUnmarshal is the decode of the limits object that
// rides on the authorizer's answers (spec 006), read on every write and
// every enumeration.
func BenchmarkJSONReadLimitsUnmarshal(b *testing.B) {
	omit := func(n int) string {
		paths := make([]string, n)
		for i := range paths {
			paths[i] = fmt.Sprintf(`"files/projects/archive-%03d/private"`, i)
		}
		return strings.Join(paths, ",")
	}
	for _, c := range []struct {
		name string
		raw  string
	}{
		{"small", `{"quota_bytes":10737418240,"versions":true,"trash":false,"omit":[` + omit(2) + `]}`},
		{"medium", `{"quota_bytes":10737418240,"versions":true,"trash":true,"omit":[` + omit(250) + `]}`},
	} {
		b.Run(c.name, func(b *testing.B) {
			raw := []byte(c.raw)
			if _, err := auth.ReadLimits(raw); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := auth.ReadLimits(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
