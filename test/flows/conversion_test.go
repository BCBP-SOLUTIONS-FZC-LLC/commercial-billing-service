//go:build integration

package flows_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func checkoutCompleted(sub string) (domain.PaymentEvent, domain.ProviderObjectState) {
	return domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_cs_" + sub, EventType: "checkout.session.completed", Kind: domain.KindCheckoutCompleted},
		domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: "cs_" + sub}, Status: "complete",
			Fields: map[string]any{"subscription": "sub_" + sub}}
}

func firstInvoicePaid(sub string, start time.Time, qty int64) (domain.PaymentEvent, domain.ProviderObjectState) {
	return domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_in_" + sub, EventType: "invoice.paid", Kind: domain.KindInvoicePaid},
		domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_" + sub}, Status: "paid",
			Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_create",
				"line_period_start": start, "line_period_end": start.AddDate(0, 1, 0), "line_quantity": qty}}
}

// §8.1.1 – §8.1.2 end to end: checkout → link → payment → saga (retry,
// advance) → TenantConverted → delayed seat emission. BIL-SAGA-1/2.
func TestConversionFlow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	e.core.usage = port.SeatUsage{ActiveUsers: 3, PendingInvitations: 1}

	// Below seats in use → 422 with seats_in_use; nothing written.
	if _, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 3}); !domain.HasCode(err, domain.ErrSeatsBelowInUse) {
		t.Fatalf("below in use: %v", err)
	}
	// I-11 down → 503 (fail closed).
	e.core.err = domain.NewError(domain.ErrIAMUnavailable, "down")
	if _, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5}); !domain.HasCode(err, domain.ErrIAMUnavailable) {
		t.Fatalf("I-11 down: %v", err)
	}
	e.core.err = nil

	res, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5})
	if err != nil {
		t.Fatal(err)
	}
	sub := res.BillingSubscriptionID
	if res.CheckoutURL == "" || res.Price.AmountMinor != 5*6900 || res.Price.Currency != "USD" {
		t.Fatalf("checkout %+v", res)
	}
	if got := scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "incomplete" {
		t.Fatalf("status %s", got)
	}
	if got := scalar[string](t, `SELECT provider_session_id FROM checkout_sessions WHERE billing_subscription_id = $1`, sub); got != "cs_"+sub {
		t.Fatalf("session %s", got)
	}
	// Same content again → the same subscription and session (idempotent by content).
	again, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5})
	if err != nil || again.BillingSubscriptionID != sub {
		t.Fatalf("same content: %+v %v", again, err)
	}
	// B-P1 before payment: still a trial.
	if v, err := e.svc.GetSubscription(ctx, owner, tenant); err != nil || v.Status != "none" || v.ActionsAvailable[0] != "start_subscription" {
		t.Fatalf("B-P1 trial: %+v %v", v, err)
	}

	// checkout.session.completed links the provider subscription.
	ev, cur := checkoutCompleted(sub)
	if out, err := e.handle(t, tenant, app.CheckoutCompleted{S: e.svc}, ev, cur); err != nil || out != "applied:subscription_linked" {
		t.Fatalf("completed: %s %v", out, err)
	}
	// invoice.paid (re-read) → active, saga rp_convert_pending, no billing.events.
	start := e.now.Truncate(time.Second)
	ev, cur = firstInvoicePaid(sub, start, 5)
	if out, err := e.handle(t, tenant, app.FirstInvoicePaid{S: e.svc}, ev, cur); err != nil || out != "applied:conversion_payment" {
		t.Fatalf("paid: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "active" {
		t.Fatalf("status %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("conversion payment emitted %d events", n)
	}
	// A redelivered invoice.paid does not create a second saga (BIL-SAGA-2).
	if _, err := e.handle(t, tenant, app.FirstInvoicePaid{S: e.svc}, ev, cur); err == nil {
		t.Log("redelivery handled as a later invoice")
	}
	if n := scalar[int](t, `SELECT count(*) FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); n != 1 {
		t.Fatalf("%d sagas", n)
	}

	// Saga: RP-3 409 operation_in_progress → stays, backoff.
	e.rp.answers = []domain.RP3Answer{{Status: 409, ErrorCode: "operation_in_progress"}, {Status: 200}}
	if _, err := e.svc.RunSagas(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if st := scalar[string](t, `SELECT state::text||'/'||attempts||'/'||last_error_code FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); st != "rp_convert_pending/1/operation_in_progress" {
		t.Fatalf("after retry: %s", st)
	}
	if e.rp.keys[0] != "convert:"+tenant+":"+sub {
		t.Fatalf("RP-3 key %s", e.rp.keys[0])
	}
	// Not due yet → not called again.
	if _, _ = e.svc.RunSagas(ctx, 10); len(e.rp.keys) != 1 {
		t.Fatalf("called before backoff: %d", len(e.rp.keys))
	}
	e.now = e.now.Add(3 * time.Second)
	_, _ = e.svc.RunSagas(ctx, 10)
	if st := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); st != "awaiting_tenant_converted" {
		t.Fatalf("after 200: %s", st)
	}

	// TenantConverted confirms; converted_at; seat emission due +120 s.
	conv := app.TenantLifecycleEvent{EventID: "0193aaaa-0000-7000-8000-000000000001", Type: app.EventTenantConverted, Time: e.now,
		TenantID: tenant, BillingSubscriptionID: sub, Plan: domain.PlanPro}
	err = pgTx(tenant, func(ctx context.Context) error {
		matched, err := e.svc.ConfirmConversion(ctx, conv)
		if err == nil && !matched {
			t.Error("not matched")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if st := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); st != "confirmed" {
		t.Fatalf("saga %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM scheduled_emissions WHERE ref_id = $1 AND payload_builder = 'seats_after_conversion'
		AND due_at > now() + interval '100 seconds'`, sub); n != 1 {
		t.Fatalf("emission %d", n)
	}
	if v, err := e.svc.GetSubscription(ctx, owner, tenant); err != nil || v.Status != "active" || v.LicensedSeats != 5 || v.RenewalEstimate.Amount.AmountMinor != 34500 {
		t.Fatalf("B-P1 active: %+v %v", v, err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1`, sub); n != 1 {
		t.Fatalf("history rows %d", n)
	}
	// A second checkout now → 409.
	if _, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5}); !domain.HasCode(err, domain.ErrSubscriptionAlreadyExists) {
		t.Fatalf("second checkout: %v", err)
	}
}

func pgTx(tenant string, fn func(ctx context.Context) error) error {
	return txRunner().RunInTenantTx(context.Background(), tenant, fn)
}

// §8.1.3: an expired checkout ends its incomplete subscription; a new
// checkout with other content replaces the open one.
func TestAbandonedCheckout(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	first, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "enterprise", Interval: "year", LicensedSeats: 2})
	if err != nil || second.BillingSubscriptionID == first.BillingSubscriptionID {
		t.Fatalf("replacement: %v", err)
	}
	if st := scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, first.BillingSubscriptionID); st != "ended" {
		t.Fatalf("replaced subscription %s", st)
	}
	ev := domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_x", EventType: "checkout.session.expired", Kind: domain.KindCheckoutExpired}
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: "cs_" + second.BillingSubscriptionID}, Status: "expired"}
	if out, err := e.handle(t, tenant, app.CheckoutExpired{S: e.svc}, ev, cur); err != nil || out != "applied:checkout_expired" {
		t.Fatalf("expired: %s %v", out, err)
	}
	if st := scalar[string](t, `SELECT status::text||'/'||coalesce(cancel_reason::text,'') FROM subscriptions WHERE billing_subscription_id = $1`, second.BillingSubscriptionID); st != "ended/" {
		t.Fatalf("expired subscription %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("events %d", n)
	}
}

// §9.3.2 failure branch, B-O9 retry, B-O1 stub with its AL-5 entry.
func TestSagaFailureAndOperatorStub(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	res, err := e.svc.StubConversion(ctx, operator, tenant, app.StubConversionRequest{Plan: "pro", Interval: "year", LicensedSeats: 40,
		Reason: "Signed order form ORD-2026-118; invoiced offline"})
	if err != nil || res.Provider != "manual" || res.State != "rp_convert_pending" {
		t.Fatalf("stub: %+v %v", res, err)
	}
	if st := scalar[string](t, `SELECT status::text||'/'||origin::text FROM subscriptions WHERE billing_subscription_id = $1`, res.BillingSubscriptionID); st != "active/operator_stub" {
		t.Fatalf("stub subscription %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.conversion.stub'`, tenant); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	// Stub twice → 409.
	if _, err := e.svc.StubConversion(ctx, operator, tenant, app.StubConversionRequest{Plan: "pro", Interval: "year", LicensedSeats: 40, Reason: "again, a long enough reason"}); !domain.HasCode(err, domain.ErrSubscriptionAlreadyExists) {
		t.Fatalf("second stub: %v", err)
	}

	// RP-3 409 tenant_offboarded → failed.
	e.rp.answers = []domain.RP3Answer{{Status: 409, ErrorCode: "tenant_offboarded"}}
	_, _ = e.svc.RunSagas(ctx, 10)
	if st := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, res.SagaID); st != "failed" {
		t.Fatalf("failed: %s", st)
	}
	// B-O9 resolved_manually is allowed on failed? retry is the path back.
	v, err := e.svc.SagaAction(ctx, operator, tenant, res.SagaID, app.SagaActionRequest{Action: "retry", Reason: "RP fixed the tenant record"})
	if err != nil || v.State != "rp_convert_pending" {
		t.Fatalf("retry: %+v %v", v, err)
	}
	_, _ = e.svc.RunSagas(ctx, 10)
	if st := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, res.SagaID); st != "awaiting_tenant_converted" {
		t.Fatalf("after retry: %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.saga.resolved'`, tenant); n != 1 {
		t.Fatalf("saga audit rows %d", n)
	}
	// Unknown saga → 404.
	if _, err := e.svc.SagaAction(ctx, operator, tenant, newTenant(), app.SagaActionRequest{Action: "retry", Reason: "a long enough reason"}); !domain.HasCode(err, domain.ErrSagaNotFound) {
		t.Fatalf("unknown saga: %v", err)
	}
}
