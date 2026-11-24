// Command server is Billing's composition root (LLD §3): it loads and
// validates configuration (§12), builds the logger, tracing and metrics
// (gincommon v2), opens the app pool (billing_app, RLS) and sysPool
// (billing_system), serves the API on APP_PORT and metrics on METRICS_PORT,
// and shuts down gracefully.
//
// Wired so far: the provider registry and its refresh, B-W1 and the webhook
// processor (Phase 3); the billing.events outbox publisher, the
// tenant-billing-q / usage-billing-q consumers and the delayed-emission
// worker (Phase 4). The saga worker and audit runner come in later phases.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
)

// buildVersion is injected with -ldflags at build time.
var buildVersion = "dev"

// Shutdown budget (terminationGracePeriodSeconds 45, §13.1): HTTP drain
// within the request timeout, then pool drain.
const (
	httpDrainTimeout  = 30 * time.Second
	workerStopTimeout = 5 * time.Second
	poolDrainTimeout  = 10 * time.Second
)

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

	log, err := logger.NewLogger(loggerEnv(cfg.AppEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		return 1
	}
	// §12: refuse to start on any missing, malformed or out-of-range value.
	if len(problems) > 0 {
		for _, p := range problems {
			log.Error("invalid configuration", map[string]any{"key": p.Key, "reason": p.Reason})
		}
		_ = gincommon.Shutdown(log)
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
	defer pools.Close()
	appPool, sysPool := pools.App, pools.System

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
	if auditP.Pool != nil {
		defer auditP.Pool.Close()
	}

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

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: ginCfg,
		Readiness: []httpadapter.Check{
			{Name: "database", Pinger: poolCheck(appPool)},
			{Name: "sys_database", Pinger: poolCheck(sysPool)},
		},
		Routes: []func(gin.IRouter){
			webhookRoute(cfg, registry, sysPool, billingMetrics, log),
			apiRoutes(cfg, ginCfg, appPool, svc, billingMetrics),
			httpadapter.DocsRoutes(api.AsyncAPI),
		},
	})
	bg := startWorkers(cfg, registry, appPool, sysPool, eventBus, svc, auditP.Runner, billingMetrics, log)

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

	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancelHTTP()
	for _, s := range []*http.Server{srv, metricsSrv} {
		if err := s.Shutdown(httpCtx); err != nil {
			log.Error("http shutdown", map[string]any{"addr": s.Addr, "error": err.Error()})
		}
	}

	// Background loops stop after HTTP (no new ledger rows arrive) and
	// before the pools drain.
	bg.stop(workerStopTimeout)

	poolCtx, cancelPool := context.WithTimeout(context.Background(), poolDrainTimeout)
	defer cancelPool()
	var wg sync.WaitGroup
	for name, p := range map[string]*pgcommon.Pool{"app": appPool, "system": sysPool} {
		wg.Go(func() {
			if err := p.DrainAndClose(poolCtx); err != nil {
				log.Error("pool drain", map[string]any{"pool": name, "error": err.Error()})
			}
		})
	}
	wg.Wait()

	if err := gincommon.Shutdown(log); err != nil {
		fmt.Fprintf(os.Stderr, "flush logger/tracer: %v\n", err)
	}
	return exit
}
