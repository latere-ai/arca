// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the label a store reports for a single part object, compared against the ETag
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Options configures the S3 client. The values come from the ARCA_BUCKET_*
// variables of spec 002; no provider is named in code, and an endpoint left
// empty falls through to the SDK's default for the region.
type Options struct {
	// Bucket is the bucket every key is written to.
	Bucket string
	// Endpoint is the S3 endpoint. Its scheme decides how integrity is
	// checked: over TLS the store verifies a trailing digest, over plain
	// HTTP the client compares the ETag it got against the digest it
	// computed.
	Endpoint string
	// Region is the signing region.
	Region string
	// AccessKey and SecretKey are static credentials. Both empty falls
	// through to the SDK's credential chain.
	AccessKey, SecretKey string
	// PathStyle addresses the bucket in the path rather than in the host,
	// for a store without virtual hosts.
	PathStyle bool
	// Logger records the degraded mode of a store without conditional
	// create. Nil is slog.Default.
	Logger *slog.Logger
	// HTTPClient sends the requests. Nil is the SDK's own client, which is
	// what a deployment uses; a test tier passes a client that trusts the
	// certificate of the endpoint it started.
	HTTPClient s3.HTTPClient
	// MaxAttempts bounds the SDK's retry of a request the store failed
	// transiently. Zero is the SDK's own budget, which is what a deployment
	// runs; a test that injects a refusal sets one, so the refusal is
	// answered once rather than waited on.
	MaxAttempts int
	// CopyLimit is the largest source Copy moves in one call, and
	// CopyPartSize is the range one part of the tail above it carries. Zero
	// is DefaultCopyLimit and DefaultCopyPartSize, the API's own maxima,
	// which is what a deployment runs. Both are a seam for a test: a tier
	// lowers them so the tail runs against a real store inside a test's time
	// budget rather than on a five gibibyte fixture. Neither is
	// configuration, and no ARCA_* variable reaches either.
	CopyLimit, CopyPartSize int64
	// PresignTTL is how long a signed download stays valid. Zero is
	// PresignTTL, the constant spec 015 fixes, which is what a deployment
	// runs. It is a seam for a test and nothing else: criterion 6 of spec
	// 003 requires a presigned GET refused after its expiry, and a tier
	// cannot wait five minutes for one, so it signs a second client's URL
	// with a second's life and outlives it. It is not configuration, and no
	// ARCA_* variable reaches it.
	PresignTTL time.Duration
}

// S3 is the bucket over the S3 API.
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	log     *slog.Logger

	// trailing says the store is reached over TLS, where a put carries a
	// trailing sha256 the store verifies on receipt. Over plain HTTP the
	// body streams off the request socket unsigned, which is what lets a
	// store reached without TLS accept a streamed upload at all and what
	// defeats the signed trailer, so integrity falls back to the ETag
	// comparison of Put.
	trailing bool

	// unconditional records a store that answered NotImplemented to
	// If-None-Match: *. The run then writes without the guard, which is a
	// degraded mode spec 012's check reports and this client logs once.
	unconditional atomic.Bool
	warnOnce      sync.Once

	// copyLimit and copyPartSize are what Copy branches on, resolved from
	// the options once so the call site reads one field and not a default.
	copyLimit, copyPartSize int64

	// presignTTL is the life PresignGet signs, resolved the same way.
	presignTTL time.Duration
}

// S3 is a Store.
var _ Store = (*S3)(nil)

// putBufferLimit bounds what a put holds in memory. A body at or under it
// becomes a bytes.Reader, which is seekable, so the SDK retries a transient
// failure; a larger body streams unbuffered and accepts one attempt, because
// a retry cannot rewind a socket.
const putBufferLimit = 8 << 20

// deleteBatch bounds one DeleteObjects call, which the API caps at a
// thousand keys.
const deleteBatch = 1000

// NewS3 opens a client. Credentials are the static pair when both are set
// and the SDK's chain otherwise, which is what lets a deployment hold its
// credentials in a role rather than in two variables.
func NewS3(ctx context.Context, o Options) (*S3, error) {
	opts := s3.Options{Region: o.Region, UsePathStyle: o.PathStyle, HTTPClient: o.HTTPClient}
	if o.MaxAttempts > 0 {
		opts.RetryMaxAttempts = o.MaxAttempts
	}
	if o.Endpoint != "" {
		opts.BaseEndpoint = aws.String(o.Endpoint)
	}
	switch {
	case o.AccessKey != "" && o.SecretKey != "":
		opts.Credentials = credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, "")
	default:
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(o.Region))
		if err != nil {
			return nil, fmt.Errorf("blob: the credential chain: %w", err)
		}
		opts.Credentials = cfg.Credentials
	}
	client := s3.New(opts)
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &S3{
		client:       client,
		presign:      s3.NewPresignClient(client),
		bucket:       o.Bucket,
		log:          log,
		trailing:     !strings.HasPrefix(o.Endpoint, "http://"),
		copyLimit:    orDefault(o.CopyLimit, DefaultCopyLimit),
		copyPartSize: orDefault(o.CopyPartSize, DefaultCopyPartSize),
		presignTTL:   orDefaultTTL(o.PresignTTL, PresignTTL),
	}, nil
}

// orDefaultTTL reads a life an option left at zero as the constant it stands
// for. It is orDefault over a duration, which that one's int64 cannot carry.
func orDefaultTTL(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// orDefault reads a bound an option left at zero as the default it stands for.
func orDefault(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

// Put writes the bytes under key and answers what the store holds.
//
// Integrity has one primary mechanism and one fallback (spec 003). Over TLS
// the put carries a trailing sha256 the store verifies on receipt, so a body
// corrupted in flight is refused by the store. Over plain HTTP the put is
// signed with an unsigned payload, which defeats the trailer, so the client
// compares the ETag the store returned against the digest it computed in the
// same pass and deletes the key on a mismatch. A multipart or encrypted ETag
// has another shape than 32 hex characters and is skipped.
func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64, o PutOptions) (Written, error) {
	var (
		sha, md5hex string
		stream      *hashing
	)
	if size >= 0 && size <= putBufferLimit {
		buf, err := io.ReadAll(body)
		if err != nil {
			return Written{}, fmt.Errorf("blob: put %q: read body: %w", key, err)
		}
		digest := sha256.Sum256(buf)
		label := md5.Sum(buf) //nolint:gosec // compared against the store's ETag, not a security claim
		sha, md5hex = hex.EncodeToString(digest[:]), hex.EncodeToString(label[:])
		body, size = bytes.NewReader(buf), int64(len(buf))
	} else {
		stream = newHashing(body)
		body = stream
	}

	out, err := s.putObject(ctx, key, body, size, o)
	if err != nil {
		return Written{}, err
	}
	if stream != nil {
		sha, md5hex, size = stream.sha256(), stream.md5(), stream.read
	}

	etag := strings.Trim(aws.ToString(out.ETag), `"`)
	if !s.trailing && singlePartETag(etag) && etag != md5hex {
		// The bytes the store holds are not the bytes that were sent. The
		// key is removed before the error returns, so no row can point at
		// a corrupted object.
		_ = s.Delete(context.WithoutCancel(ctx), key)
		return Written{}, fmt.Errorf("blob: put %q: the store holds %s and the bytes sent digest to %s", key, etag, md5hex)
	}
	return Written{Size: size, SHA256: sha, ETag: etag}, nil
}

// putObject sends one put, with the conditional create unless the store has
// already refused it, and retries once without it when the store answers
// NotImplemented and the body can rewind.
func (s *S3) putObject(ctx context.Context, key string, body io.Reader, size int64, o PutOptions) (*s3.PutObjectOutput, error) {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType(o)),
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	if s.trailing {
		input.ChecksumAlgorithm = s3types.ChecksumAlgorithmSha256
	}
	if !s.unconditional.Load() {
		input.IfNoneMatch = aws.String("*")
	}
	out, err := s.client.PutObject(ctx, input, s.signing)
	switch {
	case err == nil:
		return out, nil
	case apiCode(err) == "PreconditionFailed":
		return nil, fmt.Errorf("blob: put %q: %w", key, ErrPreconditionFailed)
	case apiCode(err) == "NotImplemented" && input.IfNoneMatch != nil:
		s.degrade()
		seeker, ok := body.(io.Seeker)
		if !ok {
			return nil, fmt.Errorf("blob: put %q: the store refuses a conditional create and the body cannot be sent twice: %w", key, err)
		}
		if _, serr := seeker.Seek(0, io.SeekStart); serr != nil {
			return nil, fmt.Errorf("blob: put %q: rewind the body: %w", key, serr)
		}
		input.IfNoneMatch = nil
		out, err = s.client.PutObject(ctx, input, s.signing)
		if err != nil {
			return nil, fmt.Errorf("blob: put %q: %w", key, err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("blob: put %q: %w", key, err)
	}
}

// signing keeps a plain HTTP connection usable: a body that streams off the
// request socket is not seekable, so its payload hash cannot be computed
// before the request is signed, and the put is signed UNSIGNED-PAYLOAD. Over
// TLS the SDK does that for itself and the trailing digest stays signed.
func (s *S3) signing(o *s3.Options) {
	if !s.trailing {
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	}
}

// degrade records and logs the store without conditional create, once per
// process.
func (s *S3) degrade() {
	s.unconditional.Store(true)
	s.warnOnce.Do(func() {
		s.log.Warn("the store refuses a conditional create; writes run unguarded",
			"bucket", s.bucket, "remedy", "a store that honors If-None-Match: *")
	})
}

// Unconditional reports whether the store refused the conditional create, so
// readiness and the check command of spec 012 name a degraded installation.
func (s *S3) Unconditional() bool { return s.unconditional.Load() }

// Get opens the object's body. The caller closes it.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, Object{}, classify("get", key, err)
	}
	return out.Body, Object{
		Size:        aws.ToInt64(out.ContentLength),
		ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
		ContentType: aws.ToString(out.ContentType),
	}, nil
}

// Head reads what the store holds about the key, without its body.
func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	return s.head(ctx, "head", key)
}

// head is the read behind Head, with the operation the error names as an
// argument: a Copy reads its source through it, and a caller of Copy that
// meets ErrNotFound reads which key the store does not hold.
func (s *S3) head(ctx context.Context, op, key string) (Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Object{}, classify(op, key, err)
	}
	return Object{
		Size:        aws.ToInt64(out.ContentLength),
		ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
		ContentType: aws.ToString(out.ContentType),
	}, nil
}

// Delete removes one key. The API answers a missing key with a success, and
// so does this.
func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("blob: delete %q: %w", key, err)
	}
	return nil
}

// DeleteMany removes up to a thousand keys per round trip and aggregates the
// per key failures, so dropping a large subtree costs one call per thousand
// keys instead of one per key.
func (s *S3) DeleteMany(ctx context.Context, keys []string) error {
	var errs []error
	for start := 0; start < len(keys); start += deleteBatch {
		end := min(start+deleteBatch, len(keys))
		ids := make([]s3types.ObjectIdentifier, 0, end-start)
		for _, k := range keys[start:end] {
			ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("blob: delete %d keys: %w", end-start, err))
			continue
		}
		for _, e := range out.Errors {
			errs = append(errs, fmt.Errorf("blob: delete %q: %s", aws.ToString(e.Key), aws.ToString(e.Message)))
		}
	}
	return errors.Join(errs...)
}

// List answers one page of keys under prefix. A caller pages on the
// truncation flag and never on a short result: a store may answer fewer keys
// than asked while more remain.
func (s *S3) List(ctx context.Context, prefix, startAfter string, max int32) (Listing, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:     aws.String(s.bucket),
		Prefix:     aws.String(prefix),
		StartAfter: aws.String(startAfter),
		MaxKeys:    aws.Int32(max),
	})
	if err != nil {
		return Listing{}, fmt.Errorf("blob: list %q: %w", prefix, err)
	}
	page := Listing{Keys: make([]string, 0, len(out.Contents)), Truncated: aws.ToBool(out.IsTruncated)}
	for _, o := range out.Contents {
		page.Keys = append(page.Keys, aws.ToString(o.Key))
	}
	return page, nil
}

// PresignGet signs one key, one method, and one expiry.
func (s *S3) PresignGet(ctx context.Context, key string, o PresignOptions) (string, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if o.Filename != "" {
		input.ResponseContentDisposition = aws.String(fmt.Sprintf("attachment; filename=%q", o.Filename))
	}
	req, err := s.presign.PresignGetObject(ctx, input, s3.WithPresignExpires(s.presignTTL))
	if err != nil {
		return "", fmt.Errorf("blob: presign %q: %w", key, err)
	}
	return req.URL, nil
}

// CreateMultipart opens an upload on one key. Its parts are invisible to a
// listing, so the session row of spec 004 is the only durable pointer to
// them.
func (s *S3) CreateMultipart(ctx context.Context, key string, o PutOptions) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType(o)),
	})
	if err != nil {
		return "", fmt.Errorf("blob: create multipart %q: %w", key, err)
	}
	return aws.ToString(out.UploadId), nil
}

// PresignPart signs one PUT for one part, so the bytes go from the client to
// the bucket.
func (s *S3) PresignPart(ctx context.Context, key, uploadID string, part int32) (string, error) {
	req, err := s.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(part),
	}, s3.WithPresignExpires(PartPresignTTL))
	if err != nil {
		return "", fmt.Errorf("blob: presign part %d of %q: %w", part, key, err)
	}
	return req.URL, nil
}

// CompleteMultipart assembles the parts and heads the result, because the
// completion answers a label and not a size, and the size is what the
// session of spec 007 is checked against.
func (s *S3) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error) {
	completed := make([]s3types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		completed = append(completed, s3types.CompletedPart{
			PartNumber: aws.Int32(p.Number),
			ETag:       aws.String(p.ETag),
		})
	}
	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		return Object{}, fmt.Errorf("blob: complete multipart %q: %w", key, err)
	}
	return s.Head(ctx, key)
}

// AbortMultipart discards an upload and its parts. An upload the store no
// longer holds is a success: that end state is the goal, and failing would
// make the reaper retry a dead upload forever.
func (s *S3) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil && apiCode(err) != "NoSuchUpload" {
		return fmt.Errorf("blob: abort multipart %q: %w", key, err)
	}
	return nil
}

// SetPublic stamps the public-read ACL on a key, or removes it. Because the
// key does not change when the path does, making an object public is a
// header change and never a re-upload.
func (s *S3) SetPublic(ctx context.Context, key string, public bool) error {
	acl := s3types.ObjectCannedACLPrivate
	if public {
		acl = s3types.ObjectCannedACLPublicRead
	}
	_, err := s.client.PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		ACL:    acl,
	})
	if err == nil {
		return nil
	}
	if aclUnsupported(apiCode(err)) {
		return fmt.Errorf("blob: set public %q: %w", key, ErrNotSupported)
	}
	return classify("set public", key, err)
}

// HeadBucket is the readiness check: the bucket is there and the credentials
// reach it.
func (s *S3) HeadBucket(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("blob: head bucket %q: %w", s.bucket, err)
	}
	return nil
}

// classify maps a refusal the caller branches on and wraps everything else.
// The match is on the API error code rather than a concrete SDK type, so it
// holds across stores.
func classify(op, key string, err error) error {
	switch apiCode(err) {
	case "NoSuchKey", "NotFound":
		return fmt.Errorf("blob: %s %q: %w", op, key, ErrNotFound)
	case "PreconditionFailed":
		return fmt.Errorf("blob: %s %q: %w", op, key, ErrPreconditionFailed)
	default:
		return fmt.Errorf("blob: %s %q: %w", op, key, err)
	}
}

// apiCode answers the store's error code, or the empty string for a
// transport failure that never reached it.
func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// aclUnsupported names the codes a store answers when it holds no object
// ACLs and offers bucket policies instead.
func aclUnsupported(code string) bool {
	switch code {
	case "NotImplemented", "MethodNotAllowed", "AccessControlListNotSupported", "UnsupportedOperation":
		return true
	default:
		return false
	}
}

// singlePartETag reports whether the label has the shape of a digest of the
// whole object: 32 hex characters. A multipart or encrypted ETag has another
// shape, and comparing it against a digest of the bytes would fail a healthy
// write.
func singlePartETag(etag string) bool {
	if len(etag) != 32 {
		return false
	}
	_, err := hex.DecodeString(etag)
	return err == nil
}

// hashing digests a body as it streams to the store, so one pass over an
// unbuffered upload yields both the object's checksum and the label to
// compare the store's ETag against.
type hashing struct {
	inner io.Reader
	sha   hashWriter
	label hashWriter
	read  int64
}

// hashWriter is what the two digests have in common.
type hashWriter interface {
	io.Writer
	Sum(b []byte) []byte
}

func newHashing(r io.Reader) *hashing {
	return &hashing{inner: r, sha: sha256.New(), label: md5.New()} //nolint:gosec // compared against the store's ETag
}

func (h *hashing) Read(p []byte) (int, error) {
	n, err := h.inner.Read(p)
	if n > 0 {
		h.read += int64(n)
		_, _ = h.sha.Write(p[:n])
		_, _ = h.label.Write(p[:n])
	}
	return n, err
}

func (h *hashing) sha256() string { return hex.EncodeToString(h.sha.Sum(nil)) }
func (h *hashing) md5() string    { return hex.EncodeToString(h.label.Sum(nil)) }
