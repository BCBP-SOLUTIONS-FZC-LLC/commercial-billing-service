package main

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"APP_ENV", "ENVIRONMENT", "MIGRATION_DATABASE_URL", "DATABASE_URL", "PG_HOST", "PG_USER", "PG_DBNAME", "PG_PASSWORD", "MIGRATE_TIMEOUT", "LOG_FILE"} {
		t.Setenv(k, "")
	}
}

func TestRunWithoutDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("ENVIRONMENT", "Prod")
	if got := run(); got != 1 {
		t.Fatalf("no DSN: exit %d, want 1", got)
	}
}

func TestRunRejectsTimeout(t *testing.T) {
	for _, v := range []string{"soon", "30s", "31m"} {
		clearEnv(t)
		t.Setenv("APP_ENV", "test")
		t.Setenv("MIGRATION_DATABASE_URL", "postgres://billing_migrator:x@127.0.0.1:1/billing?sslmode=disable")
		t.Setenv("MIGRATE_TIMEOUT", v)
		if got := run(); got != 1 {
			t.Errorf("MIGRATE_TIMEOUT=%s: exit %d, want 1", v, got)
		}
	}
}

// An unreachable server fails the run (exit 1) within the timeout.
func TestRunUnreachableDatabase(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://billing_migrator:x@127.0.0.1:1/billing?sslmode=disable&connect_timeout=1")
	t.Setenv("MIGRATE_TIMEOUT", "1m")
	if got := run(); got != 1 {
		t.Fatalf("unreachable database: exit %d, want 1", got)
	}
}

// Fail closed: in Kubernetes with APP_ENV unset the hook Job refuses to run.
func TestRunRefusesUnsetEnvInKubernetes(t *testing.T) {
	clearEnv(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://billing_migrator:x@127.0.0.1:1/billing?sslmode=disable")
	if got := run(); got != 1 {
		t.Fatalf("exit %d, want 1", got)
	}
}

func TestRunLoggerFailure(t *testing.T) {
	clearEnv(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FILE", filepath.Join(blocker, "app.log"))
	if got := run(); got != 1 {
		t.Fatalf("unwritable LOG_FILE: exit %d, want 1", got)
	}
}
