// Command migrate brings the billing database to this build's schema and
// exits (LLD §4.4, §19.1). The Helm chart runs it once per release as a
// pre-install / pre-upgrade hook Job, so server replicas (RUN_MIGRATIONS=
// false) never race for the migration lock.
//
// It connects with MIGRATION_DATABASE_URL, direct to Postgres and never
// through PgBouncer, because the migration lock is a session-scoped advisory
// lock. Exit status 0 on success (including "already up to date"), 1 on any
// failure.
//
// Order (§4.4): platform-events outbox and inbox schemas, platform-audit
// pending_audit_entries, then Billing's 000001 to 000007.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

// defaultMigrateTimeout bounds the run, lock wait included (MIGRATE_TIMEOUT
// overrides it, 1–30 m). Keep it below the Job's activeDeadlineSeconds.
const defaultMigrateTimeout = 10 * time.Minute

func main() { os.Exit(run()) }

func run() int {
	// Fail closed in Kubernetes with APP_ENV unset (config.ResolveEnv).
	env, envErr := config.ResolveEnv(os.Getenv)
	if envErr != nil {
		fmt.Fprintf(os.Stderr, "migrate: APP_ENV: %v\n", envErr)
		return 1
	}
	logEnv := env
	if config.IsDevLike(env) {
		logEnv = "dev"
	}
	log, err := logger.NewLogger(logEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: init logger: %v\n", err)
		return 1
	}
	defer func() { _ = gincommon.Shutdown(log) }()

	dsn := pgadapter.MigrationDSN()
	if dsn == "" {
		log.Error("migrate: no database DSN; set MIGRATION_DATABASE_URL (direct to Postgres, not PgBouncer)", nil)
		return 1
	}

	timeout := defaultMigrateTimeout
	if v := os.Getenv("MIGRATE_TIMEOUT"); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d < time.Minute || d > 30*time.Minute {
			log.Error("migrate: MIGRATE_TIMEOUT must be a duration between 1m and 30m", map[string]any{"value": v})
			return 1
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	if err := pgadapter.MigrateSchema(ctx, dsn, log); err != nil {
		log.Error("migrate failed", map[string]any{"error": err.Error()})
		return 1
	}
	version, dirty, err := pgadapter.DomainVersion(ctx, dsn)
	if err != nil {
		log.Error("migrate: read version", map[string]any{"error": err.Error()})
		return 1
	}
	log.Info("migrate complete", map[string]any{"domain_version": version, "dirty": dirty, "duration_ms": time.Since(start).Milliseconds()})
	return 0
}
