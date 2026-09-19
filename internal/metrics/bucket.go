// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package metrics

import (
	"context"
	"errors"
	"io"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/arca/internal/blob"
)

// The bucket's half of spec 018's table, as a decorator over
// [blob.Store] rather than as calls inside it.
//
// A decorator is what keeps the bucket contract of spec 003 to the S3 API
// and this spec's names to this package: internal/blob knows nothing about a
// registry, a recording site cannot be forgotten in a method added later
// without the compiler saying so, and a test of that package drives the
// store with nothing wrapped.
//
// Each call is one span named bucket.<op>, beside the HTTP transport span
// pkg/otel adds, because the transport span names the method and this one
// names the operation. The transfer a presigned URL carries is between the
// client and the bucket and produces no span, which is invariant 4 of spec
// 001: the span of a presign ends when the URL is signed.

// The op label values, spelled here so a method and its series cannot drift.
const (
	opGet               = "get"
	opPut               = "put"
	opHead              = "head"
	opDelete            = "delete"
	opList              = "list"
	opPresign           = "presign"
	opMultipartCreate   = "multipart_create"
	opMultipartComplete = "multipart_complete"
	opMultipartAbort    = "multipart_abort"
)

// Bucket wraps a store so every call it makes is counted, timed, and
// spanned. The wrapped store answers exactly what the inner one answers.
func (s *Set) Bucket(inner blob.Store) blob.Store { return &bucket{inner: inner, set: s} }

// bucket is the decorator. It holds no state: the counters are the set's.
type bucket struct {
	inner blob.Store
	set   *Set
}

// observe runs one call inside its span and records it. It is the one place
// a bucket call is timed, so every method below is the call and one line.
func (b *bucket) observe(ctx context.Context, op string, call func(context.Context) error) error {
	ctx, end := otel.Start(ctx, "bucket."+op)
	started := time.Now()
	err := call(ctx)
	end(err)
	took := time.Since(started)
	b.set.BucketOps.Inc(map[string]string{"op": op, "result": result(err)})
	b.set.BucketOpSeconds.Observe(map[string]string{"op": op}, took.Seconds())
	return err
}

// result classifies one call for the result label. The three refusals spec
// 003 names are their own values, because a missing object and a store that
// is down are different facts and an alert reads only the second.
func result(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, blob.ErrNotFound):
		return "not_found"
	case errors.Is(err, blob.ErrPreconditionFailed):
		return "exists"
	default:
		return "error"
	}
}

func (b *bucket) Put(ctx context.Context, key string, body io.Reader, size int64, o blob.PutOptions) (blob.Written, error) {
	var out blob.Written
	err := b.observe(ctx, opPut, func(ctx context.Context) (err error) {
		out, err = b.inner.Put(ctx, key, body, size, o)
		return err
	})
	return out, err
}

func (b *bucket) Get(ctx context.Context, key string) (io.ReadCloser, blob.Object, error) {
	var body io.ReadCloser
	var obj blob.Object
	err := b.observe(ctx, opGet, func(ctx context.Context) (err error) {
		body, obj, err = b.inner.Get(ctx, key)
		return err
	})
	return body, obj, err
}

func (b *bucket) Head(ctx context.Context, key string) (blob.Object, error) {
	var obj blob.Object
	err := b.observe(ctx, opHead, func(ctx context.Context) (err error) {
		obj, err = b.inner.Head(ctx, key)
		return err
	})
	return obj, err
}

func (b *bucket) Delete(ctx context.Context, key string) error {
	return b.observe(ctx, opDelete, func(ctx context.Context) error {
		return b.inner.Delete(ctx, key)
	})
}

// DeleteMany is one call of the delete op and not one per key: the store
// takes a thousand keys per round trip, and a series counting keys would
// read as a store being hammered when it is being used well.
func (b *bucket) DeleteMany(ctx context.Context, keys []string) error {
	return b.observe(ctx, opDelete, func(ctx context.Context) error {
		return b.inner.DeleteMany(ctx, keys)
	})
}

func (b *bucket) List(ctx context.Context, prefix, startAfter string, max int32) (blob.Listing, error) {
	var out blob.Listing
	err := b.observe(ctx, opList, func(ctx context.Context) (err error) {
		out, err = b.inner.List(ctx, prefix, startAfter, max)
		return err
	})
	return out, err
}

// PresignGet signs a download. The URL is counted once it exists: the
// transfer it authorizes never reaches this process, so nothing here can
// observe it and nothing pretends to.
func (b *bucket) PresignGet(ctx context.Context, key string, o blob.PresignOptions) (string, error) {
	var url string
	err := b.observe(ctx, opPresign, func(ctx context.Context) (err error) {
		url, err = b.inner.PresignGet(ctx, key, o)
		return err
	})
	if err == nil {
		b.set.Presigned("get", "download")
	}
	return url, err
}

func (b *bucket) PresignPart(ctx context.Context, key, uploadID string, part int32) (string, error) {
	var url string
	err := b.observe(ctx, opPresign, func(ctx context.Context) (err error) {
		url, err = b.inner.PresignPart(ctx, key, uploadID, part)
		return err
	})
	if err == nil {
		b.set.Presigned("put", "part")
	}
	return url, err
}

func (b *bucket) CreateMultipart(ctx context.Context, key string, o blob.PutOptions) (string, error) {
	var id string
	err := b.observe(ctx, opMultipartCreate, func(ctx context.Context) (err error) {
		id, err = b.inner.CreateMultipart(ctx, key, o)
		return err
	})
	return id, err
}

func (b *bucket) CompleteMultipart(ctx context.Context, key, uploadID string, parts []blob.Part) (blob.Object, error) {
	var obj blob.Object
	err := b.observe(ctx, opMultipartComplete, func(ctx context.Context) (err error) {
		obj, err = b.inner.CompleteMultipart(ctx, key, uploadID, parts)
		return err
	})
	return obj, err
}

func (b *bucket) AbortMultipart(ctx context.Context, key, uploadID string) error {
	return b.observe(ctx, opMultipartAbort, func(ctx context.Context) error {
		return b.inner.AbortMultipart(ctx, key, uploadID)
	})
}

// Copy is the object move of spec 019. Spec 018's op vocabulary has no
// member for it, and a value outside a closed vocabulary is not recorded, so
// the call passes through with its span and no counter. It is a call a
// migration makes and never one a request makes, so a series over it would
// read as flat for the life of an installation.
func (b *bucket) Copy(ctx context.Context, from, to string, o blob.PutOptions) (blob.Object, error) {
	ctx, end := otel.Start(ctx, "bucket.copy")
	obj, err := b.inner.Copy(ctx, from, to, o)
	end(err)
	return obj, err
}

// SetPublic is the object ACL. Spec 018's op vocabulary has no member for
// it, and a value outside a closed vocabulary is not recorded, so the call
// passes through with its span and no counter.
func (b *bucket) SetPublic(ctx context.Context, key string, public bool) error {
	ctx, end := otel.Start(ctx, "bucket.set_public")
	err := b.inner.SetPublic(ctx, key, public)
	end(err)
	return err
}

// HeadBucket is the readiness probe of spec 002. It runs on the cluster's
// schedule rather than on a request, so counting it would drown the series
// that answer whether requests are reaching the store.
func (b *bucket) HeadBucket(ctx context.Context) error { return b.inner.HeadBucket(ctx) }
