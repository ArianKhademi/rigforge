package character

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/httpx"
	"github.com/ArianKhademi/rigforge/api/internal/job"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

// MaxSize caps a character upload. Characters go through the api (not a
// presigned URL) because the api has to parse and validate them, so they are
// read into memory; 32 MiB keeps that safe. Videos never take this path.
const MaxSize = 32 << 20

type Handler struct {
	Store   store.Store
	Objects storage.ObjectStore
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/characters", h.list)
	rg.POST("/characters", h.create)
	rg.DELETE("/characters/:id", h.delete)
}

type characterJSON struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Builtin   bool       `json:"builtin"`
	CreatedAt *time.Time `json:"createdAt"`
}

// builtins are always available and are resolved by the worker without a
// database row: "default" is the mannequin bundled in the worker image.
var builtins = []characterJSON{
	{ID: job.CharacterDefault, Name: "Rigforge Mannequin", Builtin: true},
	{ID: job.CharacterNone, Name: "Skeleton only", Builtin: true},
}

func (h *Handler) list(c *gin.Context) {
	own, err := h.Store.ListCharacters(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	out := append([]characterJSON{}, builtins...)
	for i := range own {
		out = append(out, characterJSON{ID: own[i].ID, Name: own[i].Name, CreatedAt: &own[i].CreatedAt})
	}
	c.JSON(http.StatusOK, gin.H{"characters": out})
}

// create accepts multipart/form-data with a "file" field (the GLB) and an
// optional "name" field.
func (h *Handler) create(c *gin.Context) {
	// MaxBytesReader makes the read fail once the body exceeds the cap, so an
	// oversized upload is cut off instead of being buffered.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxSize+1<<20)
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Error(c, http.StatusRequestEntityTooLarge, "too_large", "character file exceeds 32 MiB")
			return
		}
		httpx.Error(c, http.StatusBadRequest, "invalid_request", `multipart field "file" with a .glb is required`)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, MaxSize+1))
	if err != nil {
		httpx.Error(c, http.StatusBadRequest, "invalid_request", "could not read the uploaded file")
		return
	}
	if len(data) > MaxSize {
		httpx.Error(c, http.StatusRequestEntityTooLarge, "too_large", "character file exceeds 32 MiB")
		return
	}
	if err := ValidateRig(data); err != nil {
		httpx.Error(c, http.StatusUnprocessableEntity, "invalid_rig", err.Error())
		return
	}

	name := strings.TrimSpace(c.Request.FormValue("name"))
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))
	}
	if len(name) > 100 {
		name = name[:100]
	}

	ch := &store.Character{ID: uuid.NewString(), UserID: auth.UserID(c), Name: name, Size: int64(len(data))}
	ch.ObjectKey = "characters/" + ch.ID + ".glb"
	ctx := c.Request.Context()
	if err := h.Objects.PutObject(ctx, ch.ObjectKey, "model/gltf-binary", bytes.NewReader(data), ch.Size); err != nil {
		httpx.Internal(c, err)
		return
	}
	if err := h.Store.CreateCharacter(ctx, ch); err != nil {
		_ = h.Objects.DeletePrefix(ctx, ch.ObjectKey)
		httpx.Internal(c, err)
		return
	}
	c.JSON(http.StatusCreated, characterJSON{ID: ch.ID, Name: ch.Name, CreatedAt: &ch.CreatedAt})
}

func (h *Handler) delete(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	if uuid.Validate(id) != nil {
		httpx.Error(c, http.StatusNotFound, "not_found", "character not found")
		return
	}
	ch, err := h.Store.GetCharacter(ctx, auth.UserID(c), id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.Error(c, http.StatusNotFound, "not_found", "character not found")
		return
	}
	if err != nil {
		httpx.Internal(c, err)
		return
	}
	if err := h.Store.DeleteCharacter(ctx, ch.UserID, ch.ID); err != nil {
		httpx.Internal(c, err)
		return
	}
	// Jobs already processed with this character keep their outputs: the mesh
	// was baked into each motion.glb.
	_ = h.Objects.DeletePrefix(ctx, ch.ObjectKey)
	c.Status(http.StatusNoContent)
}
