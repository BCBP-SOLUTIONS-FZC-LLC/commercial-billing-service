// Package postgres implements Billing's repository ports on PostgreSQL
// through platform-pgcommon (LLD §3.3, §4). Every pool is built by
// pgcommon.NewPool from a pgcommon.Config (BIL-OPS-1); every setting comes
// from pgcommon.ConfigFromEnv (DATABASE_URL or PG_*, PG_SSLMODE, sizing,
// PgBouncer mode, PG_STATEMENT_TIMEOUT, PG_LOCK_TIMEOUT, slow-query
// threshold); every statement runs in a pgcommon transaction on pgcommon's
// types. No package imports pgx, pgconn, pgxpool or database/sql (lint:
// depguard).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Pool metric labels (platform_db_* "pool"), one per role.
const (
	AppPoolName          = "app"
	SystemPoolName       = "system"
	AuditDrainerPoolName = "audit_drainer"
)

// Role is a database role other than billing_app (§4.1): the system pool
// (billing_system, BYPASSRLS) and the audit drainer (billing_audit_drainer).
type Role struct {
	User, Password string
	MaxConns       int32
}

// Pools are the service's connections (§3.3).
type Pools struct {
	App, System *pgcommon.Pool
	// Warnings are pgcommon.ConfigFromEnv's; log them at startup.
	Warnings []pgcommon.ConfigWarning
	// SystemReusesApp is true when no system password was configured (dev
	// only): the system pool then runs as billing_app and cross-tenant
	// queries see no rows.
	SystemReusesApp bool
}

// SpanTracer is pgcommon's query-span hook (gincommon.NewSpanTracer).
type SpanTracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, func())
}

// PoolOptions are what the composition root adds to pgcommon's config.
type PoolOptions struct {
	System Role
	Tracer SpanTracer // nil: no query spans
	Log    port.Logger
}

// OpenPools opens the app pool (billing_app, RLS: app.tenant_id bound on
// every checkout, §4.3) and the system pool (billing_system, no GUC,
// PgBouncer mode, PG_SYSTEM_MAX_CONNS). Both start from
// pgcommon.ConfigFromEnv; the system pool differs only by role and size.
func OpenPools(ctx context.Context, o PoolOptions) (*Pools, error) {
	base, warnings := pgcommon.ConfigFromEnv()
	app := base
	app.PoolName, app.GUCProvider = AppPoolName, pgcommon.GUCSetFromContext
	if o.Tracer != nil {
		app.Tracer = o.Tracer
	}
	if o.Log != nil {
		app.Logger = NewLoggerAdapter(o.Log)
	}
	sys, reuses, err := roleConfig(base, o.System, SystemPoolName)
	if err != nil {
		return nil, err
	}
	sys.GUCProvider, sys.PGBouncerMode, sys.Tracer, sys.Logger = nil, true, app.Tracer, app.Logger
	p := &Pools{Warnings: warnings, SystemReusesApp: reuses}
	if p.App, err = pgcommon.NewPool(ctx, app); err != nil {
		return nil, fmt.Errorf("app pool: %w", err)
	}
	if p.System, err = pgcommon.NewPool(ctx, sys); err != nil {
		p.App.Close()
		return nil, fmt.Errorf("system pool: %w", err)
	}
	return p, nil
}

// Close closes both pools.
func (p *Pools) Close() {
	p.System.Close()
	p.App.Close()
}

// AuditDrainerConfig is the pool platform-audit's runner uses:
// billing_audit_drainer (NOBYPASSRLS, policy audit_drainer_all, §3.3) —
// pgcommon's environment config with the drainer's credentials and size.
func AuditDrainerConfig(r Role, log port.Logger) (pgcommon.Config, error) {
	base, _ := pgcommon.ConfigFromEnv()
	cfg, reuses, err := roleConfig(base, r, AuditDrainerPoolName)
	if err != nil {
		return cfg, err
	}
	if reuses {
		return cfg, errors.New("audit drainer: PG_AUDIT_DRAINER_PASSWORD is not set")
	}
	cfg.GUCProvider, cfg.MinConns = nil, 0
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	}
	return cfg, nil
}

// roleConfig is base (pgcommon's environment config) for another role:
// same host, database, TLS and timeouts; the role's user and password; the
// role's pool size. Without a password the base DSN is kept (dev only;
// config validation requires the password elsewhere).
func roleConfig(base pgcommon.Config, r Role, pool string) (pgcommon.Config, bool, error) {
	cfg := base
	cfg.PoolName = pool
	if r.MaxConns > 0 {
		cfg.MaxConns = r.MaxConns
		if cfg.MinConns > cfg.MaxConns {
			cfg.MinConns = cfg.MaxConns
		}
	}
	if r.Password == "" {
		return cfg, true, nil
	}
	u, err := url.Parse(base.DSN)
	if err != nil || u.Scheme == "" {
		return cfg, false, fmt.Errorf("%s pool: pgcommon's DSN is not a URL (set DATABASE_URL as postgres://…)", pool)
	}
	u.User = url.UserPassword(r.User, r.Password)
	cfg.DSN = u.String()
	return cfg, false, nil
}

// MigrationDSN is pgcommon's migration DSN (MIGRATION_DATABASE_URL: direct
// to Postgres, never PgBouncer, because the migration lock is a
// session-scoped advisory lock; else the app DSN) (§12, §13.4).
func MigrationDSN() string { return pgcommon.MigrationDSNFromEnv() }
