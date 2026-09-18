// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package object

import "fmt"

// ChecksumKind says what an object's checksum is, so a caller comparing two
// objects knows whether it is comparing digests of the bytes or labels the
// store chose.
type ChecksumKind string

const (
	// ChecksumSHA256 is the hex digest of the bytes, computed as they were
	// written and verified by the store where it accepts a trailing digest
	// (spec 003). It is the kind of every object Arca wrote in one piece.
	ChecksumSHA256 ChecksumKind = "sha256"
	// ChecksumETag is the label the store returned for an object assembled
	// from parts Arca never saw, which is a digest of digests and not of
	// the bytes (spec 007).
	ChecksumETag ChecksumKind = "etag"
)

// Valid reports whether k is one of the two kinds.
func (k ChecksumKind) Valid() bool { return k == ChecksumSHA256 || k == ChecksumETag }

// ParseChecksumKind reads a kind as a row holds it.
func ParseChecksumKind(text string) (ChecksumKind, error) {
	k := ChecksumKind(text)
	if !k.Valid() {
		return "", fmt.Errorf("object: %q is no checksum kind; it is %q or %q", text, ChecksumSHA256, ChecksumETag)
	}
	return k, nil
}
