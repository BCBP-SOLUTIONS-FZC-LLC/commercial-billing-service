package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// The worst-case shutdown fits the 50 s left after the chart's 10 s preStop
// sleep (terminationGracePeriodSeconds 60): drain delay, API ∥ workers,
// pools (with pgcommon's force-close grace), metrics listener, flush.
func TestShutdownBudgetFits(t *testing.T) {
	worst := maxDrainDelay + max(httpDrainTimeout, workerStopTimeout) + poolDrainTimeout + poolForceCloseGrace +
		metricsDrainTimeout + flushBudget
	if worst > shutdownBudget {
		t.Fatalf("worst case %s exceeds the %s budget", worst, shutdownBudget)
	}
	// Each SQS consumer's Stop waits drain + 5 s; the outbox's its drain.
	if sqsDrainTimeout+5*time.Second > workerStopTimeout || outboxDrainTimeout > workerStopTimeout {
		t.Fatal("a worker's own drain exceeds workerStopTimeout")
	}
}

// SIGTERM: readiness fails first, the API listener closes only after the
// drain delay, API and workers stop in parallel, then the pools, and the
// metrics listener last (final values scraped).
func TestGracefulStopOrder(t *testing.T) {
	lc := newLifecycle(nil)
	ready := lc.readiness()
	if err := ready.Pinger.Health(context.Background()); err != nil {
		t.Fatalf("ready before shutdown: %v", err)
	}
	var (
		mu    sync.Mutex
		order []string
	)
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	start := time.Now()
	delay := 50 * time.Millisecond
	slow := func(name string) shutdownStep {
		return func(context.Context) {
			if ready.Pinger.Health(context.Background()) == nil {
				t.Errorf("%s ran while still ready", name)
			}
			if time.Since(start) < delay {
				t.Errorf("%s ran before the drain delay", name)
			}
			time.Sleep(100 * time.Millisecond)
			note(name)
		}
	}
	var metricsDeadline time.Duration
	gracefulStop(lc, delay, &recLog{}, shutdownPlan{
		parallel: []shutdownStep{slow("http"), slow("workers")},
		pools:    func(context.Context) { note("pools") },
		metrics: func(ctx context.Context) {
			dl, _ := ctx.Deadline()
			metricsDeadline = time.Until(dl)
			note("metrics")
		},
	})
	if took := time.Since(start); took > delay+180*time.Millisecond {
		t.Errorf("steps not parallel: %s", took)
	}
	if len(order) != 4 || order[2] != "pools" || order[3] != "metrics" {
		t.Errorf("order %v", order)
	}
	if metricsDeadline <= 0 || metricsDeadline > metricsDrainTimeout {
		t.Errorf("metrics step deadline %s", metricsDeadline)
	}
}

// Pools close once, in parallel and bounded: a second call (the deferred
// early-return path) is a no-op, and a pool whose close hangs past the
// deadline does not hold the process (pgxpool's Close has no limit).
func TestPoolCloserOnceAndBounded(t *testing.T) {
	log := &recLog{}
	c := newPoolCloser(log)
	a, b := &pgcommon.Pool{}, &pgcommon.Pool{}
	c.add("app", a)
	c.add("system", b)
	c.add("dup", a)
	c.add("none", nil)
	if len(c.pools) != 2 {
		t.Fatalf("pools %v", c.names)
	}
	var calls atomic.Int32
	hang := make(chan struct{})
	defer close(hang)
	c.drain = func(ctx context.Context, p *pgcommon.Pool) error {
		calls.Add(1)
		if p == b {
			<-hang // a close that never returns
		}
		return nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	c.close(ctx)
	if took := time.Since(start); took > poolForceCloseGrace+500*time.Millisecond {
		t.Errorf("close not bounded: %s", took)
	}
	if !log.has("pools did not close within the shutdown budget") {
		t.Errorf("not logged: %v", log.lines)
	}
	c.closeWithin(time.Hour) // deferred path: no second drain, returns at once
	if calls.Load() != 2 {
		t.Errorf("drain calls %d, want 2", calls.Load())
	}
}

type slowConsumer struct{ stops *atomic.Int32 }

func (slowConsumer) Start(context.Context) error { return nil }
func (c slowConsumer) Stop() error {
	time.Sleep(100 * time.Millisecond)
	c.stops.Add(1)
	return nil
}

// SQS consumers drain in parallel, not one after another.
func TestWorkersStopConsumersInParallel(t *testing.T) {
	var n atomic.Int32
	done := make(chan struct{})
	close(done)
	w := &workers{cancel: func() {}, done: done, log: &recLog{},
		consumers: []queueConsumer{{"tenant-billing-q", slowConsumer{&n}}, {"usage-billing-q", slowConsumer{&n}}}}
	start := time.Now()
	w.stop(time.Second)
	if took := time.Since(start); took > 180*time.Millisecond || n.Load() != 2 {
		t.Errorf("took %s, stopped %d", took, n.Load())
	}
}

// A loop that returns while the pod is not shutting down drops its gauge,
// is logged and restarted with backoff, and never fails liveness; a loop
// ended by cancellation is neither restarted nor reported; a
// non-restartable loop (audit runner) stays down and its own Alive check
// fails liveness, except while draining.
func TestSuperviseRestartsWithoutFailingLiveness(t *testing.T) {
	loopRestartMin, loopRestartMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { loopRestartMin, loopRestartMax = time.Second, 5*time.Minute })
	var mu sync.Mutex
	gauges := map[string][]bool{}
	lc := newLifecycle(func(loop string, running bool) { mu.Lock(); gauges[loop] = append(gauges[loop], running); mu.Unlock() })
	live := lc.liveness()
	ctx, cancel := context.WithCancel(context.Background())
	log := &recLog{}

	// Fails twice (a missing RedrivePolicy, then throttled), then runs.
	var starts atomic.Int32
	done := lc.supervise(ctx, "sqs:tenant-billing-q", log, true, func(ctx context.Context) error {
		if starts.Add(1) <= 2 {
			return errors.New("sqs: DLQ forwarding is enabled but the queue has no usable RedrivePolicy")
		}
		<-ctx.Done()
		return nil
	})
	deadline := time.Now().Add(2 * time.Second)
	for starts.Load() < 3 {
		if err := live.Pinger.Health(ctx); err != nil {
			t.Fatalf("liveness failed on a stopped loop: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("loop not restarted")
		}
		time.Sleep(time.Millisecond)
	}
	if !log.has("background loop stopped unexpectedly; restarting") {
		t.Errorf("not logged: %v", log.lines)
	}
	mu.Lock()
	if g := gauges["sqs:tenant-billing-q"]; len(g) != 5 || g[0] != true || g[1] != false || g[4] != true {
		t.Errorf("gauge sequence %v, want up, down, up, down, up", g)
	}
	mu.Unlock()
	// Cancellation (shutdown) ends supervision without a restart.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervision did not end on cancel")
	}
	if starts.Load() != 3 {
		t.Errorf("restarted after cancel: %d starts", starts.Load())
	}

	// Not restartable: one run, gauge down, supervision ends.
	lc2 := newLifecycle(nil)
	var runs atomic.Int32
	done2 := lc2.supervise(context.Background(), "audit-runner", log, false, func(context.Context) error { runs.Add(1); return nil })
	<-done2
	if runs.Load() != 1 || !log.has("not restartable") {
		t.Errorf("runs %d, lines %v", runs.Load(), log.lines)
	}
	// The audit runner's own Alive check fails liveness when not draining.
	auditErr := errors.New("auditoutbox: runner stopped")
	l3 := lc2.liveness(func() error { return auditErr })
	if err := l3.Pinger.Health(context.Background()); !errors.Is(err, auditErr) {
		t.Errorf("audit check: %v", err)
	}
	lc2.drain()
	if err := l3.Pinger.Health(context.Background()); err != nil {
		t.Errorf("liveness while draining: %v", err)
	}
}

// The backoff doubles up to the cap.
func TestSuperviseBackoffCapped(t *testing.T) {
	loopRestartMin, loopRestartMax = 2*time.Millisecond, 8*time.Millisecond
	t.Cleanup(func() { loopRestartMin, loopRestartMax = time.Second, 5*time.Minute })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var at []time.Time
	done := newLifecycle(nil).supervise(ctx, "outbox", &recLog{}, true, func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) == 6 {
			cancel()
		}
		return errors.New("boom")
	})
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 6 {
		t.Fatalf("starts %d", len(at))
	}
	if gap := at[5].Sub(at[4]); gap < loopRestartMax {
		t.Errorf("last gap %s below the cap %s", gap, loopRestartMax)
	}
}
