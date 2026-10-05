// Package server assembles the Gin router from the handler packages.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ArianKhademi/rigforge/api/internal/asset"
	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/character"
	"github.com/ArianKhademi/rigforge/api/internal/config"
	"github.com/ArianKhademi/rigforge/api/internal/httpx"
	"github.com/ArianKhademi/rigforge/api/internal/job"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
	"github.com/ArianKhademi/rigforge/api/internal/upload"
)

type Deps struct {
	Store    store.Store
	Objects  storage.ObjectStore
	Queue    job.Enqueuer
	Events   job.Subscriber
	Verifier *auth.Verifier
	Upload   config.Upload
	// Ready reports whether the dependencies the api cannot work without
	// (database, redis) are reachable; it backs the readiness probe.
	Ready       func(ctx context.Context) error
	CORSOrigins []string
}

type Server struct {
	Router  *gin.Engine
	Uploads *upload.Handler
	Jobs    *job.Service
}

func New(d Deps) *Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), httpx.RequestLogger(), httpx.CORS(d.CORSOrigins))

	// Liveness: the process is up. Kubernetes restarts the pod if this fails,
	// so it must not depend on anything external.
	health := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }
	r.GET("/healthz", health)
	r.GET("/api/health", health)
	// Readiness: dependencies are reachable. Failing this only takes the pod
	// out of the Service's endpoints; it is not restarted.
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if d.Ready != nil {
			if err := d.Ready(ctx); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	jobs := &job.Service{Store: d.Store, Queue: d.Queue}
	uploads := &upload.Handler{Store: d.Store, Objects: d.Objects, Jobs: jobs, Cfg: d.Upload}

	// Everything under /api (except /api/health above) requires a valid JWT.
	api := r.Group("/api", auth.Middleware(d.Verifier))
	api.GET("/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"userId": auth.UserID(c)}) })
	uploads.Register(api)
	(&asset.Handler{Store: d.Store, Objects: d.Objects, Jobs: jobs}).Register(api)
	(&job.Handler{Store: d.Store, Events: d.Events, Resync: 5 * time.Second}).Register(api)
	(&character.Handler{Store: d.Store, Objects: d.Objects}).Register(api)

	return &Server{Router: r, Uploads: uploads, Jobs: jobs}
}
