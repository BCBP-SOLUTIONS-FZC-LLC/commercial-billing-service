//go:build integration

package lifecycle_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func setLifecycle(t *testing.T, tenant, lifecycle string, source any) {
	t.Helper()
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = $2::iam_lifecycle, iam_suspension_source = $3,
		closed_at = CASE WHEN $2 = 'offboarded' THEN now() END WHERE tenant_id = $1::uuid`, tenant, lifecycle, source)
}

func paused(t *testing.T, sub string) string {
	t.Helper()
	return scalar[string](t, `SELECT collection_paused::text||'/'||coalesce(collection_paused_by,'') FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

// §8.10 automatic pause (AUTO_PAUSE_ON_OPERATOR_SUSPENSION, CD-24): the
// follow-up query failing is returned; a failed follow-up is logged and
// left for the next pass (provider call outside the transaction,
// BIL-CONC-2), and the pass after that applies it once.
func TestAutomaticPauseFollowUpFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.f.on("FollowUps.Due", 0)
	_, err := e.svc.RunLifecycleFollowUps(ctx, 10)
	injected(t, err, "FollowUps.Due")

	tenant, sub := e.signup(t, "pro", 4)
	setLifecycle(t, tenant, "suspended", "operator")
	for _, name := range []string{"Subs.Get", "Provider.PauseCollection", "Subs.Update", "Audit.Write"} {
		e.f.on(name, 0)
		if n, err := e.svc.RunLifecycleFollowUps(ctx, 10); err != nil || n != 1 {
			t.Fatalf("%s: %d %v", name, n, err)
		}
		if got := paused(t, sub); got != "false/" {
			t.Fatalf("%s: paused %s", name, got)
		}
	}
	e.withoutProvider("stripe", func() { _, err = e.svc.RunLifecycleFollowUps(ctx, 10) })
	if err != nil || paused(t, sub) != "false/" || !e.log.has("WARN", "lifecycle follow-up failed") {
		t.Fatalf("provider not loaded: %v %s", err, paused(t, sub))
	}
	if n, err := e.svc.RunLifecycleFollowUps(ctx, 10); err != nil || n != 1 || paused(t, sub) != "true/auto" {
		t.Fatalf("applied: %d %v %s", n, err, paused(t, sub))
	}
	if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.collection.paused'`, tenant); n != 1 {
		t.Fatalf("AL-5 entries %d", n)
	}
	if n, _ := e.svc.RunLifecycleFollowUps(ctx, 10); n != 0 {
		t.Fatalf("paused twice: %d", n)
	}
}

// B-O10: no live subscription is 404; a failed pause surfaces; the
// response view failing to read surfaces after the pause committed.
func TestOperatorPauseEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	req := app.CollectionPauseRequest{Paused: ptr(true), Reason: "Legal preservation requirement"}
	_, err := e.svc.SetCollectionPause(ctx, operator, e.tenant(), req)
	wantCode(t, err, domain.ErrSubscriptionNotFound)

	tenant, sub := e.signup(t, "pro", 4)
	e.f.on("Provider.PauseCollection", 0)
	_, err = e.svc.SetCollectionPause(ctx, operator, tenant, req)
	wantCode(t, err, domain.ErrInternal)
	if paused(t, sub) != "false/" {
		t.Fatal("paused although the provider refused")
	}
	e.f.on("Subs.Latest", 0)
	_, err = e.svc.SetCollectionPause(ctx, operator, tenant, req)
	injected(t, err, "Subs.Latest")
	if paused(t, sub) != "true/operator" {
		t.Fatal("pause not committed")
	}
}

// §8.8.4 offboarding: each failure leaves the subscription for the next
// pass; the next pass cancels the open purchase, the provider subscription
// (once per key) and ends Billing's; a stale follow-up for an ended
// subscription does nothing.
func TestOffboardingFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.signup(t, "pro", 4)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.stripe.change = []error{port.ErrActionRequired{ClientSecret: "pi_seat"}}
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-off-seat-1"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv.PreviewID}); err != nil {
		t.Fatal(err)
	}
	setLifecycle(t, tenant, "offboarded", nil)
	for _, c := range []struct {
		name string
		skip int
	}{{"Subs.Get", 0}, {"SeatPurchases.Open", 0}, {"SeatPurchases.Update", 0}, {"Checkouts.ExpireAll", 0}, {"Changes.Open", 0},
		{"Provider.CancelNow", 0}, {"Subs.Get", 1}, {"Subs.Transition", 0}} {
		e.f.on(c.name, c.skip)
		if _, err := e.svc.RunLifecycleFollowUps(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if status(t, sub) != "active" {
			t.Fatalf("%s/%d: ended", c.name, c.skip)
		}
	}
	e.withoutProvider("stripe", func() { _, _ = e.svc.RunLifecycleFollowUps(ctx, 10) })
	if status(t, sub) != "active" {
		t.Fatal("ended without the provider")
	}
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||s.cancel_reason::text||'/'||p.status::text FROM subscriptions s
		JOIN seat_purchases p USING (billing_subscription_id) WHERE s.billing_subscription_id = $1`, sub); got != "ended/offboarded/cancelled" {
		t.Fatalf("offboarded %s", got)
	}
	cancels := e.stripe.calls("CancelNow sub_" + sub + " cancel:" + sub + ":offboarded")
	if len(cancels) != 4 { // the failed call, then the two passes failing after it, then the success
		t.Fatalf("cancel calls %v", cancels)
	}
	// Another replica's stale follow-up: the subscription is already ended.
	e.follow.stale = []port.FollowUp{{TenantID: tenant, BillingSubscriptionID: sub, Action: "close"}}
	if n, err := e.svc.RunLifecycleFollowUps(ctx, 10); err != nil || n != 1 {
		t.Fatalf("stale: %d %v", n, err)
	}
	e.follow.stale = nil
	if len(e.stripe.calls("CancelNow")) != len(cancels) || len(events(t, tenant)) != 0 {
		t.Fatal("ended subscription closed again")
	}
}

// BIL-SUB-7: a draft created while paused is held (failures roll back);
// B-O16 decides it: not found, provider and write failures, then finalise
// once billing continues (AL-5 billing.invoice.suspension_disposition).
func TestHeldInvoiceEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.signup(t, "pro", 4)
	if _, err := e.svc.SetCollectionPause(ctx, operator, tenant, app.CollectionPauseRequest{Paused: ptr(true), Reason: "Fraud investigation open"}); err != nil {
		t.Fatal(err)
	}
	h := app.InvoiceUpdated{S: e.svc}
	ev := callback("evt_draft", "invoice.created", domain.KindInvoiceUpdated)
	draft := invoiceState("in_draft_"+sub, sub, "draft", "subscription_cycle", e.now, int64(4))
	for _, name := range []string{"Lock.LockTenant", "Invoices.Mirror", "Invoices.SetHeld"} {
		e.f.on(name, 0)
		_, err := e.handle(tenant, h, ev, draft)
		injected(t, err, name)
		if n := count(t, `SELECT count(*) FROM invoices WHERE tenant_id = $1 AND held_for_review`, tenant); n != 0 {
			t.Fatalf("%s: held", name)
		}
	}
	if out, err := e.handle(tenant, h, ev, draft); err != nil || out != "applied:invoice_held" {
		t.Fatalf("held: %s %v", out, err)
	}
	inv := scalar[string](t, `SELECT invoice_id::text FROM invoices WHERE provider_invoice_id = $1`, "in_draft_"+sub)

	void := app.DispositionRequest{Action: "void", Reason: "Investigation closed, waive"}
	e.f.on("Invoices.Get", 0)
	_, err := e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, void)
	injected(t, err, "Invoices.Get")
	_, err = e.svc.DisposeHeldInvoice(ctx, operator, tenant, e.tenant(), void)
	wantCode(t, err, domain.ErrInvoiceNotFound)
	_, err = e.svc.DisposeHeldInvoice(ctx, operator, e.tenant(), inv, void) // another tenant's invoice (RLS hides it)
	wantCode(t, err, domain.ErrInvoiceNotFound)
	e.withoutProvider("stripe", func() { _, err = e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, void) })
	wantCode(t, err, domain.ErrProviderNotEnabled)
	for _, name := range []string{"Provider.VoidInvoice", "Invoices.Dispose", "Audit.Write"} {
		e.f.on(name, 0)
		_, err := e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, void)
		injectedOrProvider(t, err, name)
		if got := scalar[*string](t, `SELECT disposition FROM invoices WHERE invoice_id = $1`, inv); got != nil {
			t.Fatalf("%s: disposed %s", name, *got)
		}
	}

	if _, err := e.svc.SetCollectionPause(ctx, operator, tenant, app.CollectionPauseRequest{Paused: ptr(false), Reason: "Investigation closed, continue"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.DisposeHeldInvoice(ctx, operator, tenant, inv, app.DispositionRequest{Action: "finalize", Reason: "Customer liable for the period"})
	if err != nil || res.Status != "open" || res.Disposition != "finalize" || res.DisposedBy != operator.UserID {
		t.Fatalf("finalize %+v %v", res, err)
	}
	if got := e.stripe.calls("FinalizeInvoice in_draft_" + sub + " disp:" + inv + ":finalize"); len(got) != 1 {
		t.Fatalf("finalize calls %v", got)
	}
	if got := scalar[string](t, `SELECT status||'/'||disposition FROM invoices WHERE invoice_id = $1`, inv); got != "open/finalize" {
		t.Fatalf("invoice %s", got)
	}
	if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.invoice.suspension_disposition'`, tenant); n != 1 {
		t.Fatalf("AL-5 entries %d", n)
	}
	if e.metrics.get("disposition:finalize") != 1 {
		t.Fatal("disposition metric")
	}
}
