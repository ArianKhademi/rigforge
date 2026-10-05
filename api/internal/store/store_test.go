package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/ArianKhademi/rigforge/api/internal/store"
	"github.com/ArianKhademi/rigforge/api/internal/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

// TestPostgres runs the same suite against a real database. It needs
// RIGFORGE_TEST_DATABASE_URL (make test sets it to the docker-compose
// Postgres) and is skipped otherwise so plain `go test ./...` needs no Docker.
func TestPostgres(t *testing.T) {
	url := os.Getenv("RIGFORGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RIGFORGE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Migrating twice must be a no-op (both api replicas migrate on boot).
	if err := pg.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		if err := pg.Truncate(ctx); err != nil {
			t.Fatal(err)
		}
		return pg
	})
}
