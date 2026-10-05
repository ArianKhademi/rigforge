// Package storage wraps the S3 API that Cloudflare R2 (and MinIO in dev)
// speaks. The api only needs a handful of calls, so they sit behind a small
// interface that tests replace with Fake.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound = errors.New("object not found")
	// ErrNoSuchUpload: the multipart upload id is unknown to the bucket; it was
	// already completed or aborted.
	ErrNoSuchUpload = errors.New("no such multipart upload")
	// ErrInvalidPart: complete was called with an ETag the bucket does not have
	// for that part number.
	ErrInvalidPart = errors.New("invalid part")
)

type CompletedPart struct {
	PartNumber int
	ETag       string
}

// StoredPart is a part as the bucket sees it, from ListParts.
type StoredPart struct {
	PartNumber   int
	ETag         string
	Size         int64
	LastModified time.Time
}

type ObjectInfo struct {
	Size int64
	ETag string
}

type ObjectStore interface {
	CreateMultipartUpload(ctx context.Context, key, contentType string) (uploadID string, err error)
	// PresignUploadPart returns a URL the browser can PUT one part to. The URL
	// embeds a signature for exactly this key, upload id and part number.
	PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int, ttl time.Duration) (string, error)
	ListParts(ctx context.Context, key, uploadID string) ([]StoredPart, error)
	CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) error
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error

	HeadObject(ctx context.Context, key string) (ObjectInfo, error)
	// PresignGet returns a short-lived download URL. A non-empty
	// downloadFilename makes the browser save the file under that name.
	PresignGet(ctx context.Context, key string, ttl time.Duration, downloadFilename string) (string, error)
	PutObject(ctx context.Context, key, contentType string, body io.Reader, size int64) error
	// DeletePrefix removes every object under the prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// NormalizeETag strips the quotes S3 puts around ETags so they compare equal
// no matter which side (PUT response header or ListParts XML) they came from.
func NormalizeETag(etag string) string {
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag[1 : len(etag)-1]
	}
	return etag
}
