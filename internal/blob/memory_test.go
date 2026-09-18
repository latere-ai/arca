// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// memoryUnderTest opens a map store for the shared table.
func memoryUnderTest(t *testing.T) storeUnderTest {
	t.Helper()
	m := NewMemory()
	return storeUnderTest{
		store:    m,
		prefix:   "arca/",
		partSize: 8,
		uploadPart: func(t *testing.T, key, uploadID string, part int32, body []byte) string {
			t.Helper()
			etag, err := m.UploadPart(key, uploadID, part, body)
			if err != nil {
				t.Fatalf("UploadPart: %v", err)
			}
			return etag
		},
		publicSupported: true,
	}
}

func TestMemoryHoldsTheTableEveryStoreHoldsTo(t *testing.T) {
	runStoreTable(t, memoryUnderTest)
}

func TestMemoryAnswersWhatItHolds(t *testing.T) {
	m := NewMemory()
	if _, err := m.Put(t.Context(), "arca/1f/one", strings.NewReader("one"), 3, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(t.Context(), "arca/2a/two", strings.NewReader("two"), 3, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Keys(), ","); got != "arca/1f/one,arca/2a/two" {
		t.Errorf("Keys() = %s", got)
	}
	if body, ok := m.Bytes("arca/1f/one"); !ok || string(body) != "one" {
		t.Errorf("Bytes() = %q, %v", body, ok)
	}
	if _, ok := m.Bytes("arca/1f/absent"); ok {
		t.Error("Bytes() found a key that was never written")
	}
	if m.Public("arca/1f/one") {
		t.Error("an object is public before SetPublic")
	}
	if err := m.SetPublic(t.Context(), "arca/1f/one", true); err != nil || !m.Public("arca/1f/one") {
		t.Errorf("SetPublic: %v", err)
	}
	if err := m.SetPublic(t.Context(), "arca/1f/absent", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPublic on a missing key = %v", err)
	}
	if err := m.Delete(t.Context(), "arca/1f/one"); err != nil {
		t.Fatal(err)
	}
	if m.Public("arca/1f/one") {
		t.Error("a deleted object kept its ACL")
	}
}

func TestMemoryRefusesABodyItCannotRead(t *testing.T) {
	m := NewMemory()
	if _, err := m.Put(t.Context(), "k", failingReader{}, -1, PutOptions{}); err == nil {
		t.Fatal("a body that fails to read was accepted")
	}
}

// failingReader fails on the first read, the way a client that hung up does.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestMemoryRefusesAPageOfNoKeys(t *testing.T) {
	m := NewMemory()
	if _, err := m.List(t.Context(), "arca/", "", 0); err == nil {
		t.Fatal("a page of no keys was accepted")
	}
}

func TestMemoryHoldsTheMultipartStateMachine(t *testing.T) {
	m := NewMemory()
	key := "arca/1f/parts"
	uploadID, err := m.CreateMultipart(t.Context(), key, PutOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Uploads() != 1 {
		t.Fatalf("Uploads() = %d, want 1", m.Uploads())
	}
	url, err := m.PresignPart(t.Context(), key, uploadID, 1)
	if err != nil || !strings.Contains(url, "partNumber=1") {
		t.Fatalf("PresignPart = %q, %v", url, err)
	}
	if _, err := m.PresignPart(t.Context(), key, "no-such-upload", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("PresignPart of an unknown upload = %v", err)
	}
	if _, err := m.UploadPart(key, "no-such-upload", 1, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("UploadPart of an unknown upload = %v", err)
	}
	etag, err := m.UploadPart(key, uploadID, 1, []byte("hello "))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompleteMultipart(t.Context(), key, "no-such-upload", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("CompleteMultipart of an unknown upload = %v", err)
	}
	if _, err := m.CompleteMultipart(t.Context(), key, uploadID, nil); err == nil {
		t.Error("a completion with no parts was accepted")
	}
	if _, err := m.CompleteMultipart(t.Context(), key, uploadID, []Part{{Number: 9, ETag: etag}}); err == nil {
		t.Error("a completion naming a part that was never uploaded was accepted")
	}
	if _, err := m.CompleteMultipart(t.Context(), key, uploadID, []Part{{Number: 1, ETag: "another-label"}}); err == nil {
		t.Error("a completion naming another label for a part was accepted")
	}
	if _, err := m.UploadPart(key, uploadID, 2, []byte("world")); err != nil {
		t.Fatal(err)
	}
	object, err := m.CompleteMultipart(t.Context(), key, uploadID, []Part{
		{Number: 1, ETag: etag},
		{Number: 2, ETag: etagOf([]byte("world"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if object.Size != 11 || object.ContentType != "text/plain" || !strings.HasSuffix(object.ETag, "-2") {
		t.Fatalf("the assembled object = %+v", object)
	}
	if m.Uploads() != 0 {
		t.Errorf("Uploads() = %d after the completion", m.Uploads())
	}
	if body, _ := m.Bytes(key); string(body) != "hello world" {
		t.Fatalf("the assembled object holds %q", body)
	}
	if err := m.AbortMultipart(t.Context(), key, uploadID); err != nil {
		t.Errorf("an abort of a finished upload = %v", err)
	}
}
