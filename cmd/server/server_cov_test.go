package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/logger"
)

type recLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recLog) add(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
}
func (l *recLog) Debug(m string, _ map[string]any) { l.add("debug", m) }
func (l *recLog) Info(m string, _ map[string]any)  { l.add("info", m) }
func (l *recLog) Warn(m string, _ map[string]any)  { l.add("warn", m) }
func (l *recLog) Error(m string, _ map[string]any) { l.add("error", m) }
func (l *recLog) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// §12: any invalid value refuses startup (exit 1) before anything is opened.
func TestRunRefusesInvalidConfiguration(t *testing.T) {
	t.Setenv("APP_ENV", "bogus")
	if code := run(); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
}

func TestLoggerEnv(t *testing.T) {
	for in, want := range map[string]string{"local": "dev", "dev": "dev", "test": "dev", "staging": "staging", "prod": "prod"} {
		if got := loggerEnv(in); got != want {
			t.Errorf("loggerEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

// §12: a feature flag that is off leaves its routes unregistered.
func TestDisabledRoutes(t *testing.T) {
	off := disabledRoutes(config.Config{FeatureSeatPurchase: true})
	for _, id := range []string{"B-I1", "B-P19", "B-P20", "B-P21", "B-P22", "B-P23", "B-O18"} {
		if !off[id] {
			t.Errorf("%s registered with its flag off", id)
		}
	}
	if off["B-P6"] || off["B-P7"] || len(off) != 7 {
		t.Errorf("disabled %v", off)
	}
	if all := disabledRoutes(config.Config{FeatureDirectSignup: true, FeatureTokenAddons: true, FeatureSeatPurchase: true,
		FeatureTrialCapOverride: true}); len(all) != 0 {
		t.Errorf("all on: %v", all)
	}
	if none := disabledRoutes(config.Config{}); !none["B-P6"] || !none["B-P7"] {
		t.Errorf("seat purchase off: %v", none)
	}
}

// pollLoop polls again at once after a full batch, logs a failed pass,
// and returns when ctx ends.
func TestPollLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	log := &recLog{}
	done := make(chan struct{})
	go func() {
		pollLoop(ctx, 5*time.Millisecond, 2, func(context.Context, int) (int, error) {
			switch calls.Add(1) {
			case 1:
				return 2, nil // full batch: again at once
			case 2:
				return 0, errors.New("db down")
			default:
				cancel()
				return 0, nil
			}
		}, log, "saga worker")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollLoop did not stop")
	}
	if calls.Load() != 3 || !log.has("warn saga worker pass failed") {
		t.Errorf("calls %d lines %v", calls.Load(), log.lines)
	}
}

var ginOnce sync.Once

// newMetrics is Billing's metrics on a fresh registry (gincommon's identity
// initialised once, with the identity run() uses in the integration test;
// the server's own registration on gincommon's registry
// is left to run()).
func newMetrics(t *testing.T) (*metrics.Metrics, *prometheus.Registry) {
	t.Helper()
	ginOnce.Do(func() {
		log, err := logger.NewLogger("dev")
		if err != nil {
			t.Fatal(err)
		}
		// The version run() reports: BUILD_VERSION when set (make exports
		// it), else the -ldflags default. gincommon refuses a second
		// initialisation with a different identity in the same process.
		version := os.Getenv("BUILD_VERSION")
		if version == "" {
			version = buildVersion
		}
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Logger: log, ServiceName: "commercial-billing", Domain: "commercial", Environment: "staging", BuildVersion: version})
	})
	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	return m, reg
}

func gauge(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	next:
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if v, ok := labels[l.GetName()]; ok && v != l.GetValue() {
					continue next
				}
			}
			return s.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// BIL-PROV-1: the certified gauge follows each registry refresh.
func TestCertifiedGauge(t *testing.T) {
	m, reg := newMetrics(t)
	certifiedGauge(m)([]provider.Status{{Provider: "stripe", AdapterVersion: "1.0.0", Enabled: true}, {Provider: "manual", AdapterVersion: "1.0.0"}})
	if v, ok := gauge(t, reg, metrics.ProviderCertified, map[string]string{"provider": "stripe"}); !ok || v != 1 {
		t.Errorf("stripe = %v (%v)", v, ok)
	}
	if v, ok := gauge(t, reg, metrics.ProviderCertified, map[string]string{"provider": "manual"}); !ok || v != 0 {
		t.Errorf("manual = %v (%v)", v, ok)
	}
}

type snapReader struct {
	err    error
	cancel context.CancelFunc
	calls  int
}

func (s *snapReader) MetricsSnapshot(context.Context, time.Time) (port.MetricsSnapshot, error) {
	s.calls++
	s.cancel()
	if s.err != nil {
		return port.MetricsSnapshot{}, s.err
	}
	return port.MetricsSnapshot{SagasOverdue: 4}, nil
}

// The snapshot exporter sets the gauges, and keeps them on a failed read.
func TestExportSnapshot(t *testing.T) {
	m, reg := newMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &snapReader{cancel: cancel}
	exportSnapshot(ctx, r, m, 30*time.Minute, &recLog{})
	if v, ok := gauge(t, reg, metrics.SagasOverdue, nil); !ok || v != 4 || r.calls != 1 {
		t.Errorf("overdue = %v (%v), calls %d", v, ok, r.calls)
	}
	// A failure while running is logged; the gauges keep their value.
	ctx, cancel = context.WithCancel(context.Background())
	log := &recLog{}
	exportSnapshot(ctx, &snapReader{err: errors.New("db down"), cancel: func() {
		go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	}}, m, time.Minute, log)
	if v, _ := gauge(t, reg, metrics.SagasOverdue, nil); v != 4 {
		t.Errorf("gauge changed on a failed read: %v", v)
	}
	if !log.has("metrics snapshot failed") {
		t.Errorf("failure not logged: %v", log.lines)
	}
}

type fakeDLQ struct {
	err, then error
	calls     int
}

func (f *fakeDLQ) SendToDLQ(context.Context, string, []byte, map[string]string, string) error {
	return nil
}
func (f *fakeDLQ) ResolveDLQ(context.Context, string) (string, error) {
	f.calls++
	if f.calls > 1 && f.then != nil {
		return "", f.then
	}
	return "dlq", f.err
}

// requireDLQ: outside dev a queue without a usable RedrivePolicy refuses
// startup; in dev that queue runs without DLQ forwarding; a transient
// lookup failure is retried, then only warns with forwarding on.
func TestRequireDLQ(t *testing.T) {
	dlqStartupAttempts, dlqStartupBackoff = 3, time.Millisecond
	t.Cleanup(func() { dlqStartupAttempts, dlqStartupBackoff = 3, 2*time.Second })
	ctx := context.Background()
	prod := config.Config{AppEnv: "prod"}
	if fwd, err := requireDLQ(ctx, &fakeDLQ{}, "q", prod, &recLog{}); err != nil || !fwd {
		t.Errorf("configured: %v %v", fwd, err)
	}
	for _, e := range []error{events.ErrDLQNotConfigured, events.ErrDLQInvalidRedrivePolicy} {
		if _, err := requireDLQ(ctx, &fakeDLQ{err: fmt.Errorf("resolve: %w", e)}, "q", prod, &recLog{}); err == nil || !errors.Is(err, e) {
			t.Errorf("prod %v: %v", e, err)
		}
	}
	log := &recLog{}
	if fwd, err := requireDLQ(ctx, &fakeDLQ{err: events.ErrDLQNotConfigured}, "q", config.Config{AppEnv: "dev"}, log); err != nil || fwd ||
		!log.has("DLQ forwarding off") {
		t.Errorf("dev: %v %v %v", fwd, err, log.lines)
	}
	log = &recLog{}
	f := &fakeDLQ{err: errors.New("throttled")}
	if fwd, err := requireDLQ(ctx, f, "q", prod, log); err != nil || !fwd || !log.has("could not resolve the DLQ") || f.calls != 3 {
		t.Errorf("transient: %v %v %d %v", fwd, err, f.calls, log.lines)
	}
	// A transient failure that turns out permanent on retry fails startup.
	f = &fakeDLQ{err: errors.New("throttled"), then: events.ErrDLQNotConfigured}
	if _, err := requireDLQ(ctx, f, "q", prod, &recLog{}); !errors.Is(err, events.ErrDLQNotConfigured) {
		t.Errorf("transient then permanent: %v", err)
	}
}

type fakeConsumer struct{ stopErr error }

func (fakeConsumer) Start(context.Context) error { return nil }
func (c fakeConsumer) Stop() error               { return c.stopErr }

// stop logs a consumer that fails to stop and gives up on a processor that
// does not return within the timeout.
func TestWorkersStop(t *testing.T) {
	log := &recLog{}
	canceled := false
	done := make(chan struct{})
	w := &workers{cancel: func() { canceled = true }, done: done,
		consumers: []queueConsumer{{"a", fakeConsumer{stopErr: errors.New("stuck")}}, {"b", fakeConsumer{}}}, log: log}
	start := time.Now()
	w.stop(20 * time.Millisecond)
	if !canceled || !log.has("error sqs consumer stop") || time.Since(start) < 20*time.Millisecond {
		t.Errorf("canceled=%v lines=%v", canceled, log.lines)
	}
	close(done)
	w.stop(time.Hour) // returns at once when the processor is done
}
