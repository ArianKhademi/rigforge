package job

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/ArianKhademi/rigforge/api/internal/store"
)

const (
	// Stream is the Redis stream the workers read with consumer group "workers".
	Stream = "jobs"
	// eventChannelPrefix + jobID is the pub/sub channel a worker publishes to
	// whenever it changes a job row.
	eventChannelPrefix = "job-events:"
)

// RedisQueue is the producer side of the job queue. Retries, acks, dead
// lettering and reclaiming all live in the worker (worker/rigforge_worker/queue.py).
type RedisQueue struct {
	Client *redis.Client
}

func (q *RedisQueue) Enqueue(ctx context.Context, j *store.Job) error {
	// The message is deliberately tiny: ids only. The worker reads everything
	// else from the job row, so a message can never go stale.
	return q.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: Stream,
		Values: map[string]any{
			"jobId":   j.ID,
			"assetId": j.AssetID,
			"type":    j.Type,
			"attempt": strconv.Itoa(j.Attempt),
		},
	}).Err()
}

// RedisEvents subscribes to a job's pub/sub channel.
type RedisEvents struct {
	Client *redis.Client
}

// Subscribe returns a channel that receives a tick whenever the worker
// publishes for jobID, and a function that ends the subscription. The
// subscription is confirmed by Redis before Subscribe returns, so a caller
// that reads the job row afterwards cannot miss an update in between.
func (e *RedisEvents) Subscribe(ctx context.Context, jobID string) (<-chan struct{}, func(), error) {
	sub := e.Client.Subscribe(ctx, eventChannelPrefix+jobID)
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, nil, err
	}
	wake := make(chan struct{}, 1)
	go func() {
		for range sub.Channel() {
			// Non-blocking send: the payload is irrelevant (the handler
			// re-reads the row), so several quick updates collapse into one tick.
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	return wake, func() { _ = sub.Close() }, nil
}
