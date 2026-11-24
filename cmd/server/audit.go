package main

import (
	"context"
	"fmt"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// auditParts is AL-5 direct-write in outbox mode (§3.3, §10.6).
type auditParts struct {
	writer *pgadapter.AuditWriter
	runner *auditoutbox.Runner // nil when no drainer password is set (dev)
	pool   *pgcommon.Pool
}

// buildAudit reads AUDIT_* (failing fast on a malformed value), registers
// the library metrics, checks billing_app can enqueue, and builds the
// runner on the billing_audit_drainer pool.
func buildAudit(ctx context.Context, cfg config.Config, appCfg pgcommon.Config, appPool *pgcommon.Pool, env string, log port.Logger) (*auditParts, error) {
	acfg, err := auditconfig.FromEnv("")
	if err != nil {
		return nil, fmt.Errorf("AUDIT_* configuration: %w", err)
	}
	if _, err := audit.InitMetrics(audit.MetricsIdentity{Domain: "commercial", Service: cfg.AppName, Environment: env},
		gincommon.MetricsRegisterer()); err != nil {
		return nil, fmt.Errorf("platform-audit metrics: %w", err)
	}
	if err := auditoutbox.CheckEnqueueAccess(ctx, appPool); err != nil {
		return nil, fmt.Errorf("billing_app cannot enqueue audit entries: %w", err)
	}
	enq, err := auditoutbox.NewEnqueuer(acfg.EnqueueConfig(log))
	if err != nil {
		return nil, err
	}
	out := &auditParts{writer: pgadapter.NewAuditWriter(enq)}
	if cfg.PGAuditDrainerPassword == "" {
		log.Warn("PG_AUDIT_DRAINER_PASSWORD unset; audit entries queue in pending_audit_entries and are not delivered (dev only)", nil)
		return out, nil
	}
	dcfg, err := pgadapter.AuditDrainerPoolConfig(appCfg.DSN, cfg.PGAuditDrainerUser, cfg.PGAuditDrainerPassword, int32(cfg.PGAuditDrainerMaxConns), log)
	if err != nil {
		return nil, err
	}
	if out.pool, err = pgcommon.NewPool(ctx, dcfg); err != nil {
		return nil, fmt.Errorf("audit drainer pool: %w", err)
	}
	client, err := acfg.NewClient(log)
	if err != nil {
		return nil, err
	}
	if out.runner, err = auditoutbox.NewRunner(acfg.RunnerConfig(out.pool, client, enq, log)); err != nil {
		return nil, err
	}
	return out, nil
}
