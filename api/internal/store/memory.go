package store

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process Store for unit tests. It mirrors the Postgres
// semantics (ownership checks, status guards, cascades); storetest verifies
// that both implementations agree.
type Memory struct {
	mu         sync.Mutex
	uploads    map[string]*Upload
	parts      map[string]map[int]Part
	assets     map[string]*Asset
	jobs       map[string]*Job
	characters map[string]*Character
	// Now is swappable so tests can age rows without sleeping.
	Now func() time.Time
}

func NewMemory() *Memory {
	return &Memory{
		uploads:    map[string]*Upload{},
		parts:      map[string]map[int]Part{},
		assets:     map[string]*Asset{},
		jobs:       map[string]*Job{},
		characters: map[string]*Character{},
		Now:        time.Now,
	}
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) CreateUpload(_ context.Context, u *Upload) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u.CreatedAt, u.UpdatedAt = m.Now(), m.Now()
	cp := *u
	m.uploads[u.ID] = &cp
	m.parts[u.ID] = map[int]Part{}
	return nil
}

func (m *Memory) GetUpload(_ context.Context, userID, id string) (*Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[id]
	if !ok || u.UserID != userID {
		return nil, ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (m *Memory) ListParts(_ context.Context, uploadID string) ([]Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Part, 0, len(m.parts[uploadID]))
	for _, p := range m.parts[uploadID] {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Part) int { return cmp.Compare(a.PartNumber, b.PartNumber) })
	return out, nil
}

func (m *Memory) RecordPart(_ context.Context, uploadID string, partNumber int, etag string) (Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[uploadID]
	if !ok || u.Status != UploadUploading {
		return Part{}, ErrConflict
	}
	u.UpdatedAt = m.Now()
	if existing, ok := m.parts[uploadID][partNumber]; ok && existing.ETag == etag {
		return existing, nil
	}
	p := Part{PartNumber: partNumber, ETag: etag, RecordedAt: m.Now()}
	m.parts[uploadID][partNumber] = p
	return p, nil
}

func (m *Memory) FinishUpload(_ context.Context, uploadID string, asset *Asset, job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[uploadID]
	if !ok || u.Status != UploadUploading {
		return ErrConflict
	}
	u.Status, u.UpdatedAt = UploadCompleted, m.Now()
	asset.CreatedAt, asset.UpdatedAt = m.Now(), m.Now()
	a := *asset
	m.assets[a.ID] = &a
	m.putJob(job)
	return nil
}

func (m *Memory) AbortUpload(_ context.Context, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[uploadID]
	if !ok || u.Status != UploadUploading {
		return ErrConflict
	}
	u.Status, u.UpdatedAt = UploadAborted, m.Now()
	return nil
}

func (m *Memory) StaleUploads(_ context.Context, cutoff time.Time, limit int) ([]Upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Upload
	for _, u := range m.uploads {
		if u.Status == UploadUploading && u.UpdatedAt.Before(cutoff) {
			out = append(out, *u)
		}
	}
	slices.SortFunc(out, func(a, b Upload) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) ListAssets(_ context.Context, userID string, f AssetFilter) ([]Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Asset
	for _, a := range m.assets {
		if a.UserID != userID {
			continue
		}
		if f.Query != "" && !strings.Contains(strings.ToLower(a.Name), strings.ToLower(f.Query)) {
			continue
		}
		cp := *a
		cp.Job = m.latestJob(a.ID)
		out = append(out, cp)
	}
	newestFirst := func(a, b Asset) int { return b.CreatedAt.Compare(a.CreatedAt) }
	slices.SortFunc(out, func(a, b Asset) int {
		switch f.Sort {
		case SortOldest:
			return a.CreatedAt.Compare(b.CreatedAt)
		case SortName:
			if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
		case SortDuration:
			// Longest first, assets without a duration last.
			switch {
			case a.DurationSeconds != nil && b.DurationSeconds == nil:
				return -1
			case a.DurationSeconds == nil && b.DurationSeconds != nil:
				return 1
			case a.DurationSeconds != nil && *a.DurationSeconds != *b.DurationSeconds:
				return cmp.Compare(*b.DurationSeconds, *a.DurationSeconds)
			}
		}
		return newestFirst(a, b)
	})
	return out, nil
}

func (m *Memory) GetAsset(_ context.Context, userID, id string) (*Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assets[id]
	if !ok || a.UserID != userID {
		return nil, ErrNotFound
	}
	cp := *a
	cp.Job = m.latestJob(id)
	return &cp, nil
}

func (m *Memory) DeleteAsset(_ context.Context, userID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assets[id]
	if !ok || a.UserID != userID {
		return ErrNotFound
	}
	delete(m.assets, id)
	for jid, j := range m.jobs {
		if j.AssetID == id {
			delete(m.jobs, jid)
		}
	}
	for uid, u := range m.uploads {
		if u.AssetID == id {
			delete(m.uploads, uid)
			delete(m.parts, uid)
		}
	}
	return nil
}

func (m *Memory) CreateJob(_ context.Context, job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assets[job.AssetID]
	if !ok || a.UserID != job.UserID {
		return ErrNotFound
	}
	a.Status, a.UpdatedAt = AssetProcessing, m.Now()
	m.putJob(job)
	return nil
}

func (m *Memory) GetJob(_ context.Context, userID, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok || j.UserID != userID {
		return nil, ErrNotFound
	}
	cp := *j
	return &cp, nil
}

func (m *Memory) LatestJobForAsset(_ context.Context, assetID string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.latestJob(assetID); j != nil {
		return j, nil
	}
	return nil, ErrNotFound
}

func (m *Memory) MarkJobEnqueued(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok && j.EnqueuedAt == nil {
		now := m.Now()
		j.EnqueuedAt = &now
	}
	return nil
}

func (m *Memory) UnenqueuedJobs(_ context.Context, cutoff time.Time, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if j.EnqueuedAt == nil && j.Status == JobQueued && j.CreatedAt.Before(cutoff) {
			out = append(out, *j)
		}
	}
	slices.SortFunc(out, func(a, b Job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) CreateCharacter(_ context.Context, c *Character) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.CreatedAt = m.Now()
	cp := *c
	m.characters[c.ID] = &cp
	return nil
}

func (m *Memory) ListCharacters(_ context.Context, userID string) ([]Character, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Character
	for _, c := range m.characters {
		if c.UserID == userID {
			out = append(out, *c)
		}
	}
	slices.SortFunc(out, func(a, b Character) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (m *Memory) GetCharacter(_ context.Context, userID, id string) (*Character, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.characters[id]
	if !ok || c.UserID != userID {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *Memory) DeleteCharacter(_ context.Context, userID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.characters[id]
	if !ok || c.UserID != userID {
		return ErrNotFound
	}
	delete(m.characters, id)
	return nil
}

// SetJobState lets tests play the worker's part (the worker writes these
// columns directly in Postgres).
func (m *Memory) SetJobState(id string, status JobStatus, progress int, errText *string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.Status, j.Progress, j.Error, j.UpdatedAt = status, progress, errText, m.Now()
	}
}

// SetAssetReady marks an asset processed, as the worker does when a job finishes.
func (m *Memory) SetAssetReady(id string, durationSeconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.assets[id]; ok {
		a.Status, a.DurationSeconds, a.UpdatedAt = AssetReady, &durationSeconds, m.Now()
	}
}

// putJob and latestJob expect m.mu to be held.
func (m *Memory) putJob(job *Job) {
	job.CreatedAt, job.UpdatedAt = m.Now(), m.Now()
	cp := *job
	m.jobs[job.ID] = &cp
}

func (m *Memory) latestJob(assetID string) *Job {
	var latest *Job
	for _, j := range m.jobs {
		if j.AssetID != assetID {
			continue
		}
		if latest == nil || j.CreatedAt.After(latest.CreatedAt) ||
			(j.CreatedAt.Equal(latest.CreatedAt) && j.ID > latest.ID) {
			latest = j
		}
	}
	if latest == nil {
		return nil
	}
	cp := *latest
	return &cp
}
