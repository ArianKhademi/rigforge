package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory ObjectStore for unit tests. It enforces the multipart
// rules the upload flow depends on: complete fails on an unknown upload id or
// a wrong ETag, and a completed or aborted upload id stops existing.
type Fake struct {
	mu      sync.Mutex
	nextID  int
	uploads map[string]*fakeUpload
	Objects map[string][]byte
}

type fakeUpload struct {
	key   string
	parts map[int]fakePart
}

type fakePart struct {
	data []byte
	etag string
	at   time.Time
}

func NewFake() *Fake {
	return &Fake{uploads: map[string]*fakeUpload{}, Objects: map[string][]byte{}}
}

func (f *Fake) CreateMultipartUpload(_ context.Context, key, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("mpu-%d", f.nextID)
	f.uploads[id] = &fakeUpload{key: key, parts: map[int]fakePart{}}
	return id, nil
}

func (f *Fake) PresignUploadPart(_ context.Context, key, uploadID string, partNumber int, _ time.Duration) (string, error) {
	return fmt.Sprintf("https://fake.invalid/%s?uploadId=%s&partNumber=%d", key, uploadID, partNumber), nil
}

// PutPart plays the browser's role: it "uploads" a part and returns its ETag.
func (f *Fake) PutPart(uploadID string, partNumber int, data []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := md5.Sum(data)
	etag := hex.EncodeToString(sum[:])
	f.uploads[uploadID].parts[partNumber] = fakePart{data: data, etag: etag, at: time.Now()}
	return etag
}

func (f *Fake) ListParts(_ context.Context, _, uploadID string) ([]StoredPart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[uploadID]
	if !ok {
		return nil, ErrNoSuchUpload
	}
	var out []StoredPart
	for n, p := range u.parts {
		out = append(out, StoredPart{PartNumber: n, ETag: p.etag, Size: int64(len(p.data)), LastModified: p.at})
	}
	slices.SortFunc(out, func(a, b StoredPart) int { return a.PartNumber - b.PartNumber })
	return out, nil
}

func (f *Fake) CompleteMultipartUpload(_ context.Context, key, uploadID string, parts []CompletedPart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[uploadID]
	if !ok {
		return ErrNoSuchUpload
	}
	var object []byte
	for _, p := range parts {
		stored, ok := u.parts[p.PartNumber]
		if !ok || stored.etag != NormalizeETag(p.ETag) {
			return fmt.Errorf("%w: part %d", ErrInvalidPart, p.PartNumber)
		}
		object = append(object, stored.data...)
	}
	f.Objects[key] = object
	delete(f.uploads, uploadID)
	return nil
}

func (f *Fake) AbortMultipartUpload(_ context.Context, _, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.uploads[uploadID]; !ok {
		return ErrNoSuchUpload
	}
	delete(f.uploads, uploadID)
	return nil
}

// OpenUploads reports how many multipart uploads are still in flight.
func (f *Fake) OpenUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *Fake) HeadObject(_ context.Context, key string) (ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.Objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Size: int64(len(data))}, nil
}

func (f *Fake) PresignGet(_ context.Context, key string, _ time.Duration, downloadFilename string) (string, error) {
	url := "https://fake.invalid/" + key
	if downloadFilename != "" {
		url += "?download=" + downloadFilename
	}
	return url, nil
}

func (f *Fake) PutObject(_ context.Context, key, _ string, body io.Reader, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Objects[key] = data
	return nil
}

func (f *Fake) DeletePrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.Objects {
		if strings.HasPrefix(key, prefix) {
			delete(f.Objects, key)
		}
	}
	return nil
}
