// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import (
	"strings"
	"testing"
)

func TestChecksumKindsAreTwo(t *testing.T) {
	for _, k := range []ChecksumKind{ChecksumSHA256, ChecksumETag} {
		got, err := ParseChecksumKind(string(k))
		if err != nil || got != k {
			t.Errorf("ParseChecksumKind(%q) = %q, %v", k, got, err)
		}
	}
	for _, text := range []string{"", "md5", "SHA256", "crc32"} {
		got, err := ParseChecksumKind(text)
		if err == nil {
			t.Errorf("ParseChecksumKind(%q) = %q, want an error", text, got)
			continue
		}
		if !strings.Contains(err.Error(), `"sha256"`) {
			t.Errorf("the error does not name the kinds: %v", err)
		}
	}
}
