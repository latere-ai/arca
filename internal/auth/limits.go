// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"fmt"
	"strings"

	"latere.ai/x/arca/object"
)

// Limits is the limits object of one answer (spec 006), read whole. Arca
// stores no limit: each member holds for the answer that carried it and for
// the question that answer decided, and an answer that carries none leaves
// the zero value, which limits nothing.
//
// It is read in one place so the members are honored or refused together. A
// handler that read only the member it acts on would admit a request whose
// answer carried another member it could not read.
type Limits struct {
	// QuotaBytes is limits.quota_bytes, the bytes the space may hold for
	// this answer's ttl, and nil where the answer named none (spec 010).
	QuotaBytes *int64
	// NoVersions is limits.versions: false. The write this answer admits
	// keeps no version of the content it replaces (spec 028).
	NoVersions bool
	// NoTrash is limits.trash: false. The delete this answer admits is
	// permanent (spec 028).
	NoTrash bool
	// Omit is limits.omit, the subtrees this answer leaves out of an
	// enumeration: every path at or under one of the prefixes (spec 028).
	// It is read on a question that enumerates and shapes no question about
	// one object.
	Omit []string
}

// ReadLimits decodes an answer's limits object. A member that is not the
// type spec 006 gives it, and an omit entry that is not a path of a space,
// is an error: the endpoint said something this server cannot honor, and
// acting on the rest of the answer would ignore a limit the endpoint set. A
// handler answers it as no decision and writes nothing.
//
// A member this server does not know is not read, so an endpoint may carry
// members for a newer server.
func ReadLimits(raw json.RawMessage) (Limits, error) {
	if len(raw) == 0 {
		return Limits{}, nil
	}
	var wire struct {
		QuotaBytes *int64    `json:"quota_bytes"`
		Versions   *bool     `json:"versions"`
		Trash      *bool     `json:"trash"`
		Omit       *[]string `json:"omit"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Limits{}, fmt.Errorf("auth: read the answer's limits: %w", err)
	}
	limits := Limits{
		QuotaBytes: wire.QuotaBytes,
		NoVersions: wire.Versions != nil && !*wire.Versions,
		NoTrash:    wire.Trash != nil && !*wire.Trash,
	}
	if wire.Omit != nil {
		for i, prefix := range *wire.Omit {
			if err := omittable(prefix); err != nil {
				return Limits{}, fmt.Errorf("auth: read limits.omit[%d]: %w", i, err)
			}
		}
		limits.Omit = *wire.Omit
	}
	return limits, nil
}

// omittable holds one entry of limits.omit to the shape of a path of a
// space: rooted in the plane with something under it, no leading or trailing
// slash, and no segment that is empty, relative, or holds a control
// character. The plane on its own is not an entry: an endpoint that leaves
// out everything denies the question.
func omittable(prefix string) error {
	if _, _, err := object.SplitPath(prefix); err != nil {
		return err
	}
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("the prefix %q ends in a slash", prefix)
	}
	for segment := range strings.SplitSeq(prefix, "/") {
		switch segment {
		case "":
			return fmt.Errorf("the prefix %q has an empty segment", prefix)
		case ".", "..":
			return fmt.Errorf("the prefix %q names a segment that is not a name", prefix)
		}
	}
	if strings.ContainsFunc(prefix, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("the prefix holds a character this server does not store")
	}
	return nil
}
