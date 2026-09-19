// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// seedSource puts one object at from and answers its bytes, which is what
// every case below copies.
func seedSource(t *testing.T, store *S3, from string, size int) []byte {
	t.Helper()
	body := bytes.Repeat([]byte("s"), size)
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "text/markdown"}); err != nil {
		t.Fatalf("seed the source: %v", err)
	}
	return body
}

func TestACopyLandsTheBytesAndTheTypeAtTheDestination(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	from, to := "drive/u-1/files/notes.md", "arca/1f/0192f0c3-6c1a-7b3e-9a2e-6b7c8d9e0a1f"
	body := seedSource(t, store, from, 64)

	copied, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copied.Size != int64(len(body)) {
		t.Errorf("the destination is %d bytes, want %d", copied.Size, len(body))
	}
	if copied.ContentType != "text/markdown" {
		t.Errorf("the destination carries %q, want the source's media type", copied.ContentType)
	}
	if copied.ETag == "" {
		t.Error("the copy answered no label")
	}
	held, ok := fake.Bytes(to)
	if !ok || !bytes.Equal(held, body) {
		t.Fatalf("the destination holds %d bytes of another content", len(held))
	}
	if source, _ := fake.Bytes(from); !bytes.Equal(source, body) {
		t.Error("the copy changed the source")
	}
	// The source is read once for its size and its media type, and the copy
	// is the second call: two round trips, and no body between them.
	if n := fake.Calls("HeadObject"); n != 1 {
		t.Errorf("the copy read the source %d times, want 1", n)
	}
	if n := fake.Calls("CopyObject"); n != 1 {
		t.Errorf("the copy made %d calls, want 1", n)
	}
	if n := fake.Calls("CreateMultipartUpload"); n != 0 {
		t.Errorf("a source under the limit opened %d uploads", n)
	}
}

func TestACopyReplacesTheMediaTypeWhenTheCallerNamesOne(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	from, to := "drive/u-1/files/notes.md", "arca/1f/named"
	seedSource(t, store, from, 8)

	copied, err := store.Copy(t.Context(), from, to, PutOptions{ContentType: "application/json"})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copied.ContentType != "application/json" {
		t.Errorf("the destination carries %q", copied.ContentType)
	}
	held, err := store.Head(t.Context(), to)
	if err != nil || held.ContentType != "application/json" {
		t.Fatalf("Head = %+v, %v", held, err)
	}
}

func TestACopyOntoAKeyThatExistsIsRefusedAndChangesNothing(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	from, to := "drive/u-1/files/notes.md", "arca/1f/taken"
	seedSource(t, store, from, 16)
	held := []byte("the bytes already at the destination")
	if _, err := store.Put(t.Context(), to, bytes.NewReader(held), int64(len(held)), PutOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Copy(t.Context(), from, to, PutOptions{}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("the copy onto an existing key = %v, want ErrPreconditionFailed", err)
	}
	if got, _ := fake.Bytes(to); !bytes.Equal(got, held) {
		t.Fatalf("the refused copy changed the destination to %q", got)
	}
	if store.Unconditional() {
		t.Error("a refusal was read as a store without the condition")
	}
}

func TestACopyOfAMissingSourceIsNotFoundAndNamesTheSource(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	from := "drive/u-1/files/never-written"

	_, err := store.Copy(t.Context(), from, "arca/1f/nothing", PutOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("the copy of a missing source = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), from) {
		t.Errorf("the error names no source: %v", err)
	}
	if n := fake.Calls("CopyObject"); n != 0 {
		t.Errorf("a missing source was copied %d times", n)
	}
}

func TestAStoreWithoutAConditionalCopyRunsDegradedAndCopiesAnyway(t *testing.T) {
	fake := newFakeS3(t, false)
	var log lines
	store := fake.store(t, Options{Logger: slog.New(slog.NewTextHandler(&log, nil))})
	from, to := "drive/u-1/files/notes.md", "arca/1f/unguarded"
	body := seedSource(t, store, from, 32)

	fake.FailOnce("CopyObject", "NotImplemented", http.StatusNotImplemented)
	if _, err := store.Copy(t.Context(), from, to, PutOptions{}); err != nil {
		t.Fatalf("Copy against a store without the condition: %v", err)
	}
	if !store.Unconditional() {
		t.Error("the store was not recorded as running unguarded")
	}
	if got, _ := fake.Bytes(to); !bytes.Equal(got, body) {
		t.Error("the retried copy did not land the bytes")
	}
	if n := fake.Calls("CopyObject"); n != 2 {
		t.Errorf("the copy was attempted %d times, want the guarded one and the retry", n)
	}
	// A second copy no longer carries the guard, so the store is asked once.
	if _, err := store.Copy(t.Context(), from, "arca/1f/unguarded-again", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := fake.Calls("CopyObject"); n != 3 {
		t.Errorf("the second copy made %d calls in total, want 3", n)
	}
	if got := strings.Count(log.String(), "refuses a conditional create"); got != 1 {
		t.Fatalf("the degraded mode was logged %d times:\n%s", got, log.String())
	}
}

func TestACopyEscapesAKeyOutsideAPathsAlphabet(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	// Drive built a key from a path a person named and appended a suffix on
	// a versioned write, so a space and an at sign both reach the source.
	from, to := "drive/u-1/files/my notes.md@ab12ef34cd56", "arca/1f/escaped"
	body := seedSource(t, store, from, 24)

	if _, err := store.Copy(t.Context(), from, to, PutOptions{}); err != nil {
		t.Fatalf("Copy of a key with a space: %v", err)
	}
	if got, _ := fake.Bytes(to); !bytes.Equal(got, body) {
		t.Fatal("the escaped copy did not land the bytes")
	}
	if got := copySource("bucket", from); got != "bucket/drive/u-1/files/my%20notes.md@ab12ef34cd56" {
		t.Errorf("the source is named %q", got)
	}
}

func TestACopyAboveTheLimitGoesThroughTheTail(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{CopyLimit: 8, CopyPartSize: 4})
	from, to := "drive/u-1/files/big.bin", "arca/1f/tailed"
	body := seedSource(t, store, from, 10)

	copied, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copied.Size != int64(len(body)) {
		t.Errorf("the destination is %d bytes, want %d", copied.Size, len(body))
	}
	if copied.ContentType != "text/markdown" {
		t.Errorf("the tail lost the media type: %q", copied.ContentType)
	}
	if !strings.HasSuffix(copied.ETag, "-3") {
		t.Errorf("the label is %q, want the composite of three parts", copied.ETag)
	}
	if got, _ := fake.Bytes(to); !bytes.Equal(got, body) {
		t.Fatalf("the tail assembled %d bytes of another content", len(got))
	}
	if n := fake.Calls("UploadPartCopy"); n != 3 {
		t.Errorf("the tail copied %d ranges, want 3 of a ten byte source at four bytes a part", n)
	}
	if fake.Uploads() != 0 {
		t.Error("the tail left an upload open")
	}
	if n := fake.Calls("UploadPart"); n != 0 {
		t.Error("the tail sent a body instead of a range")
	}
}

func TestATailThatFailsPartWayLeavesNoUploadOpen(t *testing.T) {
	for _, c := range []struct {
		name      string
		operation string
		code      string
		status    int
		want      error
	}{
		{"a range the store refuses", "UploadPartCopy", "InternalError", http.StatusInternalServerError, nil},
		{"a completion the store refuses", "CompleteMultipartUpload", "InternalError", http.StatusInternalServerError, nil},
		{"a destination another writer took first", "CompleteMultipartUpload", "PreconditionFailed", http.StatusPreconditionFailed, ErrPreconditionFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeS3(t, false)
			store := fake.store(t, Options{CopyLimit: 8, CopyPartSize: 4})
			from, to := "drive/u-1/files/big.bin", "arca/1f/abandoned"
			seedSource(t, store, from, 10)

			fake.Fail(c.operation, c.code, c.status)
			_, err := store.Copy(t.Context(), from, to, PutOptions{})
			if err == nil {
				t.Fatal("the failure was not surfaced")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("the copy = %v, want %v", err, c.want)
			}
			fake.Clear(c.operation)
			if fake.Uploads() != 0 {
				t.Error("the failed tail left an upload open, and its parts have no row to reach them by")
			}
			if _, held := fake.Bytes(to); held {
				t.Error("the failed tail left a destination behind")
			}
		})
	}
}

func TestATailWithoutAConditionalCompletionRunsDegraded(t *testing.T) {
	fake := newFakeS3(t, false)
	var log lines
	store := fake.store(t, Options{CopyLimit: 8, CopyPartSize: 4, Logger: slog.New(slog.NewTextHandler(&log, nil))})
	from, to := "drive/u-1/files/big.bin", "arca/1f/unguarded-tail"
	body := seedSource(t, store, from, 10)

	fake.FailOnce("CompleteMultipartUpload", "NotImplemented", http.StatusNotImplemented)
	if _, err := store.Copy(t.Context(), from, to, PutOptions{}); err != nil {
		t.Fatalf("Copy against a store without the condition: %v", err)
	}
	if !store.Unconditional() {
		t.Error("the store was not recorded as running unguarded")
	}
	if got, _ := fake.Bytes(to); !bytes.Equal(got, body) {
		t.Error("the retried completion did not assemble the bytes")
	}
	if fake.Uploads() != 0 {
		t.Error("the retried completion left an upload open")
	}
	if got := strings.Count(log.String(), "refuses a conditional create"); got != 1 {
		t.Fatalf("the degraded mode was logged %d times:\n%s", got, log.String())
	}
}

func TestATailIsRefusedWhenTheStoreLabelsNoPart(t *testing.T) {
	fake := newFakeS3(t, false)
	fake.partCopyWithoutLabel = true
	store := fake.store(t, Options{CopyLimit: 8, CopyPartSize: 4})
	from, to := "drive/u-1/files/big.bin", "arca/1f/unlabelled"
	seedSource(t, store, from, 10)

	_, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "no label") {
		t.Fatalf("a part with no label = %v", err)
	}
	if fake.Uploads() != 0 {
		t.Error("the refusal left an upload open")
	}
}

func TestTheCopyLimitDefaultsToWhatTheAPIHolds(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	if store.copyLimit != DefaultCopyLimit || store.copyPartSize != DefaultCopyPartSize {
		t.Fatalf("the bounds are %d and %d", store.copyLimit, store.copyPartSize)
	}
	// Ten thousand parts is what an upload holds, so the default part size
	// reaches past the largest object the API stores at all.
	if DefaultCopyPartSize*10_000 < 5<<40 {
		t.Error("the default part size cannot carry a five tebibyte object in ten thousand parts")
	}
}

func TestTheMapCopiesTheWayTheClientDoes(t *testing.T) {
	store := NewMemory()
	from, to := "drive/u-1/files/notes.md", "arca/1f/copied"
	body := []byte("the bytes of one object")
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "text/markdown"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPublic(t.Context(), from, true); err != nil {
		t.Fatal(err)
	}

	copied, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copied.Size != int64(len(body)) || copied.ContentType != "text/markdown" {
		t.Errorf("the destination is %+v", copied)
	}
	if got, _ := store.Bytes(to); !bytes.Equal(got, body) {
		t.Errorf("the destination holds %q", got)
	}
	// Publicity does not travel with the bytes, the way an object ACL does
	// not travel with a CopyObject.
	if store.Public(to) {
		t.Error("the copy carried the ACL of the source")
	}
	if _, err := store.Copy(t.Context(), from, to, PutOptions{}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("a copy onto an existing key = %v", err)
	}
	if _, err := store.Copy(t.Context(), "drive/u-1/files/gone", to, PutOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a copy of a missing source = %v", err)
	}
	named, err := store.Copy(t.Context(), from, "arca/1f/named", PutOptions{ContentType: "application/json"})
	if err != nil || named.ContentType != "application/json" {
		t.Errorf("a copy with a media type = %+v, %v", named, err)
	}
}

func TestTheCounterCountsACopy(t *testing.T) {
	inner := NewMemory()
	store := NewCounting(inner)
	from, to := "drive/u-1/files/notes.md", "arca/1f/counted"
	body := []byte("x")
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), 1, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Copy(t.Context(), from, to, PutOptions{}); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n := store.Calls(MethodCopy); n != 1 {
		t.Errorf("the counter saw %d copies", n)
	}
	// A move never deletes, and this is how a test proves it.
	if n := store.Calls(MethodDelete) + store.Calls(MethodDeleteMany); n != 0 {
		t.Errorf("the copy deleted %d times", n)
	}
	store.FailNth(MethodCopy, 2, ErrNotSupported)
	if _, err := store.Copy(t.Context(), from, "arca/1f/refused", PutOptions{}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("the injected failure = %v", err)
	}
}
