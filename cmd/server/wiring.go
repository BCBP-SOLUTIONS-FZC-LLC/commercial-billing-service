package main

import (
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/webhook"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/bootstrap"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// workers holds the in-pod background loops (§13.1).
type workers struct {
	cancel    context.CancelFunc
	done      <-chan struct{} // closed when the webhook processor's supervision ended
	outbox    *outbox.Runner
	consumers []queueConsumer
	audit     *auditoutbox.Runner
	log       port.Logger
}

// startWorkers starts the registry refresh, the webhook processor, the
// outbox publisher, the SQS consumers and the delayed-emission worker,
// each under lc's supervision: a loop that returns while the pod is not
// shutting down drops its commercial_billing_background_loop_running gauge
// (BillingBackgroundLoopStopped) and is restarted in-process with backoff;
// liveness is not affected (lifecycle.go). The platform-audit runner is
// single-use: it is not restarted, and its Alive check fails liveness.
func startWorkers(cfg config.Config, reg *provider.Registry, appPool, sysPool *pgcommon.Pool, b *bus, svc *app.Service,
	auditRunner *auditoutbox.Runner, m *metrics.Metrics, lc *lifecycle, log port.Logger) *workers {
	ctx, cancel := context.WithCancel(context.Background())
	w := &workers{cancel: cancel, outbox: b.outbox, consumers: b.consumers, audit: auditRunner, log: log}
	run := func(name string, loop func(ctx context.Context) error) <-chan struct{} {
		return lc.supervise(ctx, name, log, true, loop)
	}
	loop := func(f func(ctx context.Context)) func(ctx context.Context) error {
		return func(ctx context.Context) error { f(ctx); return nil }
	}
	if auditRunner != nil {
		lc.supervise(ctx, "audit-runner", log, false, auditRunner.Start)
	}
	run("saga-worker", loop(func(ctx context.Context) {
		pollLoop(ctx, cfg.SagaPollInterval, sagaBatch, svc.RunSagas, log, "saga worker")
	}))
	run("lifecycle-follow-up", loop(func(ctx context.Context) {
		pollLoop(ctx, followUpInterval, sagaBatch, svc.RunLifecycleFollowUps, log, "lifecycle follow-up")
	}))
	run("reconciliation", loop(func(ctx context.Context) {
		pollLoop(ctx, reconPollInterval, 1, svc.RunReconciliation, log, "reconciliation run")
	}))
	if b.outbox != nil {
		run("outbox", b.outbox.Start)
	}
	for _, c := range b.consumers {
		run("sqs:"+c.name, c.Start)
	}
	run("emissions", loop(func(ctx context.Context) { b.emissions.Run(ctx, cfg.EmissionPollInterval) }))
	proc := &app.WebhookProcessor{
		Ledger:      pgadapter.NewLedgerRepository(sysPool),
		Providers:   reg,
		Tx:          pgadapter.NewTxRunner(appPool),
		Handlers:    bootstrap.PaymentHandlers(svc),
		MaxAttempts: cfg.WebhookMaxAttempts,
		Log:         log,
		Metrics:     m,
	}
	run("metrics-snapshot", loop(func(ctx context.Context) {
		exportSnapshot(ctx, pgadapter.NewSnapshotRepository(sysPool), m, cfg.SagaAlertAfter, log)
	}))
	run("provider-registry", loop(func(ctx context.Context) { reg.Run(ctx, cfg.ProviderRegistryRefresh, certifiedGauge(m)) }))
	w.done = run("webhook-processor", loop(func(ctx context.Context) { proc.Run(ctx, cfg.WebhookPollInterval) }))
	return w
}

// stop cancels the loops and stops the SQS consumers, the audit runner and
// the outbox runner in parallel (each drains its in-flight work), waiting
// at most timeout in total; the webhook processor's return is awaited in
// the same window.
func (w *workers) stop(timeout time.Duration) {
	w.cancel()
	var wg sync.WaitGroup
	for _, c := range w.consumers {
		wg.Go(func() {
			if err := c.Stop(); err != nil {
				w.log.Error("sqs consumer stop", map[string]any{"queue": c.name, "error": err.Error()})
			}
		})
	}
	if w.audit != nil {
		wg.Go(func() {
			sctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := w.audit.Stop(sctx); err != nil {
				w.log.Error("audit runner stop", map[string]any{"error": err.Error()})
			}
		})
	}
	if w.outbox != nil {
		wg.Go(func() {
			if err := w.outbox.Stop(); err != nil {
				w.log.Error("outbox runner stop", map[string]any{"error": err.Error()})
			}
		})
	}
	all := make(chan struct{})
	go func() {
		wg.Wait()
		<-w.done
		close(all)
	}()
	select {
	case <-all:
	case <-time.After(timeout):
		w.log.Warn("background loops did not all stop within the shutdown budget", map[string]any{"timeout_ms": timeout.Milliseconds()})
	}
}

// webhookRoute mounts B-W1 (§5.4.3) with no authentication middleware (BIL-AUTH-5).
func webhookRoute(cfg config.Config, reg *provider.Registry, sysPool *pgcommon.Pool, m *metrics.Metrics, log port.Logger) func(gin.IRouter) {
	h := &webhook.Handler{
		Providers: reg, Ledger: pgadapter.NewLedgerRepository(sysPool), Customers: pgadapter.NewSystemRepository(sysPool),
		MaxBodyBytes: cfg.WebhookMaxBodyBytes, Log: log,
		OnSignatureFailure: m.SignatureFailure, OnDuplicate: m.WebhookDuplicate,
	}
	return h.Register
}

// apiRoutes mounts the tenant, internal and operator routes (§5) on the
// use cases; a route whose flow is not built yet answers 501.
func apiRoutes(cfg config.Config, ginCfg gincommon.Config, appPool *pgcommon.Pool, svc *app.Service, m *metrics.Metrics) func(gin.IRouter) {
	api := &httpadapter.APIConfig{
		GinConfig: ginCfg, Tenant: svc, Internal: svc, Operator: svc,
		Idem: &httpadapter.Idempotency{Repo: pgadapter.NewIdempotencyRepository(appPool), Tx: pgadapter.NewTxRunner(appPool),
			TTL: cfg.IdempotencyTTL, Lock: idempotencyLock},
		OperatorPathLive: cfg.OperatorPathLive, DevLike: config.IsDevLike(cfg.AppEnv), BFFCallerPrincipal: cfg.BFFCallerPrincipal,
		Disabled: disabledRoutes(cfg), OnOperatorAction: m.OperatorAction,
	}
	return api.Register
}

// reconPollInterval is how often a pod looks for a queued reconciliation run.
const reconPollInterval = 30 * time.Second

// followUpInterval paces the lifecycle follow-ups (§8.8.4, §8.10).
const followUpInterval = 10 * time.Second

// sagaBatch is how many due sagas (or follow-ups) one pass handles.
const sagaBatch = 20

// idempotencyLock is §9.2's in-flight lock.
const idempotencyLock = 30 * time.Second

// disabledRoutes are the routes behind a feature flag that is off (§12):
// not registered, so they answer 404.
func disabledRoutes(cfg config.Config) map[string]bool {
	off := map[string]bool{}
	set := func(enabled bool, ids ...string) {
		for _, id := range ids {
			if !enabled {
				off[id] = true
			}
		}
	}
	set(cfg.FeatureDirectSignup, "B-I1")
	set(cfg.FeatureTokenAddons, "B-P19", "B-P20", "B-P21", "B-P22", "B-P23")
	set(cfg.FeatureSeatPurchase, "B-P6", "B-P7")
	set(cfg.FeatureTrialCapOverride, "B-O18")
	return off
}

// snapshotInterval is the §11.2 exporter period for the database gauges.
const snapshotInterval = 30 * time.Second

// exportSnapshot refreshes the database-derived gauges every 30 s
// (webhook backlog, sagas, subscriptions, pauses, held drafts, emissions).
func exportSnapshot(ctx context.Context, r port.SnapshotReader, m *metrics.Metrics, sagaAlertAfter time.Duration, log port.Logger) {
	t := time.NewTicker(snapshotInterval)
	defer t.Stop()
	for {
		snap, err := r.MetricsSnapshot(ctx, time.Now().Add(-sagaAlertAfter))
		if err == nil {
			m.SetSnapshot(snap)
		} else if ctx.Err() == nil {
			log.Warn("metrics snapshot failed; gauges keep their last value", map[string]any{"error": err.Error()})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// certifiedGauge sets commercial_billing_provider_certified from a
// registry refresh (BIL-PROV-1).
func certifiedGauge(m *metrics.Metrics) func([]provider.Status) {
	return func(st []provider.Status) {
		for _, p := range st {
			m.ProviderCertified(p.Provider, p.AdapterVersion, p.Enabled)
		}
	}
}
