//go:build integration

package flows_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func status(t *testing.T, sub string) string {
	t.Helper()
	return scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

// §8.8.1 → §8.9 (past_due): a cycle payment fails → past_due with
// TenantPaymentPastDue (invoice_ref is Billing's id), a second failure
// emits nothing (BIL-EVT-7), B-P14 gives the open-invoice link, the paid
// invoice reactivates with TenantReactivated {previous_status: past_due}.
func TestBIL_EVT_7_DunningAndRecovery(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	start := time.Date(2026, 12, 24, 10, 0, 0, 0, time.UTC)
	failed := invoiceState("in_cycle1", sub, "open", "subscription_cycle", start, 10)
	for i, id := range []string{"evt_f1", "evt_f2"} {
		out, err := e.handle(t, tenant, app.PaymentFailed{S: e.svc}, callback(id, "invoice.payment_failed", domain.KindInvoicePaymentFailed), failed)
		if i == 0 && (err != nil || out != "applied:past_due") || i == 1 && err == nil {
			t.Fatalf("failure %d: %s %v", i, out, err)
		}
	}
	if got := events(t, tenant); !slices.Equal(got, []string{"TenantPaymentPastDue"}) {
		t.Fatalf("events %v", got)
	}
	ref := scalar[string](t, `SELECT payload->'data'->>'invoice_ref' FROM outbox_events WHERE tenant_id = $1`, tenant)
	if inv := scalar[string](t, `SELECT provider_invoice_id FROM invoices WHERE invoice_id = $1`, ref); inv != "in_cycle1" {
		t.Fatalf("invoice_ref %s → %s", ref, inv)
	}
	// B-P14 on past_due → the provider's open-invoice page.
	r, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{})
	if err != nil || r.Kind != "open_invoice" || r.PaymentURL != "https://invoice.example/in_cycle1" {
		t.Fatalf("B-P14 %+v %v", r, err)
	}
	paid := invoiceState("in_cycle1", sub, "paid", "subscription_cycle", start, 10)
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_p1", "invoice.paid", domain.KindInvoicePaid), paid); err != nil || out != "applied:reactivated" {
		t.Fatalf("recovery: %s %v", out, err)
	}
	if status(t, sub) != "active" {
		t.Fatal("not active")
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'previous_status' FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantReactivated'`, tenant); got != "past_due" {
		t.Fatalf("previous_status %s", got)
	}
	// B-P14 on an active subscription → 409.
	if _, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{}); !domain.HasCode(err, domain.ErrSubscriptionNotLapsed) {
		t.Fatalf("active: %v", err)
	}
}

// §8.8.2: B-P12 cancels at period end (no event, scheduled reduction
// cancelled), B-P13 resumes, B-P12 again; the provider ends it →
// cancelled {reason: customer} with TenantSubscriptionCancelled. Then B-P14
// reactivation by checkout on the same billing_subscription_id: seats
// event first, then TenantReactivated (BIL-EVT-5).
func TestCancellationAndReactivation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 5}); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: e.version(t, sub), Reason: "switching_vendor"})
	if err != nil || v.AutoRenew || slices.Contains(v.ActionsAvailable, "cancel") || !slices.Contains(v.ActionsAvailable, "resume") {
		t.Fatalf("cancel %+v %v", v, err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM seat_change_schedules WHERE billing_subscription_id = $1 AND status = 'cancelled'`, sub); n != 1 {
		t.Fatalf("reduction not cancelled")
	}
	if len(events(t, tenant)) != 0 {
		t.Fatalf("event on B-P12: %v", events(t, tenant))
	}
	if v, err = e.svc.ResumeSubscription(ctx, owner, tenant, app.ResumeRequest{RecordVersion: e.version(t, sub)}); err != nil || !v.AutoRenew {
		t.Fatalf("resume %+v %v", v, err)
	}
	if _, err := e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: e.version(t, sub), Reason: "too_expensive"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.stripe.called("SetCancelAtPeriodEnd")); n != 3 {
		t.Fatalf("provider calls %d", n)
	}
	ended := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + sub}, Status: "canceled"}
	if out, err := e.handle(t, tenant, app.SubscriptionEnded{S: e.svc}, callback("evt_del", "customer.subscription.deleted", domain.KindSubscriptionEnded), ended); err != nil || out != "applied:cancelled" {
		t.Fatalf("ended: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "cancelled/customer" {
		t.Fatalf("cancelled %s", got)
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'reason' FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSubscriptionCancelled'`, tenant); got != "customer" {
		t.Fatalf("event reason %s", got)
	}

	// Reactivation: seats below in use → 422; then checkout for 12 seats.
	e.core.usage.ActiveUsers = 11
	if _, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{LicensedSeats: ptr(10)}); !domain.HasCode(err, domain.ErrSeatsBelowInUse) {
		t.Fatalf("below in use: %v", err)
	}
	r, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{LicensedSeats: ptr(12)})
	if err != nil || r.Kind != "checkout" || r.CheckoutSessionID == "" {
		t.Fatalf("checkout %+v %v", r, err)
	}
	// The hosted checkout completes on a new provider subscription …
	cs := scalar[string](t, `SELECT provider_session_id FROM checkout_sessions WHERE session_id = $1`, r.CheckoutSessionID)
	done := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: cs}, Status: "complete",
		Fields: map[string]any{"subscription": "sub_new_" + sub}}
	if _, err := e.handle(t, tenant, app.CheckoutCompleted{S: e.svc}, callback("evt_cs2", "checkout.session.completed", domain.KindCheckoutCompleted), done); err != nil {
		t.Fatal(err)
	}
	// … and its first invoice is paid.
	inv := invoiceState("in_react", "new_"+sub, "paid", "subscription_create", e.now, 12)
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_rp", "invoice.paid", domain.KindInvoicePaid), inv); err != nil || out != "applied:reactivated" {
		t.Fatalf("reactivation: %s %v", out, err)
	}
	if status(t, sub) != "active" {
		t.Fatal("not active")
	}
	if got := events(t, tenant); !slices.Equal(got, []string{"TenantSubscriptionCancelled", "TenantSeatsChanged", "TenantReactivated"}) {
		t.Fatalf("event order %v", got)
	}
}

func ptr[T any](v T) *T { return &v }

// F-4 (rev 0.71): B-P14 on a cancelled subscription may choose another
// interval (§5.4.1 B-P14). Once the checkout completes, the subscription's
// interval and the seat item's price row follow what the checkout bought,
// so renewals and previews price the new interval (BIL-SUB-8).
func TestReactivationOnAnotherIntervalUpdatesSubscription(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	before := scalar[string](t, `SELECT interval::text FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	other := "year"
	if before == "year" {
		other = "month"
	}
	if _, err := e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: e.version(t, sub), Reason: "too_expensive"}); err != nil {
		t.Fatal(err)
	}
	ended := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + sub}, Status: "canceled"}
	if _, err := e.handle(t, tenant, app.SubscriptionEnded{S: e.svc}, callback("evt_del", "customer.subscription.deleted", domain.KindSubscriptionEnded), ended); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{Interval: other})
	if err != nil || r.Kind != "checkout" {
		t.Fatalf("checkout %+v %v", r, err)
	}
	cs := scalar[string](t, `SELECT provider_session_id FROM checkout_sessions WHERE session_id = $1`, r.CheckoutSessionID)
	done := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: cs}, Status: "complete",
		Fields: map[string]any{"subscription": "sub_new_" + sub}}
	if _, err := e.handle(t, tenant, app.CheckoutCompleted{S: e.svc}, callback("evt_cs2", "checkout.session.completed", domain.KindCheckoutCompleted), done); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT s.interval::text||'/'||p.interval::text||'/'||p.provider_price_id FROM subscriptions s
		JOIN subscription_items i USING (billing_subscription_id) JOIN plan_prices p ON p.price_id = i.price_id
		WHERE s.billing_subscription_id = $1`, sub); got != other+"/"+other+"/price_pro_"+other {
		t.Fatalf("after the reactivation checkout: %s (was %s)", got, before)
	}
}

// D-L5 (rev 0.73): B-P12 keeps the owner's reason and comment on the
// status-history row of the cancellation; B-P13 (resume) records none.
func TestCancellationReasonStored(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	if _, err := e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: e.version(t, sub), Reason: "missing_features",
		Comment: "We need SSO for every department."}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT cancel_request_reason||'/'||cancel_request_comment FROM subscription_status_history
		WHERE billing_subscription_id = $1 AND cause = 'api:B-P12'`, sub); got != "missing_features/We need SSO for every department." {
		t.Fatalf("history %s", got)
	}
	if _, err := e.svc.ResumeSubscription(ctx, owner, tenant, app.ResumeRequest{RecordVersion: e.version(t, sub)}); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1 AND cause = 'api:B-P13'
		AND cancel_request_reason IS NULL AND cancel_request_comment IS NULL`, sub); n != 1 {
		t.Fatalf("B-P13 rows without a reason: %d", n)
	}
}
