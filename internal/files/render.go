// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package files

import (
	"net/http"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/arca/internal/store"
)

// Object is the body every write and every metadata read of one path
// answers, and one entry of a listing. It carries both halves of the
// checksum, so a client can tell a digest of the bytes from the store's
// label for an object Arca never saw (specs 007 and 013).
type Object struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum"`
	ChecksumKind string `json:"checksum_kind"`
	ContentType  string `json:"content_type"`
	IsPublic     bool   `json:"is_public"`
	Modified     string `json:"modified"`
	// URL is where a public object is readable without a token, and is
	// absent on every other object.
	URL string `json:"url,omitempty"`
}

// Version is one entry of a path's history.
type Version struct {
	VersionNo    int    `json:"version_no"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum"`
	ChecksumKind string `json:"checksum_kind"`
	ContentType  string `json:"content_type"`
	CreatedBy    string `json:"created_by"`
	SupersededAt string `json:"superseded_at"`
}

// Trashed is one entry of a space's trash: what was removed, when, and when
// the reaper takes it for good, so a caller knows how long it has.
type Trashed struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	CreatedBy string `json:"created_by"`
	DeletedAt string `json:"deleted_at"`
	PurgesAt  string `json:"purges_at"`
}

// Starred is one entry of the caller's stars, which crosses spaces, so it
// renders the space in full.
type Starred struct {
	Owner       string `json:"owner"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Checksum    string `json:"checksum"`
	ContentType string `json:"content_type"`
	Modified    string `json:"modified"`
}

// Listing is the page a subtree answers: the envelope of spec 013 with the
// common prefixes of spec 005 beside it, synthesised from the rows so a
// browser can render a tree this server does not store.
type Listing struct {
	Entries  []Object `json:"entries"`
	Prefixes []string `json:"prefixes"`
	// Space is what the whole space holds, present on a listing of a plane
	// root and absent on every other. A caller listing the root of a space
	// is asking what that space has, and the answer costs one query beside
	// the page; deeper in the tree the number would answer a question nobody
	// asked, because the ledger counts a space and not a subtree.
	Space      *Space `json:"space,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Space is the usage of spec 010 as a root listing reports it: the bytes the
// ledger counts and the live paths under them. It is the pair of numbers
// spec 012's overview already carries per space, which is deliberate: an
// owner and an administrator read one fact and not two.
type Space struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
}

// Manifest is what a materialize answers: one presigned URL per object of a
// subtree, with every path relative to the root.
type Manifest struct {
	Root string `json:"root"`
	// PinnedAt is null here. This route pins nothing; the leased form of
	// spec 009 is the one that does.
	PinnedAt *string        `json:"pinned_at"`
	Files    []ManifestFile `json:"files"`
}

// ManifestFile is one object of a manifest.
type ManifestFile struct {
	Path     string `json:"path"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
}

// Render renders one row, with the URL a public object is readable at when
// this installation has one to give.
func (s *Service) Render(f store.File) Object {
	o := Object{
		Path: f.Path, Size: f.SizeBytes, Checksum: f.Checksum,
		ChecksumKind: string(f.ChecksumKind), ContentType: f.ContentType,
		IsPublic: f.IsPublic, Modified: stamp(f.UpdatedAt),
	}
	if f.IsPublic && s.cfg.PublicCDNURL != "" {
		o.URL = s.cfg.PublicCDNURL + "/" + f.ObjectID.Key(s.cfg.BucketPrefix)
	}
	return o
}

// version renders one entry of a history.
func version(v store.Version) Version {
	return Version{
		VersionNo: v.VersionNo, Size: v.SizeBytes, Checksum: v.Checksum,
		ChecksumKind: string(v.ChecksumKind), ContentType: v.ContentType,
		CreatedBy: v.CreatedBy, SupersededAt: stamp(v.SupersededAt),
	}
}

// trashed renders one entry of a trash, with the deadline the retention
// window puts on it.
func trashed(f store.File, retention time.Duration) Trashed {
	e := Trashed{Path: f.Path, Size: f.SizeBytes, CreatedBy: f.CreatedBy}
	if f.DeletedAt != nil {
		e.DeletedAt = stamp(*f.DeletedAt)
		e.PurgesAt = stamp(f.DeletedAt.Add(retention))
	}
	return e
}

// starred renders one of the caller's stars.
func starred(s store.Star) Starred {
	return Starred{
		Owner: s.Owner, Path: s.Path, Size: s.File.SizeBytes, Checksum: s.File.Checksum,
		ContentType: s.File.ContentType, Modified: stamp(s.File.UpdatedAt),
	}
}

// stamp renders a time the way every timestamp of this API is rendered: RFC
// 3339 in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// write sends one JSON body.
func write[T any](w http.ResponseWriter, status int, body T) { httpjson.Write(w, status, body) }
