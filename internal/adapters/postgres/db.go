// Package postgres implements Billing's repository ports on PostgreSQL
// through platform-pgcommon (LLD §3.3, §4). Every pool is built by
// pgcommon.NewPool (BIL-OPS-1).
package postgres

import (
	"fmt"
	"net/url"
	"os"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// SystemPoolName is the pool metric label of the BYPASSRLS system pool
// (billing_system), so its platform_db_* series stay separate.
const SystemPoolName = "system"

// AppPoolConfig returns the pgcommon config for the RLS-bound app pool
// (billing_app): DSN and sizing from pgcommon.ConfigFromEnv, with the
// transaction-local app.tenant_id GUC bound on every checkout (§3.3, §4.3).
func AppPoolConfig(log port.Logger) (pgcommon.Config, []pgcommon.ConfigWarning) {
	cfg, warnings := pgcommon.ConfigFromEnv()
	cfg.GUCProvider = pgcommon.GUCSetFromContext
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	}
	return cfg, warnings
}

// SystemPoolConfig returns the pgcommon config for sysPool (billing_system,
// BYPASSRLS), used only for webhook pre-resolution, cron tenant listing and
// reconciliation (§3.3). It has no GUC provider, keeps PgBouncer mode on and
// inherits sizing from the environment; PG_SYSTEM_MAX_CONNS caps it.
func SystemPoolConfig(dsn string, maxConns int32, log port.Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.PoolName = SystemPoolName
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	if _, set := os.LookupEnv("PG_MIN_CONNS"); !set {
		cfg.MinConns = 0
	}
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	}
	return cfg
}

// SystemDSNFromEnv returns the sysPool DSN: SYSTEM_DATABASE_URL when set,
// else the app DSN. In developer environments that fallback keeps a
// single-role setup working; cross-tenant queries then see zero rows.
func SystemDSNFromEnv() string {
	if dsn := os.Getenv("SYSTEM_DATABASE_URL"); dsn != "" {
		return dsn
	}
	cfg, _ := pgcommon.ConfigFromEnv()
	return cfg.DSN
}

// MigrationDSNFromEnv returns the DSN for cmd/migrate: MIGRATION_DATABASE_URL
// (direct to Postgres, never PgBouncer, because the migration lock is a
// session-scoped advisory lock), else the app DSN (§12, §13.4).
func MigrationDSNFromEnv() string {
	return pgcommon.MigrationDSNFromEnv()
}

// AuditDrainerPoolConfig returns the pool for platform-audit's runner:
// billing_audit_drainer (NOBYPASSRLS, policy audit_drainer_all), the app
// DSN's host and database with the drainer's credentials (§3.3, §12).
func AuditDrainerPoolConfig(appDSN, user, password string, maxConns int32, log port.Logger) (pgcommon.Config, error) {
	cfg, _ := pgcommon.ConfigFromEnv()
	u, err := url.Parse(appDSN)
	if err != nil || u.Scheme == "" {
		return cfg, fmt.Errorf("audit drainer DSN: the app DSN is not a URL")
	}
	u.User = url.UserPassword(user, password)
	cfg.DSN, cfg.PoolName, cfg.GUCProvider, cfg.MinConns = u.String(), "audit_drainer", nil, 0
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	}
	return cfg, nil
}
