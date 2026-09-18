// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// s3UnderTest opens the client over the in-process endpoint, on the
// connection the case asks for: plain HTTP, where integrity rests on the
// ETag comparison, and TLS, where the store verifies the trailing digest.
func s3UnderTest(overTLS bool) func(t *testing.T) storeUnderTest {
	return func(t *testing.T) storeUnderTest {
		t.Helper()
		fake := newFakeS3(t, overTLS)
		store := fake.store(t, Options{})
		return storeUnderTest{
			store:    store,
			prefix:   "arca/",
			partSize: 8,
			uploadPart: func(t *testing.T, key, uploadID string, part int32, body []byte) string {
				t.Helper()
				return uploadPartTo(t, fake, store, key, uploadID, part, body)
			},
			publicSupported: true,
		}
	}
}

// uploadPartTo sends one part the way a client does: a PUT against the
// presigned URL, with nothing of the server in between.
func uploadPartTo(t *testing.T, fake *fakeS3, store *S3, key, uploadID string, part int32, body []byte) string {
	t.Helper()
	url, err := store.PresignPart(t.Context(), key, uploadID, part)
	if err != nil {
		t.Fatalf("PresignPart: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	resp, err := fake.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT the part: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT the part answered %d", resp.StatusCode)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`)
}

func TestStoreOverPlainHTTPHoldsTheTableEveryStoreHoldsTo(t *testing.T) {
	runStoreTable(t, s3UnderTest(false))
}

func TestStoreOverTLSHoldsTheTableEveryStoreHoldsTo(t *testing.T) {
	runStoreTable(t, s3UnderTest(true))
}

func TestTheConnectionDecidesHowIntegrityIsChecked(t *testing.T) {
	plain := newFakeS3(t, false)
	if plain.store(t, Options{}).trailing {
		t.Error("a plain HTTP endpoint was given the trailing digest, which its unsigned payload defeats")
	}
	tls := newFakeS3(t, true)
	if !tls.store(t, Options{}).trailing {
		t.Error("a TLS endpoint was not given the trailing digest")
	}
	if s, err := NewS3(t.Context(), Options{Bucket: "b", Region: "us-east-1"}); err != nil || !s.trailing {
		t.Errorf("an endpoint left to the SDK's default = %v, %v", s, err)
	}
}

// TestABodyCorruptedInFlightFailsThePutAndLeavesNoKey is criterion 5 of spec
// 003, once per connection: over TLS the store refuses the bytes because the
// trailing digest does not match them, and over plain HTTP the client
// refuses them because the label the store returned does not match the
// digest it computed, and removes the key it wrote.
func TestABodyCorruptedInFlightFailsThePutAndLeavesNoKey(t *testing.T) {
	for _, overTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", overTLS), func(t *testing.T) {
			fake := newFakeS3(t, overTLS)
			client := fake.srv.Client()
			client.Transport = corrupting{inner: client.Transport}
			store := fake.store(t, Options{HTTPClient: client})

			body := []byte("the bytes that leave are not the bytes that arrive")
			_, err := store.Put(t.Context(), "arca/1f/corrupted", bytes.NewReader(body), int64(len(body)), PutOptions{})
			if err == nil {
				t.Fatal("a corrupted body was accepted")
			}
			if _, held := fake.Bytes("arca/1f/corrupted"); held {
				t.Fatalf("the corrupted write left a key behind: %v", err)
			}
		})
	}
}

// corrupting flips one byte of every request body, the way a faulty link
// does. It sits in the transport rather than in the body, so the digest the
// SDK computed is taken before the corruption, which is what makes the two
// mechanisms observable.
type corrupting struct{ inner http.RoundTripper }

func (c corrupting) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body == nil || r.Method != http.MethodPut {
		return c.inner.RoundTrip(r)
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	if i := bytes.LastIndexByte(raw, 'b'); i >= 0 {
		raw[i] = 'B'
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return c.inner.RoundTrip(r)
}

// TestAStoreWithoutConditionalCreateRunsDegradedAndSaysSoOnce is criterion 4
// of spec 003. The check subcommand that reports it is spec 012's; what this
// client owes is the retry, the record, and one line in the log.
func TestAStoreWithoutConditionalCreateRunsDegradedAndSaysSoOnce(t *testing.T) {
	fake := newFakeS3(t, false)
	var log lines
	store := fake.store(t, Options{Logger: slog.New(slog.NewTextHandler(&log, nil))})
	fake.FailOnce("PutObject", "NotImplemented", http.StatusNotImplemented)

	body := []byte("written anyway")
	if _, err := store.Put(t.Context(), "arca/1f/degraded", bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
		t.Fatalf("Put against a store without conditional create: %v", err)
	}
	if !store.Unconditional() {
		t.Error("the run was not recorded as unconditional")
	}
	if held, ok := fake.Bytes("arca/1f/degraded"); !ok || !bytes.Equal(held, body) {
		t.Fatalf("the retried put stored %q, %v", held, ok)
	}
	// The second write of the same key now overwrites, which is the
	// degraded mode itself, and says nothing further in the log.
	if _, err := store.Put(t.Context(), "arca/1f/degraded", bytes.NewReader(body), int64(len(body)), PutOptions{}); err != nil {
		t.Fatalf("the second put: %v", err)
	}
	if got := strings.Count(log.String(), "refuses a conditional create"); got != 1 {
		t.Fatalf("the degraded mode was logged %d times:\n%s", got, log.String())
	}
}

func TestAStreamedPutCannotBeRetriedWithoutTheCondition(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	fake.FailOnce("PutObject", "NotImplemented", http.StatusNotImplemented)

	_, err := store.Put(t.Context(), "arca/1f/streamed", strings.NewReader("a body of unknown length"), -1, PutOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot be sent twice") {
		t.Fatalf("Put = %v, want the refusal that names the body it cannot rewind", err)
	}
	if !store.Unconditional() {
		t.Error("the store was not recorded as unconditional")
	}
}

func TestAStreamedPutDigestsWhatItSent(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	body := "a body whose length the caller does not know"
	written, err := store.Put(t.Context(), "arca/1f/streamed", strings.NewReader(body), -1, PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if written.Size != int64(len(body)) {
		t.Errorf("Written.Size = %d, want %d", written.Size, len(body))
	}
	if held, _ := fake.Bytes("arca/1f/streamed"); string(held) != body {
		t.Errorf("the store holds %q", held)
	}
}

func TestAReadThatFailsIsNotAMissingObject(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	fake.Fail("GetObject", "SlowDown", http.StatusServiceUnavailable)
	rc, _, err := store.Get(t.Context(), "arca/1f/throttled")
	if rc != nil {
		_ = rc.Close()
	}
	if err == nil {
		t.Fatal("a throttled read answered a body")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a throttled read answered as a missing object: %v", err)
	}
	fake.Clear("GetObject")
	fake.Fail("HeadObject", "SlowDown", http.StatusServiceUnavailable)
	if _, err := store.Head(t.Context(), "arca/1f/throttled"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a throttled head = %v", err)
	}
}

func TestEveryCallSurfacesTheStoresRefusal(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	body := []byte("x")
	if _, err := store.Put(t.Context(), "arca/1f/refused", bytes.NewReader(body), 1, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	uploadID, err := store.CreateMultipart(t.Context(), "arca/1f/parts", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		operation string
		call      func() error
	}{
		{"PutObject", func() error {
			_, err := store.Put(t.Context(), "arca/1f/new", bytes.NewReader(body), 1, PutOptions{})
			return err
		}},
		{"DeleteObject", func() error { return store.Delete(t.Context(), "arca/1f/refused") }},
		{"DeleteObjects", func() error { return store.DeleteMany(t.Context(), []string{"arca/1f/refused"}) }},
		{"ListObjectsV2", func() error {
			_, err := store.List(t.Context(), "arca/", "", 10)
			return err
		}},
		{"CreateMultipartUpload", func() error {
			_, err := store.CreateMultipart(t.Context(), "arca/1f/parts", PutOptions{})
			return err
		}},
		{"CompleteMultipartUpload", func() error {
			_, err := store.CompleteMultipart(t.Context(), "arca/1f/parts", uploadID, []Part{{Number: 1, ETag: "x"}})
			return err
		}},
		{"AbortMultipartUpload", func() error { return store.AbortMultipart(t.Context(), "arca/1f/parts", uploadID) }},
		{"PutObjectAcl", func() error { return store.SetPublic(t.Context(), "arca/1f/refused", true) }},
		{"HeadBucket", func() error { return store.HeadBucket(t.Context()) }},
	} {
		fake.Fail(c.operation, "InternalError", http.StatusInternalServerError)
		if err := c.call(); err == nil {
			t.Errorf("%s: a refusal was not surfaced", c.operation)
		}
		fake.Clear(c.operation)
	}
}

func TestAStoreWithoutObjectACLsSaysSo(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	body := []byte("x")
	if _, err := store.Put(t.Context(), "arca/1f/acl", bytes.NewReader(body), 1, PutOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"NotImplemented", "MethodNotAllowed", "AccessControlListNotSupported", "UnsupportedOperation"} {
		fake.Fail("PutObjectAcl", code, http.StatusNotImplemented)
		if err := store.SetPublic(t.Context(), "arca/1f/acl", true); !errors.Is(err, ErrNotSupported) {
			t.Errorf("SetPublic against a store answering %s = %v", code, err)
		}
	}
	fake.Clear("PutObjectAcl")
	if err := store.SetPublic(t.Context(), "arca/1f/absent", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPublic on a missing key = %v", err)
	}
	if err := store.SetPublic(t.Context(), "arca/1f/acl", true); err != nil {
		t.Fatal(err)
	}
	if got := fake.ACL("arca/1f/acl"); got != "public-read" {
		t.Errorf("the stamped ACL is %q", got)
	}
	if err := store.SetPublic(t.Context(), "arca/1f/acl", false); err != nil {
		t.Fatal(err)
	}
	if got := fake.ACL("arca/1f/acl"); got != "private" {
		t.Errorf("the removed ACL is %q", got)
	}
}

// TestManyKeysGoInOneCallPerThousand is criterion 8 of spec 003: the round
// trips are counted at the endpoint, because the interface call is one
// whatever the number of keys.
func TestManyKeysGoInOneCallPerThousand(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	keys := make([]string, 0, 2500)
	for i := range 2500 {
		keys = append(keys, fmt.Sprintf("arca/1f/many-%04d", i))
	}
	keys = append(keys, "arca/1f/held-undeletable")
	if err := store.DeleteMany(t.Context(), keys); err == nil {
		t.Fatal("the key the endpoint refused was not reported")
	} else if !strings.Contains(err.Error(), "held-undeletable") {
		t.Fatalf("the failure does not name the key: %v", err)
	}
	if got := fake.Calls("DeleteObjects"); got != 3 {
		t.Fatalf("2501 keys took %d calls, want 3", got)
	}
	if err := store.DeleteMany(t.Context(), nil); err != nil {
		t.Errorf("DeleteMany of no keys = %v", err)
	}
}

func TestTheCompletionReadsTheAssembledSizeBack(t *testing.T) {
	fake := newFakeS3(t, false)
	store := fake.store(t, Options{})
	key := "arca/1f/assembled"
	uploadID, err := store.CreateMultipart(t.Context(), key, PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	etag := uploadPartTo(t, fake, store, key, uploadID, 1, []byte("one part"))
	fake.Fail("HeadObject", "InternalError", http.StatusInternalServerError)
	if _, err := store.CompleteMultipart(t.Context(), key, uploadID, []Part{{Number: 1, ETag: etag}}); err == nil {
		t.Fatal("a completion whose size could not be read answered an object")
	}
}

func TestCredentialsFallThroughToTheChain(t *testing.T) {
	s, err := NewS3(t.Context(), Options{Bucket: "bucket", Region: "us-east-1", Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewS3 without static credentials: %v", err)
	}
	if s.bucket != "bucket" {
		t.Errorf("bucket = %q", s.bucket)
	}
}

// lines collects a log for an assertion about what was written.
type lines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
