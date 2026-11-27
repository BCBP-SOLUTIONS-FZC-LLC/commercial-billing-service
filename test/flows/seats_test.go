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

// §8.5: preview → purchase → paid now → confirmed, seat item, history,
// TenantSeatsChanged {reason: purchase, billing_ref: purchase_id}.
func TestSeatPurchase(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 5})
	if err != nil || pv.SeatsAfter != 15 || pv.AmountSource != "provider_preview" || pv.AmountDueNow.AmountMinor != 11500 {
		t.Fatalf("preview %+v %v", pv, err)
	}
	// Different add_seats than previewed → 409 PREVIEW_STALE.
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-st1"), tenant, app.SeatPurchaseRequest{AddSeats: 6, PreviewID: pv.PreviewID}); !domain.HasCode(err, domain.ErrPreviewStale) {
		t.Fatalf("stale: %v", err)
	}
	res, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-001"), tenant, app.SeatPurchaseRequest{AddSeats: 5, PreviewID: pv.PreviewID})
	if err != nil || res.Status != "confirmed" || *res.SeatsAfter != 15 || res.SeatUpdate != "in_progress" {
		t.Fatalf("purchase %+v %v", res, err)
	}
	if len(e.stripe.called("ChangeQuantity 10->15 invoice_now=true keep=false seatpur:"+res.PurchaseID)) != 1 {
		t.Fatalf("provider: %v", e.stripe.called("ChangeQuantity"))
	}
	if q := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); q != 15 {
		t.Fatalf("quantity %d", q)
	}
	ev := scalar[string](t, `SELECT (payload->'data'->>'licensed_seats')||'/'||(payload->'data'->>'previous_licensed_seats')||'/'||(payload->'data'->>'billing_ref')
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'`, tenant)
	if ev != "15/10/"+res.PurchaseID {
		t.Fatalf("event %s", ev)
	}
	// The preview was used: the seat count moved → PREVIEW_STALE on reuse.
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-002"), tenant, app.SeatPurchaseRequest{AddSeats: 5, PreviewID: pv.PreviewID}); !domain.HasCode(err, domain.ErrPreviewStale) {
		t.Fatalf("reuse: %v", err)
	}
	// B-P8 / B-P9.
	if v, err := e.svc.GetSeatPurchase(ctx, owner, tenant, res.PurchaseID); err != nil || v.Status != "confirmed" {
		t.Fatalf("get %+v %v", v, err)
	}
	if l, err := e.svc.ListSeatPurchases(ctx, owner, tenant, app.Page{Limit: 10}); err != nil || len(l.Items) != 1 {
		t.Fatalf("list %+v %v", l, err)
	}
}

// 3-D Secure → requires_action; one open purchase at a time; invoice.paid
// confirms; a repeat is ignored. Decline → failed, nothing added.
func TestSeatPurchaseAsync(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, _ := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	e.stripe.change = []confirmAnswer{{err: port.ErrActionRequired{ClientSecret: "pi_s"}}}
	res, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-3ds"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	if err != nil || res.Status != "requires_action" || res.ClientAction.ClientSecret != "pi_s" {
		t.Fatalf("3ds %+v %v", res, err)
	}
	pv2, _ := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-2nd"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv2.PreviewID}); !domain.HasCode(err, domain.ErrSeatPurchaseInProgress) {
		t.Fatalf("second open: %v", err)
	}
	h := app.FirstInvoicePaid{S: e.svc, Next: app.UpgradeInvoicePaid{S: e.svc, Next: app.SeatInvoicePaid{S: e.svc, Next: app.DowngradeOnCycle{S: e.svc}}}}
	paid := domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_sp", EventType: "invoice.paid", Kind: domain.KindInvoicePaid}
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_seatpur:" + res.PurchaseID}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "currency": "USD",
			"amount_paid_minor": scalar[int64](t, `SELECT amount_minor FROM seat_purchases WHERE purchase_id = $1`, res.PurchaseID)}}
	if out, err := e.handle(t, tenant, h, paid, cur); err != nil || out != "applied:seat_purchase" {
		t.Fatalf("paid: %s %v", out, err)
	}
	if _, err := e.handle(t, tenant, h, paid, cur); err == nil {
		t.Fatal("repeat confirmation not ignored")
	}
	if q := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); q != 12 {
		t.Fatalf("quantity %d", q)
	}
	pv3, _ := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	e.stripe.change = []confirmAnswer{{err: port.ErrDeclined{Code: "insufficient_funds"}}}
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-dcl"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv3.PreviewID}); !domain.HasCode(err, domain.ErrPaymentDeclined) {
		t.Fatalf("declined: %v", err)
	}
	if q := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); q != 12 {
		t.Fatalf("quantity after decline %d", q)
	}
}

// §8.5.3 fallback calculator below the provider minimum: seats added with
// no charge, deferred amount stored and carried on the event (BIL-SEAT-5).
func TestSeatPurchaseDeferred(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	e.stripe.fallback = true
	// 12 hours before the period end (the fake's period ends 2026-12-24 10:00).
	e.now = time.Date(2026, 12, 23, 22, 0, 0, 0, time.UTC)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if err != nil || pv.AmountSource != "billing_calculator" || pv.AmountDueNow.AmountMinor != 0 || pv.DeferredAmount == nil || pv.DeferralReason != "below_provider_minimum" {
		t.Fatalf("preview %+v %v", pv, err)
	}
	res, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-def"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv.PreviewID})
	if err != nil || res.Status != "confirmed" {
		t.Fatalf("purchase %+v %v", res, err)
	}
	if len(e.stripe.called("ChangeQuantity 10->11 invoice_now=false")) != 1 {
		t.Fatalf("provider: %v", e.stripe.called("ChangeQuantity"))
	}
	row := scalar[string](t, `SELECT amount_minor||'/'||deferred_amount_minor||'/'||amount_unrounded_minor||'/'||period_seconds FROM seat_purchases WHERE purchase_id = $1`, res.PurchaseID)
	if row != "0/"+itoa(pv.DeferredAmount.AmountMinor)+"/"+pv.AmountUnroundedMinor+"/2592000" {
		t.Fatalf("row %s", row)
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'deferral_reason' FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'`, tenant); got != "below_provider_minimum" {
		t.Fatalf("event %s", got)
	}
	_ = sub
}

// §8.6.1: B-P10 schedules (replacing an earlier one), no provider call and
// no event (BIL-SEAT-4); B-P11 cancels; validation errors.
func TestScheduledReduction(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 100)
	e.core.usage = port.SeatUsage{ActiveUsers: 55}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 100}); !domain.HasCode(err, domain.ErrTargetNotBelowCurrent) {
		t.Fatalf("not below: %v", err)
	}
	v, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 60})
	if err != nil || v.Status != "scheduled" || *v.SeatsInUse != 55 || *v.RenewalEstimateSeats != 60 {
		t.Fatalf("schedule %+v %v", v, err)
	}
	e.core.usage = port.SeatUsage{ActiveUsers: 75}
	v2, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 50})
	if err != nil || *v2.RenewalEstimateSeats != 75 {
		t.Fatalf("replace %+v %v", v2, err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM seat_change_schedules WHERE billing_subscription_id = $1 AND status = 'scheduled'`, sub); n != 1 {
		t.Fatalf("open schedules %d", n)
	}
	// I-11 down: still scheduled, fields null.
	e.core.err = domain.NewError(domain.ErrIAMUnavailable, "down")
	if v3, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 40}); err != nil || v3.SeatsInUse != nil {
		t.Fatalf("I-11 down %+v %v", v3, err)
	}
	e.core.err = nil
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged' AND payload->'data'->>'reason' <> 'purchase'`, tenant); n != 0 {
		t.Fatalf("event on schedule: %d", n)
	}
	if err := e.svc.CancelReduction(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CancelReduction(ctx, owner, tenant); !domain.HasCode(err, domain.ErrScheduleNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
}
