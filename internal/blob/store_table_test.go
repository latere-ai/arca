// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// storeUnderTest is one implementation the shared table runs against: the
// map, the client over the in-process endpoint, and, under the tiers tag,
// the client over MinIO. Criterion 12 of spec 003 is this table passing for
// each of them.
type storeUnderTest struct {
	// store is the implementation.
	store Store
	// prefix keeps one run's keys apart from another's, which matters
	// against a store that outlives the test.
	prefix string
	// partSize is the smallest part the store accepts. A real store refuses
	// a part below five mebibytes; a fake accepts anything.
	partSize int
	// uploadPart sends one part the way a client would and answers the
	// label the store gave it.
	uploadPart func(t *testing.T, key, uploadID string, part int32, body []byte) string
	// publicSupported says the store stamps an object ACL.
	publicSupported bool
}

// runStoreTable is the one table of behaviours every Store holds to.
func runStoreTable(t *testing.T, open func(t *testing.T) storeUnderTest) {
	t.Helper()

	t.Run("a put reads back as what was written", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "read-back"
		body := []byte("the bytes of one object")
		written, err := s.store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "text/plain"})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		digest := sha256.Sum256(body)
		if written.Size != int64(len(body)) || written.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("Written = %+v, want the size and digest of the body", written)
		}
		if written.ETag == "" {
			t.Error("the store returned no label for the object")
		}
		head, err := s.store.Head(t.Context(), key)
		if err != nil || head.Size != int64(len(body)) || head.ContentType != "text/plain" {
			t.Fatalf("Head = %+v, %v", head, err)
		}
		rc, object, err := s.store.Get(t.Context(), key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer func() { _ = rc.Close() }()
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("Get read %q, %v", got, err)
		}
		if object.Size != int64(len(body)) {
			t.Errorf("Get object = %+v", object)
		}
	})

	t.Run("a put onto a key that exists is refused and changes nothing", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "conditional"
		first := []byte("first")
		if _, err := s.store.Put(t.Context(), key, bytes.NewReader(first), int64(len(first)), PutOptions{}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		second := []byte("second")
		if _, err := s.store.Put(t.Context(), key, bytes.NewReader(second), int64(len(second)), PutOptions{}); !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("the second put returned %v, want ErrPreconditionFailed", err)
		}
		rc, _, err := s.store.Get(t.Context(), key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer func() { _ = rc.Close() }()
		if got, _ := io.ReadAll(rc); !bytes.Equal(got, first) {
			t.Fatalf("the refused put changed the object to %q", got)
		}
	})

	t.Run("a copy lands the bytes at another key and leaves the source", func(t *testing.T) {
		s := open(t)
		from, to := s.prefix+"copy-source", s.prefix+"copy-destination"
		body := []byte("the bytes one object moves with")
		if _, err := s.store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "text/markdown"}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		copied, err := s.store.Copy(t.Context(), from, to, PutOptions{})
		if err != nil {
			t.Fatalf("Copy: %v", err)
		}
		if copied.Size != int64(len(body)) {
			t.Errorf("the destination is %d bytes, want %d", copied.Size, len(body))
		}
		if copied.ContentType != "text/markdown" {
			t.Errorf("the destination carries %q, want the source's media type", copied.ContentType)
		}
		rc, held, err := s.store.Get(t.Context(), to)
		if err != nil {
			t.Fatalf("Get the destination: %v", err)
		}
		defer func() { _ = rc.Close() }()
		if got, _ := io.ReadAll(rc); !bytes.Equal(got, body) {
			t.Fatalf("the destination holds %q", got)
		}
		if held.Size != int64(len(body)) {
			t.Errorf("the destination reads back as %d bytes", held.Size)
		}
		// A move never deletes. The source keys go at the sunset of spec
		// 019 and not with the copy.
		if source, err := s.store.Head(t.Context(), from); err != nil || source.Size != int64(len(body)) {
			t.Fatalf("the copy changed the source: %+v, %v", source, err)
		}
		if _, err := s.store.Copy(t.Context(), s.prefix+"copy-of-nothing", s.prefix+"copy-nowhere", PutOptions{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("a copy of a missing source = %v", err)
		}
	})

	t.Run("a missing key is not found", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "never-written"
		if _, _, err := s.store.Get(t.Context(), key); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get of a missing key = %v", err)
		}
		if _, err := s.store.Head(t.Context(), key); !errors.Is(err, ErrNotFound) {
			t.Errorf("Head of a missing key = %v", err)
		}
	})

	t.Run("a delete removes the key and a second delete succeeds", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "deleted"
		body := []byte("gone soon")
		if _, err := s.store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.store.Delete(t.Context(), key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.store.Head(t.Context(), key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Head after Delete = %v", err)
		}
		if err := s.store.Delete(t.Context(), key); err != nil {
			t.Fatalf("the second Delete = %v, want a success", err)
		}
	})

	t.Run("many keys go at once", func(t *testing.T) {
		s := open(t)
		keys := []string{s.prefix + "many-1", s.prefix + "many-2", s.prefix + "many-3"}
		for _, k := range keys {
			if _, err := s.store.Put(t.Context(), k, strings.NewReader("x"), 1, PutOptions{}); err != nil {
				t.Fatalf("Put %q: %v", k, err)
			}
		}
		if err := s.store.DeleteMany(t.Context(), keys); err != nil {
			t.Fatalf("DeleteMany: %v", err)
		}
		for _, k := range keys {
			if _, err := s.store.Head(t.Context(), k); !errors.Is(err, ErrNotFound) {
				t.Errorf("Head %q after DeleteMany = %v", k, err)
			}
		}
	})

	t.Run("a listing pages on the truncation flag", func(t *testing.T) {
		s := open(t)
		prefix := s.prefix + "listed/"
		for i := range 3 {
			key := fmt.Sprintf("%s%d", prefix, i)
			if _, err := s.store.Put(t.Context(), key, strings.NewReader("x"), 1, PutOptions{}); err != nil {
				t.Fatalf("Put %q: %v", key, err)
			}
		}
		page, err := s.store.List(t.Context(), prefix, "", 2)
		if err != nil || len(page.Keys) != 2 || !page.Truncated {
			t.Fatalf("first page = %+v, %v", page, err)
		}
		next, err := s.store.List(t.Context(), prefix, page.Keys[1], 2)
		if err != nil || len(next.Keys) != 1 || next.Truncated {
			t.Fatalf("second page = %+v, %v", next, err)
		}
		if next.Keys[0] != prefix+"2" {
			t.Fatalf("the second page starts at %q", next.Keys[0])
		}
	})

	t.Run("a presigned read names one key and asks for one name", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "presigned"
		if _, err := s.store.Put(t.Context(), key, strings.NewReader("x"), 1, PutOptions{}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		plain, err := s.store.PresignGet(t.Context(), key, PresignOptions{})
		if err != nil || !strings.Contains(plain, key) {
			t.Fatalf("PresignGet = %q, %v", plain, err)
		}
		if strings.Contains(plain, "content-disposition") {
			t.Errorf("a read with no filename asked for an attachment: %q", plain)
		}
		named, err := s.store.PresignGet(t.Context(), key, PresignOptions{Filename: "notes.txt"})
		if err != nil || !strings.Contains(named, "content-disposition") {
			t.Fatalf("PresignGet with a filename = %q, %v", named, err)
		}
	})

	t.Run("an upload in parts assembles into one object", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "multipart"
		uploadID, err := s.store.CreateMultipart(t.Context(), key, PutOptions{})
		if err != nil {
			t.Fatalf("CreateMultipart: %v", err)
		}
		first := bytes.Repeat([]byte("a"), s.partSize)
		second := []byte("tail")
		parts := []Part{
			{Number: 1, ETag: s.uploadPart(t, key, uploadID, 1, first)},
			{Number: 2, ETag: s.uploadPart(t, key, uploadID, 2, second)},
		}
		object, err := s.store.CompleteMultipart(t.Context(), key, uploadID, parts)
		if err != nil {
			t.Fatalf("CompleteMultipart: %v", err)
		}
		if want := int64(len(first) + len(second)); object.Size != want {
			t.Fatalf("the assembled object is %d bytes, want %d", object.Size, want)
		}
		rc, _, err := s.store.Get(t.Context(), key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer func() { _ = rc.Close() }()
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, append(bytes.Clone(first), second...)) {
			t.Fatalf("the assembled object holds %d bytes of another content", len(got))
		}
	})

	t.Run("an abort of a finished upload succeeds", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "aborted"
		uploadID, err := s.store.CreateMultipart(t.Context(), key, PutOptions{})
		if err != nil {
			t.Fatalf("CreateMultipart: %v", err)
		}
		if err := s.store.AbortMultipart(t.Context(), key, uploadID); err != nil {
			t.Fatalf("AbortMultipart: %v", err)
		}
		if err := s.store.AbortMultipart(t.Context(), key, uploadID); err != nil {
			t.Fatalf("the second AbortMultipart = %v, want a success", err)
		}
	})

	t.Run("publicity is stamped or reported as unsupported", func(t *testing.T) {
		s := open(t)
		key := s.prefix + "public"
		if _, err := s.store.Put(t.Context(), key, strings.NewReader("x"), 1, PutOptions{}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		err := s.store.SetPublic(t.Context(), key, true)
		switch {
		case s.publicSupported && err != nil:
			t.Fatalf("SetPublic: %v", err)
		case !s.publicSupported && !errors.Is(err, ErrNotSupported):
			t.Fatalf("SetPublic on a store without object ACLs = %v", err)
		case !s.publicSupported:
			return
		}
		if err := s.store.SetPublic(t.Context(), key, false); err != nil {
			t.Fatalf("SetPublic false: %v", err)
		}
	})

	t.Run("the bucket answers its probe", func(t *testing.T) {
		s := open(t)
		if err := s.store.HeadBucket(t.Context()); err != nil {
			t.Fatalf("HeadBucket: %v", err)
		}
	})
}
