//go:build integration

package main

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
)

// migrate brings a fresh database (the service's roles from
// scripts/init-db.sql) to this build's schema, and a second run finds it
// up to date.
func TestRunMigratesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("billing"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		tcpostgres.WithInitScripts("../../scripts/init-db.sql"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword("billing_migrator", "devpassword")

	clearEnv(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("MIGRATION_DATABASE_URL", u.String())
	t.Setenv("MIGRATE_TIMEOUT", "5m")
	for i := range 2 {
		if got := run(); got != 0 {
			t.Fatalf("run %d: exit %d, want 0", i+1, got)
		}
	}
	version, dirty, err := pgadapter.DomainVersion(ctx, u.String())
	if err != nil || dirty || version < 5 {
		t.Fatalf("domain version %d dirty %v: %v", version, dirty, err)
	}
}
