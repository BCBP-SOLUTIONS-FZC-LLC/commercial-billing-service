package app

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

type finished struct {
	status  domain.LedgerStatus
	outcome string
	next    *time.Time
}

type fakeLedger struct {
	port.LedgerRepo
	rows     []domain.LedgerEntry
	done     map[string]finished
	deferred []string
}

func (f *fakeLedger) Claim(context.Context, int, time.Duration) ([]domain.LedgerEntry, error) {
	r := f.rows
	f.rows = nil
	return r, nil
}

func (f *fakeLedger) Finish(_ context.Context, _, id string, st domain.LedgerStatus, outcome string, next *time.Time) error {
	f.done[id] = finished{st, outcome, next}
	return nil
}

// Defer records a deferral (status failed, retry time, attempt given back).
func (f *fakeLedger) Defer(_ context.Context, _, id, outcome string, next time.Time) error {
	f.done[id] = finished{domain.LedgerFailed, outcome, &next}
	f.deferred = append(f.deferred, id)
	return nil
}

type fakeProvider struct {
	port.PaymentProvider
	fetchErr error
	fetched  []domain.ProviderObjectRef
}

func (f *fakeProvider) FetchCurrent(_ context.Context, ref domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	f.fetched = append(f.fetched, ref)
	return domain.ProviderObjectState{Ref: ref, Status: "paid"}, f.fetchErr
}

type lookup map[string]port.PaymentProvider

func (l lookup) Loaded(n string) (port.PaymentProvider, bool) { p, ok := l[n]; return p, ok }

type fakeTx struct{ tenants []string }

func (f *fakeTx) RunInTenantTx(ctx context.Context, tenant string, fn func(context.Context) error) error {
	f.tenants = append(f.tenants, tenant)
	return fn(ctx)
}

type handlerFunc func(ctx context.Context, tenant string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error)

func (h handlerFunc) Handle(ctx context.Context, t string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	return h(ctx, t, ev, cur)
}

func strp(s string) *string { return &s }

func row(id, kind string, tenant *string, payload string, attempts int) domain.LedgerEntry {
	return domain.LedgerEntry{Provider: "stripe", ProviderEventID: id, EventType: "invoice.paid", NormalisedKind: kind,
		TenantID: tenant, ProviderObjectRef: strp("in_1"), Payload: []byte(payload), Attempts: attempts, ProviderCreatedAt: time.Now()}
}

var now = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func setup(rows []domain.LedgerEntry, h port.PaymentEventHandler, prov *fakeProvider) (*WebhookProcessor, *fakeLedger, *fakeTx) {
	l := &fakeLedger{rows: rows, done: map[string]finished{}}
	tx := &fakeTx{}
	handlers := map[domain.NormalisedKind]port.PaymentEventHandler{}
	if h != nil {
		handlers[domain.KindInvoicePaid] = h
	}
	return &WebhookProcessor{Ledger: l, Providers: lookup{"stripe": prov}, Tx: tx, Handlers: handlers,
		MaxAttempts: 10, Now: func() time.Time { return now }}, l, tx
}

func TestProcessorOutcomes(t *testing.T) {
	applied := handlerFunc(func(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
		return "applied:conversion_payment", nil
	})
	prov := &fakeProvider{}
	rows := []domain.LedgerEntry{
		row("evt_ok", "invoice_paid", strp("t1"), `{"object":{"customer":"cus_1","metadata":{"tenant_id":"t1"}}}`, 1),
		row("evt_ignored", "ignored", strp("t1"), `{}`, 1),
		row("evt_unknown_customer", "invoice_paid", nil, `{}`, 1),
		row("evt_mismatch", "invoice_paid", strp("t1"), `{"object":{"metadata":{"tenant_id":"t2"}}}`, 1),
		row("evt_no_handler", "subscription_updated", strp("t1"), `{}`, 1),
	}
	p, l, tx := setup(rows, applied, prov)
	if n, err := p.ProcessBatch(context.Background()); err != nil || n != 5 {
		t.Fatalf("batch: %d %v", n, err)
	}
	want := map[string]finished{
		"evt_ok":               {domain.LedgerProcessed, "applied:conversion_payment", nil},
		"evt_ignored":          {domain.LedgerIgnored, "ignored:unhandled_type", nil},
		"evt_unknown_customer": {domain.LedgerFailed, "failed:unknown_customer", nil},
		"evt_mismatch":         {domain.LedgerFailed, "failed:metadata_mismatch", nil},
		"evt_no_handler":       {domain.LedgerIgnored, "ignored:no_handler", nil},
	}
	for id, w := range want {
		got := l.done[id]
		if got.status != w.status || got.outcome != w.outcome || (got.next == nil) != (w.next == nil) {
			t.Errorf("%s: %+v, want %+v", id, got, w)
		}
	}
	// BIL-PAY-4: the handled row was re-read first; BIL-PAY-3: only it ran in a tenant tx.
	if len(prov.fetched) != 1 || prov.fetched[0] != (domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_1"}) {
		t.Errorf("re-read %v", prov.fetched)
	}
	if len(tx.tenants) != 1 || tx.tenants[0] != "t1" {
		t.Errorf("tenant transactions %v", tx.tenants)
	}
}

func TestHandlerIgnoredAndRetry(t *testing.T) {
	ignore := handlerFunc(func(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
		return "", &port.IgnoredEvent{Reason: "transition_not_allowed"}
	})
	p, l, _ := setup([]domain.LedgerEntry{row("evt_1", "invoice_paid", strp("t1"), `{}`, 1)}, ignore, &fakeProvider{})
	_, _ = p.ProcessBatch(context.Background())
	if l.done["evt_1"].outcome != "ignored:transition_not_allowed" || l.done["evt_1"].status != domain.LedgerIgnored {
		t.Fatalf("got %+v", l.done["evt_1"])
	}

	// Transient failure → failed with a backoff time.
	failing := handlerFunc(func(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
		return "", domain.NewError(domain.ErrDBUnavailable, "db down")
	})
	p, l, _ = setup([]domain.LedgerEntry{row("evt_2", "invoice_paid", strp("t1"), `{}`, 3)}, failing, &fakeProvider{})
	_, _ = p.ProcessBatch(context.Background())
	got := l.done["evt_2"]
	if got.status != domain.LedgerFailed || got.outcome != "failed:DB_UNAVAILABLE" || got.next == nil || !got.next.Equal(now.Add(4*time.Second)) {
		t.Fatalf("retry: %+v", got)
	}

	// Provider re-read failure is retried too.
	p, l, _ = setup([]domain.LedgerEntry{row("evt_3", "invoice_paid", strp("t1"), `{}`, 1)}, failing, &fakeProvider{fetchErr: port.ErrProviderUnavailable})
	_, _ = p.ProcessBatch(context.Background())
	if l.done["evt_3"].outcome != "failed:PROVIDER_UNAVAILABLE" || l.done["evt_3"].next == nil {
		t.Fatalf("fetch failure: %+v", l.done["evt_3"])
	}

	// Exhausted attempts → parked (no next attempt).
	p, l, _ = setup([]domain.LedgerEntry{row("evt_4", "invoice_paid", strp("t1"), `{}`, 10)}, failing, &fakeProvider{})
	_, _ = p.ProcessBatch(context.Background())
	if l.done["evt_4"].status != domain.LedgerFailed || l.done["evt_4"].next != nil {
		t.Fatalf("parked: %+v", l.done["evt_4"])
	}
}

// A callback that needs an operator decision is parked: failed with no
// next attempt (BIL-OPS-6), never retried into a domain write.
func TestBIL_OPS_6_ParkedCallbackIsNotRetried(t *testing.T) {
	park := handlerFunc(func(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
		return "", &port.ParkedEvent{Reason: "checkout_superseded"}
	})
	p, l, _ := setup([]domain.LedgerEntry{row("evt_p", "invoice_paid", strp("t1"), `{}`, 1)}, park, &fakeProvider{})
	_, _ = p.ProcessBatch(context.Background())
	if got := l.done["evt_p"]; got.status != domain.LedgerFailed || got.outcome != "failed:checkout_superseded" || got.next != nil {
		t.Fatalf("parked: %+v", got)
	}
}

func TestBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second, 10: 5 * time.Minute, 30: 5 * time.Minute} {
		if got := Backoff(attempt); got != want {
			t.Errorf("attempt %d: %s, want %s", attempt, got, want)
		}
	}
}

func TestUnloadedProvider(t *testing.T) {
	p, l, _ := setup([]domain.LedgerEntry{row("evt_1", "invoice_paid", strp("t1"), `{}`, 1)}, nil, nil)
	p.Providers = lookup{}
	_, _ = p.ProcessBatch(context.Background())
	if l.done["evt_1"].outcome != "failed:provider_not_loaded" {
		t.Fatalf("got %+v", l.done["evt_1"])
	}
}

// F-7 (rev 0.71): a callback deferred while a conversion reversal runs is
// retried a minute later without spending an attempt, even past the budget;
// any other failure at the budget still parks.
func TestDeferredByConversionReversalNeverParks(t *testing.T) {
	unwinding := handlerFunc(func(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
		return "", errConversionUnwinding
	})
	p, l, _ := setup([]domain.LedgerEntry{row("evt_unwind", "invoice_paid", strp("t1"), `{}`, 10)}, unwinding, &fakeProvider{})
	_, _ = p.ProcessBatch(context.Background())
	got := l.done["evt_unwind"]
	if len(l.deferred) != 1 || got.status != domain.LedgerFailed || got.outcome != "failed:REQUEST_IN_PROGRESS" ||
		got.next == nil || !got.next.Equal(now.Add(time.Minute)) {
		t.Fatalf("deferral %+v %v", got, l.deferred)
	}
	other := handlerFunc(func(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
		return "", domain.NewError(domain.ErrRequestInProgress, "another request in progress")
	})
	p, l, _ = setup([]domain.LedgerEntry{row("evt_busy", "invoice_paid", strp("t1"), `{}`, 10)}, other, &fakeProvider{})
	_, _ = p.ProcessBatch(context.Background())
	if len(l.deferred) != 0 || l.done["evt_busy"].next != nil {
		t.Fatalf("a non-reversal failure at the budget must park: %+v", l.done["evt_busy"])
	}
}
