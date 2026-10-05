package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ArianKhademi/rigforge/api/internal/store"
	"github.com/ArianKhademi/rigforge/api/internal/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

// TestPostgres runs the same suite against a real database. It needs
// RIGFORGE_TEST_DATABASE_URL (make test sets it to the docker-compose
// Postgres) and is skipped otherwise so plain `go test ./...` needs no Docker.
//
// The suite empties the tables between cases, so it works in a schema of its
// own. Go runs test packages in parallel, and the integration tests use the
// same database: truncating the shared tables would pull rows out from under
// them.
func TestPostgres(t *testing.T) {
	base := os.Getenv("RIGFORGE_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("RIGFORGE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("storetest_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(ctx)
	})

	// search_path makes every connection of the pool resolve table names in
	// the private schema first.
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	pg, err := store.NewPostgres(ctx, u.String())
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
