// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build tiers

// The store tier of spec 014: internal/blob against a real MinIO. What it
// proves is what a fake cannot: that a store honors the conditional create
// under a race, that a presigned URL is one key and one method, that a
// listing pages the way the API pages, and that a body corrupted in flight
// is refused.
//
// It runs when E2E_S3_ENDPOINT is set and skips otherwise, and every run
// writes under a prefix of its own, which it removes when the test ends.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// skipWithoutTheStack is the remediation a tier prints when the stack is
// not there. It names both variables and the command that starts them.
const skipWithoutTheStack = "set E2E_DATABASE_URL and E2E_S3_ENDPOINT (make up)"

// stackEndpoint answers MinIO's endpoint, or the reason to skip.
func stackEndpoint() (string, string) {
	endpoint := os.Getenv("E2E_S3_ENDPOINT")
	if endpoint == "" || os.Getenv("E2E_DATABASE_URL") == "" {
		return "", skipWithoutTheStack
	}
	return endpoint, ""
}

// tier opens a client against the MinIO of the stack, under a prefix of
// its own.
func tier(t *testing.T) (*S3, string) {
	t.Helper()
	endpoint, reason := stackEndpoint()
	if reason != "" {
		t.Skip(reason)
	}
	store, err := NewS3(t.Context(), Options{
		Bucket:    envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint:  endpoint,
		Region:    "us-east-1",
		AccessKey: envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey: envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	prefix := fmt.Sprintf("test-%d-%d/", time.Now().UnixNano(), os.Getpid())
	t.Cleanup(func() { sweep(t, store, prefix) })
	return store, prefix
}

// envOr answers the variable or the default of spec 014's table.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// sweep removes everything one run wrote, so the bucket holds nothing of
// it afterwards.
func sweep(t *testing.T, store *S3, prefix string) {
	t.Helper()
	// The test's context is already cancelled when a cleanup runs, and a
	// sweep on a cancelled context leaves the prefix behind.
	ctx := context.WithoutCancel(t.Context())
	for {
		page, err := store.List(ctx, prefix, "", 1000)
		if err != nil {
			t.Errorf("sweep the prefix: %v", err)
			return
		}
		if len(page.Keys) == 0 {
			return
		}
		if err := store.DeleteMany(ctx, page.Keys); err != nil {
			t.Errorf("sweep the prefix: %v", err)
			return
		}
		if !page.Truncated {
			return
		}
	}
}

// minioPartSize is the smallest part a real store accepts for any part but
// the last.
const minioPartSize = 5 << 20

func TestStoreBlobHoldsTheTableEveryStoreHoldsTo(t *testing.T) {
	runStoreTable(t, func(t *testing.T) storeUnderTest {
		t.Helper()
		store, prefix := tier(t)
		return storeUnderTest{
			store:    store,
			prefix:   prefix,
			partSize: minioPartSize,
			uploadPart: func(t *testing.T, key, uploadID string, part int32, body []byte) string {
				t.Helper()
				url, err := store.PresignPart(t.Context(), key, uploadID, part)
				if err != nil {
					t.Fatalf("PresignPart: %v", err)
				}
				return put(t, url, body)
			},
			// The MinIO of the stack answers NotImplemented to an
			// object ACL and offers bucket policies instead, which is
			// the case spec 003 has blob report as ErrNotSupported so
			// the read still answers. It is why the table takes the
			// support as a field rather than assuming it.
			publicSupported: false,
		}
	})
}

// put sends one body to a presigned URL and answers the label the store
// gave it, which is what a client does with a part.
func put(t *testing.T, url string, body []byte) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT the part: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT answered %d: %s", resp.StatusCode, raw)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`)
}

func TestStoreAConditionalCreateHoldsUnderARaceOfWriters(t *testing.T) {
	store, prefix := tier(t)
	key := prefix + "contended"
	body := []byte("the first writer's bytes")
	if _, err := store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	second := []byte("the second writer's bytes")
	_, err := store.Put(t.Context(), key, bytes.NewReader(second), int64(len(second)), PutOptions{})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("the second put = %v, want ErrPreconditionFailed", err)
	}
	if store.Unconditional() {
		t.Fatal("MinIO was recorded as a store without conditional create")
	}
	rc, _, err := store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if held, _ := io.ReadAll(rc); !bytes.Equal(held, body) {
		t.Fatalf("the refused put changed the object to %q", held)
	}
}

func TestStoreAPresignedReadIsOneKeyAndOneMethod(t *testing.T) {
	store, prefix := tier(t)
	key := prefix + "presigned"
	body := []byte("the bytes a redirect points at")
	if _, err := store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	url, err := store.PresignGet(t.Context(), key, PresignOptions{Filename: "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	status, header, read := fetch(t, http.MethodGet, url)
	if status != http.StatusOK || !bytes.Equal(read, body) {
		t.Fatalf("the presigned read answered %d %q", status, read)
	}
	if got := header.Get("Content-Disposition"); !strings.Contains(got, "notes.txt") {
		t.Errorf("the download name is %q", got)
	}
	// The signature covers the method and the key, so neither can be
	// swapped for another.
	if status, _, _ := fetch(t, http.MethodPut, url); status == http.StatusOK {
		t.Error("the signed URL served another method")
	}
	other := strings.Replace(url, "presigned", "another", 1)
	if status, _, _ := fetch(t, http.MethodGet, other); status == http.StatusOK {
		t.Error("the signed URL served another key")
	}
}

// TestStoreAPresignedReadIsRefusedAfterItsExpiry is the third refusal of
// criterion 6 of spec 003: the signature carries a life, and the store is
// what enforces it.
//
// The life a deployment signs is PresignTTL, five minutes, which no test can
// wait out. Options.PresignTTL is the seam that exists for this criterion: a
// second client over the same bucket signs for one second, which is the
// shortest life the S3 API admits, and the test outlives it. The ordinary
// client wrote the key, so what expires is the signature and not the object.
func TestStoreAPresignedReadIsRefusedAfterItsExpiry(t *testing.T) {
	store, prefix := tier(t)
	key := prefix + "expiring"
	body := []byte("the bytes a stale redirect no longer points at")
	if _, err := store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	brief, err := NewS3(t.Context(), Options{
		Bucket:    envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint:  os.Getenv("E2E_S3_ENDPOINT"),
		Region:    "us-east-1",
		AccessKey: envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey: envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle: true, PresignTTL: time.Second,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	url, err := brief.PresignGet(t.Context(), key, PresignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The URL is good while it lives, so what the second request reads is
	// the expiry and not a signature the store never accepted.
	if status, _, read := fetch(t, http.MethodGet, url); status != http.StatusOK || !bytes.Equal(read, body) {
		t.Fatalf("the presigned read answered %d %q inside its life", status, read)
	}
	time.Sleep(2 * time.Second)
	if status, _, read := fetch(t, http.MethodGet, url); status != http.StatusForbidden {
		t.Fatalf("the presigned read answered %d %q past its life, want 403", status, read)
	}
	// The object itself is untouched: the ordinary client still reads it.
	if _, _, err := store.Get(t.Context(), key); err != nil {
		t.Fatalf("the expiry took the object with it: %v", err)
	}
}

// fetch sends one request and answers the status, the headers, and the
// body.
func fetch(t *testing.T, method, url string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s the signed URL: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestStoreAListingPagesOnTheFlagAndNotOnAShortResult(t *testing.T) {
	store, prefix := tier(t)
	for i := range 12 {
		key := fmt.Sprintf("%spaged/%02d", prefix, i)
		if _, err := store.Put(t.Context(), key, strings.NewReader("x"), 1, PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]int{}
	after := ""
	for range 20 {
		page, err := store.List(t.Context(), prefix+"paged/", after, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range page.Keys {
			seen[key]++
		}
		if !page.Truncated {
			break
		}
		after = page.Keys[len(page.Keys)-1]
	}
	if len(seen) != 12 {
		t.Fatalf("the walk saw %d keys of 12", len(seen))
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("%s was listed %d times", key, count)
		}
	}
}

func TestStoreABodyCorruptedInFlightFailsThePutAndLeavesNoKey(t *testing.T) {
	store, prefix := tier(t)
	corrupted, err := NewS3(t.Context(), Options{
		Bucket:     envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint:   os.Getenv("E2E_S3_ENDPOINT"),
		Region:     "us-east-1",
		AccessKey:  envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey:  envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle:  true,
		HTTPClient: &http.Client{Transport: corrupting{inner: http.DefaultTransport}},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := prefix + "corrupted"
	body := []byte("the bytes that leave are not the bytes that arrive")
	if _, err := corrupted.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), PutOptions{}); err == nil {
		t.Fatal("a corrupted body was accepted")
	}
	if _, err := store.Head(t.Context(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the corrupted write left a key behind: %v", err)
	}
}

func TestStoreAnAbortOfAFinishedUploadSucceeds(t *testing.T) {
	store, prefix := tier(t)
	key := prefix + "aborted"
	uploadID, err := store.CreateMultipart(t.Context(), key, PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	url, err := store.PresignPart(t.Context(), key, uploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	etag := put(t, url, bytes.Repeat([]byte("a"), minioPartSize))
	if _, err := store.CompleteMultipart(t.Context(), key, uploadID, []Part{{Number: 1, ETag: etag}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AbortMultipart(t.Context(), key, uploadID); err != nil {
		t.Fatalf("an abort of a finished upload = %v", err)
	}
	if _, err := store.Head(t.Context(), key); err != nil {
		t.Fatalf("the abort removed the assembled object: %v", err)
	}
}

// The bounds the copy cases run the tail under. A source above five
// gibibytes is what the API refuses in one call, and a fixture of that size
// is minutes of a store's time for a branch six lines long, so the tier
// lowers the two bounds instead and copies twelve mebibytes in three ranges.
// Five mebibytes is the smallest part a real store accepts for any part but
// the last, so the part size is that and the limit is just above it.
const (
	tierCopyPartSize = 5 << 20
	tierCopyLimit    = tierCopyPartSize + 1
	tierCopySize     = 12 << 20
)

// tierCopying opens a client whose copy branches at the tier's bounds rather
// than at the API's.
func tierCopying(t *testing.T) (*S3, string) {
	t.Helper()
	endpoint, reason := stackEndpoint()
	if reason != "" {
		t.Skip(reason)
	}
	store, err := NewS3(t.Context(), Options{
		Bucket:       envOr("E2E_S3_BUCKET", "arca-test"),
		Endpoint:     endpoint,
		Region:       "us-east-1",
		AccessKey:    envOr("E2E_S3_KEY", "minioadmin"),
		SecretKey:    envOr("E2E_S3_SECRET", "minioadmin"),
		PathStyle:    true,
		CopyLimit:    tierCopyLimit,
		CopyPartSize: tierCopyPartSize,
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	prefix := fmt.Sprintf("test-%d-%d/", time.Now().UnixNano(), os.Getpid())
	t.Cleanup(func() { sweep(t, store, prefix) })
	return store, prefix
}

// TestStoreACopyMovesASmallObjectServerSide is the one call branch of the
// object move of spec 019 against a real store: the bytes arrive at the
// destination and the source is untouched, with no body crossing the
// network, which is what a server side copy is for.
func TestStoreACopyMovesASmallObjectServerSide(t *testing.T) {
	store, prefix := tier(t)
	from, to := prefix+"drive/u-1/files/notes.md", prefix+"1f/small"
	body := []byte("the bytes one object moves with")
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "text/markdown"}); err != nil {
		t.Fatal(err)
	}

	copied, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copied.Size != int64(len(body)) || copied.ContentType != "text/markdown" {
		t.Errorf("the destination is %+v", copied)
	}
	head, err := store.Head(t.Context(), to)
	if err != nil || head.Size != int64(len(body)) {
		t.Fatalf("Head the destination = %+v, %v", head, err)
	}
	// The label a single copy carries is the digest of the whole object, so
	// the destination reads back under the label the copy answered.
	if head.ETag != copied.ETag {
		t.Errorf("the copy answered %q and the store holds %q", copied.ETag, head.ETag)
	}
	rc, _, err := store.Get(t.Context(), to)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, body) {
		t.Fatalf("the destination reads back as %q", got)
	}
	if _, err := store.Head(t.Context(), from); err != nil {
		t.Fatalf("the copy removed the source: %v", err)
	}
}

// TestStoreACopyOfAPublicObjectIsStampedAndNotCarried holds the move's rule
// for a public row against a real store: a copy carries no ACL, so the
// destination is stamped afterwards, and a store that holds no object ACLs
// says so rather than failing the move.
func TestStoreACopyOfAPublicObjectIsStampedAndNotCarried(t *testing.T) {
	store, prefix := tier(t)
	from, to := prefix+"drive/u-1/files/avatar.png", prefix+"1f/public"
	body := []byte("the bytes of a public object")
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	// The MinIO the stack pins answers NotImplemented to an object ACL and
	// offers bucket policies instead, which spec 003 has blob report as
	// ErrNotSupported. The source is stamped where the store stamps at all,
	// and the move's re-stamp of the destination is read the same way.
	stamped := store.SetPublic(t.Context(), from, true)
	if stamped != nil && !errors.Is(stamped, ErrNotSupported) {
		t.Fatalf("SetPublic on the source = %v", stamped)
	}

	copied, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copied.ContentType != "image/png" {
		t.Errorf("the destination carries %q", copied.ContentType)
	}
	switch err := store.SetPublic(t.Context(), to, true); {
	case stamped == nil && err != nil:
		t.Fatalf("the re-stamp of the destination = %v on a store that stamps", err)
	case stamped != nil && !errors.Is(err, ErrNotSupported):
		t.Fatalf("the re-stamp of the destination = %v on a store without object ACLs", err)
	}
	rc, _, err := store.Get(t.Context(), to)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, body) {
		t.Fatalf("the destination reads back as %q", got)
	}
}

// TestStoreACopyAboveTheLimitGoesThroughTheTail is the multipart branch
// against a real store, at the bounds tierCopying lowers so a twelve
// mebibyte fixture reaches it.
func TestStoreACopyAboveTheLimitGoesThroughTheTail(t *testing.T) {
	store, prefix := tierCopying(t)
	from, to := prefix+"drive/u-1/files/big.bin", prefix+"1f/tailed"
	body := bytes.Repeat([]byte("z"), tierCopySize)
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}

	copied, err := store.Copy(t.Context(), from, to, PutOptions{})
	if err != nil {
		t.Fatalf("Copy through the tail: %v", err)
	}
	if copied.Size != int64(len(body)) {
		t.Errorf("the destination is %d bytes, want %d", copied.Size, len(body))
	}
	// Two ranges of five mebibytes and a remainder, assembled, so the store
	// labels the destination the way it labels an upload in parts.
	if !strings.HasSuffix(copied.ETag, "-3") {
		t.Errorf("the label is %q, want the composite of three parts", copied.ETag)
	}
	head, err := store.Head(t.Context(), to)
	if err != nil || head.Size != int64(len(body)) {
		t.Fatalf("Head the destination = %+v, %v", head, err)
	}
	rc, _, err := store.Get(t.Context(), to)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the destination reads back as %d bytes, %v", len(got), err)
	}
	if _, err := store.Head(t.Context(), from); err != nil {
		t.Fatalf("the tail removed the source: %v", err)
	}
}

// TestStoreTheConditionalCopyIsNotHonoredByEveryStore records what the
// stack's MinIO answers to If-None-Match: * on a copy, which is the finding
// the object move of spec 019 rests its resume on.
//
// The store neither honors the condition nor refuses it: it overwrites. So
// a move cannot read a refusal as "the destination is already there" and
// cannot read a success as "it was not". It reads the destination instead,
// which is what tools/move-objects does before it copies anything. The
// completion of a copied tail is the other half of the finding: the same
// store does hold to the condition there.
func TestStoreTheConditionalCopyIsNotHonoredByEveryStore(t *testing.T) {
	store, prefix := tierCopying(t)
	from, to := prefix+"drive/u-1/files/notes.md", prefix+"1f/contended"
	body := []byte("the first bytes")
	if _, err := store.Put(t.Context(), from, bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	held := []byte("the bytes already at the destination")
	if _, err := store.Put(t.Context(), to, bytes.NewReader(held), int64(len(held)), PutOptions{}); err != nil {
		t.Fatal(err)
	}

	switch _, err := store.Copy(t.Context(), from, to, PutOptions{}); {
	case errors.Is(err, ErrPreconditionFailed):
		t.Log("the store honors the conditional copy")
	case err == nil:
		t.Log("the store ignores the conditional copy and overwrites; the move reads the destination first")
	default:
		t.Fatalf("the conditional copy = %v", err)
	}
	if store.Unconditional() {
		t.Error("a copy recorded the store as refusing the condition, which is a third answer again")
	}

	big, bigTo := prefix+"drive/u-1/files/big.bin", prefix+"1f/contended-tail"
	large := bytes.Repeat([]byte("z"), tierCopySize)
	if _, err := store.Put(t.Context(), big, bytes.NewReader(large), int64(len(large)), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Copy(t.Context(), big, bigTo, PutOptions{}); err != nil {
		t.Fatalf("the first tail: %v", err)
	}
	if _, err := store.Copy(t.Context(), big, bigTo, PutOptions{}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("the second tail onto the same key = %v, want ErrPreconditionFailed", err)
	}
}

// TestStoreTheTierSkipsWithoutTheStack is criterion 5 of spec 014: a tier
// whose variables are unset skips with the remediation in its message.
func TestStoreTheTierSkipsWithoutTheStack(t *testing.T) {
	t.Setenv("E2E_S3_ENDPOINT", "")
	if _, reason := stackEndpoint(); reason != skipWithoutTheStack {
		t.Fatalf("the skip reason is %q", reason)
	}
	t.Setenv("E2E_S3_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("E2E_DATABASE_URL", "")
	if _, reason := stackEndpoint(); reason == "" {
		t.Fatal("a tier ran with half the stack configured")
	}
}
