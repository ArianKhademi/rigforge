// Command api is the Rigforge HTTP api.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ArianKhademi/rigforge/api/internal/auth"
	"github.com/ArianKhademi/rigforge/api/internal/config"
	"github.com/ArianKhademi/rigforge/api/internal/job"
	"github.com/ArianKhademi/rigforge/api/internal/server"
	"github.com/ArianKhademi/rigforge/api/internal/storage"
	"github.com/ArianKhademi/rigforge/api/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// ctx is cancelled on SIGINT/SIGTERM; Kubernetes sends SIGTERM on rollout.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	// Postgres may still be starting when the api pod comes up; wait for it
	// instead of crash-looping.
	if err := waitFor(ctx, "postgres", 60*time.Second, db.Ping); err != nil {
		return err
	}
	if err := db.Migrate(ctx); err != nil {
		return err
	}

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()

	objects, err := storage.NewS3(ctx, storage.S3Config(cfg.S3))
	if err != nil {
		return err
	}
	if os.Getenv("S3_CREATE_BUCKET") == "true" {
		if err := waitFor(ctx, "object storage", 60*time.Second, objects.EnsureBucket); err != nil {
			return err
		}
	}

	srv := server.New(server.Deps{
		Store:    db,
		Objects:  objects,
		Queue:    &job.RedisQueue{Client: rdb},
		Events:   &job.RedisEvents{Client: rdb},
		Verifier: auth.NewVerifier(cfg.Auth.JWKSURL, cfg.Auth.Issuer, cfg.Auth.Audience),
		Upload:   cfg.Upload,
		Ready: func(ctx context.Context) error {
			return errors.Join(db.Ping(ctx), rdb.Ping(ctx).Err())
		},
		CORSOrigins: cfg.CORSOrigins,
	})

	// Background loops share the process lifetime.
	go srv.Uploads.RunReaper(ctx)
	go srv.Jobs.RunDispatcher(ctx, 10*time.Second)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Router,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE endpoint keeps responses open for minutes.
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	slog.Info("api listening", "addr", cfg.Addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: stop accepting connections and give in-flight
	// requests a few seconds. Open SSE streams end when their context does.
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// waitFor retries check once a second until it succeeds or the timeout passes.
func waitFor(ctx context.Context, name string, timeout time.Duration, check func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := check(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.Join(errors.New(name+" not reachable"), err)
		}
		slog.Info("waiting for dependency", "name", name, "err", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
