package upload

import (
	"context"
	"log/slog"
	"time"
)

// RunReaper aborts uploads that have been idle longer than Cfg.IdleTimeout.
// Parts of an unfinished multipart upload are invisible in the bucket but are
// stored and billed until the upload is completed or aborted, so abandoned
// uploads have to be cleaned up actively. It blocks until ctx is cancelled.
//
// Every api replica runs a reaper. They can race on the same upload; that is
// harmless because aborting twice is a no-op on both the bucket and the row.
func (h *Handler) RunReaper(ctx context.Context) {
	ticker := time.NewTicker(h.Cfg.ReaperInterval)
	defer ticker.Stop()
	for {
		if n, err := h.ReapOnce(ctx, time.Now()); err != nil {
			slog.Error("upload reaper", "err", err)
		} else if n > 0 {
			slog.Info("upload reaper aborted idle uploads", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReapOnce aborts one batch of idle uploads and reports how many it aborted.
func (h *Handler) ReapOnce(ctx context.Context, now time.Time) (int, error) {
	stale, err := h.Store.StaleUploads(ctx, now.Add(-h.Cfg.IdleTimeout), 100)
	if err != nil {
		return 0, err
	}
	aborted := 0
	for i := range stale {
		if err := h.abortUpload(ctx, &stale[i]); err != nil {
			slog.Error("abort idle upload", "upload_id", stale[i].ID, "err", err)
			continue
		}
		aborted++
	}
	return aborted, nil
}
