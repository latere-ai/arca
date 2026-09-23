// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the label S3 reports for a single part object, not a security claim
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Memory is the bucket in a map, with the error vocabulary of the S3 client:
// the conditional create, the missing key, and the multipart state machine.
// A handler test runs against it and needs no bucket (spec 014, the unit
// tier).
//
// It is not a substitute for the store tier. A fake that accepts a
// conditional create whenever a real store would is the bug invariant 1 is
// written against, so the same table of behaviors runs against MinIO too.
type Memory struct {
	// PresignBase is the origin the signed URLs of this store point at. A
	// test that reads one only checks its shape.
	PresignBase string

	mu         sync.Mutex
	objects    map[string]memoryObject
	uploads    map[string]*memoryUpload
	public     map[string]bool
	nextUpload int
}

type memoryObject struct {
	data        []byte
	contentType string
	etag        string
}

type memoryUpload struct {
	key         string
	contentType string
	parts       map[int32][]byte
}

// NewMemory opens an empty store.
func NewMemory() *Memory {
	return &Memory{
		PresignBase: "https://bucket.invalid",
		objects:     map[string]memoryObject{},
		uploads:     map[string]*memoryUpload{},
		public:      map[string]bool{},
	}
}

// Keys answers every key the store holds, in lexical order, so a test reads
// the bucket without a listing call.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Bytes answers the stored bytes of one key, so a test asserts what a write
// left without reading it back through the interface.
func (m *Memory) Bytes(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return bytes.Clone(o.data), ok
}

// Public reports whether SetPublic stamped the key.
func (m *Memory) Public(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.public[key]
}

// Uploads answers the number of open multipart uploads, so a test proves an
// abort left none.
func (m *Memory) Uploads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.uploads)
}

// UploadPart stores one part of an open upload. The S3 client's caller sends
// a part to a presigned URL, which this store has no listener for, so a test
// that drives the multipart flow calls this instead.
func (m *Memory) UploadPart(key, uploadID string, part int32, body []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[uploadID]
	if !ok || u.key != key {
		return "", fmt.Errorf("blob: upload %q of %q: %w", uploadID, key, ErrNotFound)
	}
	u.parts[part] = bytes.Clone(body)
	return etagOf(body), nil
}

// Put writes the bytes under key, refusing a key the store already holds.
func (m *Memory) Put(_ context.Context, key string, body io.Reader, _ int64, o PutOptions) (Written, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return Written{}, fmt.Errorf("blob: put %q: read body: %w", key, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objects[key]; exists {
		return Written{}, fmt.Errorf("blob: put %q: %w", key, ErrPreconditionFailed)
	}
	sum := sha256.Sum256(data)
	etag := etagOf(data)
	m.objects[key] = memoryObject{data: data, contentType: contentType(o), etag: etag}
	return Written{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ETag: etag}, nil
}

// Copy moves the bytes of one key to another, refusing a destination the
// store already holds.
//
// Publicity does not travel with the bytes, the way an object ACL does not
// travel with a CopyObject: the object move of spec 019 re-stamps the
// destination through SetPublic, and this store keeps that rule, so a test of
// the move against the map proves what it proves against a bucket.
func (m *Memory) Copy(_ context.Context, from, to string, o PutOptions) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	source, held := m.objects[from]
	if !held {
		return Object{}, fmt.Errorf("blob: copy the source %q: %w", from, ErrNotFound)
	}
	if _, exists := m.objects[to]; exists {
		return Object{}, fmt.Errorf("blob: copy %q: %w", to, ErrPreconditionFailed)
	}
	copied := memoryObject{
		data:        bytes.Clone(source.data),
		contentType: source.contentType,
		etag:        source.etag,
	}
	if o.ContentType != "" {
		copied.contentType = o.ContentType
	}
	m.objects[to] = copied
	return object(copied), nil
}

// Get opens the object's body.
func (m *Memory) Get(_ context.Context, key string) (io.ReadCloser, Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, Object{}, fmt.Errorf("blob: get %q: %w", key, ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(o.data)), object(o), nil
}

// Head reads what the store holds about the key.
func (m *Memory) Head(_ context.Context, key string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return Object{}, fmt.Errorf("blob: head %q: %w", key, ErrNotFound)
	}
	return object(o), nil
}

// Delete removes one key, and a key that is already gone is a success.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	delete(m.public, key)
	return nil
}

// DeleteMany removes many keys.
func (m *Memory) DeleteMany(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if err := m.Delete(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// List answers one page of keys under prefix.
func (m *Memory) List(_ context.Context, prefix, startAfter string, max int32) (Listing, error) {
	if max <= 0 {
		return Listing{}, fmt.Errorf("blob: list %q: max is %d, and a page holds at least one key", prefix, max)
	}
	var page Listing
	for _, k := range m.Keys() {
		if !strings.HasPrefix(k, prefix) || k <= startAfter {
			continue
		}
		if int32(len(page.Keys)) == max {
			page.Truncated = true
			break
		}
		page.Keys = append(page.Keys, k)
	}
	return page, nil
}

// PresignGet answers a URL of the shape the S3 client signs. Nothing serves
// it: a test that follows a redirect runs against the store tier.
func (m *Memory) PresignGet(_ context.Context, key string, o PresignOptions) (string, error) {
	q := url.Values{"X-Amz-Expires": {fmt.Sprint(int(PresignTTL.Seconds()))}}
	if o.Filename != "" {
		q.Set("response-content-disposition", fmt.Sprintf("attachment; filename=%q", o.Filename))
	}
	return m.PresignBase + "/" + key + "?" + q.Encode(), nil
}

// CreateMultipart opens an upload on one key.
func (m *Memory) CreateMultipart(_ context.Context, key string, o PutOptions) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextUpload++
	id := fmt.Sprintf("upload-%d", m.nextUpload)
	m.uploads[id] = &memoryUpload{key: key, contentType: contentType(o), parts: map[int32][]byte{}}
	return id, nil
}

// PresignPart answers a URL of the shape the S3 client signs. A test sends
// its part through UploadPart.
func (m *Memory) PresignPart(_ context.Context, key, uploadID string, part int32) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.uploads[uploadID]; !ok || u.key != key {
		return "", fmt.Errorf("blob: presign part %d of %q: %w", part, uploadID, ErrNotFound)
	}
	q := url.Values{
		"uploadId":      {uploadID},
		"partNumber":    {fmt.Sprint(part)},
		"X-Amz-Expires": {fmt.Sprint(int(PartPresignTTL.Seconds()))},
	}
	return m.PresignBase + "/" + key + "?" + q.Encode(), nil
}

// CompleteMultipart assembles the parts in the order the caller lists them.
func (m *Memory) CompleteMultipart(_ context.Context, key, uploadID string, parts []Part) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[uploadID]
	if !ok || u.key != key {
		return Object{}, fmt.Errorf("blob: complete %q: %w", uploadID, ErrNotFound)
	}
	if len(parts) == 0 {
		return Object{}, fmt.Errorf("blob: complete %q: no parts", uploadID)
	}
	var assembled, digests []byte
	for _, p := range parts {
		body, held := u.parts[p.Number]
		if !held {
			return Object{}, fmt.Errorf("blob: complete %q: part %d was never uploaded", uploadID, p.Number)
		}
		if etagOf(body) != strings.Trim(p.ETag, `"`) {
			return Object{}, fmt.Errorf("blob: complete %q: part %d carries another label", uploadID, p.Number)
		}
		assembled = append(assembled, body...)
		sum := md5.Sum(body) //nolint:gosec // the store's own label, reproduced
		digests = append(digests, sum[:]...)
	}
	composite := md5.Sum(digests) //nolint:gosec // the store's own label, reproduced
	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(composite[:]), len(parts))
	m.objects[key] = memoryObject{data: assembled, contentType: u.contentType, etag: etag}
	delete(m.uploads, uploadID)
	return Object{Size: int64(len(assembled)), ETag: etag, ContentType: u.contentType}, nil
}

// AbortMultipart discards an upload, and an upload that is already gone is a
// success.
func (m *Memory) AbortMultipart(_ context.Context, _, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.uploads, uploadID)
	return nil
}

// SetPublic records the ACL this store would stamp.
func (m *Memory) SetPublic(_ context.Context, key string, public bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; !ok {
		return fmt.Errorf("blob: set public %q: %w", key, ErrNotFound)
	}
	if public {
		m.public[key] = true
		return nil
	}
	delete(m.public, key)
	return nil
}

// HeadBucket always answers: the map is there.
func (m *Memory) HeadBucket(context.Context) error { return nil }

// object renders the stored object as the interface answers it.
func object(o memoryObject) Object {
	return Object{Size: int64(len(o.data)), ETag: o.etag, ContentType: o.contentType}
}

// etagOf is the label a store reports for an object written in one piece.
func etagOf(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec // the store's own label, reproduced
	return hex.EncodeToString(sum[:])
}
