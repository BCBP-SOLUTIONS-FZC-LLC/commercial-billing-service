//go:build integration

package flows_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// mirrorPaid mirrors a paid invoice of kind for the tenant.
func (e *env) mirrorPaid(t *testing.T, tenant, sub, providerID, kind string, amount int64) string {
	t.Helper()
	cur := invoiceState(providerID, sub, "paid", "subscription_create", e.now, 1)
	cur.Fields["kind"], cur.Fields["amount_due_minor"], cur.Fields["amount_paid_minor"] = kind, amount, amount
	if _, err := e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_m_"+providerID, "invoice.paid", domain.KindInvoiceUpdated), cur); err != nil {
		t.Fatal(err)
	}
	return scalar[string](t, `SELECT invoice_id::text FROM invoices WHERE provider_invoice_id = $1`, providerID)
}

func refundCallback(e *env, t *testing.T, tenant, providerRefundID, status string) (string, error) {
	cur := invoiceState(providerRefundID, "", status, "", e.now, 0)
	cur.Ref.Kind = domain.ObjectRefund
	return e.handle(t, tenant, app.RefundUpdated{S: e.svc}, callback("evt_"+providerRefundID, "refund.updated", domain.KindRefundUpdated), cur)
}

// B-O2 credit: negative balance transaction, AL-5; currency checked.
func TestAdjustment(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.activeSubscription(t, "pro", 3)
	res, err := e.svc.IssueAdjustment(ctx, withKey(operator, "adj-0001"), tenant, app.AdjustmentRequest{Kind: "credit",
		Amount: app.Money{AmountMinor: 50000, Currency: "USD"}, Description: "Service credit", Reason: "SLA credit approved by Finance"})
	if err != nil || res.ProviderRef == "" || len(e.stripe.called("CreditBalance -50000 adj:"+res.AdjustmentID)) != 1 {
		t.Fatalf("%+v %v %v", res, err, e.stripe.called("CreditBalance"))
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.adjustment.issued'`, tenant); n != 1 {
		t.Fatalf("audit %d", n)
	}
	if _, err := e.svc.IssueAdjustment(ctx, withKey(operator, "adj-0002"), tenant, app.AdjustmentRequest{Kind: "debit",
		Amount: app.Money{AmountMinor: 1, Currency: "AED"}, Description: "x x", Reason: "a long enough reason"}); !domain.HasCode(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("currency: %v", err)
	}
}

// B-O3 on an add-on invoice: category rule (BIL-ADD-3), proportional
// tokens, and on succeeded TenantQuotaAddOnChanged {refund} (BIL-ADD-4).
func TestAddonRefund(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t)
	p, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-ref-add1"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_5m", PackQuantity: 2, AcceptTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	inv := e.mirrorPaid(t, tenant, "", "in_addon:"+p.PurchaseID, "token_addon", 39800)
	if _, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-0001"), tenant, app.RefundRequest{InvoiceID: inv,
		Amount: app.Money{AmountMinor: 100, Currency: "USD"}, Category: "goodwill", Reason: "a long enough reason"}); !domain.HasCode(err, domain.ErrAddonRefundCategory) {
		t.Fatalf("category: %v", err)
	}
	if _, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-0002"), tenant, app.RefundRequest{InvoiceID: inv,
		Amount: app.Money{AmountMinor: 39801, Currency: "USD"}, Category: "duplicate_charge", Reason: "a long enough reason"}); !domain.HasCode(err, domain.ErrRefundExceedsPaid) {
		t.Fatalf("exceeds: %v", err)
	}
	r, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-0003"), tenant, app.RefundRequest{InvoiceID: inv,
		Amount: app.Money{AmountMinor: 19900, Currency: "USD"}, Category: "duplicate_charge", Reason: "Charged twice at 10:05 and 10:06"})
	if err != nil || r.Status != "pending" {
		t.Fatalf("refund %+v %v", r, err)
	}
	if out, err := refundCallback(e, t, tenant, "re_refund:"+r.RefundID, "succeeded"); err != nil || out != "applied:refund_succeeded" {
		t.Fatalf("callback %s %v", out, err)
	}
	ev := scalar[string](t, `SELECT (payload->'data'->>'delta')||'/'||(payload->'data'->>'reason')||'/'||(payload->'data'->>'purchase_ref')||'/'||(payload->'data'->>'billing_ref')
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantQuotaAddOnChanged' AND payload->'data'->>'reason' = 'refund'`, tenant)
	if ev != "-5000000/refund/"+p.PurchaseID+"/"+r.RefundID {
		t.Fatalf("event %s", ev)
	}
	if n := scalar[int64](t, `SELECT amount_refunded_minor FROM invoices WHERE invoice_id = $1`, inv); n != 19900 {
		t.Fatalf("refunded %d", n)
	}
	if _, err := refundCallback(e, t, tenant, "re_refund:"+r.RefundID, "succeeded"); err == nil {
		t.Fatal("repeat applied")
	}
}

// §8.2: a fraudulent dispute on the conversion payment within 7 days asks
// for a decision; B-O8 calls RP-4, cancels, refunds, ends the subscription
// (operator_reversal), reverts saga and dispute; no billing.events.
func TestConversionReversal(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	mustNoErr(t, sql(ctx, `UPDATE subscriptions SET converted_at = now() WHERE billing_subscription_id = $1::uuid`, sub))
	// TenantConverted applied: the account is active.
	mustNoErr(t, sql(ctx, `UPDATE billing_accounts SET iam_lifecycle = 'active', iam_last_event_at = now() WHERE tenant_id = $1::uuid`, tenant))
	e.mirrorPaid(t, tenant, sub, "in_conv_"+sub, "subscription_create", 20700)
	d := invoiceState("dp_1_"+sub, "", "needs_response", "", e.now, 0)
	d.Ref.Kind = domain.ObjectDispute
	d.Fields = map[string]any{"reason": "fraudulent", "payment_ref": "ch_1", "invoice": "in_conv_" + sub, "created": e.now}
	if _, err := e.handle(t, tenant, app.DisputeOpened{S: e.svc}, callback("evt_dp", "charge.dispute.created", domain.KindDisputeOpened), d); err != nil {
		t.Fatal(err)
	}
	disp := scalar[string](t, `SELECT dispute_id::text||'/'||revert_decision||'/'||is_conversion_payment FROM disputes WHERE tenant_id = $1`, tenant)
	if disp[37:] != "pending/true" {
		t.Fatalf("dispute %s", disp)
	}
	res, err := e.svc.RevertConversion(ctx, withKey(operator, "rev-0001"), tenant, app.ReversalRequest{DisputeID: disp[:36], Refund: true,
		Reason: "Early fraud warning on conversion payment"})
	if err != nil || res.ReversalStatus != "reverted" || res.RefundID == nil {
		t.Fatalf("reversal %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||s.cancel_reason::text||'/'||g.state::text||'/'||d.revert_decision
		FROM subscriptions s JOIN conversion_sagas g USING (billing_subscription_id) JOIN disputes d ON d.tenant_id = s.tenant_id
		WHERE s.billing_subscription_id = $1`, sub); got != "ended/operator_reversal/reverted/reverted" {
		t.Fatalf("state %s", got)
	}
	if len(e.stripe.called("Refund in_conv_"+sub+" 20700 fraudulent")) != 1 || len(e.stripe.called("CancelNow sub_"+sub)) != 1 {
		t.Fatalf("provider %v", e.stripe.calls)
	}
	if got := events(t, tenant); len(got) != 0 {
		t.Fatalf("events %v", got)
	}
	// RP-4 returned the tenant to a trial: Billing records it, so the
	// tenant can convert again through B-P2 (§8.2 step 3, rev 0.46).
	if got := scalar[string](t, `SELECT iam_lifecycle::text FROM billing_accounts WHERE tenant_id = $1`, tenant); got != "trial" {
		t.Fatalf("iam_lifecycle %s", got)
	}
	if _, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5}); err != nil {
		t.Fatalf("reconversion: %v", err)
	}

	// Outside the window → 409.
	tenant2, sub2 := e.activeSubscription(t, "pro", 3)
	mustNoErr(t, sql(ctx, `UPDATE subscriptions SET converted_at = now() - interval '8 days' WHERE billing_subscription_id = $1::uuid`, sub2))
	if _, err := e.svc.RevertConversion(ctx, operator, tenant2, app.ReversalRequest{Reason: "a long enough reason"}); !domain.HasCode(err, domain.ErrRevertWindowExpired) {
		t.Fatalf("window: %v", err)
	}
}

// B-O9 refund before TenantConverted: refund_pending, provider cancelled,
// subscription ended (signup_failed); the refund succeeding closes the
// saga (refunded).
func TestSagaRefund(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3) // direct signup: awaiting_tenant_converted
	e.mirrorPaid(t, tenant, sub, "in_first_"+sub, "subscription_create", 20700)
	sagaID := scalar[string](t, `SELECT saga_id::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub)
	v, err := e.svc.SagaAction(ctx, withKey(operator, "saga-ref-1"), tenant, sagaID, app.SagaActionRequest{Action: "refund", Reason: "Signup could not complete"})
	if err != nil || v.State != "refund_pending" {
		t.Fatalf("refund %+v %v", v, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "ended/signup_failed" {
		t.Fatalf("subscription %s", got)
	}
	ref := scalar[string](t, `SELECT provider_refund_id FROM refunds WHERE tenant_id = $1 AND category = 'signup_failed'`, tenant)
	if _, err := refundCallback(e, t, tenant, ref, "succeeded"); err != nil {
		t.Fatal(err)
	}
	if st := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, sagaID); st != "refunded" {
		t.Fatalf("saga %s", st)
	}
}
