//go:build integration

package flows_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func setLifecycle(t *testing.T, tenant, lifecycle string, source any) {
	t.Helper()
	if err := sql(context.Background(), `UPDATE billing_accounts SET iam_lifecycle = $2::iam_lifecycle, iam_suspension_source = $3,
		closed_at = CASE WHEN $2 = 'offboarded' THEN now() END WHERE tenant_id = $1::uuid`, tenant, lifecycle, source); err != nil {
		t.Fatal(err)
	}
}

func paused(t *testing.T, sub string) string {
	t.Helper()
	return scalar[string](t, `SELECT collection_paused::text||'/'||coalesce(collection_paused_by,'') FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

// §8.10: operator suspension → automatic pause (provider and Billing, AL-5);
// a draft created meanwhile is held; B-O16 cannot finalise while paused but
// can void; reactivation lifts the automatic pause.
func TestOperatorSuspension(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	setLifecycle(t, tenant, "suspended", "operator")
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got := paused(t, sub); got != "true/auto" {
		t.Fatalf("pause %s", got)
	}
	if len(e.stripe.called("PauseCollection sub_"+sub+" true")) != 1 {
		t.Fatalf("provider pause: %v", e.stripe.called("PauseCollection"))
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.collection.paused'`, tenant); n != 1 {
		t.Fatalf("audit %d", n)
	}
	// Paused: tenant purchases refused (409 COLLECTION_PAUSED).
	if _, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1}); !domain.HasCode(err, domain.ErrCollectionPaused) {
		t.Fatalf("paused purchase: %v", err)
	}

	// A renewal draft while paused is held for review.
	draft := invoiceState("in_draft", sub, "draft", "subscription_cycle", time.Date(2026, 12, 24, 10, 0, 0, 0, time.UTC), 10)
	if out, err := e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_draft", "invoice.created", domain.KindInvoiceUpdated), draft); err != nil || out != "applied:invoice_held" {
		t.Fatalf("held: %s %v", out, err)
	}
	inv := scalar[string](t, `SELECT invoice_id::text FROM invoices WHERE provider_invoice_id = 'in_draft'`)
	if _, err := e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, app.DispositionRequest{Action: "finalize", Reason: "Customer liable for the period"}); !domain.HasCode(err, domain.ErrCollectionPaused) {
		t.Fatalf("finalize while paused: %v", err)
	}
	res, err := e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, app.DispositionRequest{Action: "void", Reason: "False-positive investigation closed"})
	if err != nil || res.Status != "void" || res.Disposition != "void" {
		t.Fatalf("void %+v %v", res, err)
	}
	if _, err := e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, app.DispositionRequest{Action: "void", Reason: "again, a long enough reason"}); !domain.HasCode(err, domain.ErrInvoiceNotHeld) {
		t.Fatalf("second decision: %v", err)
	}

	// Reactivation lifts the automatic pause.
	setLifecycle(t, tenant, "active", nil)
	_, _ = e.svc.RunLifecycleFollowUps(ctx, 100)
	if got := paused(t, sub); got != "false/" {
		t.Fatalf("resume %s", got)
	}
}

// B-O10: an operator pause is never lifted by a reactivation; "continue
// billing" clears it.
func TestOperatorPauseOverride(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	v, err := e.svc.SetCollectionPause(ctx, operator, tenant, app.CollectionPauseRequest{Paused: ptr(true), Reason: "Legal preservation requirement"})
	if err != nil || !v.CollectionPaused {
		t.Fatalf("pause %+v %v", v, err)
	}
	_, _ = e.svc.RunLifecycleFollowUps(ctx, 100) // account is active: an operator pause stays
	if got := paused(t, sub); got != "true/operator" {
		t.Fatalf("after follow-up %s", got)
	}
	if v, err := e.svc.SetCollectionPause(ctx, operator, tenant, app.CollectionPauseRequest{Paused: ptr(false), Reason: "Contractual obligation to keep billing"}); err != nil || v.CollectionPaused {
		t.Fatalf("continue %+v %v", v, err)
	}
}

// §8.8.4 offboarding: open purchase cancelled, provider subscription
// cancelled now, subscription ended (offboarded), no bus event.
func TestBIL_SUB_2_Offboarding(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, _ := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	e.stripe.change = []confirmAnswer{{err: errActionRequired()}}
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-off-0001"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv.PreviewID}); err != nil {
		t.Fatal(err)
	}
	setLifecycle(t, tenant, "offboarded", nil)
	_, _ = e.svc.RunLifecycleFollowUps(ctx, 100)
	if got := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "ended/offboarded" {
		t.Fatalf("subscription %s", got)
	}
	if got := scalar[string](t, `SELECT status::text FROM seat_purchases WHERE billing_subscription_id = $1`, sub); got != "cancelled" {
		t.Fatalf("purchase %s", got)
	}
	if len(e.stripe.called("CancelNow sub_"+sub+" cancel:"+sub+":offboarded")) != 1 {
		t.Fatalf("provider cancel: %v", e.stripe.called("CancelNow"))
	}
	if got := events(t, tenant); len(got) != 0 {
		t.Fatalf("events %v", got)
	}
	// Ended: the follow-up no longer picks it up.
	before := len(e.stripe.called("CancelNow"))
	_, _ = e.svc.RunLifecycleFollowUps(ctx, 100)
	if len(e.stripe.called("CancelNow")) != before {
		t.Fatal("closed twice")
	}
}
