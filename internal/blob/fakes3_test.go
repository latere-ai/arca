// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"bufio"
	"bytes"
	"crypto/md5" //nolint:gosec // the label a store reports, reproduced
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is an S3 endpoint in process, serving what the SDK sends: the
// object calls, the batched delete, the listing, the four multipart calls,
// the object ACL, and the bucket probe. It exists because the unit tier has
// no bucket beside it and because the two failures spec 003 names, a store
// that answers NotImplemented and a body corrupted in flight, are cases a
// real store will not produce on demand.
//
// It is not a substitute for MinIO. The store tier runs the same table of
// behaviours against a real store, which is where a signature, a policy, and
// a store's own idea of an ETag are proved.
type fakeS3 struct {
	srv       *httptest.Server
	overTLS   bool
	corruptor func([]byte) []byte

	mu      sync.Mutex
	objects map[string]fakeObject
	parts   map[string]map[int32][]byte
	keys    map[string]string // upload id to key
	acl     map[string]string
	calls   map[string]int
	faults  map[string]fakeFault
	nextID  int
}

type fakeObject struct {
	data        []byte
	contentType string
	etag        string
}

// fakeFault is a refusal the endpoint answers for one operation, either for
// every call or for the next one alone.
type fakeFault struct {
	code   string
	status int
	once   bool
}

// newFakeS3 starts the endpoint. Over TLS the SDK sends a trailing sha256
// the endpoint verifies; over plain HTTP it sends the body unsigned and
// unchecksummed, which is the fallback the client's ETag comparison guards.
func newFakeS3(t *testing.T, overTLS bool) *fakeS3 {
	t.Helper()
	f := &fakeS3{
		overTLS: overTLS,
		objects: map[string]fakeObject{},
		parts:   map[string]map[int32][]byte{},
		keys:    map[string]string{},
		acl:     map[string]string{},
		calls:   map[string]int{},
		faults:  map[string]fakeFault{},
	}
	if overTLS {
		f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	} else {
		f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	}
	t.Cleanup(f.srv.Close)
	return f
}

// store opens a client against the endpoint.
func (f *fakeS3) store(t *testing.T, o Options) *S3 {
	t.Helper()
	o.Endpoint, o.Region, o.PathStyle, o.HTTPClient = f.srv.URL, "us-east-1", true, f.srv.Client()
	if o.Bucket == "" {
		o.Bucket = "bucket"
	}
	if o.AccessKey == "" {
		o.AccessKey, o.SecretKey = "key", "secret"
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 1
	}
	s, err := NewS3(t.Context(), o)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s
}

// Fail makes every call of the operation answer the code with the status.
func (f *fakeS3) Fail(operation, code string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[operation] = fakeFault{code: code, status: status}
}

// FailOnce makes the next call of the operation answer the code with the
// status, and the calls after it run.
func (f *fakeS3) FailOnce(operation, code string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[operation] = fakeFault{code: code, status: status, once: true}
}

// Clear removes an injected refusal.
func (f *fakeS3) Clear(operation string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.faults, operation)
}

// Calls answers how often the operation was called, so a test counts round
// trips rather than interface calls.
func (f *fakeS3) Calls(operation string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[operation]
}

// Bytes answers the stored bytes of a key.
func (f *fakeS3) Bytes(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	return bytes.Clone(o.data), ok
}

// ACL answers the canned ACL stamped on a key.
func (f *fakeS3) ACL(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acl[key]
}

// Uploads answers the number of open multipart uploads.
func (f *fakeS3) Uploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.parts)
}

// serve routes one request to the operation the SDK meant by it.
func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), "bucket")
	key = strings.TrimPrefix(key, "/")
	q := r.URL.Query()
	operation := f.operation(r, key, q)

	f.mu.Lock()
	f.calls[operation]++
	fault, faulty := f.faults[operation]
	if faulty && fault.once {
		delete(f.faults, operation)
	}
	f.mu.Unlock()
	if faulty {
		f.refuse(w, r, fault.status, fault.code)
		return
	}

	switch operation {
	case "PutObject":
		f.putObject(w, r, key)
	case "GetObject":
		f.getObject(w, r, key)
	case "HeadObject":
		f.headObject(w, key)
	case "DeleteObject":
		f.deleteObject(w, key)
	case "DeleteObjects":
		f.deleteObjects(w, r)
	case "ListObjectsV2":
		f.listObjects(w, q)
	case "CreateMultipartUpload":
		f.createMultipart(w, key)
	case "UploadPart":
		f.uploadPart(w, r, q)
	case "CompleteMultipartUpload":
		f.completeMultipart(w, r, key, q.Get("uploadId"))
	case "AbortMultipartUpload":
		f.abortMultipart(w, r, q.Get("uploadId"))
	case "PutObjectAcl":
		f.putACL(w, r, key)
	case "HeadBucket":
		w.WriteHeader(http.StatusOK)
	default:
		f.refuse(w, r, http.StatusBadRequest, "UnknownOperation")
	}
}

// operation names what the SDK asked for, from the method, the path, and the
// query the API puts its sub-resources in.
func (f *fakeS3) operation(r *http.Request, key string, q map[string][]string) string {
	has := func(name string) bool { _, ok := q[name]; return ok }
	switch {
	case key == "" && r.Method == http.MethodHead:
		return "HeadBucket"
	case key == "" && r.Method == http.MethodGet:
		return "ListObjectsV2"
	case key == "" && r.Method == http.MethodPost && has("delete"):
		return "DeleteObjects"
	case r.Method == http.MethodPost && has("uploads"):
		return "CreateMultipartUpload"
	case r.Method == http.MethodPost && has("uploadId"):
		return "CompleteMultipartUpload"
	case r.Method == http.MethodDelete && has("uploadId"):
		return "AbortMultipartUpload"
	case r.Method == http.MethodPut && has("uploadId"):
		return "UploadPart"
	case r.Method == http.MethodPut && has("acl"):
		return "PutObjectAcl"
	case r.Method == http.MethodPut:
		return "PutObject"
	case r.Method == http.MethodGet:
		return "GetObject"
	case r.Method == http.MethodHead:
		return "HeadObject"
	case r.Method == http.MethodDelete:
		return "DeleteObject"
	default:
		return "Unknown"
	}
}

// putObject stores the body, refusing a key that exists when the put carries
// the conditional create and refusing a body whose trailing digest does not
// match what arrived.
func (f *fakeS3) putObject(w http.ResponseWriter, r *http.Request, key string) {
	data, err := f.body(r)
	if err != nil {
		f.refuse(w, r, http.StatusBadRequest, "BadDigest")
		return
	}
	if f.corruptor != nil {
		data = f.corruptor(data)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
		f.refuseLocked(w, r, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	sum := md5.Sum(data) //nolint:gosec // the label the store reports
	etag := hex.EncodeToString(sum[:])
	f.objects[key] = fakeObject{data: data, contentType: r.Header.Get("Content-Type"), etag: etag}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

// body reads the request body, decoding the aws-chunked framing and
// verifying the trailing digest the SDK sends over TLS.
func (f *fakeS3) body(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if r.Header.Get("Content-Encoding") != "aws-chunked" {
		return raw, nil
	}
	data, trailer, err := decodeChunked(raw)
	if err != nil {
		return nil, err
	}
	if want := trailer["x-amz-checksum-sha256"]; want != "" {
		sum := sha256.Sum256(data)
		if base64.StdEncoding.EncodeToString(sum[:]) != want {
			return nil, fmt.Errorf("the trailing digest does not match the bytes")
		}
	}
	return data, nil
}

// decodeChunked reads the aws-chunked framing: a sequence of sized chunks
// and the trailer that follows the zero length one.
func decodeChunked(raw []byte) (data []byte, trailer map[string]string, err error) {
	trailer = map[string]string{}
	br := bufio.NewReader(bytes.NewReader(raw))
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, nil, err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("chunk size %q: %w", strings.TrimSpace(line), err)
		}
		if size == 0 {
			break
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, nil, err
		}
		data = append(data, chunk...)
		if _, err := br.Discard(2); err != nil { // the CRLF after the chunk
			return nil, nil, err
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF && strings.TrimSpace(line) == "" {
			break
		}
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			trailer[name] = value
		}
		if err == io.EOF {
			break
		}
	}
	return data, trailer, nil
}

func (f *fakeS3) getObject(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		f.refuse(w, r, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.Header().Set("ETag", `"`+o.etag+`"`)
	w.Header().Set("Content-Type", o.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(o.data)
}

func (f *fakeS3) headObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	o, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("ETag", `"`+o.etag+`"`)
	w.Header().Set("Content-Type", o.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) deleteObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	delete(f.objects, key)
	delete(f.acl, key)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeS3) deleteObjects(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := xml.Unmarshal(raw, &request); err != nil {
		f.refuse(w, r, http.StatusBadRequest, "MalformedXML")
		return
	}
	var refused []string
	f.mu.Lock()
	for _, o := range request.Objects {
		if strings.HasSuffix(o.Key, "-undeletable") {
			refused = append(refused, o.Key)
			continue
		}
		delete(f.objects, o.Key)
	}
	f.mu.Unlock()
	var body strings.Builder
	body.WriteString(`<DeleteResult>`)
	for _, k := range refused {
		fmt.Fprintf(&body, `<Error><Key>%s</Key><Code>AccessDenied</Code><Message>the key is held</Message></Error>`, k)
	}
	body.WriteString(`</DeleteResult>`)
	f.write(w, http.StatusOK, body.String())
}

func (f *fakeS3) listObjects(w http.ResponseWriter, q map[string][]string) {
	first := func(name string) string {
		if v := q[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	maximum, _ := strconv.Atoi(first("max-keys"))
	if maximum <= 0 {
		maximum = 1000
	}
	prefix, after := first("prefix"), first("start-after")

	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	f.mu.Unlock()
	sort.Strings(keys)

	truncated := len(keys) > maximum
	keys = keys[:min(len(keys), maximum)]
	var body strings.Builder
	fmt.Fprintf(&body, `<ListBucketResult><IsTruncated>%t</IsTruncated>`, truncated)
	for _, k := range keys {
		fmt.Fprintf(&body, `<Contents><Key>%s</Key><Size>0</Size></Contents>`, k)
	}
	body.WriteString(`</ListBucketResult>`)
	f.write(w, http.StatusOK, body.String())
}

func (f *fakeS3) createMultipart(w http.ResponseWriter, key string) {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("upload-%d", f.nextID)
	f.parts[id] = map[int32][]byte{}
	f.keys[id] = key
	f.mu.Unlock()
	f.write(w, http.StatusOK, fmt.Sprintf(
		`<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id))
}

func (f *fakeS3) uploadPart(w http.ResponseWriter, r *http.Request, q map[string][]string) {
	id := q["uploadId"][0]
	number, _ := strconv.Atoi(q["partNumber"][0])
	data, err := f.body(r)
	if err != nil {
		f.refuse(w, r, http.StatusBadRequest, "BadDigest")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts, ok := f.parts[id]
	if !ok {
		f.refuseLocked(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	parts[int32(number)] = data
	sum := md5.Sum(data) //nolint:gosec // the label the store reports
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) completeMultipart(w http.ResponseWriter, r *http.Request, key, id string) {
	var request struct {
		Parts []struct {
			Number int32 `xml:"PartNumber"`
		} `xml:"Part"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = xml.Unmarshal(raw, &request)

	f.mu.Lock()
	defer f.mu.Unlock()
	parts, ok := f.parts[id]
	if !ok {
		f.refuseLocked(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	var assembled, digests []byte
	for _, p := range request.Parts {
		body, held := parts[p.Number]
		if !held {
			f.refuseLocked(w, r, http.StatusBadRequest, "InvalidPart")
			return
		}
		assembled = append(assembled, body...)
		sum := md5.Sum(body) //nolint:gosec // the label the store reports
		digests = append(digests, sum[:]...)
	}
	composite := md5.Sum(digests) //nolint:gosec // the label the store reports
	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(composite[:]), len(request.Parts))
	f.objects[key] = fakeObject{data: assembled, contentType: DefaultContentType, etag: etag}
	delete(f.parts, id)
	delete(f.keys, id)
	f.writeLocked(w, http.StatusOK, fmt.Sprintf(
		`<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>&quot;%s&quot;</ETag></CompleteMultipartUploadResult>`, key, etag))
}

func (f *fakeS3) abortMultipart(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.parts[id]; !ok {
		f.refuseLocked(w, r, http.StatusNotFound, "NoSuchUpload")
		return
	}
	delete(f.parts, id)
	delete(f.keys, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeS3) putACL(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok {
		f.refuseLocked(w, r, http.StatusNotFound, "NoSuchKey")
		return
	}
	f.acl[key] = r.Header.Get("X-Amz-Acl")
	w.WriteHeader(http.StatusOK)
}

// refuse answers the error document the SDK reads a code from.
func (f *fakeS3) refuse(w http.ResponseWriter, r *http.Request, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuseLocked(w, r, status, code)
}

func (f *fakeS3) refuseLocked(w http.ResponseWriter, r *http.Request, status int, code string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	f.writeLocked(w, status, fmt.Sprintf(
		`<Error><Code>%s</Code><Message>the endpoint refused the request</Message></Error>`, code))
}

func (f *fakeS3) write(w http.ResponseWriter, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeLocked(w, status, body)
}

func (f *fakeS3) writeLocked(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+body)
}
