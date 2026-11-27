package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/manual"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

type logEntry struct {
	level, msg string
	fields     map[string]any
}

type recLogger struct {
	mu      sync.Mutex
	entries []logEntry
	notify  chan logEntry
}

func (l *recLogger) add(level, msg string, f map[string]any) {
	l.mu.Lock()
	l.entries = append(l.entries, logEntry{level, msg, f})
	l.mu.Unlock()
	if l.notify != nil {
		select {
		case l.notify <- logEntry{level, msg, f}:
		default:
		}
	}
}
func (l *recLogger) Debug(m string, f map[string]any) { l.add("debug", m, f) }
func (l *recLogger) Info(m string, f map[string]any)  { l.add("info", m, f) }
func (l *recLogger) Warn(m string, f map[string]any)  { l.add("warn", m, f) }
func (l *recLogger) Error(m string, f map[string]any) { l.add("error", m, f) }

// An uncertified loaded provider is logged at ERROR with its versions
// (BillingProviderNotCertified, BIL-PROV-1).
func TestRefreshLogsUncertified(t *testing.T) {
	stripe := fakeProvider{name: "stripe", version: "1.0.0+stripeapi.x"}
	log := &recLogger{}
	r, err := New([]port.PaymentProvider{stripe}, []string{"stripe"}, &fakeCerts{}, "cert-1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(log.entries) != 1 || log.entries[0].level != "error" || log.entries[0].fields["provider"] != "stripe" ||
		log.entries[0].fields["adapter_version"] != "1.0.0+stripeapi.x" || log.entries[0].fields["suite_version"] != "cert-1.0" {
		t.Fatalf("log %+v", log.entries)
	}
}

// Run refreshes on every tick and reports statuses; it stops with ctx.
func TestRunReportsStatuses(t *testing.T) {
	certs := &fakeCerts{valid: map[string]bool{"manual|" + manual.AdapterVersion + "|cert-1.0": true}}
	r, err := New([]port.PaymentProvider{manual.New()}, []string{"manual"}, certs, "cert-1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan []Status, 1)
	done := make(chan struct{})
	go func() {
		r.Run(ctx, time.Millisecond, func(st []Status) {
			select {
			case got <- st:
			default:
			}
		})
		close(done)
	}()
	select {
	case st := <-got:
		if len(st) != 1 || st[0].Provider != "manual" || !st[0].Enabled {
			t.Errorf("statuses %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh within 5s")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when ctx ended")
	}
	if _, err := r.ForNewAccount("manual"); err != nil {
		t.Errorf("Run must apply the refreshed state: %v", err)
	}
}

// A failed refresh is logged at WARN, keeps the previous state and does not
// report statuses (a database blip never disables running work, BIL-PROV-2).
func TestRunKeepsStateOnFailure(t *testing.T) {
	certs := &fakeCerts{err: errors.New("db down")}
	log := &recLogger{notify: make(chan logEntry, 1)}
	r, err := New([]port.PaymentProvider{manual.New()}, []string{"manual"}, certs, "cert-1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	r.enabled["manual"] = true // previously certified
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	reported := make(chan struct{}, 1)
	go func() {
		r.Run(ctx, time.Millisecond, func([]Status) {
			select {
			case reported <- struct{}{}:
			default:
			}
		})
		close(done)
	}()
	select {
	case e := <-log.notify:
		if e.level != "warn" || e.fields["error"] == nil {
			t.Errorf("log %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no failed refresh within 5s")
	}
	cancel()
	<-done
	select {
	case <-reported:
		t.Error("a failed refresh must not report statuses")
	default:
	}
	if _, err := r.ForNewAccount("manual"); err != nil {
		t.Errorf("previous state must be kept: %v", err)
	}
}
