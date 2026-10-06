// Package integration runs the upload flow against real services: Postgres, a
// real S3 endpoint (MinIO from docker-compose, or R2) and Redis. Unlike the
// unit tests it PUTs bytes to genuine presigned URLs, so it proves the
// signatures, ETags and multipart semantics the fakes only imitate.
//
// It is skipped unless the RIGFORGE_TEST_* variables are set; `make test`
// sets them for the docker-compose stack.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/auth/authtest"
	"github.com/ArianKhademi/rigforge/api/internal/config"
	"github.com/ArianKhademi/rigforge/api/internal/job"
	"github.com/ArianKhademi/rigforge/api/internal/server"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

const (
	user = "integration-user"
	// 5 MiB is the smallest part size S3, R2 and MinIO accept for any part
	// but the last, so a 12 MiB file is the smallest real three-part upload.
	partSize = 5 << 20
	fileSize = 12 << 20
)

type env struct {
	t       *testing.T
	api     *httptest.Server
	token   string
	objects *storage.S3
	redis   *redis.Client
	pg      *store.Postgres
}

func setup(t *testing.T) *env {
	t.Helper()
	dbURL, s3Endpoint, redisURL := os.Getenv("RIGFORGE_TEST_DATABASE_URL"),
		os.Getenv("RIGFORGE_TEST_S3_ENDPOINT"), os.Getenv("RIGFORGE_TEST_REDIS_URL")
	if dbURL == "" || s3Endpoint == "" || redisURL == "" {
		t.Skip("RIGFORGE_TEST_DATABASE_URL, RIGFORGE_TEST_S3_ENDPOINT and RIGFORGE_TEST_REDIS_URL not all set")
	}
	ctx := context.Background()

	pg, err := store.NewPostgres(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	objects, err := storage.NewS3(ctx, storage.S3Config{
		Endpoint: s3Endpoint, PublicEndpoint: s3Endpoint, Region: "auto",
		Bucket:          envOr("RIGFORGE_TEST_S3_BUCKET", "rigforge-test"),
		AccessKeyID:     envOr("RIGFORGE_TEST_S3_ACCESS_KEY_ID", "rigforge"),
		SecretAccessKey: envOr("RIGFORGE_TEST_S3_SECRET_ACCESS_KEY", "rigforge-dev-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })

	issuer := authtest.New(t)
	verifier, err := auth.NewVerifier(issuer.JWKSURL(), authtest.IssuerName, authtest.Audience)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Deps{
		Store:    pg,
		Objects:  objects,
		Queue:    &job.RedisQueue{Client: rdb},
		Events:   &job.RedisEvents{Client: rdb},
		Verifier: verifier,
		Upload: config.Upload{
			PartSize: partSize, MaxSize: 1 << 30, PresignTTL: 15 * time.Minute,
			IdleTimeout: 24 * time.Hour, ReaperInterval: time.Hour,
		},
	})
	api := httptest.NewServer(srv.Router)
	t.Cleanup(api.Close)

	return &env{t: t, api: api, token: issuer.Token(t, user, time.Hour), objects: objects, redis: rdb, pg: pg}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// call makes an authenticated JSON request to the api and decodes the reply.
func (e *env) call(method, path string, body, out any) int {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

type upload struct {
	UploadID  string `json:"uploadId"`
	AssetID   string `json:"assetId"`
	PartCount int    `json:"partCount"`
}

type uploadState struct {
	Status string `json:"status"`
	Parts  []struct {
		PartNumber int    `json:"partNumber"`
		ETag       string `json:"etag"`
	} `json:"parts"`
	StorageParts []struct {
		PartNumber   int       `json:"partNumber"`
		ETag         string    `json:"etag"`
		Size         int64     `json:"size"`
		LastModified time.Time `json:"lastModified"`
	} `json:"storageParts"`
}

func (e *env) create(size int) upload {
	e.t.Helper()
	var up upload
	if code := e.call(http.MethodPost, "/api/uploads",
		map[string]any{"filename": "clip.mp4", "size": size, "contentType": "video/mp4"}, &up); code != http.StatusCreated {
		e.t.Fatalf("create upload: status %d", code)
	}
	e.t.Cleanup(func() { // leave neither an open multipart upload nor objects behind
		e.call(http.MethodDelete, "/api/uploads/"+up.UploadID, nil, nil)
		e.call(http.MethodDelete, "/api/assets/"+up.AssetID, nil, nil)
	})
	return up
}

// putPart does what the browser does: asks the api for a presigned URL and
// PUTs the bytes straight to the bucket. It returns the ETag header.
func (e *env) putPart(up upload, n int, data []byte) string {
	e.t.Helper()
	var presigned struct {
		URLs []struct {
			PartNumber int    `json:"partNumber"`
			URL        string `json:"url"`
		} `json:"urls"`
	}
	if code := e.call(http.MethodPost, "/api/uploads/"+up.UploadID+"/parts",
		map[string]any{"partNumbers": []int{n}}, &presigned); code != http.StatusOK {
		e.t.Fatalf("presign part %d: status %d", n, code)
	}
	req, _ := http.NewRequest(http.MethodPut, presigned.URLs[0].URL, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("PUT part %d to the bucket: %d %s", n, resp.StatusCode, body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		e.t.Fatalf("bucket returned no ETag for part %d", n)
	}
	return etag
}

func (e *env) record(up upload, n int, etag string) {
	e.t.Helper()
	if code := e.call(http.MethodPut, fmt.Sprintf("/api/uploads/%s/parts/%d", up.UploadID, n),
		map[string]string{"etag": etag}, nil); code != http.StatusOK {
		e.t.Fatalf("record part %d: status %d", n, code)
	}
}

func randomFile(t *testing.T) []byte {
	t.Helper()
	data := make([]byte, fileSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func part(data []byte, n int) []byte {
	start := (n - 1) * partSize
	return data[start:min(start+partSize, len(data))]
}

func TestMultipartUploadEndToEnd(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	data := randomFile(t)
	up := e.create(len(data))
	if up.PartCount != 3 {
		t.Fatalf("partCount = %d, want 3", up.PartCount)
	}

	// Upload out of order, as concurrent PUTs finish.
	for _, n := range []int{3, 1, 2} {
		e.record(up, n, e.putPart(up, n, part(data, n)))
	}

	// The bucket's own part list agrees with what the api recorded.
	var state uploadState
	e.call(http.MethodGet, "/api/uploads/"+up.UploadID+"?storage=true", nil, &state)
	if len(state.Parts) != 3 || len(state.StorageParts) != 3 {
		t.Fatalf("recorded %d parts, bucket has %d; want 3 and 3", len(state.Parts), len(state.StorageParts))
	}
	for i := range state.Parts {
		if state.Parts[i].ETag != state.StorageParts[i].ETag {
			t.Fatalf("part %d: recorded ETag %s, bucket ETag %s", i+1, state.Parts[i].ETag, state.StorageParts[i].ETag)
		}
	}

	var done struct{ AssetID, JobID string }
	if code := e.call(http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil, &done); code != http.StatusCreated {
		t.Fatalf("complete: status %d", code)
	}

	// The assembled object is byte-identical to the source file.
	u, err := e.pg.GetUpload(ctx, user, up.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	url, _ := e.objects.PresignGet(ctx, u.ObjectKey, time.Minute, "")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	stored, _ := io.ReadAll(resp.Body)
	if sha256.Sum256(stored) != sha256.Sum256(data) {
		t.Fatalf("stored object (%d bytes) differs from the uploaded file (%d bytes)", len(stored), len(data))
	}

	// The job is on the Redis stream for the workers.
	msgs, err := e.redis.XRange(ctx, job.Stream, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Values["jobId"] == done.JobID {
			found = true
			if m.Values["assetId"] != done.AssetID || m.Values["type"] != "process_video" || m.Values["attempt"] != "1" {
				t.Fatalf("stream message = %v", m.Values)
			}
			e.redis.XDel(ctx, job.Stream, m.ID) // do not leave it for a real worker
		}
	}
	if !found {
		t.Fatal("completed upload did not put a job on the stream")
	}

	// The multipart upload id is gone from the bucket.
	if _, err := e.objects.ListParts(ctx, u.ObjectKey, u.S3UploadID); !errors.Is(err, storage.ErrNoSuchUpload) {
		t.Fatalf("ListParts after complete: %v, want ErrNoSuchUpload", err)
	}
}

// TestResumeDoesNotResendCompletedParts is the small-scale version of
// scripts/upload_resume_test.sh: a client dies part-way, a new client resumes
// from server state, and the bucket shows the early parts were never rewritten.
func TestResumeDoesNotResendCompletedParts(t *testing.T) {
	e := setup(t)
	data := randomFile(t)
	up := e.create(len(data))

	// First client: part 1 is uploaded and reported; part 2 reaches the bucket
	// but the client dies before reporting it.
	e.record(up, 1, e.putPart(up, 1, part(data, 1)))
	e.putPart(up, 2, part(data, 2))

	var before uploadState
	e.call(http.MethodGet, "/api/uploads/"+up.UploadID+"?storage=true", nil, &before)
	if len(before.Parts) != 1 || len(before.StorageParts) != 2 {
		t.Fatalf("before resume: %d recorded, %d in bucket; want 1 and 2", len(before.Parts), len(before.StorageParts))
	}

	// Second client knows only the upload id. Reconcile adopts part 2 from the bucket.
	var resumed uploadState
	if code := e.call(http.MethodPost, "/api/uploads/"+up.UploadID+"/reconcile", nil, &resumed); code != http.StatusOK {
		t.Fatalf("reconcile: status %d", code)
	}
	if len(resumed.Parts) != 2 {
		t.Fatalf("after reconcile %d parts recorded, want 2", len(resumed.Parts))
	}
	// So only part 3 is left to send.
	e.record(up, 3, e.putPart(up, 3, part(data, 3)))

	var after uploadState
	e.call(http.MethodGet, "/api/uploads/"+up.UploadID+"?storage=true", nil, &after)
	for i := 0; i < 2; i++ {
		b, a := before.StorageParts[i], after.StorageParts[i]
		if a.ETag != b.ETag || !a.LastModified.Equal(b.LastModified) {
			t.Fatalf("part %d was rewritten during resume: before %+v, after %+v", b.PartNumber, b, a)
		}
	}
	if code := e.call(http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil, nil); code != http.StatusCreated {
		t.Fatalf("complete after resume: status %d", code)
	}
}

func TestBucketRejectsWrongETagAtComplete(t *testing.T) {
	e := setup(t)
	data := randomFile(t)
	up := e.create(len(data))
	e.record(up, 1, e.putPart(up, 1, part(data, 1)))
	e.record(up, 2, e.putPart(up, 2, part(data, 2)))
	e.putPart(up, 3, part(data, 3))
	e.record(up, 3, "00000000000000000000000000000000")

	var body struct {
		Error struct{ Code string }
	}
	if code := e.call(http.MethodPost, "/api/uploads/"+up.UploadID+"/complete", nil, &body); code != http.StatusConflict || body.Error.Code != "invalid_part" {
		t.Fatalf("complete with a wrong ETag: %d %q, want 409 invalid_part", code, body.Error.Code)
	}
}

func TestAbortReleasesPartsInTheBucket(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	data := randomFile(t)
	up := e.create(len(data))
	e.record(up, 1, e.putPart(up, 1, part(data, 1)))

	u, _ := e.pg.GetUpload(ctx, user, up.UploadID)
	if code := e.call(http.MethodDelete, "/api/uploads/"+up.UploadID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("abort: status %d", code)
	}
	if _, err := e.objects.ListParts(ctx, u.ObjectKey, u.S3UploadID); !errors.Is(err, storage.ErrNoSuchUpload) {
		t.Fatalf("ListParts after abort: %v, want ErrNoSuchUpload", err)
	}
}
