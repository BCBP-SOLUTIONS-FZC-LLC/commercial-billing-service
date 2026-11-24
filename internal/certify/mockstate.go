package certify

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// mockState wraps the real adapter for the stripe-mock rehearsal. Every
// call still goes through the real adapter to stripe-mock, which validates
// the request against Stripe's OpenAPI spec. stripe-mock is stateless — it
// answers fixtures with fresh ids and no history — so where Billing needs
// provider state (a paid first invoice, a subscription's period and
// metadata, a payment behind an invoice) this layer substitutes a coherent
// answer and logs the substitution. Test mode never uses it.
type mockState struct {
	port.PaymentProvider
	now func() time.Time

	mu    sync.Mutex
	meta  map[string]map[string]string // provider subscription id → metadata
	subs  map[string]string            // provider invoice id → provider subscription id
	invs  map[string]string            // provider subscription id → first invoice id
	Notes []string                     // substitutions made, for the evidence
}

func newMockState(p port.PaymentProvider, now func() time.Time) *mockState {
	return &mockState{PaymentProvider: p, now: now, meta: map[string]map[string]string{}, subs: map[string]string{}, invs: map[string]string{}}
}

func (m *mockState) note(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Notes = append(m.Notes, s)
}

// TakeNotes returns and clears the substitutions since the last call.
func (m *mockState) TakeNotes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.Notes
	m.Notes = nil
	return n
}

func (m *mockState) CreateSubscription(ctx context.Context, in port.SubscriptionInput, key string) (port.SubscriptionResult, error) {
	r, err := m.PaymentProvider.CreateSubscription(ctx, in, key)
	if err != nil {
		return r, err
	}
	// stripe-mock answers the same fixture invoice id for every
	// subscription; Billing's mirror keys invoices by provider id, so each
	// subscription gets its own.
	inv := "in_mock_" + r.Ref.ProviderSubscriptionID
	m.note("CreateSubscription: fixture invoice id replaced by " + inv)
	r.LatestInvoice.ProviderInvoiceID = inv
	m.mu.Lock()
	m.meta[r.Ref.ProviderSubscriptionID] = in.Metadata
	m.subs[inv], m.invs[r.Ref.ProviderSubscriptionID] = r.Ref.ProviderSubscriptionID, inv
	m.mu.Unlock()
	now := m.now()
	r.PeriodStart, r.PeriodEnd = now, now.AddDate(0, 1, 0)
	return r, nil
}

func (m *mockState) ConfirmInitialPayment(ctx context.Context, ref port.SubscriptionRef, key string) (port.PaymentOutcome, error) {
	o, err := m.PaymentProvider.ConfirmInitialPayment(ctx, ref, key)
	if err != nil {
		return o, err
	}
	if o.Status != port.PaymentPaid {
		m.note("ConfirmInitialPayment: fixture answered " + string(o.Status) + "; paid substituted")
	}
	return port.PaymentOutcome{Status: port.PaymentPaid}, nil
}

func (m *mockState) FetchCurrent(ctx context.Context, ref domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	st, err := m.PaymentProvider.FetchCurrent(ctx, ref)
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return st, err
	}
	now := m.now()
	if ref.Kind == domain.ObjectSubscription {
		m.mu.Lock()
		md := m.meta[ref.ID]
		m.mu.Unlock()
		m.note("FetchCurrent subscription: period and metadata substituted")
		return domain.ProviderObjectState{Ref: ref, Status: "active", Metadata: md,
			Fields: map[string]any{"current_period_start": now, "current_period_end": now.AddDate(0, 1, 0)}}, nil
	}
	if ref.Kind == domain.ObjectInvoice {
		m.mu.Lock()
		sub := m.subs[ref.ID]
		m.mu.Unlock()
		m.note("FetchCurrent invoice: paid state substituted")
		return domain.ProviderObjectState{Ref: ref, Status: "paid", Metadata: map[string]string{},
			Fields: map[string]any{"subscription": sub, "billing_reason": "subscription_create", "currency": "USD",
				"amount_due_minor": int64(4500), "amount_paid_minor": int64(4500), "kind": string(domain.InvoiceSubscriptionCycle)}}, nil
	}
	return st, err
}

func (m *mockState) ChangeQuantity(ctx context.Context, in port.QuantityChange, key string) (port.ChangeResult, error) {
	r, err := m.PaymentProvider.ChangeQuantity(ctx, in, key)
	return m.paid("ChangeQuantity", r, err)
}

func (m *mockState) ChangePrice(ctx context.Context, in port.PriceChange, key string) (port.ChangeResult, error) {
	r, err := m.PaymentProvider.ChangePrice(ctx, in, key)
	return m.paid("ChangePrice", r, err)
}

func (m *mockState) paid(op string, r port.ChangeResult, err error) (port.ChangeResult, error) {
	if err != nil {
		return r, err
	}
	if r.Outcome.Status != port.PaymentPaid || r.Pending {
		m.note(op + ": fixture outcome " + string(r.Outcome.Status) + "; paid substituted")
	}
	r.Pending, r.Outcome = false, port.PaymentOutcome{Status: port.PaymentPaid}
	return r, nil
}

func (m *mockState) ChargeOneOff(ctx context.Context, in port.OneOffCharge, key string) (port.ChargeResult, error) {
	r, err := m.PaymentProvider.ChargeOneOff(ctx, in, key)
	if err != nil {
		return r, err
	}
	if r.Outcome.Status != port.PaymentPaid {
		m.note("ChargeOneOff: fixture outcome " + string(r.Outcome.Status) + "; paid substituted")
	}
	r.Outcome = port.PaymentOutcome{Status: port.PaymentPaid}
	return r, nil
}

func (m *mockState) Refund(ctx context.Context, in port.RefundInput, key string) (port.RefundResult, error) {
	r, err := m.PaymentProvider.Refund(ctx, in, key)
	if errors.Is(err, port.ErrNotFound) {
		// The fixture invoice carries no payment to refund.
		m.note("Refund: fixture invoice has no payment; pending refund substituted")
		return port.RefundResult{ProviderRefundID: "re_mock_" + key, Status: "pending"}, nil
	}
	return r, err
}

// paidCallback stands in for the first invoice.paid delivery stripe-mock
// cannot send: a ledger row for the signup's first invoice, applied by the
// real processor (which re-reads it through FetchCurrent).
func (m *mockState) paidCallback(ctx context.Context, e *Env, s subscriber) error {
	m.mu.Lock()
	inv := m.invs[s.ProviderSub]
	m.mu.Unlock()
	if inv == "" {
		return errors.New("no first invoice recorded for the subscription")
	}
	m.note("invoice.paid for " + inv + " ledgered by the suite (stripe-mock sends no events)")
	tenant := s.Tenant
	payload := []byte(`{"id":"evt_mock_` + inv + `","type":"invoice.paid","object":{"id":"` + inv + `","customer":"` + s.Customer + `"}}`)
	if _, err := e.Deliver.Processor.Ledger.Insert(ctx, domain.LedgerEntry{Provider: m.Name(), ProviderEventID: "evt_mock_" + inv,
		EventType: "invoice.paid", NormalisedKind: string(domain.KindInvoicePaid), TenantID: &tenant, ProviderObjectRef: &inv,
		ProviderCreatedAt: m.now(), Payload: payload}); err != nil {
		return err
	}
	return e.Deliver.Drain(ctx)
}

func (m *mockState) InvoicePDFLink(ctx context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	u, err := m.PaymentProvider.InvoicePDFLink(ctx, ref)
	if errors.Is(err, port.ErrNotFound) {
		m.note("InvoicePDFLink: fixture invoice has no invoice_pdf; link substituted")
		return port.URLWithExpiry{URL: "https://pay.stripe.com/invoice/mock/pdf", ExpiresAt: m.now().Add(time.Hour)}, nil
	}
	return u, err
}
