// Package job creates processing jobs, puts them on the Redis stream the
// Python workers consume, and serves job status (plain GET and an SSE stream).
package job

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/store"
)

const (
	// Built-in character ids. Anything else must be a character the user uploaded.
	CharacterDefault = "default" // the bundled mannequin
	CharacterNone    = "none"    // skeleton-only output, no mesh

	// MaxAttempts is how many times a job may fail before it is dead-lettered.
	// The worker enforces it; the api only stores it so the UI can show "2/3".
	MaxAttempts = 3
)

var ErrUnknownCharacter = errors.New("unknown character")

// Enqueuer puts a job on the queue. Implemented by RedisQueue.
type Enqueuer interface {
	Enqueue(ctx context.Context, j *store.Job) error
}

type Service struct {
	Store store.Store
	Queue Enqueuer
}

// New builds (but does not persist) the first-attempt job for an asset.
func (s *Service) New(assetID, userID, characterID string) *store.Job {
	return &store.Job{
		ID:          uuid.NewString(),
		AssetID:     assetID,
		UserID:      userID,
		Type:        store.JobTypeProcessVideo,
		CharacterID: characterID,
		Status:      store.JobQueued,
		Attempt:     1,
		MaxAttempts: MaxAttempts,
	}
}

// ResolveCharacter validates the character a job should use. Empty means the
// default mannequin.
func (s *Service) ResolveCharacter(ctx context.Context, userID, id string) (string, error) {
	switch id {
	case "", CharacterDefault:
		return CharacterDefault, nil
	case CharacterNone:
		return CharacterNone, nil
	}
	if uuid.Validate(id) != nil {
		return "", ErrUnknownCharacter
	}
	if _, err := s.Store.GetCharacter(ctx, userID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrUnknownCharacter
		}
		return "", err
	}
	return id, nil
}

// EnqueueNow tries to put a freshly committed job on the stream. Failure is
// logged, not returned: the row is already durable and RunDispatcher retries.
func (s *Service) EnqueueNow(ctx context.Context, j *store.Job) {
	// The request context may be cancelled the moment the client disconnects;
	// enqueueing should still finish.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.enqueue(ctx, j); err != nil {
		slog.Warn("enqueue failed, dispatcher will retry", "job_id", j.ID, "err", err)
	}
}

func (s *Service) enqueue(ctx context.Context, j *store.Job) error {
	if err := s.Queue.Enqueue(ctx, j); err != nil {
		return err
	}
	// XADD first, then mark. If we crash between the two the dispatcher
	// enqueues the job a second time, which is safe because workers treat
	// processing as idempotent per job id. The opposite order could lose a job.
	return s.Store.MarkJobEnqueued(ctx, j.ID)
}

// RunDispatcher is the safety net for the gap between "job row committed" and
// "message on the stream" (a small transactional-outbox): it periodically
// enqueues queued jobs that have no enqueued_at. Blocks until ctx is done.
func (s *Service) RunDispatcher(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := s.DispatchOnce(ctx, time.Now().Add(-interval)); err != nil {
				slog.Error("job dispatcher", "err", err)
			} else if n > 0 {
				slog.Info("job dispatcher enqueued stranded jobs", "count", n)
			}
		}
	}
}

// DispatchOnce enqueues jobs created before cutoff that never reached the
// stream. The cutoff keeps it from racing the request that is enqueueing a
// job it created a moment ago.
func (s *Service) DispatchOnce(ctx context.Context, cutoff time.Time) (int, error) {
	jobs, err := s.Store.UnenqueuedJobs(ctx, cutoff, 100)
	if err != nil {
		return 0, err
	}
	sent := 0
	for i := range jobs {
		if err := s.enqueue(ctx, &jobs[i]); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
