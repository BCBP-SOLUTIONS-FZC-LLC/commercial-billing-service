// Command server is Billing's composition root (LLD §3): it loads and
// validates configuration (§12), builds the logger, tracing and metrics
// (gincommon v2), opens the app pool (billing_app, RLS) and sysPool
// (billing_system), serves the API on APP_PORT and metrics on METRICS_PORT,
// and shuts down gracefully.
//
// Wired so far: the provider registry and its refresh, B-W1 and the webhook
// processor (Phase 3), and the billing.events outbox publisher (Phase 4).
// The saga worker, audit runner, SQS consumers and delayed emissions are
// added in later phases.
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

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/http"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
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

	// Schema: in Kubernetes the migrate hook Job owns migrations and replicas
	// run with RUN_MIGRATIONS=false (§12, §19.1); local runs may migrate here.
	if cfg.RunMigrations {
		migCtx, cancelMig := context.WithTimeout(context.Background(), cfg.MigrateTimeout)
		err := pgadapter.MigrateSchema(migCtx, pgadapter.MigrationDSNFromEnv(), log)
		cancelMig()
		if err != nil {
			log.Error("migrate at startup", map[string]any{"error": err.Error()})
			return 1
		}
	}

	// App pool: billing_app, RLS-bound, app.tenant_id GUC on every checkout.
	appCfg, cfgWarnings := pgadapter.AppPoolConfig(log)
	for _, w := range cfgWarnings {
		log.Warn("postgres config warning", map[string]any{"key": w.Key, "reason": w.Reason})
	}
	appCfg.Tracer = gincommon.NewSpanTracer(ginCfg.ServiceName)
	appPool, err := pgcommon.NewPool(context.Background(), appCfg)
	if err != nil {
		log.Error("connect app pool", map[string]any{"error": err.Error()})
		return 1
	}
	defer appPool.Close()

	// sysPool: billing_system, BYPASSRLS, cross-tenant work only.
	sysDSN := pgadapter.SystemDSNFromEnv()
	sysCfg := pgadapter.SystemPoolConfig(sysDSN, int32(cfg.PGSystemMaxConns), log)
	sysCfg.Tracer = gincommon.NewSpanTracer(ginCfg.ServiceName)
	sysPool, err := pgcommon.NewPool(context.Background(), sysCfg)
	if err != nil {
		log.Error("connect system pool", map[string]any{"error": err.Error()})
		return 1
	}
	defer sysPool.Close()
	if sysDSN == appCfg.DSN {
		log.Warn("SYSTEM_DATABASE_URL not set; sysPool reuses the app DSN and cross-tenant queries will be RLS-filtered", nil)
	}

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
	registry, err := providers(cfg, sysPool, log)
	if err != nil {
		log.Error("payment providers", map[string]any{"error": err.Error()})
		return 1
	}
	if _, err := registry.Refresh(context.Background()); err != nil {
		log.Error("provider registry: first certification read failed; no provider enabled for new accounts until the next refresh", map[string]any{"error": err.Error()})
	}

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: ginCfg,
		Readiness: []httpadapter.Check{
			{Name: "database", Pinger: poolCheck(appPool)},
			{Name: "sys_database", Pinger: poolCheck(sysPool)},
		},
		Routes: []func(gin.IRouter){webhookRoute(cfg, registry, sysPool, log)},
	})
	// billing.events publisher (§7.3.8): Glue schema versions are resolved
	// once here, so a deploy ahead of schema registration fails to start.
	ob, err := outboxRunner(context.Background(), cfg, appPool, log)
	if err != nil {
		log.Error("billing.events outbox", map[string]any{"error": err.Error()})
		return 1
	}
	bg := startWorkers(cfg, registry, appPool, sysPool, ob, log)

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
