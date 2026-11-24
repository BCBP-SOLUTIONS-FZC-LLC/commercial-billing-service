// Command jobs is Billing's single CronJob binary: one --job=<name> per
// CronJob of LLD §13.2 (concurrencyPolicy Forbid, startingDeadlineSeconds
// 120). It builds the same use cases as the server (internal/bootstrap),
// runs one job once and exits: 0 when the job ran (items that failed are
// logged and retried by the next run), 1 when it could not run.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/bootstrap"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

// buildVersion is injected with -ldflags at build time.
var buildVersion = "dev"

// Jobs is the authoritative list from LLD §13.2 (ten CronJobs).
var Jobs = map[string]string{
	"saga-sweeper":           "* * * * *",
	"seat-renewal":           "*/15 * * * *",
	"downgrade-apply":        "*/15 * * * *",
	"purchase-expiry":        "*/5 * * * *",
	"seat-projection-check":  "7,37 * * * *",
	"pii-minimisation":       "0 5 * * *",
	"outbox-prune":           "0 3 * * *",
	"processed-events-prune": "0 4 * * *",
	"renewal-repricing":      "20 * * * *",
	"idempotency-prune":      "30 4 * * *",
}

// Exit statuses.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 64
)

// jobTimeout bounds one run, well inside the shortest schedule interval of
// the longer jobs; the every-minute saga-sweeper finishes in seconds.
const jobTimeout = 10 * time.Minute

// deps is what a job needs.
type deps struct {
	svc *app.Service
	cfg config.Config
	// prune helpers on the app pool
	pruneOutbox    func(ctx context.Context, olderThan time.Duration) (int64, error)
	pruneProcessed func(ctx context.Context, retention time.Duration) (int64, error)
}

// runners maps each job to its use case.
var runners = map[string]func(ctx context.Context, d deps) (app.JobResult, error){
	"saga-sweeper":          func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.SweepSagas(ctx) },
	"seat-renewal":          func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.RunSeatRenewal(ctx) },
	"downgrade-apply":       func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.RunDowngradeApply(ctx) },
	"purchase-expiry":       func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.ExpireStale(ctx) },
	"seat-projection-check": func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.CheckSeatProjection(ctx) },
	"pii-minimisation":      func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.MinimisePII(ctx) },
	"renewal-repricing":     func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.RunRepricing(ctx) },
	"idempotency-prune":     func(ctx context.Context, d deps) (app.JobResult, error) { return d.svc.PruneIdempotency(ctx) },
	"outbox-prune": func(ctx context.Context, d deps) (app.JobResult, error) {
		n, err := d.pruneOutbox(ctx, time.Duration(d.cfg.OutboxRetentionDays)*24*time.Hour)
		return app.JobResult{Handled: int(n)}, err
	},
	"processed-events-prune": func(ctx context.Context, d deps) (app.JobResult, error) {
		n, err := d.pruneProcessed(ctx, time.Duration(d.cfg.ProcessedEventsTTLDays)*24*time.Hour)
		return app.JobResult{Handled: int(n)}, err
	},
}

func main() { os.Exit(run(os.Args[1:], execute)) }

// run parses the flags and hands a known job to exec.
func run(args []string, exec func(name string) int) int {
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	name := fs.String("job", "", "job to run: "+strings.Join(names(), ", "))
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if _, ok := Jobs[*name]; !ok {
		fmt.Fprintf(os.Stderr, "jobs: unknown or missing --job %q; valid: %s\n", *name, strings.Join(names(), ", "))
		return exitUsage
	}
	return exec(*name)
}

// execute builds the service and runs one job once.
func execute(name string) int {
	cfg, problems := config.Load()
	if cfg.BuildVersion == "" {
		cfg.BuildVersion = buildVersion
	}
	log, err := logger.NewLogger(loggerEnv(cfg.AppEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logger: %v\n", err)
		return exitFailed
	}
	defer func() { _ = gincommon.Shutdown(log) }()
	if len(problems) > 0 {
		for _, p := range problems {
			log.Error("invalid configuration", map[string]any{"key": p.Key, "reason": p.Reason})
		}
		return exitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	d, closeAll, err := build(ctx, cfg, log)
	if err != nil {
		log.Error("job setup failed", map[string]any{"job": name, "error": err.Error()})
		return exitFailed
	}
	defer closeAll()
	return runJob(ctx, name, d, log)
}

// runJob runs one job and logs its outcome with the
// commercial_cross_tenant_job_runs_total labels (job, outcome).
func runJob(ctx context.Context, name string, d deps, log port.Logger) int {
	started := time.Now()
	res, err := runners[name](ctx, d)
	outcome := "success"
	switch {
	case err != nil:
		outcome = "failure"
	case res.Failed > 0:
		outcome = "partial"
	}
	fields := map[string]any{"job": name, "outcome": outcome, "handled": res.Handled, "failed": res.Failed,
		"duration_ms": time.Since(started).Milliseconds(), "metric": "commercial_cross_tenant_job_runs_total"}
	if err != nil {
		fields["error"] = err.Error()
		log.Error("job run failed", fields)
		return exitFailed
	}
	log.Info("job run finished", fields)
	return exitOK
}

// build opens the pools and builds the use cases. Jobs never start the
// webhook processor, consumers or outbox publisher (the server pods do).
func build(ctx context.Context, cfg config.Config, log port.Logger) (deps, func(), error) {
	appPool, sysPool, err := bootstrap.Pools(ctx, cfg, cfg.AppName+"-jobs", log)
	if err != nil {
		return deps{}, nil, err
	}
	closers := []func(){appPool.Close, sysPool.Close}
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(err error) (deps, func(), error) { closeAll(); return deps{}, nil, err }
	registry, err := bootstrap.Providers(cfg, sysPool, log)
	if err != nil {
		return fail(err)
	}
	if _, err := registry.Refresh(ctx); err != nil {
		log.Warn("provider registry: certification read failed; running work continues", map[string]any{"error": err.Error()})
	}
	appCfg, _ := pgadapter.AppPoolConfig(log) // the drainer pool reuses the app DSN host
	auditP, err := bootstrap.Audit(ctx, cfg, appCfg, appPool, cfg.AppEnv, log)
	if err != nil {
		return fail(err)
	}
	if auditP.Pool != nil {
		closers = append(closers, auditP.Pool.Close) // the server's runner delivers; jobs only enqueue
	}
	events, err := bootstrap.Events()
	if err != nil {
		return fail(err)
	}
	inbox, err := pgadapter.NewInboxRunner(appPool, app.ConsumerTenantBilling, app.ConsumerUsageBilling)
	if err != nil {
		return fail(err)
	}
	svc := bootstrap.NewService(cfg, registry, appPool, sysPool, events, auditP.Writer, nil, log)
	return deps{svc: svc, cfg: cfg,
		pruneOutbox: func(ctx context.Context, d time.Duration) (int64, error) {
			return pgadapter.PruneOutbox(ctx, appPool, d)
		},
		pruneProcessed: inbox.Prune,
	}, closeAll, nil
}

func loggerEnv(appEnv string) string {
	if config.IsDevLike(appEnv) {
		return "dev"
	}
	return appEnv
}

func names() []string {
	out := make([]string, 0, len(Jobs))
	for n := range Jobs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
