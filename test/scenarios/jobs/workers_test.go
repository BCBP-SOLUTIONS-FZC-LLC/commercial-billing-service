//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// ── scheduled-emission worker (BIL-EVT-6, §7.3.1) ───────────────────────

type emissions struct {
	port.EmissionRepo
	f *faults
	// stop, when set, is called on the DueTenants call numbered stopAt.
	stop   func()
	stopAt int
}

func (r *emissions) DueTenants(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if r.stop != nil && r.f.count("Emissions.DueTenants")+1 == r.stopAt {
		r.stop()
	}
	if err := r.f.hit("Emissions.DueTenants"); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return r.EmissionRepo.DueTenants(ctx, now, limit)
}

func (r *emissions) ClaimDue(ctx context.Context, tenantID string, now time.Time) ([]domain.ScheduledEmission, error) {
	if err := r.f.hit("Emissions.ClaimDue"); err != nil {
		return nil, err
	}
	return r.EmissionRepo.ClaimDue(ctx, tenantID, now)
}

type locker struct {
	port.TenantLocker
	f *faults
}

func (l locker) LockTenant(ctx context.Context, tenantID string) error {
	if err := l.f.hit("Lock.LockTenant"); err != nil {
		return err
	}
	return l.TenantLocker.LockTenant(ctx, tenantID)
}

type enqueuer struct {
	port.EventEnqueuer
	f *faults
}

func (q enqueuer) Enqueue(ctx context.Context, p domain.EventPayload, m port.EventMeta) (string, error) {
	if err := q.f.hit("Events.Enqueue"); err != nil {
		return "", err
	}
	return q.EventEnqueuer.Enqueue(ctx, p, m)
}

func (e *env) emissionWorker() (*app.EmissionWorker, *emissions) {
	em := &emissions{EmissionRepo: e.svc.Emissions, f: e.f}
	return &app.EmissionWorker{Emissions: em, Subs: e.svc.Subs, Tx: e.svc.Tx, Lock: locker{e.svc.Lock, e.f},
		Events: enqueuer{e.svc.Events, e.f}, BatchSize: 10, Now: func() time.Time { return e.now }, Log: e.log, Metrics: e.met}, em
}

// emission queues a due emission for sub and returns its id.
func (e *env) emission(tenant, sub, eventType, builder string) string {
	e.t.Helper()
	id := newTenant()
	e.exec(`INSERT INTO scheduled_emissions (emission_id, tenant_id, event_type, payload_builder, ref_id, due_at)
		VALUES ($1, $2, $3, $4, $5, now() - interval '1 minute')`, id, tenant, eventType, builder, sub)
	return id
}

func emissionStatus(t *testing.T, id string) string {
	t.Helper()
	return scalar[string](t, `SELECT status FROM scheduled_emissions WHERE emission_id = $1`, id)
}

// BIL-EVT-6: a failure selecting tenants fails the pass; a failure for one
// tenant (lock, claim, subscription or item read, enqueue) is logged and
// the emission stays pending for the next pass, which then emits it.
func TestEmissionWorkerRetriesEachFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.active("pro", 4)
	e.exec(`DELETE FROM scheduled_emissions WHERE tenant_id = $1`, tenant)
	id := e.emission(tenant, sub, string(domain.EventTenantSeatsChanged), domain.BuilderSeatsResync)
	e.now = time.Now().UTC()
	w, _ := e.emissionWorker()

	e.f.fail("Emissions.DueTenants", 0)
	if _, err := w.RunOnce(ctx); !errors.Is(err, errInjected) {
		t.Fatalf("scan: %v", err)
	}
	e.f.clear()
	for _, name := range []string{"Lock.LockTenant", "Emissions.ClaimDue", "Subs.Get", "Subs.Item", "Events.Enqueue"} {
		e.f.fail(name, 0)
		if n, err := w.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("%s: %d %v", name, n, err)
		}
		if st := emissionStatus(t, id); st != "pending" {
			t.Fatalf("%s: emission %s", name, st)
		}
		e.f.clear()
	}
	if !e.log.has("WARN scheduled emission failed; retried on the next pass") {
		t.Fatal("failures not logged")
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := emissionStatus(t, id); st != "emitted" {
		t.Fatalf("emission %s", st)
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'reason'||'/'||(payload->'data'->>'licensed_seats') FROM outbox_events
		WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged' ORDER BY ordering_seq DESC LIMIT 1`, tenant); got != "resync/4" {
		t.Fatalf("event %s", got)
	}
}

// BIL-EVT-6: an emission whose event type the builder does not produce, or
// whose subscription has no seat item any more, is cancelled (logged), not
// emitted; seats_after_conversion records the purchase-to-event latency
// (BIL-EVT-6 includes SEATS_AFTER_CONVERSION_DELAY).
func TestEmissionWorkerCancelsUnbuildableAndTimesConversionSeats(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	t1, s1 := e.active("pro", 4)
	t2, s2 := e.active("pro", 4)
	t3, s3 := e.active("pro", 6)
	e.exec(`DELETE FROM scheduled_emissions`)
	wrongType := e.emission(t1, s1, "TenantPlanChanged", domain.BuilderSeatsResync)
	e.exec(`DELETE FROM subscription_items WHERE billing_subscription_id = $1`, s2)
	noItem := e.emission(t2, s2, string(domain.EventTenantSeatsChanged), domain.BuilderSeatsResync)
	conv := e.emission(t3, s3, string(domain.EventTenantSeatsChanged), domain.BuilderSeatsAfterConversion)
	e.now = time.Now().UTC()
	w, _ := e.emissionWorker()
	if n, err := w.RunOnce(ctx); err != nil || n != 3 {
		t.Fatalf("pass %d %v", n, err)
	}
	for id, want := range map[string]string{wrongType: "cancelled", noItem: "cancelled", conv: "emitted"} {
		if st := emissionStatus(t, id); st != want {
			t.Fatalf("emission %s: %s, want %s", id, st, want)
		}
	}
	if !e.log.has("INFO scheduled emission cancelled") {
		t.Fatal("cancellation not logged")
	}
	if e.met.get("purchase_to_event:"+port.PurchaseKindConversionSeats) != 1 {
		t.Fatalf("metric %v", e.met.counts)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id IN ($1, $2) AND event_type = 'TenantSeatsChanged'
		AND payload->'data'->>'reason' = 'resync'`, t1, t2); n != 0 {
		t.Fatalf("events for cancelled emissions %d", n)
	}
}

// Run: a full batch polls again at once; a failed scan is logged and waits
// for the interval; cancellation stops the loop.
func TestEmissionWorkerRunLoop(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.active("pro", 4)
	e.exec(`DELETE FROM scheduled_emissions`)
	id := e.emission(tenant, sub, string(domain.EventTenantSeatsChanged), domain.BuilderSeatsResync)
	e.now = time.Now().UTC()
	w, em := e.emissionWorker()
	w.BatchSize = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.f.failN("Emissions.DueTenants", 1, 1, errInjected) // pass 1 full batch, pass 2 fails
	em.stop, em.stopAt = cancel, 3
	done := make(chan struct{})
	go func() { w.Run(ctx, time.Millisecond); close(done) }()
	<-done
	if st := emissionStatus(t, id); st != "emitted" {
		t.Fatalf("emission %s", st)
	}
	if !e.log.has("WARN scheduled emission scan failed") {
		t.Fatal("failed scan not logged")
	}
	if n := e.f.count("Emissions.DueTenants"); n < 3 {
		t.Fatalf("scans %d", n)
	}
}

// ── webhook processor (§8.15) ───────────────────────────────────────────

type ledger struct {
	port.LedgerRepo
	f      *faults
	stop   func()
	stopAt int
}

func (l *ledger) Claim(ctx context.Context, limit int, claimFor time.Duration) ([]domain.LedgerEntry, error) {
	if l.stop != nil && l.f.count("Ledger.Claim")+1 == l.stopAt {
		l.stop()
	}
	if err := l.f.hit("Ledger.Claim"); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return l.LedgerRepo.Claim(ctx, limit, claimFor)
}

func (l *ledger) Finish(ctx context.Context, provider, id string, st domain.LedgerStatus, outcome string, next *time.Time) error {
	if err := l.f.hit("Ledger.Finish"); err != nil {
		return err
	}
	return l.LedgerRepo.Finish(ctx, provider, id, st, outcome, next)
}

// handler records the tenants it is called for and answers err.
type handler struct {
	mu      sync.Mutex
	tenants []string
	err     error
}

func (h *handler) Handle(_ context.Context, tenantID string, _ domain.PaymentEvent, _ domain.ProviderObjectState) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tenants = append(h.tenants, tenantID)
	return "", h.err
}

func (e *env) processor(handlers map[domain.NormalisedKind]port.PaymentEventHandler) (*app.WebhookProcessor, *ledger) {
	l := &ledger{LedgerRepo: pgadapter.NewLedgerRepository(db.System), f: e.f}
	return &app.WebhookProcessor{Ledger: l, Providers: e.svc.Providers, Tx: e.svc.Tx, Handlers: handlers, MaxAttempts: 1, BatchSize: 5,
		ClaimFor: 30 * time.Second, Log: e.log, Metrics: e.met}, l
}

// ledgerRow inserts a received row; tenant "" leaves the customer unresolved.
func (e *env) ledgerRow(id, kind, tenant string) {
	e.t.Helper()
	var tid any
	if tenant != "" {
		tid = tenant
	}
	e.exec(`INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, tenant_id, provider_object_ref,
		livemode, provider_created_at, payload) VALUES ('stripe', $1, 'test.event', $2, $3, 'in_'||$1, false, now(), '{}')`, id, kind, tid)
}

func ledgerState(t *testing.T, id string) string {
	t.Helper()
	return scalar[string](t, `SELECT status::text||'/'||coalesce(outcome,'-') FROM payment_event_ledger WHERE provider_event_id = $1`, id)
}

// §8.15: a handler that reports no outcome is recorded applied:<kind>; an
// unknown customer is failed without retry and alerted (BIL-PAY-3); after
// WEBHOOK_MAX_ATTEMPTS a failing row is parked (SEV-3); every outcome is
// counted (commercial_billing_webhooks_total). A failed claim fails the batch.
func TestWebhookProcessorOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ok, failing := &handler{}, &handler{err: errors.New("handler failed")}
	p, _ := e.processor(map[domain.NormalisedKind]port.PaymentEventHandler{domain.KindInvoicePaid: ok, domain.KindInvoicePaymentFailed: failing})
	e.f.fail("Ledger.Claim", 0)
	if _, err := p.ProcessBatch(ctx); !errors.Is(err, errInjected) {
		t.Fatalf("claim: %v", err)
	}
	e.f.clear()

	tenant, other := newTenant(), newTenant()
	e.ledgerRow("evt_jobs_ok", string(domain.KindInvoicePaid), tenant)
	e.ledgerRow("evt_jobs_nocus", string(domain.KindInvoicePaid), "")
	e.ledgerRow("evt_jobs_park", string(domain.KindInvoicePaymentFailed), other)
	if n, err := p.ProcessBatch(ctx); err != nil || n != 3 {
		t.Fatalf("batch %d %v", n, err)
	}
	if got := ledgerState(t, "evt_jobs_ok"); got != "processed/applied:"+string(domain.KindInvoicePaid) {
		t.Fatalf("applied %s", got)
	}
	if got := ledgerState(t, "evt_jobs_nocus"); got != "failed/failed:unknown_customer" {
		t.Fatalf("unknown customer %s", got)
	}
	if got := ledgerState(t, "evt_jobs_park"); got != "failed/failed:INTERNAL" {
		t.Fatalf("parked %s", got)
	}
	if y := scalar[int](t, `SELECT extract(year FROM next_attempt_at)::int FROM payment_event_ledger WHERE provider_event_id = 'evt_jobs_park'`); y != 9999 {
		t.Fatalf("parked row scheduled for a retry (%d)", y)
	}
	if len(ok.tenants) != 1 || ok.tenants[0] != tenant {
		t.Fatalf("handler tenants %v", ok.tenants)
	}
	if !e.log.has("ERROR webhook for an unknown provider customer") || !e.log.has("ERROR webhook processing exhausted its attempts") {
		t.Fatal("alerts not logged")
	}
	if e.met.get("webhook:"+string(domain.KindInvoicePaid)+":processed") != 1 || e.met.get("webhook:"+string(domain.KindInvoicePaid)+":failed") != 1 {
		t.Fatalf("metrics %v", e.met.counts)
	}
}

// §8.15: when the outcome cannot be recorded the error is logged and the
// row stays claimed, so it is reclaimed after CLAIM_FOR.
func TestWebhookProcessorFinishFailureLeavesRowClaimed(t *testing.T) {
	e := newEnv(t)
	p, _ := e.processor(map[domain.NormalisedKind]port.PaymentEventHandler{domain.KindInvoicePaid: &handler{}})
	e.ledgerRow("evt_jobs_finish", string(domain.KindInvoicePaid), newTenant())
	e.f.fail("Ledger.Finish", 0)
	if n, err := p.ProcessBatch(context.Background()); err != nil || n != 1 {
		t.Fatalf("batch %d %v", n, err)
	}
	if got := ledgerState(t, "evt_jobs_finish"); got != "processing/-" {
		t.Fatalf("row %s", got)
	}
	if !e.log.has("ERROR ledger outcome not recorded") {
		t.Fatal("not logged")
	}
}

// Run: rows found → claim again at once; a failed claim is logged and
// waits for the interval; cancellation stops the loop. With no clock set,
// retries are scheduled from the wall clock.
func TestWebhookProcessorRunLoop(t *testing.T) {
	e := newEnv(t)
	p, l := e.processor(map[domain.NormalisedKind]port.PaymentEventHandler{domain.KindInvoicePaid: &handler{err: errors.New("try again")}})
	p.MaxAttempts = 0 // default (10): the row is retried, not parked
	e.ledgerRow("evt_jobs_loop", string(domain.KindInvoicePaid), newTenant())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.f.failN("Ledger.Claim", 1, 1, errInjected) // claim 1 finds the row, claim 2 fails
	l.stop, l.stopAt = cancel, 3
	start := time.Now()
	done := make(chan struct{})
	go func() { p.Run(ctx, time.Millisecond); close(done) }()
	<-done
	if got := ledgerState(t, "evt_jobs_loop"); got != "failed/failed:INTERNAL" {
		t.Fatalf("row %s", got)
	}
	next := scalar[time.Time](t, `SELECT next_attempt_at FROM payment_event_ledger WHERE provider_event_id = 'evt_jobs_loop'`)
	if next.Before(start) || next.After(time.Now().Add(time.Minute)) {
		t.Fatalf("retry at %s (Backoff from now)", next)
	}
	if !e.log.has("WARN webhook claim failed") {
		t.Fatal("failed claim not logged")
	}
}
