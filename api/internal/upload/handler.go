package upload

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/config"
	"github.com/ArianKhademi/rigforge/api/internal/httpx"
	"github.com/ArianKhademi/rigforge/api/internal/job"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

// maxPresignBatch bounds one presign request; the web client asks for a few
// parts at a time (its concurrency), so this is only a guard.
const maxPresignBatch = 100

type Handler struct {
	Store   store.Store
	Objects storage.ObjectStore
	Jobs    *job.Service
	Cfg     config.Upload
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/uploads", h.create)
	rg.GET("/uploads/:id", h.get)
	rg.POST("/uploads/:id/reconcile", h.reconcile)
	rg.POST("/uploads/:id/parts", h.presignParts)
	rg.PUT("/uploads/:id/parts/:n", h.recordPart)
	rg.POST("/uploads/:id/complete", h.complete)
	rg.DELETE("/uploads/:id", h.abort)
}

type partJSON struct {
	PartNumber int       `json:"partNumber"`
	ETag       string    `json:"etag"`
	RecordedAt time.Time `json:"recordedAt"`
}

type storedPartJSON struct {
	PartNumber   int       `json:"partNumber"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

type uploadJSON struct {
	UploadID    string             `json:"uploadId"`
	AssetID     string             `json:"assetId"`
	Filename    string             `json:"filename"`
	Size        int64              `json:"size"`
	ContentType string             `json:"contentType"`
	PartSize    int64              `json:"partSize"`
	PartCount   int                `json:"partCount"`
	Status      store.UploadStatus `json:"status"`
	Parts       []partJSON         `json:"parts"`
	// StorageParts is the bucket's own view (ListParts), only with ?storage=true.
	StorageParts []storedPartJSON `json:"storageParts,omitempty"`
}

// ---- POST /api/uploads ----

type createRequest struct {
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

func (h *Handler) create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "body must be JSON {filename, size, contentType}")
		return
	}
	req.Filename = strings.TrimSpace(filepath.Base(req.Filename))
	switch {
	case req.Filename == "" || req.Filename == "." || len(req.Filename) > 255:
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "filename is required (max 255 characters)")
		return
	case !strings.HasPrefix(req.ContentType, "video/"):
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "contentType must be a video/* type")
		return
	case req.Size > h.Cfg.MaxSize:
		httpx.Error(c, http.StatusRequestEntityTooLarge, "too_large", "file exceeds the maximum upload size")
		return
	}
	partCount, err := PartCount(req.Size, h.Cfg.PartSize)
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx := c.Request.Context()
	u := &store.Upload{
		ID:          uuid.NewString(),
		UserID:      auth.UserID(c),
		AssetID:     uuid.NewString(),
		Filename:    req.Filename,
		ContentType: req.ContentType,
		Size:        req.Size,
		PartSize:    h.Cfg.PartSize,
		PartCount:   partCount,
		Status:      store.UploadUploading,
	}
	// Everything belonging to an asset lives under one prefix, so deleting the
	// asset is a single prefix delete. The key never contains user-supplied
	// text apart from a sanitised extension.
	u.ObjectKey = "assets/" + u.AssetID + "/source" + safeExt(req.Filename)

	if u.S3UploadID, err = h.Objects.CreateMultipartUpload(ctx, u.ObjectKey, u.ContentType); err != nil {
		httpx.Internal(c, err)
		return
	}
	if err := h.Store.CreateUpload(ctx, u); err != nil {
		// Do not leave a multipart upload in the bucket that no row points to.
		_ = h.Objects.AbortMultipartUpload(context.WithoutCancel(ctx), u.ObjectKey, u.S3UploadID)
		httpx.Internal(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"uploadId":  u.ID,
		"assetId":   u.AssetID,
		"partSize":  u.PartSize,
		"partCount": u.PartCount,
	})
}

// ---- GET /api/uploads/:id ----

func (h *Handler) get(c *gin.Context) {
	u, ok := h.load(c)
	if !ok {
		return
	}
	body, err := h.describe(c.Request.Context(), u)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	if c.Query("storage") == "true" && u.Status == store.UploadUploading {
		stored, err := h.Objects.ListParts(c.Request.Context(), u.ObjectKey, u.S3UploadID)
		if err != nil {
			httpx.Internal(c, err)
			return
		}
		body.StorageParts = make([]storedPartJSON, len(stored))
		for i, p := range stored {
			body.StorageParts[i] = storedPartJSON(p)
		}
	}
	c.JSON(http.StatusOK, body)
}

// ---- POST /api/uploads/:id/reconcile ----

// reconcile adopts parts that are in the bucket but were never reported. That
// happens when a client dies between a successful PUT and the ETag report: the
// bytes are safely stored, and without this step a resuming client would send
// them again. The bucket is the source of truth for what was uploaded.
func (h *Handler) reconcile(c *gin.Context) {
	u, ok := h.load(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if u.Status != store.UploadUploading {
		httpx.Error(c, http.StatusConflict, "not_uploading", "upload is "+string(u.Status))
		return
	}
	stored, err := h.Objects.ListParts(ctx, u.ObjectKey, u.S3UploadID)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	recorded, err := h.Store.ListParts(ctx, u.ID)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	known := make(map[int]string, len(recorded))
	for _, p := range recorded {
		known[p.PartNumber] = p.ETag
	}
	adopted := 0
	for _, sp := range stored {
		// Only adopt a part whose size is exactly what this part number should
		// hold; anything else is not a part our client produced.
		_, want, err := PartSpan(u.Size, u.PartSize, u.PartCount, sp.PartNumber)
		if err != nil || sp.Size != want || known[sp.PartNumber] == sp.ETag {
			continue
		}
		if _, err := h.Store.RecordPart(ctx, u.ID, sp.PartNumber, sp.ETag); err != nil {
			httpx.Internal(c, err)
			return
		}
		adopted++
	}
	if adopted > 0 {
		slog.Info("reconciled upload from bucket", "upload_id", u.ID, "adopted_parts", adopted)
	}
	body, err := h.describe(ctx, u)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	c.JSON(http.StatusOK, body)
}

// ---- POST /api/uploads/:id/parts ----

type presignRequest struct {
	PartNumbers []int `json:"partNumbers"`
}

func (h *Handler) presignParts(c *gin.Context) {
	u, ok := h.load(c)
	if !ok {
		return
	}
	var req presignRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.PartNumbers) == 0 || len(req.PartNumbers) > maxPresignBatch {
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "partNumbers must list 1 to 100 part numbers")
		return
	}
	if u.Status != store.UploadUploading {
		httpx.Error(c, http.StatusConflict, "not_uploading", "upload is "+string(u.Status))
		return
	}
	type presigned struct {
		PartNumber int    `json:"partNumber"`
		URL        string `json:"url"`
	}
	urls := make([]presigned, 0, len(req.PartNumbers))
	for _, n := range req.PartNumbers {
		if n < 1 || n > u.PartCount {
			httpx.Error(c, http.StatusBadRequest, "invalid_part", "part "+strconv.Itoa(n)+" is out of range")
			return
		}
		url, err := h.Objects.PresignUploadPart(c.Request.Context(), u.ObjectKey, u.S3UploadID, n, h.Cfg.PresignTTL)
		if err != nil {
			httpx.Internal(c, err)
			return
		}
		urls = append(urls, presigned{PartNumber: n, URL: url})
	}
	c.JSON(http.StatusOK, gin.H{"urls": urls, "expiresAt": time.Now().Add(h.Cfg.PresignTTL)})
}

// ---- PUT /api/uploads/:id/parts/:n ----

type recordRequest struct {
	ETag string `json:"etag"`
}

func (h *Handler) recordPart(c *gin.Context) {
	u, ok := h.load(c)
	if !ok {
		return
	}
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n < 1 || n > u.PartCount {
		httpx.Error(c, http.StatusBadRequest, "invalid_part", "part number is out of range")
		return
	}
	var req recordRequest
	etag := ""
	if err := c.ShouldBindJSON(&req); err == nil {
		etag = storage.NormalizeETag(strings.TrimSpace(req.ETag))
	}
	if etag == "" {
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "etag is required")
		return
	}
	// The ETag is not checked against the bucket here: that would cost a
	// request per part. The bucket verifies every ETag when the upload is
	// completed, and a wrong one fails there.
	part, err := h.Store.RecordPart(c.Request.Context(), u.ID, n, etag)
	if errors.Is(err, store.ErrConflict) {
		httpx.Error(c, http.StatusConflict, "not_uploading", "upload is no longer accepting parts")
		return
	}
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	c.JSON(http.StatusOK, partJSON(part))
}

// ---- POST /api/uploads/:id/complete ----

type completeRequest struct {
	Name        string `json:"name"`
	CharacterID string `json:"characterId"`
}

func (h *Handler) complete(c *gin.Context) {
	u, ok := h.load(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var req completeRequest
	_ = c.ShouldBindJSON(&req) // the body is optional

	switch u.Status {
	case store.UploadAborted:
		httpx.Error(c, http.StatusConflict, "aborted", "upload was aborted")
		return
	case store.UploadCompleted:
		// A client that lost the response retries complete; answer with the
		// asset and job the first call created.
		h.respondCompleted(c, u, http.StatusOK)
		return
	}

	characterID, err := h.Jobs.ResolveCharacter(ctx, u.UserID, req.CharacterID)
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid_character", "unknown characterId")
		return
	}

	parts, err := h.Store.ListParts(ctx, u.ID)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	recorded := make([]int, len(parts))
	completed := make([]storage.CompletedPart, len(parts))
	for i, p := range parts {
		recorded[i] = p.PartNumber
		completed[i] = storage.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	if missing := MissingParts(u.PartCount, recorded); len(missing) > 0 {
		httpx.ErrorWithDetails(c, http.StatusConflict, "missing_parts",
			"upload cannot complete until every part is recorded", gin.H{"missing": missing})
		return
	}

	err = h.Objects.CompleteMultipartUpload(ctx, u.ObjectKey, u.S3UploadID, completed)
	switch {
	case errors.Is(err, storage.ErrInvalidPart):
		httpx.Error(c, http.StatusConflict, "invalid_part",
			"the bucket rejected a recorded ETag; re-upload the affected part")
		return
	case errors.Is(err, storage.ErrNoSuchUpload):
		// Either an earlier complete call got this far and then died before the
		// database commit (the object exists; carry on), or the upload is gone.
		if _, headErr := h.Objects.HeadObject(ctx, u.ObjectKey); headErr != nil {
			httpx.Error(c, http.StatusConflict, "upload_gone", "the multipart upload no longer exists in storage")
			return
		}
	case err != nil:
		httpx.Internal(c, err)
		return
	}

	// Trust but verify: the object the bucket assembled must be exactly as
	// large as the file the client declared.
	info, err := h.Objects.HeadObject(ctx, u.ObjectKey)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	if info.Size != u.Size {
		_ = h.Objects.DeletePrefix(ctx, "assets/"+u.AssetID+"/")
		_ = h.Store.AbortUpload(ctx, u.ID)
		httpx.ErrorWithDetails(c, http.StatusUnprocessableEntity, "size_mismatch",
			"assembled object size differs from the declared size",
			gin.H{"declared": u.Size, "stored": info.Size})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSuffix(u.Filename, filepath.Ext(u.Filename))
	}
	asset := &store.Asset{
		ID: u.AssetID, UserID: u.UserID, Name: name, SourceKey: u.ObjectKey,
		SourceSize: u.Size, ContentType: u.ContentType, Status: store.AssetProcessing,
	}
	newJob := h.Jobs.New(asset.ID, u.UserID, characterID)

	err = h.Store.FinishUpload(ctx, u.ID, asset, newJob)
	if errors.Is(err, store.ErrConflict) {
		// A concurrent complete call won the race; report its result.
		h.respondCompleted(c, u, http.StatusOK)
		return
	}
	if err != nil {
		httpx.Internal(c, err)
		return
	}

	// The job row is committed; putting it on the stream is best effort here.
	// If this fails (or the process dies right now) job.Dispatcher finds the
	// row with enqueued_at IS NULL and enqueues it.
	h.Jobs.EnqueueNow(ctx, newJob)

	c.JSON(http.StatusCreated, gin.H{"assetId": asset.ID, "jobId": newJob.ID})
}

func (h *Handler) respondCompleted(c *gin.Context, u *store.Upload, status int) {
	j, err := h.Store.LatestJobForAsset(c.Request.Context(), u.AssetID)
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	c.JSON(status, gin.H{"assetId": u.AssetID, "jobId": j.ID})
}

// ---- DELETE /api/uploads/:id ----

func (h *Handler) abort(c *gin.Context) {
	u, ok := h.load(c)
	if !ok {
		return
	}
	switch u.Status {
	case store.UploadCompleted:
		httpx.Error(c, http.StatusConflict, "completed", "upload already completed; delete the asset instead")
		return
	case store.UploadAborted:
		c.Status(http.StatusNoContent) // idempotent
		return
	}
	if err := h.abortUpload(c.Request.Context(), u); err != nil {
		httpx.Internal(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// abortUpload releases the parts held by the bucket, then marks the row.
func (h *Handler) abortUpload(ctx context.Context, u *store.Upload) error {
	err := h.Objects.AbortMultipartUpload(ctx, u.ObjectKey, u.S3UploadID)
	if err != nil && !errors.Is(err, storage.ErrNoSuchUpload) {
		return err
	}
	if err := h.Store.AbortUpload(ctx, u.ID); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}

// ---- helpers ----

// load fetches the upload named in the path for the calling user, writing the
// 404 itself when it does not exist or belongs to someone else.
func (h *Handler) load(c *gin.Context) (*store.Upload, bool) {
	id := c.Param("id")
	if uuid.Validate(id) != nil {
		httpx.Error(c, http.StatusNotFound, "not_found", "upload not found")
		return nil, false
	}
	u, err := h.Store.GetUpload(c.Request.Context(), auth.UserID(c), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "not_found", "upload not found")
		return nil, false
	}
	if err != nil {
		httpx.Internal(c, err)
		return nil, false
	}
	return u, true
}

func (h *Handler) describe(ctx context.Context, u *store.Upload) (*uploadJSON, error) {
	parts, err := h.Store.ListParts(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	body := &uploadJSON{
		UploadID: u.ID, AssetID: u.AssetID, Filename: u.Filename, Size: u.Size,
		ContentType: u.ContentType, PartSize: u.PartSize, PartCount: u.PartCount,
		Status: u.Status, Parts: make([]partJSON, len(parts)),
	}
	for i, p := range parts {
		body.Parts[i] = partJSON(p)
	}
	return body, nil
}

// safeExt keeps a short alphanumeric file extension (".mp4") and drops
// anything else, so object keys never carry arbitrary user text.
func safeExt(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if len(ext) < 2 || len(ext) > 6 {
		return ""
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return ext
}
