package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/httpx"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

// Subscriber delivers a tick whenever a job changed. Implemented by RedisEvents.
type Subscriber interface {
	Subscribe(ctx context.Context, jobID string) (wake <-chan struct{}, stop func(), err error)
}

type Handler struct {
	Store  store.Store
	Events Subscriber
	// Resync is how often the SSE stream re-reads the row even without a
	// pub/sub tick. Pub/sub is fire-and-forget, so this bounds how stale a
	// stream can get if a tick is lost; it also doubles as the keepalive.
	Resync time.Duration
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/jobs/:id", h.get)
	rg.GET("/jobs/:id/events", h.events)
}

// JSON is the wire shape of a job, shared with the asset endpoints.
type JSON struct {
	ID          string          `json:"id"`
	AssetID     string          `json:"assetId"`
	Type        string          `json:"type"`
	CharacterID string          `json:"characterId"`
	Status      store.JobStatus `json:"status"`
	Progress    int             `json:"progress"`
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"maxAttempts"`
	Error       *string         `json:"error"`
	RetryAt     *time.Time      `json:"retryAt"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	StartedAt   *time.Time      `json:"startedAt"`
	FinishedAt  *time.Time      `json:"finishedAt"`
}

func ToJSON(j *store.Job) *JSON {
	if j == nil {
		return nil
	}
	return &JSON{
		ID: j.ID, AssetID: j.AssetID, Type: j.Type, CharacterID: j.CharacterID,
		Status: j.Status, Progress: j.Progress, Attempt: j.Attempt, MaxAttempts: j.MaxAttempts,
		Error: j.Error, RetryAt: j.RetryAt, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

func terminal(s store.JobStatus) bool {
	return s == store.JobDone || s == store.JobFailed
}

func (h *Handler) load(c *gin.Context) (*store.Job, bool) {
	id := c.Param("id")
	if uuid.Validate(id) != nil {
		httpx.Error(c, http.StatusNotFound, "not_found", "job not found")
		return nil, false
	}
	j, err := h.Store.GetJob(c.Request.Context(), auth.UserID(c), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "not_found", "job not found")
		return nil, false
	}
	if err != nil {
		httpx.Internal(c, err)
		return nil, false
	}
	return j, true
}

func (h *Handler) get(c *gin.Context) {
	if j, ok := h.load(c); ok {
		c.JSON(http.StatusOK, ToJSON(j))
	}
}

// events streams job state as server-sent events until the job reaches a
// terminal state or the client disconnects.
//
// Postgres is the source of truth. A pub/sub message from the worker only
// means "the row changed, read it again", so a lost or duplicated message can
// never produce a wrong event, and the stream works across api replicas
// without sticky sessions.
func (h *Handler) events(c *gin.Context) {
	j, ok := h.load(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	// Subscribe before sending the first snapshot: an update that lands in
	// between is then delivered as a tick instead of being missed.
	wake, stop, err := h.Events.Subscribe(ctx, j.ID)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	defer stop()

	header := c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no") // tell nginx-style proxies not to buffer
	c.Writer.WriteHeader(http.StatusOK)

	var last []byte
	// send writes the current job state if it differs from what was last sent
	// and reports whether the stream should end.
	send := func() (done bool) {
		current, err := h.Store.GetJob(ctx, j.UserID, j.ID)
		if err != nil {
			return true // job deleted or database gone: end the stream
		}
		payload, _ := json.Marshal(ToJSON(current))
		if string(payload) != string(last) {
			fmt.Fprintf(c.Writer, "event: job\ndata: %s\n\n", payload)
			c.Writer.Flush()
			last = payload
		}
		return terminal(current.Status)
	}

	if send() {
		return
	}
	ticker := time.NewTicker(h.Resync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
			// A comment line keeps idle proxies from closing the connection.
			fmt.Fprint(c.Writer, ": keepalive\n\n")
			c.Writer.Flush()
		}
		if send() {
			return
		}
	}
}
