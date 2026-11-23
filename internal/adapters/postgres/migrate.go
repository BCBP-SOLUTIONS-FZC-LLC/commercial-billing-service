package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/inbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

// migrationsFS holds Billing's own migrations, 000001 to 000005 (LLD §4.4).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// DomainMigrationsTable tracks Billing's own migration versions. The
// platform libraries track theirs in their own tables.
const DomainMigrationsTable = "billing_schema_migrations"

// MigrateSchema brings the database to this build's schema in the order of
// LLD §4.4. Each library applies its own schema with its own tracking table:
//
//  1. platform-events outbox (outbox_events, outbox_dead_letters) and inbox
//     (processed_events): "000000_outbox_schema";
//  2. platform-audit pending_audit_entries (auditoutbox.ApplySchema), which
//     must exist before 000005 adds RLS to it;
//  3. Billing's migrations 000001 to 000005.
//
// dsn must reach Postgres directly, never PgBouncer: the migration lock is a
// session-scoped advisory lock (§12 MIGRATION_DATABASE_URL).
func MigrateSchema(ctx context.Context, dsn string, log port.Logger) error {
	var logger pgdomain.Logger
	if log != nil {
		logger = NewLoggerAdapter(log)
	}
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn, Logger: logger}); err != nil {
		return explainMigrationError("outbox schema", err)
	}
	if err := inbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn, Logger: logger}); err != nil {
		return explainMigrationError("inbox schema", err)
	}
	if err := auditoutbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn, Logger: logger}); err != nil {
		return explainMigrationError("audit outbox schema", err)
	}
	if err := domainRunner(dsn, logger).Up(ctx); err != nil {
		return explainMigrationError("domain migrations", err)
	}
	return nil
}

// DomainVersion returns the applied Billing migration version.
func DomainVersion(ctx context.Context, dsn string) (version uint, dirty bool, err error) {
	return domainRunner(dsn, nil).Version(ctx)
}

func domainRunner(dsn string, logger pgdomain.Logger) *pgmigrate.Runner {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(fmt.Sprintf("embedded migrations: %v", err)) // compile-time layout; cannot fail at runtime
	}
	return &pgmigrate.Runner{FS: sub, DSN: dsn, Logger: logger, MigrationsTable: DomainMigrationsTable}
}

func explainMigrationError(stage string, err error) error {
	switch {
	case errors.Is(err, pgmigrate.ErrMigrationDirty):
		return fmt.Errorf("%s: a previous migration failed part-way and left the schema dirty; repair it by hand, then force the version (LLD §19.5): %w", stage, err)
	case errors.Is(err, pgmigrate.ErrVersionNotInSource):
		return fmt.Errorf("%s: the database schema is newer than this image (rolled back to an older build?): %w", stage, err)
	default:
		return fmt.Errorf("%s: %w", stage, err)
	}
}
