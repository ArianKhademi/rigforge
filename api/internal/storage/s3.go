package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type S3Config struct {
	Endpoint        string
	PublicEndpoint  string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

type S3 struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
}

func NewS3(ctx context.Context, c S3Config) (*S3, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(c.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("s3 config: %w", err)
	}
	options := func(endpoint string) func(*s3.Options) {
		return func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			// Path-style (endpoint/bucket/key) works on R2 and MinIO alike and
			// needs no per-bucket DNS.
			o.UsePathStyle = true
			// Newer SDKs add a CRC32 checksum to every request by default and,
			// for presigned URLs, sign a checksum header the browser would then
			// have to send. R2 does not need it; only send checksums when an
			// operation requires one.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	}
	return &S3{
		bucket: c.Bucket,
		client: s3.NewFromConfig(cfg, options(c.Endpoint)),
		// Presigning is a local HMAC computation, no network call. It uses the
		// public endpoint because the host name is part of the signature and
		// the browser must reach the exact same host.
		presign: s3.NewPresignClient(s3.NewFromConfig(cfg, options(c.PublicEndpoint))),
	}, nil
}

// EnsureBucket creates the bucket if missing. Used for MinIO in dev and kind;
// on R2 the bucket is created once in the dashboard and this is a no-op.
func (s *S3) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err == nil {
		return nil
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket})
	var owned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil
	}
	return err
}

func (s *S3) CreateMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &s.bucket, Key: &key, ContentType: &contentType,
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.UploadId), nil
}

func (s *S3) PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket: &s.bucket, Key: &key, UploadId: &uploadID, PartNumber: aws.Int32(int32(partNumber)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func (s *S3) ListParts(ctx context.Context, key, uploadID string) ([]StoredPart, error) {
	var parts []StoredPart
	// ListParts returns at most 1000 parts per page.
	pager := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{
		Bucket: &s.bucket, Key: &key, UploadId: &uploadID,
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, p := range page.Parts {
			parts = append(parts, StoredPart{
				PartNumber:   int(aws.ToInt32(p.PartNumber)),
				ETag:         NormalizeETag(aws.ToString(p.ETag)),
				Size:         aws.ToInt64(p.Size),
				LastModified: aws.ToTime(p.LastModified),
			})
		}
	}
	return parts, nil
}

func (s *S3) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) error {
	completed := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		completed[i] = types.CompletedPart{
			PartNumber: aws.Int32(int32(p.PartNumber)),
			ETag:       aws.String(`"` + NormalizeETag(p.ETag) + `"`),
		}
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &s.bucket, Key: &key, UploadId: &uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	return mapError(err)
}

func (s *S3) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: &s.bucket, Key: &key, UploadId: &uploadID,
	})
	return mapError(err)
}

func (s *S3) HeadObject(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, mapError(err)
	}
	return ObjectInfo{Size: aws.ToInt64(out.ContentLength), ETag: NormalizeETag(aws.ToString(out.ETag))}, nil
}

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration, downloadFilename string) (string, error) {
	in := &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}
	if downloadFilename != "" {
		// response-content-disposition is part of the signed query string, so
		// the bucket serves the file as an attachment with this name.
		in.ResponseContentDisposition = aws.String(
			mime.FormatMediaType("attachment", map[string]string{"filename": downloadFilename}))
	}
	req, err := s.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func (s *S3) PutObject(ctx context.Context, key, contentType string, body io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, ContentType: &contentType,
		Body: body, ContentLength: aws.Int64(size),
	})
	return err
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket, Prefix: &prefix,
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		// An asset has five objects, so one DeleteObject per key is simpler
		// than building batch DeleteObjects requests.
		for _, obj := range page.Contents {
			if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: &s.bucket, Key: obj.Key,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// mapError turns the S3 error codes the upload flow reacts to into sentinels.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchUpload":
			return fmt.Errorf("%w: %s", ErrNoSuchUpload, apiErr.ErrorMessage())
		case "InvalidPart", "InvalidPartOrder":
			return fmt.Errorf("%w: %s", ErrInvalidPart, apiErr.ErrorMessage())
		case "NoSuchKey", "NotFound":
			return fmt.Errorf("%w: %s", ErrNotFound, apiErr.ErrorMessage())
		}
	}
	return err
}
