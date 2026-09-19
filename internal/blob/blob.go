// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package blob is the bucket behind one interface. Every call Arca makes
// against an S3 compatible store is here and nowhere else: put, copy, get,
// head, delete, presign, multipart, the object ACL, and the bucket probe.
//
// Keys are opaque here. The package object derives them from an object id,
// so this package knows nothing about who owns a byte or what path it sits
// at, which is what makes a move a row update (spec 001, invariant 8).
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

// Store is the bucket. The S3 client of this package implements it against
// any store that serves the S3 API; Memory implements it in process for a
// test that needs no bucket.
type Store interface {
	// Put writes body under key and answers what the store holds. Every put
	// carries If-None-Match: *, so a second write of one key is refused
	// rather than served: with a fresh id per write of content, a collision
	// is a fault and never an overwrite. A size below zero means unknown.
	Put(ctx context.Context, key string, body io.Reader, size int64, o PutOptions) (Written, error)
	// Copy moves the bytes of one key to another inside the bucket, server
	// side, and answers what the destination holds. The destination carries
	// the same If-None-Match: * as a put, and a source above the API's single
	// copy maximum is moved range by range. The call carries no ACL: the
	// object move of spec 019 re-stamps a public destination through
	// SetPublic.
	Copy(ctx context.Context, from, to string, o PutOptions) (Object, error)
	// Get opens the object's body. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
	// Head reads what the store holds about an object, without its body.
	Head(ctx context.Context, key string) (Object, error)
	// Delete removes one key. A key that is already gone is a success.
	Delete(ctx context.Context, key string) error
	// DeleteMany removes many keys in one round trip per thousand and
	// aggregates the per key failures into one error.
	DeleteMany(ctx context.Context, keys []string) error
	// List answers up to max keys under prefix after startAfter, with the
	// store's truncation flag. A caller pages on the flag and never on a
	// short result.
	List(ctx context.Context, prefix, startAfter string, max int32) (Listing, error)
	// PresignGet signs one key, one method, and one expiry, so a private
	// download leaves the server as a redirect.
	PresignGet(ctx context.Context, key string, o PresignOptions) (string, error)
	// CreateMultipart opens an upload on one key and answers the store's
	// upload id.
	CreateMultipart(ctx context.Context, key string, o PutOptions) (uploadID string, err error)
	// PresignPart signs one PUT for one part of one upload, so the bytes go
	// from the client to the bucket and never through the server.
	PresignPart(ctx context.Context, key, uploadID string, part int32) (string, error)
	// CompleteMultipart assembles the parts and answers the object.
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error)
	// AbortMultipart discards an upload and its parts. An upload that is
	// already gone is a success: that end state is the goal, and failing
	// would make the reaper retry a dead upload forever.
	AbortMultipart(ctx context.Context, key, uploadID string) error
	// SetPublic stamps the public-read ACL on a key, or removes it. A store
	// that offers bucket policies instead answers ErrNotSupported.
	SetPublic(ctx context.Context, key string, public bool) error
	// HeadBucket is the readiness check of spec 002: the bucket is there and
	// the credentials reach it.
	HeadBucket(ctx context.Context) error
}

// The three outcomes a caller branches on. Everything else is wrapped and
// surfaced: a throttle or an outage must never reach a caller as a missing
// object (spec 001, invariant 2).
var (
	// ErrNotFound is a key the store does not hold.
	ErrNotFound = errors.New("blob: object not found")
	// ErrPreconditionFailed is a put onto a key that already exists. The
	// stored bytes are untouched.
	ErrPreconditionFailed = errors.New("blob: key already exists")
	// ErrNotSupported is a call the store does not serve, which today is the
	// object ACL of SetPublic.
	ErrNotSupported = errors.New("blob: the store does not support this call")
)

// PresignTTL bounds a presigned download. A presigned URL is a bearer
// credential for one object, so a short life bounds what a leaked redirect
// is worth (spec 015).
const PresignTTL = 5 * time.Minute

// PartPresignTTL bounds a presigned part upload. Long enough for a slow
// browser to send a multi-gigabyte body with pauses; the session expiry of
// spec 007 is the real end of life.
const PartPresignTTL = 24 * time.Hour

// DefaultContentType is what an object carries when a put names none.
const DefaultContentType = "application/octet-stream"

// PutOptions carries what the store records beside the bytes.
type PutOptions struct {
	// ContentType is the media type the store answers on a read. Empty is
	// DefaultContentType.
	ContentType string
}

// PresignOptions shapes one signed download.
type PresignOptions struct {
	// Filename, when set, makes the store answer the object as an
	// attachment under that name, so a browser saves the file under its own
	// name instead of the key. Empty leaves the object inline, which is what
	// a preview and a programmatic fetch want.
	Filename string
}

// Written is what a put stored.
type Written struct {
	// Size is the number of bytes the store received.
	Size int64
	// SHA256 is the hex digest of those bytes, computed as they were
	// written.
	SHA256 string
	// ETag is the store's label for the object, without its quotes.
	ETag string
}

// Object is what the store holds about one key.
type Object struct {
	// Size is the object's length in bytes.
	Size int64
	// ETag is the store's label for the object, without its quotes. For an
	// object assembled from parts it is a digest of digests and not of the
	// bytes.
	ETag string
	// ContentType is the media type the object was written with.
	ContentType string
}

// Listing is one page of keys.
type Listing struct {
	// Keys are the keys of this page, in the store's lexical order.
	Keys []string
	// Truncated says more keys remain after this page.
	Truncated bool
}

// Part pairs a part number with the label the store returned for it.
type Part struct {
	// Number is the part number, from 1 upward.
	Number int32
	// ETag is what the store answered when the part was uploaded.
	ETag string
}

// contentType is the media type a put records: the caller's, or the default.
func contentType(o PutOptions) string {
	if o.ContentType == "" {
		return DefaultContentType
	}
	return o.ContentType
}
