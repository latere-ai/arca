// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package object is the object model a platform built on Arca imports: the
// identifier every write of content mints, the bucket key that derives from
// it, the plane a path is rooted in, and the kinds of checksum an object
// carries. Nothing here reaches a store; internal/blob owns the bucket and
// internal/store owns the database.
//
// The key derives from the id and never from the path (spec 001, invariant
// 8), which is what makes a move a row update and what lets one bucket carry
// several installations under different prefixes.
package object

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ID names one immutable sequence of bytes: the canonical 36 character text
// of a UUIDv7. Every write of content mints a fresh one, so a key is written
// once and never rewritten, and the bytes a version points at stay
// addressable after an overwrite.
type ID string

// NewID mints the id of one write of content. UUIDv7 leads with a
// millisecond timestamp, so ids sort by the order they were minted in, and
// ends with random bytes, which is what the shard of Key reads.
func NewID() ID { return ID(uuid.Must(uuid.NewV7()).String()) }

// ParseID reads an id in its canonical text form: 36 characters, lowercase
// hexadecimal, four dashes. A UUID in any other spelling the parser accepts,
// braced or prefixed with urn:uuid:, is refused, because a key holds one
// spelling and two spellings of one id would be two keys.
//
// The version is not checked. NewID mints version 7 and every key Arca
// writes carries one, but a store filled before that decision still reads.
func ParseID(text string) (ID, error) {
	u, err := uuid.Parse(text)
	if err != nil {
		return "", fmt.Errorf("object: %q is not a UUID: %w", text, err)
	}
	if u.String() != text {
		return "", fmt.Errorf("object: %q is not the canonical text of %s", text, u)
	}
	return ID(text), nil
}

// Shard is the two characters a key puts the id under: the tail of the id,
// because a UUIDv7 leads with a timestamp and sharding on the head would
// drive every write of one hour into one prefix. The tail is random and
// spreads writes over 256 prefixes.
func (id ID) Shard() string {
	if len(id) != idLen {
		return ""
	}
	return string(id[idLen-2:])
}

// idLen is the length of the canonical text form.
const idLen = 36

// Key is the bucket key of the object: the configured prefix, the shard, and
// the id.
//
//	arca/1f/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f
//
// The prefix is what an operator sets in ARCA_BUCKET_PREFIX, normalised to
// end in a slash before it reaches here. An id that came from neither NewID
// nor ParseID has no key, and Key answers the empty string.
func (id ID) Key(prefix string) string {
	shard := id.Shard()
	if shard == "" {
		return ""
	}
	return prefix + shard + "/" + string(id)
}

// ParseKey reads back the id a key was derived from, so a caller sweeping a
// bucket learns what it is looking at. A key under another prefix, in another
// shape, or whose shard disagrees with its id is refused: the sweep of spec
// 010 compares what it finds against the rows, and a key it cannot read is a
// key it must not delete.
func ParseKey(prefix, key string) (ID, error) {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", fmt.Errorf("object: key %q is not under the prefix %q", key, prefix)
	}
	shard, text, ok := strings.Cut(rest, "/")
	if !ok {
		return "", fmt.Errorf("object: key %q has no shard", key)
	}
	id, err := ParseID(text)
	if err != nil {
		return "", err
	}
	if shard != id.Shard() {
		return "", fmt.Errorf("object: key %q carries the shard %q, and %s shards to %q", key, shard, id, id.Shard())
	}
	return id, nil
}
