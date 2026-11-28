// Command server is Billing's composition root (LLD §3): it loads and
// validates configuration (§12), builds the logger, tracing and metrics
// (gincommon v2), opens the app pool (billing_app, RLS) and sysPool
// (billing_system), serves the API on APP_PORT and metrics on METRICS_PORT,
// and shuts down gracefully within the budget in lifecycle.go.
//
// In-pod work (§13.1): the provider registry and its refresh, B-W1 and the
// webhook processor, the billing.events outbox publisher, the
// tenant-billing-q consumer (usage-billing-q only with
// USAGE_BILLING_CONSUMER_ENABLED, BL-OQ-24), the delayed-emission worker,
// the saga worker, lifecycle follow-ups, reconciliation runs, the metrics
// snapshot exporter and the platform-audit runner. Each runs supervised: an
// unexpected stop is logged, alerted (BillingBackgroundLoopStopped) and
// restarted in-process with backoff (lifecycle.go). Deterministic checks
// (configuration, Glue schema versions, each queue's RedrivePolicy) run
// before the listeners open, so a bad deploy fails startup instead.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/api"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/observability"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/bootstrap"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
)

// buildVersion is injected with -ldflags at build time.
var buildVersion = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	cfg, problems := config.Load()
	if cfg.BuildVersion == "" {
		cfg.BuildVersion = buildVersion
	}
	if !config.IsDevLike(cfg.AppEnv) {
		gin.SetMode(gin.ReleaseMode)
	}

	rawLog, err := logger.NewLogger(loggerEnv(cfg.AppEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		return 1
	}
	// Every line passes the §10.1 redaction list (LOG_HASH_SALT); the raw
	// logger is kept only for the final flush.
	log := app.RedactingLogger(rawLog, cfg.LogHashSalt)
	// Deferred first so it runs last: every log line of the shutdown (pool
	// drain, tracer) is written before the logger and tracer flush.
	defer func() {
		if err := gincommon.Shutdown(rawLog); err != nil {
			fmt.Fprintf(os.Stderr, "flush logger/tracer: %v\n", err)
		}
	}()
	// §12: refuse to start on any missing, malformed or out-of-range value.
	if len(problems) > 0 {
		for _, p := range problems {
			log.Error("invalid configuration", map[string]any{"key": p.Key, "reason": p.Reason})
		}
		return 1
	}

	ginCfg := gincommon.Config{
		Logger:         log,
		ServiceName:    cfg.AppName,
		BuildVersion:   cfg.BuildVersion,
		Domain:         "commercial",
		Environment:    cfg.AppEnv,
		RequestTimeout: cfg.RequestTimeout,
		// Problem type URIs: https://errors.bcbpsolutions.com/commercial/<error> (§5.1).
		ProblemTypeBase: "https://errors.bcbpsolutions.com",
	}

	// Tracing with the same identity as metrics and logs (§11.3).
	shutdownTracing, err := gincommon.InitTracingWithConfig(gincommon.TracingConfig{
		ServiceName:  ginCfg.ServiceName,
		BuildVersion: ginCfg.BuildVersion,
		Domain:       ginCfg.Domain,
		Environment:  ginCfg.Environment,
		Logger:       log,
	})
	if err != nil {
		log.Error("init tracing", map[string]any{"error": err.Error()})
		return 1
	}
	defer shutdownTracing()

	// Metrics registry and identity before any pool, so pgcommon's pool
	// gauges register on gincommon's registerer (§11.2).
	_ = gincommon.ObservabilityMiddlewares(ginCfg)
	identity := gincommon.MetricsConstLabels()
	pgWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(identity), gincommon.MetricsRegisterer())
	for _, w := range pgWarnings {
		log.Warn("platform-pgcommon metric disabled", map[string]any{"metric": w.Metric, "error": w.Error()})
	}
	if err != nil {
		log.Error("init platform-pgcommon metrics", map[string]any{"error": err.Error()})
		return 1
	}
	// Billing's §11.2 metrics on the same registry.
	billingMetrics, err := metrics.New(gincommon.MetricsRegisterer())
	if err != nil {
		log.Error("init billing metrics", map[string]any{"error": err.Error()})
		return 1
	}
	// platform-events' platform_* series (queue and DLQ depth, outbox
	// pending / oldest age, dead letters) that §11.5 alerts read; without
	// this call the library's metrics are no-ops. After metrics.New, so the
	// shared platform_dependency_request_seconds is reused.
	evWarnings, err := events.InitMetrics(events.MetricsIdentity{Domain: identity["domain"], Service: identity["service"],
		Environment: identity["environment"]}, gincommon.MetricsRegisterer())
	for _, w := range evWarnings {
		log.Warn("platform-events metric disabled", map[string]any{"metric": w.Metric, "error": w.Error()})
	}
	if err != nil {
		log.Error("init platform-events metrics", map[string]any{"error": err.Error()})
		return 1
	}
	// Spans and outbound-call metrics through gincommon (§11.2, §11.3).
	app.SetTracer(observability.NewTracer(ginCfg.ServiceName))
	httpx.SetObserver(billingMetrics.Dependency)

	// Schema: in Kubernetes the migrate hook Job owns migrations and replicas
	// run with RUN_MIGRATIONS=false (§12, §19.1); local runs may migrate here.
	if cfg.RunMigrations {
		migCtx, cancelMig := context.WithTimeout(context.Background(), cfg.MigrateTimeout)
		err := pgadapter.MigrateSchema(migCtx, pgadapter.MigrationDSN(), log)
		cancelMig()
		if err != nil {
			log.Error("migrate at startup", map[string]any{"error": err.Error()})
			return 1
		}
	}

	// Pools through pgcommon: billing_app (RLS, app.tenant_id on every
	// checkout) and billing_system (BYPASSRLS, cross-tenant work only).
	pools, err := bootstrap.Pools(context.Background(), cfg, ginCfg.ServiceName, log)
	if err != nil {
		log.Error("connect database", map[string]any{"error": err.Error()})
		return 1
	}
	// Every pool closes once, bounded (DrainAndClose): on the graceful path
	// inside gracefulStop, on an early return here, before the tracer and
	// logger flush.
	closer := newPoolCloser(log)
	defer closer.closeWithin(poolDrainTimeout)
	appPool, sysPool := pools.App, pools.System
	closer.add("app", appPool)
	closer.add("system", sysPool)

	poolCheck := func(p *pgcommon.Pool) httpadapter.Pinger {
		return httpadapter.PingerFunc(func(ctx context.Context) error {
			if hs := p.Health(ctx); !hs.Healthy {
				return errors.New("database not healthy")
			}
			return nil
		})
	}
	// Payment providers and the certification gate (§3.5): read once at
	// startup; a provider without a valid record stays loaded for running work.
	registry, err := bootstrap.Providers(cfg, sysPool, log)
	if err != nil {
		log.Error("payment providers", map[string]any{"error": err.Error()})
		return 1
	}
	if st, err := registry.Refresh(context.Background()); err != nil {
		log.Error("provider registry: first certification read failed; no provider enabled for new accounts until the next refresh", map[string]any{"error": err.Error()})
	} else {
		certifiedGauge(billingMetrics)(st)
	}

	// AL-5 direct-write in outbox mode (§3.3, §10.6).
	auditP, err := bootstrap.Audit(context.Background(), cfg, appPool, cfg.AppEnv, log)
	if err != nil {
		log.Error("platform-audit", map[string]any{"error": err.Error()})
		return 1
	}
	closer.add("audit_drainer", auditP.Pool)

	// Use cases (§8), then the event bus (§7): billing.events publisher (Glue
	// schema versions are resolved here, so a deploy ahead of schema
	// registration fails to start), tenant-billing-q (whose TenantConverted
	// confirms sagas) and usage-billing-q, delayed emissions.
	svc := bootstrap.NewService(cfg, registry, appPool, sysPool, nil, auditP.Writer, billingMetrics, log)
	eventBus, err := buildBusWith(context.Background(), cfg, appPool, sysPool, svc, log)
	if err != nil {
		log.Error("event bus", map[string]any{"error": err.Error()})
		return 1
	}
	eventBus.events = metrics.CountingEnqueuer{Next: eventBus.events, Metrics: billingMetrics}
	eventBus.emissions.Events, eventBus.emissions.Metrics = eventBus.events, billingMetrics
	svc.Events = eventBus.events

	lc := newLifecycle(billingMetrics.LoopRunning)
	var auditAlive []func() error
	if auditP.Runner != nil {
		auditAlive = append(auditAlive, auditP.Runner.Alive) // §13.1: liveness, never readiness
	}
	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: ginCfg,
		Readiness: []httpadapter.Check{
			lc.readiness(),
			{Name: "database", Pinger: poolCheck(appPool)},
			{Name: "sys_database", Pinger: poolCheck(sysPool)},
		},
		Liveness: []httpadapter.Check{lc.liveness(auditAlive...)},
		Routes: []func(gin.IRouter){
			webhookRoute(cfg, registry, sysPool, billingMetrics, log),
			apiRoutes(cfg, ginCfg, appPool, svc, billingMetrics),
			httpadapter.DocsRoutes(api.AsyncAPI),
		},
	})
	bg := startWorkers(cfg, registry, appPool, sysPool, eventBus, svc, auditP.Runner, billingMetrics, lc, log)

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.AppPort),
		Handler:           router.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Metrics on their own listener so a NetworkPolicy can admit the
	// monitoring namespace without exposing the API (§10.2, §13.1).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", gincommon.MetricsHandler())
	metricsSrv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.MetricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second, // above Prometheus' 10 s scrape timeout
		IdleTimeout:       60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("service starting", map[string]any{
		"service": ginCfg.ServiceName, "version": ginCfg.BuildVersion, "env": cfg.AppEnv,
		"addr": srv.Addr, "metrics_addr": metricsSrv.Addr,
		"feature_direct_signup": cfg.FeatureDirectSignup, "feature_token_addons": cfg.FeatureTokenAddons,
		"feature_seat_purchase": cfg.FeatureSeatPurchase,
	})
	serveErr := make(chan error, 2)
	for _, s := range []*http.Server{srv, metricsSrv} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}()
	}

	exit := 0
	select {
	case <-quit:
		log.Info("shutdown signal received; draining", nil)
	case err := <-serveErr:
		log.Error("listener failed", map[string]any{"error": err.Error()})
		exit = 1
	}

	// Readiness off → drain delay → API listener ∥ background loops →
	// pools → metrics listener; then (deferred) tracer → logger flush
	// (budget in lifecycle.go).
	shutdownHTTP := func(s *http.Server, timeout time.Duration) shutdownStep {
		return func(ctx context.Context) {
			hctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := s.Shutdown(hctx); err != nil {
				log.Error("http shutdown", map[string]any{"addr": s.Addr, "error": err.Error()})
			}
		}
	}
	gracefulStop(lc, cfg.ShutdownDrainDelay, log, shutdownPlan{
		parallel: []shutdownStep{
			shutdownHTTP(srv, httpDrainTimeout),
			func(context.Context) { bg.stop(workerStopTimeout) },
		},
		pools:   closer.close,
		metrics: shutdownHTTP(metricsSrv, metricsDrainTimeout),
	})
	log.Info("shutdown complete", map[string]any{"exit": exit})
	return exit
}
