// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package blob

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The object move of spec 019 is one server side copy per distinct key, from
// the key a predecessor wrote to the key the object id derives (spec 003).
// The bytes never leave the store: CopyObject moves them inside the bucket,
// and the tail above the API's single copy maximum moves them range by range
// with UploadPartCopy.

// DefaultCopyLimit is the largest source one CopyObject moves. The S3 API
// refuses a single copy of a source above five gibibytes, which is where the
// multipart tail begins.
const DefaultCopyLimit = 5 << 30

// DefaultCopyPartSize is the range one part of a copied tail carries. An
// upload holds ten thousand parts at most, so a gibibyte per part reaches ten
// tebibytes, which is past the five tebibyte object the API holds at all.
const DefaultCopyPartSize = 1 << 30

// Copy moves the bytes of one key to another inside the bucket, server side,
// and answers what the destination now holds.
//
// The destination carries If-None-Match: *, the collision guard of spec 003:
// a key derives from an id that is minted once, so a copy onto a key that
// already holds bytes is a fault and never an overwrite. A store that answers
// NotImplemented to the guard is retried once without it and recorded as
// unconditional, the degraded mode of spec 003. A store that neither honors
// the guard nor refuses it overwrites silently, which is why a caller proving
// a move reads the destination back rather than trusting the copy's answer.
//
// PutOptions names the destination's media type. Empty carries the source's
// metadata over, which is what a move wants; a value replaces it.
//
// The source is read once for its size, because the size is what chooses
// between the one call and the tail, and its media type, because a create of
// a multipart destination names one. So a copy is two round trips and not one.
func (s *S3) Copy(ctx context.Context, from, to string, o PutOptions) (Object, error) {
	source, err := s.head(ctx, "copy the source", from)
	if err != nil {
		return Object{}, err
	}
	if source.Size > s.copyLimit {
		return s.copyInParts(ctx, from, to, source, o)
	}
	return s.copyWhole(ctx, from, to, source, o)
}

// copyWhole moves a source the API takes in one call.
func (s *S3) copyWhole(ctx context.Context, from, to string, source Object, o PutOptions) (Object, error) {
	input := &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(to),
		CopySource: aws.String(copySource(s.bucket, from)),
	}
	if o.ContentType != "" {
		input.ContentType = aws.String(o.ContentType)
		input.MetadataDirective = s3types.MetadataDirectiveReplace
	}
	if !s.unconditional.Load() {
		input.IfNoneMatch = aws.String("*")
	}
	out, err := s.client.CopyObject(ctx, input)
	if err != nil && apiCode(err) == "NotImplemented" && input.IfNoneMatch != nil {
		s.degrade()
		input.IfNoneMatch = nil
		out, err = s.client.CopyObject(ctx, input)
	}
	if err != nil {
		return Object{}, classify("copy", to, err)
	}
	copied := Object{Size: source.Size, ContentType: copiedType(o, source)}
	if out.CopyObjectResult != nil {
		copied.ETag = strings.Trim(aws.ToString(out.CopyObjectResult.ETag), `"`)
	}
	return copied, nil
}

// copyInParts moves a source above the single copy maximum, range by range.
//
// The parts of an abandoned copy are invisible to a listing and carry no
// session row, so nothing in this tree would ever sweep them: the upload is
// aborted on the way out of every failure, on a context the caller's
// cancellation does not reach.
func (s *S3) copyInParts(ctx context.Context, from, to string, source Object, o PutOptions) (Object, error) {
	contentType := copiedType(o, source)
	uploadID, err := s.CreateMultipart(ctx, to, PutOptions{ContentType: contentType})
	if err != nil {
		return Object{}, err
	}
	copied, err := s.copyParts(ctx, from, to, uploadID, source, contentType)
	if err != nil {
		_ = s.AbortMultipart(context.WithoutCancel(ctx), to, uploadID)
		return Object{}, err
	}
	return copied, nil
}

// copyParts copies every range of the source into the open upload and
// assembles them.
func (s *S3) copyParts(ctx context.Context, from, to, uploadID string, source Object, contentType string) (Object, error) {
	sourceKey := copySource(s.bucket, from)
	var parts []s3types.CompletedPart
	for start, number := int64(0), int32(1); start < source.Size; start, number = start+s.copyPartSize, number+1 {
		end := min(start+s.copyPartSize, source.Size) - 1
		out, err := s.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket:          aws.String(s.bucket),
			Key:             aws.String(to),
			UploadId:        aws.String(uploadID),
			PartNumber:      aws.Int32(number),
			CopySource:      aws.String(sourceKey),
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
		})
		if err != nil {
			return Object{}, fmt.Errorf("blob: copy %q to %q: part %d of %d bytes: %w", from, to, number, source.Size, err)
		}
		if out.CopyPartResult == nil || out.CopyPartResult.ETag == nil {
			return Object{}, fmt.Errorf("blob: copy %q to %q: part %d: the store answered no label for the part, "+
				"and a completion names every part by its label", from, to, number)
		}
		parts = append(parts, s3types.CompletedPart{PartNumber: aws.Int32(number), ETag: out.CopyPartResult.ETag})
	}
	return s.completeCopy(ctx, to, uploadID, parts, source, contentType)
}

// completeCopy assembles the copied parts under the same guard the one call
// carries, so both branches of a copy refuse a destination that already
// exists wherever the store honors the condition.
func (s *S3) completeCopy(ctx context.Context, to, uploadID string, parts []s3types.CompletedPart, source Object, contentType string) (Object, error) {
	input := &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(to),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}
	if !s.unconditional.Load() {
		input.IfNoneMatch = aws.String("*")
	}
	out, err := s.client.CompleteMultipartUpload(ctx, input)
	if err != nil && apiCode(err) == "NotImplemented" && input.IfNoneMatch != nil {
		s.degrade()
		input.IfNoneMatch = nil
		out, err = s.client.CompleteMultipartUpload(ctx, input)
	}
	if err != nil {
		return Object{}, classify("copy", to, err)
	}
	return Object{
		Size:        source.Size,
		ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
		ContentType: contentType,
	}, nil
}

// copySource is the bucket and key the API names a source by, escaped the way
// a path is. A key holds whatever a predecessor put in it, and Drive appended
// a suffix to a path that already carried whatever a person named a file, so
// a space or any other character outside a path's alphabet reaches here.
func copySource(bucket, key string) string {
	return (&url.URL{Path: bucket + "/" + key}).EscapedPath()
}

// copiedType is the media type the destination carries: the caller's where
// one is named, and the source's otherwise, because a move keeps what the
// object was written as.
func copiedType(o PutOptions, source Object) string {
	if o.ContentType != "" {
		return o.ContentType
	}
	if source.ContentType != "" {
		return source.ContentType
	}
	return DefaultContentType
}
