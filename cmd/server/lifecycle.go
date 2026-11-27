package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Shutdown budget (§13.1). The chart sets terminationGracePeriodSeconds 60
// with a 10 s preStop sleep that runs before SIGTERM reaches the process,
// so everything below must finish within 50 s of SIGTERM:
//
//	readiness flips to 503 at once                          0 s
//	SHUTDOWN_DRAIN_DELAY (default 2 s, at most 5 s)         ≤  5 s
//	API listener drain ∥ background loops (in parallel)     ≤ 25 s
//	  - HTTP Shutdown (in-flight requests)                    25 s
//	  - SQS consumers, each in parallel: SQS_DRAIN_TIMEOUT
//	    (capped 15 s) + platform-events' 5 s                  20 s
//	  - outbox runner (OUTBOX_DRAIN_TIMEOUT capped 15 s),
//	    audit runner, webhook processor                       ≤ 20 s
//	pools (app, system, audit drainer), in parallel, once:
//	  DrainAndClose 10 s + pgcommon's 1 s force-close grace  ≤ 11 s
//	metrics listener (served until here, final scrape)      ≤  2 s
//	tracer, then logger flush (last, so every log above
//	  reaches the exporter)                                  ≤  5 s
//	worst case                                                48 s  (< 50 s)
//
// TestShutdownBudgetFits asserts the sum. A request that runs past the HTTP
// drain is cut; REQUEST_TIMEOUT (default 30 s) bounds requests in normal
// operation, not at shutdown.
const (
	shutdownBudget      = 50 * time.Second
	maxDrainDelay       = 5 * time.Second // SHUTDOWN_DRAIN_DELAY upper bound (config)
	httpDrainTimeout    = 25 * time.Second
	workerStopTimeout   = 20 * time.Second
	poolDrainTimeout    = 10 * time.Second
	poolForceCloseGrace = 1 * time.Second // pgcommon DrainAndClose returns within deadline + ~1 s
	metricsDrainTimeout = 2 * time.Second
	flushBudget         = 5 * time.Second
)

// Restart backoff of a supervised loop that stopped unexpectedly.
var (
	loopRestartMin = time.Second
	loopRestartMax = 5 * time.Minute
)

// lifecycle carries the probe state the router reads: draining once
// SIGTERM arrived (readiness 503), and the supervision of the background
// loops.
//
// A loop that stops unexpectedly does NOT fail liveness (§13.1, rev 0.59
// proposal): it is restarted in-process with backoff while its
// commercial_billing_background_loop_running gauge reads 0
// (BillingBackgroundLoopStopped pages). A pod restart would not fix a
// deterministic failure (a queue without a RedrivePolicy, a revoked IAM
// permission) and would take the tenant API and B-W1 down with it on every
// replica at once; a transient failure is recovered by the in-process
// restart as a pod restart would. Deterministic configuration checks run
// before the listener opens instead, so a bad deploy fails startup.
// Liveness keeps only the platform-audit runner's Alive (a stalled or
// stopped runner, which cannot be restarted in-process).
type lifecycle struct {
	draining atomic.Bool
	gauge    func(loop string, running bool)
}

func newLifecycle(gauge func(loop string, running bool)) *lifecycle {
	if gauge == nil {
		gauge = func(string, bool) {}
	}
	return &lifecycle{gauge: gauge}
}

// drain makes /readyz fail: endpoints and the mesh stop routing new
// requests here while the listener still serves in-flight ones.
func (l *lifecycle) drain() { l.draining.Store(true) }

// readiness is the /readyz check that fails while draining.
func (l *lifecycle) readiness() httpadapter.Check {
	return httpadapter.Check{Name: "serving", Pinger: httpadapter.PingerFunc(func(context.Context) error {
		if l.draining.Load() {
			return errors.New("shutting down")
		}
		return nil
	})}
}

// liveness is the /healthz check: the extra checks (the platform-audit
// runner's Alive), skipped while draining, when loops stop on purpose.
// Stopped loops are restarted and alerted, never a liveness failure.
func (l *lifecycle) liveness(extra ...func() error) httpadapter.Check {
	return httpadapter.Check{Name: "background_loops", Pinger: httpadapter.PingerFunc(func(context.Context) error {
		if l.draining.Load() {
			return nil
		}
		for _, check := range extra {
			if err := check(); err != nil {
				return err
			}
		}
		return nil
	})}
}

// supervise runs loop in its own goroutine. A loop that returns while ctx
// is still live and the pod is not draining stopped unexpectedly: it is
// logged at ERROR and its commercial_billing_background_loop_running gauge
// drops to 0 (BillingBackgroundLoopStopped). A restartable loop is started
// again after a backoff (1 s doubling to 5 min, reset once a run lasted
// longer than the cap); the gauge returns to 1 on each start. A loop that
// is not restartable (the platform-audit runner, single-use) stays down
// and its own liveness check reports it. The returned channel closes when
// the supervision ends (shutdown, or a non-restartable loop stopped).
func (l *lifecycle) supervise(ctx context.Context, name string, log port.Logger, restartable bool, loop func(ctx context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		backoff := loopRestartMin
		for {
			l.gauge(name, true)
			started := time.Now()
			err := loop(ctx)
			if ctx.Err() != nil || l.draining.Load() {
				return
			}
			why := "returned"
			if err != nil {
				why = err.Error()
			}
			l.gauge(name, false)
			if !restartable {
				log.Error("background loop stopped unexpectedly; not restartable", map[string]any{"loop": name, "error": why})
				return
			}
			if time.Since(started) > loopRestartMax {
				backoff = loopRestartMin
			}
			log.Error("background loop stopped unexpectedly; restarting", map[string]any{"loop": name, "error": why,
				"retry_in_ms": backoff.Milliseconds()})
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			backoff = min(2*backoff, loopRestartMax)
		}
	}()
	return done
}

// shutdownStep is one part of the drain (an HTTP server's Shutdown, the
// workers' stop).
type shutdownStep func(ctx context.Context)

// shutdownPlan is the drain after readiness is off: the API listener and
// the background loops in parallel, then the pools, then the metrics
// listener (kept serving to the end so the final values are scraped).
type shutdownPlan struct {
	parallel []shutdownStep
	pools    shutdownStep
	metrics  shutdownStep
}

// gracefulStop runs the shutdown sequence of the budget above: readiness
// off, the drain delay, then the API listener and background loops in
// parallel, then the pools, then the metrics listener. The tracer and
// logger flush after it returns (main.go's deferred calls).
func gracefulStop(l *lifecycle, delay time.Duration, log port.Logger, plan shutdownPlan) {
	l.drain()
	log.Info("shutdown: readiness off; draining", map[string]any{"drain_delay_ms": delay.Milliseconds()})
	if delay > 0 {
		time.Sleep(delay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(httpDrainTimeout, workerStopTimeout))
	defer cancel()
	var wg sync.WaitGroup
	for _, step := range plan.parallel {
		wg.Go(func() { step(ctx) })
	}
	wg.Wait()
	if plan.pools != nil {
		pctx, cancelPools := context.WithTimeout(context.Background(), poolDrainTimeout)
		plan.pools(pctx)
		cancelPools()
	}
	if plan.metrics != nil {
		mctx, cancelMetrics := context.WithTimeout(context.Background(), metricsDrainTimeout)
		plan.metrics(mctx)
		cancelMetrics()
	}
}

// poolCloser closes every pool exactly once with DrainAndClose, in
// parallel and bounded: the graceful path calls it with the drain
// deadline, and the deferred call on an early return is a no-op after
// that. pgxpool's Close (no time limit) is never called, so a connection
// still held after a force-close cannot hang the process until SIGKILL.
type poolCloser struct {
	once  sync.Once
	log   port.Logger
	names []string
	pools []*pgcommon.Pool
	// drain is DrainAndClose (tests replace it).
	drain func(ctx context.Context, p *pgcommon.Pool) error
}

func newPoolCloser(log port.Logger) *poolCloser {
	return &poolCloser{log: log, drain: func(ctx context.Context, p *pgcommon.Pool) error { return p.DrainAndClose(ctx) }}
}

// add registers a pool (nil and duplicates are ignored).
func (c *poolCloser) add(name string, p *pgcommon.Pool) {
	if p == nil {
		return
	}
	for _, q := range c.pools {
		if q == p {
			return
		}
	}
	c.names, c.pools = append(c.names, name), append(c.pools, p)
}

// close drains and closes every pool once. It returns by ctx's deadline
// plus poolForceCloseGrace even if a pool's close does not.
func (c *poolCloser) close(ctx context.Context) {
	c.once.Do(func() {
		all := make(chan struct{})
		go func() {
			defer close(all)
			var wg sync.WaitGroup
			for i, p := range c.pools {
				wg.Go(func() {
					if err := c.drain(ctx, p); err != nil {
						c.log.Error("pool drain", map[string]any{"pool": c.names[i], "error": err.Error()})
					}
				})
			}
			wg.Wait()
		}()
		var bound <-chan time.Time
		if dl, ok := ctx.Deadline(); ok {
			t := time.NewTimer(time.Until(dl) + poolForceCloseGrace)
			defer t.Stop()
			bound = t.C
		}
		select {
		case <-all:
		case <-bound:
			c.log.Error("pools did not close within the shutdown budget; exiting anyway", map[string]any{"pools": c.names})
		}
	})
}

// closeWithin is close with its own deadline (deferred early-return path).
func (c *poolCloser) closeWithin(d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c.close(ctx)
}
