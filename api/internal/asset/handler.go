// Package asset serves the user's motion assets: browse, detail with preview
// URLs, export downloads, reprocess and delete.
package asset

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/httpx"
	"github.com/ArianKhademi/rigforge/api/internal/job"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

const (
	// viewTTL covers URLs the page itself loads (poster, preview video,
	// motion.glb). An hour lets someone keep a preview open and still seek in
	// the video, which issues new range requests against the same URL.
	viewTTL = time.Hour
	// exportTTL covers explicit downloads: the URL is fetched immediately, so
	// it only needs to live long enough for the browser to start the request.
	exportTTL = 2 * time.Minute
)

// Output file names under assets/{assetId}/; the worker writes the same names.
const (
	fileMotionGLB = "motion.glb"
	fileMotionBVH = "motion.bvh"
	filePreview   = "preview.mp4"
	filePoster    = "poster.jpg"
)

type Handler struct {
	Store   store.Store
	Objects storage.ObjectStore
	Jobs    *job.Service
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/assets", h.list)
	rg.GET("/assets/:id", h.get)
	rg.DELETE("/assets/:id", h.delete)
	rg.GET("/assets/:id/export", h.export)
	rg.POST("/assets/:id/jobs", h.reprocess)
}

type urlsJSON struct {
	Poster  string `json:"poster,omitempty"`
	Preview string `json:"preview,omitempty"`
	Motion  string `json:"motion,omitempty"`
}

type assetJSON struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Status          store.AssetStatus `json:"status"`
	SourceSize      int64             `json:"sourceSize"`
	ContentType     string            `json:"contentType"`
	DurationSeconds *float64          `json:"durationSeconds"`
	Width           *int              `json:"width"`
	Height          *int              `json:"height"`
	FPS             *float64          `json:"fps"`
	FrameCount      *int              `json:"frameCount"`
	CreatedAt       time.Time         `json:"createdAt"`
	UpdatedAt       time.Time         `json:"updatedAt"`
	Job             *job.JSON         `json:"job"`
	// URLs are presigned GETs; they are only present once the asset is ready.
	URLs urlsJSON `json:"urls"`
}

func key(assetID, file string) string { return "assets/" + assetID + "/" + file }

// toJSON renders an asset. detail=false (the browse grid) only signs the
// poster; the preview page gets all three URLs.
func (h *Handler) toJSON(ctx context.Context, a *store.Asset, detail bool) (*assetJSON, error) {
	out := &assetJSON{
		ID: a.ID, Name: a.Name, Status: a.Status, SourceSize: a.SourceSize, ContentType: a.ContentType,
		DurationSeconds: a.DurationSeconds, Width: a.Width, Height: a.Height, FPS: a.FPS,
		FrameCount: a.FrameCount, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, Job: job.ToJSON(a.Job),
	}
	if a.Status != store.AssetReady {
		return out, nil
	}
	var err error
	if out.URLs.Poster, err = h.Objects.PresignGet(ctx, key(a.ID, filePoster), viewTTL, ""); err != nil {
		return nil, err
	}
	if !detail {
		return out, nil
	}
	if out.URLs.Preview, err = h.Objects.PresignGet(ctx, key(a.ID, filePreview), viewTTL, ""); err != nil {
		return nil, err
	}
	if out.URLs.Motion, err = h.Objects.PresignGet(ctx, key(a.ID, fileMotionGLB), viewTTL, ""); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *Handler) list(c *gin.Context) {
	filter := store.AssetFilter{Query: c.Query("q"), Sort: store.SortNewest}
	switch s := store.AssetSort(c.Query("sort")); s {
	case store.SortNewest, store.SortOldest, store.SortName, store.SortDuration:
		filter.Sort = s
	case "":
	default:
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "sort must be newest, oldest, name or duration")
		return
	}
	ctx := c.Request.Context()
	assets, err := h.Store.ListAssets(ctx, auth.UserID(c), filter)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	out := make([]*assetJSON, len(assets))
	for i := range assets {
		if out[i], err = h.toJSON(ctx, &assets[i], false); err != nil {
			httpx.Internal(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"assets": out})
}

func (h *Handler) load(c *gin.Context) (*store.Asset, bool) {
	id := c.Param("id")
	if uuid.Validate(id) != nil {
		httpx.Error(c, http.StatusNotFound, "not_found", "asset not found")
		return nil, false
	}
	a, err := h.Store.GetAsset(c.Request.Context(), auth.UserID(c), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "not_found", "asset not found")
		return nil, false
	}
	if err != nil {
		httpx.Internal(c, err)
		return nil, false
	}
	return a, true
}

func (h *Handler) get(c *gin.Context) {
	a, ok := h.load(c)
	if !ok {
		return
	}
	body, err := h.toJSON(c.Request.Context(), a, true)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	c.JSON(http.StatusOK, body)
}

// export returns a short-lived download URL for motion.glb or motion.bvh. The
// browser downloads straight from the bucket; the api never proxies the bytes.
func (h *Handler) export(c *gin.Context) {
	a, ok := h.load(c)
	if !ok {
		return
	}
	var file string
	switch c.Query("format") {
	case "glb":
		file = fileMotionGLB
	case "bvh":
		file = fileMotionBVH
	default:
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "format must be glb or bvh")
		return
	}
	if a.Status != store.AssetReady {
		httpx.Error(c, http.StatusConflict, "not_ready", "asset has no outputs yet")
		return
	}
	filename := a.Name + "." + c.Query("format")
	url, err := h.Objects.PresignGet(c.Request.Context(), key(a.ID, file), exportTTL, filename)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url, "filename": filename, "expiresAt": time.Now().Add(exportTTL)})
}

type reprocessRequest struct {
	CharacterID string `json:"characterId"`
}

// reprocess queues a new job for an existing asset, e.g. to apply the motion
// to a different character or to retry after a dead-lettered job. Outputs are
// keyed by asset id, so the new job overwrites the old files.
func (h *Handler) reprocess(c *gin.Context) {
	a, ok := h.load(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var req reprocessRequest
	_ = c.ShouldBindJSON(&req)

	if a.Job != nil && a.Job.Status != store.JobDone && a.Job.Status != store.JobFailed {
		httpx.Error(c, http.StatusConflict, "already_processing", "a job for this asset is still running")
		return
	}
	characterID, err := h.Jobs.ResolveCharacter(ctx, a.UserID, req.CharacterID)
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid_character", "unknown characterId")
		return
	}
	newJob := h.Jobs.New(a.ID, a.UserID, characterID)
	if err := h.Store.CreateJob(ctx, newJob); err != nil {
		httpx.Internal(c, err)
		return
	}
	h.Jobs.EnqueueNow(ctx, newJob)
	c.JSON(http.StatusCreated, job.ToJSON(newJob))
}

func (h *Handler) delete(c *gin.Context) {
	a, ok := h.load(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	// Row first, objects second: once the row is gone the user no longer sees
	// the asset. If the object delete then fails we leak storage (and log it)
	// rather than show an asset whose files are half gone.
	if err := h.Store.DeleteAsset(ctx, a.UserID, a.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		httpx.Internal(c, err)
		return
	}
	if err := h.Objects.DeletePrefix(context.WithoutCancel(ctx), "assets/"+a.ID+"/"); err != nil {
		slog.Error("delete asset objects", "asset_id", a.ID, "err", err)
	}
	c.Status(http.StatusNoContent)
}
