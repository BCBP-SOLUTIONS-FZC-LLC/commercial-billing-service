package metrics_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// fresh is Billing's metrics alone on a new registry.
func fresh(t *testing.T) (*metrics.Metrics, *prometheus.Registry) {
	t.Helper()
	registered(t) // gincommon identity
	reg := prometheus.NewRegistry()
	m, err := metrics.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	return m, reg
}

// value returns the sample of family name whose labels include want.
func value(t *testing.T, g prometheus.Gatherer, name string, want map[string]string) (float64, bool) {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	next:
		for _, s := range f.GetMetric() {
			have := map[string]string{}
			for _, l := range s.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if have[k] != v {
					continue next
				}
			}
			return sampleValue(s), true
		}
	}
	return 0, false
}

func sampleValue(s *dto.Metric) float64 {
	switch {
	case s.Counter != nil:
		return s.Counter.GetValue()
	case s.Gauge != nil:
		return s.Gauge.GetValue()
	}
	return 0
}

// Registering Billing's metrics twice on one registry is refused.
func TestNewRefusesDuplicateRegistration(t *testing.T) {
	_, reg := fresh(t)
	if _, err := metrics.New(reg); err == nil {
		t.Fatal("second registration accepted")
	}
}

type enq struct{ err error }

func (e enq) Enqueue(context.Context, domain.EventPayload, port.EventMeta) (string, error) {
	return "evt-1", e.err
}

// CountingEnqueuer counts an enqueued event, and not a refused one.
func TestCountingEnqueuer(t *testing.T) {
	m, reg := fresh(t)
	ev := domain.TenantReactivated{TenantID: "t", Source: domain.ReactivationSource}
	id, err := metrics.CountingEnqueuer{Next: enq{}, Metrics: m}.Enqueue(context.Background(), ev, port.EventMeta{})
	if err != nil || id != "evt-1" {
		t.Fatalf("enqueue: %q %v", id, err)
	}
	if _, err := (metrics.CountingEnqueuer{Next: enq{err: errors.New("invalid")}, Metrics: m}).Enqueue(context.Background(), ev, port.EventMeta{}); err == nil {
		t.Fatal("error swallowed")
	}
	if v, ok := value(t, reg, metrics.EventsEmittedTotal, map[string]string{"event_type": string(ev.EventType())}); !ok || v != 1 {
		t.Errorf("events emitted = %v (found %v), want 1", v, ok)
	}
}

func TestSagasOverdueAndRepaired(t *testing.T) {
	m, reg := fresh(t)
	m.SagasOverdue(3)
	if v, ok := value(t, reg, metrics.SagasOverdue, nil); !ok || v != 3 {
		t.Errorf("sagas overdue = %v (found %v)", v, ok)
	}
	if metrics.Repaired(true) != "true" || metrics.Repaired(false) != "false" {
		t.Error("Repaired label values")
	}
}

// An unreachable or malformed Pushgateway URL is an error, never a panic.
func TestPushRunTransportErrors(t *testing.T) {
	registered(t)
	if err := metrics.PushRun("http://[::1", "seat-renewal"); err == nil || !strings.Contains(err.Error(), "pushgateway request") {
		t.Errorf("malformed URL: %v", err)
	}
	if err := metrics.PushRun("http://127.0.0.1:1", "seat-renewal"); err == nil || !strings.Contains(err.Error(), "pushgateway:") {
		t.Errorf("unreachable: %v", err)
	}
}
