//go:build integration

package commerce_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

const reason = "Customer charged in error on renewal"

func refundReq(inv string, amount int64, category string) app.RefundRequest {
	return app.RefundRequest{InvoiceID: inv, Amount: app.Money{AmountMinor: amount, Currency: "USD"}, Category: category, Reason: reason}
}

// B-O2 (§8.11): validation, the intent row before the provider call
// (BIL-CONC-2), provider failures leave the intent unreferenced, a debit is a
// positive balance transaction, a missing client key is generated, and an
// amount above REVIEW_THRESHOLD_MINOR raises the review alert (HLD §11.6).
func TestAdjustmentPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	credit := app.AdjustmentRequest{Kind: "credit", Amount: app.Money{AmountMinor: 500, Currency: "USD"}, Description: "Service credit",
		Reason: "SLA credit approved by Finance"}
	_, err := e.svc.IssueAdjustment(ctx, operator, newTenant(), credit)
	isCode(t, "no account", err, domain.ErrSubscriptionNotFound)

	tenant, _ := e.activeSubscription(t, "pro", 3)
	zero := credit
	zero.Amount.AmountMinor = 0
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "adj-zero-01"), tenant, zero)
	isCode(t, "zero amount", err, domain.ErrValidation)

	e.fail("Money.InsertAdjustment", 0)
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "adj-ins-001"), tenant, credit)
	isInjected(t, "intent write", err)
	if n := len(e.p.called("CreditBalance")); n != 0 {
		t.Fatalf("provider called without an intent row: %d", n)
	}

	e.reg.hidden["stripe"] = true
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "adj-prov-01"), tenant, credit)
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")

	e.p.on("CreditBalance", answer{err: errProvider})
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "adj-perr-01"), tenant, credit)
	isCode(t, "provider down", err, domain.ErrProviderUnavailable)
	if n := count(t, `SELECT count(*) FROM adjustments WHERE tenant_id = $1 AND provider_ref IS NULL`, tenant); n != 2 {
		t.Fatalf("unreferenced intents %d, want 2", n)
	}

	// The provider credited but the reference write fails: the error
	// surfaces, no AL-5 entry is written, the intent keeps no reference.
	e.fail("Money.SetAdjustmentRef", 0)
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "adj-ref-001"), tenant, credit)
	isInjected(t, "reference write", err)
	if n := len(e.p.called("CreditBalance -500 adj:")); n != 2 {
		t.Fatalf("credit calls %d, want 2", n)
	}
	if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.adjustment.issued'`, tenant); n != 0 {
		t.Fatalf("audit entries %d", n)
	}

	big := app.AdjustmentRequest{Kind: "debit", Amount: app.Money{AmountMinor: 150000, Currency: "USD"}, Description: "Late setup fee",
		Reason: "Setup fee agreed with the customer"}
	res, err := e.svc.IssueAdjustment(ctx, operator, tenant, big)
	if err != nil || res.ProviderRef != "cbtxn_adj:"+res.AdjustmentID {
		t.Fatalf("debit %+v %v", res, err)
	}
	if n := len(e.p.called("CreditBalance 150000 adj:" + res.AdjustmentID)); n != 1 {
		t.Fatalf("debit call %v", e.p.called("CreditBalance"))
	}
	if got := scalar[string](t, `SELECT left(idempotency_key, 4)||'/'||provider_ref FROM adjustments WHERE adjustment_id = $1`, res.AdjustmentID); got != "adj:/cbtxn_adj:"+res.AdjustmentID {
		t.Fatalf("row %s", got)
	}
	if !e.log.has("WARN", "REVIEW_THRESHOLD_MINOR", "BillingOperatorReviewNeeded", "adjustment", "150000") {
		t.Fatalf("review alert not logged: %v", e.log.lines)
	}
}

// B-O3 (§8.11): invoice checks, the pending row before the provider call, a
// provider refund that settles at once (succeeded or failed), and storage
// failures after the provider call (the refund stays pending without a
// provider id; reconciliation flags it).
func TestRefundPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	paid := e.mirror(t, tenant, sub, "in_paid_"+sub, "subscription_create", "paid", 20700)
	open := e.mirror(t, tenant, sub, "in_open_"+sub, "subscription_cycle", "open", 20700)

	_, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-nf-0001"), tenant, refundReq(newTenant(), 100, "billing_error"))
	isCode(t, "unknown invoice", err, domain.ErrInvoiceNotFound)
	e.fail("Invoices.Get", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-db-0001"), tenant, refundReq(paid, 100, "billing_error"))
	isInjected(t, "invoice read", err)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-op-0001"), tenant, refundReq(open, 100, "billing_error"))
	isCode(t, "open invoice", err, domain.ErrValidation)
	aed := refundReq(paid, 100, "billing_error")
	aed.Amount.Currency = "AED"
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-cur-001"), tenant, aed)
	isCode(t, "currency", err, domain.ErrCurrencyMismatch)
	if n := count(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("refused refunds recorded: %d", n)
	}

	e.reg.hidden["stripe"] = true
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-pv-0001"), tenant, refundReq(paid, 100, "billing_error"))
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("Refund", answer{err: errProvider})
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-pe-0001"), tenant, refundReq(paid, 100, "billing_error"))
	isCode(t, "provider down", err, domain.ErrProviderUnavailable)
	e.fail("Money.UpdateRefund", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-up-0001"), tenant, refundReq(paid, 100, "billing_error"))
	isInjected(t, "refund update", err)
	e.fail("Audit.Write", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-au-0001"), tenant, refundReq(paid, 100, "billing_error"))
	isInjected(t, "AL-5 write", err)
	e.p.on("Refund", answer{status: "succeeded"})
	e.fail("Invoices.AddRefunded", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-ar-0001"), tenant, refundReq(paid, 100, "billing_error"))
	isInjected(t, "refunded total", err)
	if n := count(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND status = 'pending' AND provider_refund_id IS NULL`, tenant); n != 5 {
		t.Fatalf("pending refunds without a provider id %d, want 5", n)
	}
	if n := len(e.p.called("Refund in_paid_" + sub + " 100 " + string(port.RefundReasonRequestedByCustomer) + " refund:")); n != 4 {
		t.Fatalf("provider refunds %d, want 4", n)
	}

	// The provider settles at once: succeeded adds to the refunded total;
	// failed leaves it unchanged.
	e.p.on("Refund", answer{status: "succeeded"}, answer{status: "failed"})
	ok, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-ok-0001"), tenant, refundReq(paid, 300, "fraud"))
	if err != nil || ok.Status != "succeeded" || ok.Amount.AmountMinor != 300 {
		t.Fatalf("succeeded %+v %v", ok, err)
	}
	ko, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-ko-0001"), tenant, refundReq(paid, 200, "duplicate_charge"))
	if err != nil || ko.Status != "failed" {
		t.Fatalf("failed %+v %v", ko, err)
	}
	if n := scalar[int64](t, `SELECT amount_refunded_minor FROM invoices WHERE invoice_id = $1`, paid); n != 300 {
		t.Fatalf("refunded total %d, want 300", n)
	}
	if got := scalar[string](t, `SELECT string_agg(status||':'||provider_refund_id, ',' ORDER BY amount_minor DESC) FROM refunds WHERE refund_id IN ($1, $2)`,
		ok.RefundID, ko.RefundID); got != "succeeded:re_refund:"+ok.RefundID+",failed:re_refund:"+ko.RefundID {
		t.Fatalf("rows %s", got)
	}
	if len(e.p.called("Refund in_paid_"+sub+" 300 fraudulent refund:"+ok.RefundID)) != 1 ||
		len(e.p.called("Refund in_paid_"+sub+" 200 duplicate refund:"+ko.RefundID)) != 1 {
		t.Fatalf("provider reasons %v", e.p.called("Refund"))
	}
}

// BIL-ADD-3 / BIL-ADD-4: an add-on invoice refund that settles at once
// emits TenantQuotaAddOnChanged {refund} with the proportional tokens; the
// event failing rolls the settlement back. A signup_failed refund closes
// only a refund_pending saga.
func TestAddonAndSignupRefundSettlement(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t, "pro", 5)
	p, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-ar-buy-01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 2, AcceptTerms: true})
	if err != nil || p.Status != "confirmed" {
		t.Fatalf("purchase %+v %v", p, err)
	}
	inv := e.mirror(t, tenant, "", "in_addon:"+p.PurchaseID, "token_addon", "paid", 9800)

	e.fail("Addons.ByProviderInvoice", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-ad-0001"), tenant, refundReq(inv, 4900, "duplicate_charge"))
	isInjected(t, "purchase lookup", err)
	e.p.on("Refund", answer{status: "succeeded"}, answer{status: "succeeded"})
	e.fail("Events.Enqueue", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-ad-0002"), tenant, refundReq(inv, 4900, "duplicate_charge"))
	isInjected(t, "reversal event", err)
	if n := outbox(t, tenant, "TenantQuotaAddOnChanged"); n != 1 {
		t.Fatalf("events after the failed settlement %d, want the purchase only", n)
	}
	r, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-ad-0003"), tenant, refundReq(inv, 4900, "duplicate_charge"))
	if err != nil || r.Status != "succeeded" {
		t.Fatalf("refund %+v %v", r, err)
	}
	got := scalar[string](t, `SELECT (payload->'data'->>'delta')||'/'||(payload->'data'->>'purchase_ref')||'/'||
		(payload->>'id' = (SELECT addon_event_id::text FROM refunds WHERE refund_id = $2))::text
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantQuotaAddOnChanged' AND payload->'data'->>'reason' = 'refund'`, tenant, r.RefundID)
	if got != "-1000000/"+p.PurchaseID+"/true" {
		t.Fatalf("reversal event %s", got)
	}

	// signup_failed on a saga that is not refund_pending: the saga stays.
	t2, s2 := e.activeSubscription(t, "pro", 3)
	conv := e.mirror(t, t2, s2, "in_conv_"+s2, "subscription_create", "paid", 20700)
	e.p.on("Refund", answer{status: "succeeded"})
	e.fail("Subs.Latest", 0)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "ref-sf-0001"), t2, refundReq(conv, 100, domain.RefundCategorySignupFailed))
	isInjected(t, "saga close", err)
	e.p.on("Refund", answer{status: "succeeded"})
	if _, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-sf-0002"), t2, refundReq(conv, 100, domain.RefundCategorySignupFailed)); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, s2); got != "awaiting_tenant_converted" {
		t.Fatalf("saga %s", got)
	}
	if n := scalar[int64](t, `SELECT amount_refunded_minor FROM invoices WHERE invoice_id = $1`, conv); n != 100 {
		t.Fatalf("refunded %d, want 100 (the failed settlement rolled back)", n)
	}
}

func refundState(id, status string) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectRefund, ID: id}, Status: status}
}

// refund.updated (§8.11): only refund objects, only Billing's pending
// refunds; canceled is a failure; a still-pending status waits.
func TestRefundCallbackPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	paid := e.mirror(t, tenant, sub, "in_paid_"+sub, "subscription_create", "paid", 20700)
	r1, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-cb-0001"), tenant, refundReq(paid, 1000, "goodwill"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-cb-0002"), tenant, refundReq(paid, 2000, "goodwill"))
	if err != nil {
		t.Fatal(err)
	}
	h := app.RefundUpdated{S: e.svc}
	ev := callback("evt_ru", "refund.updated", domain.KindRefundUpdated)
	cb := func(id, status string) (string, error) { return e.handle(tenant, h, ev, refundState(id, status)) }

	inv := refundState("re_refund:"+r1.RefundID, "succeeded")
	inv.Ref.Kind = domain.ObjectInvoice
	_, err = e.handle(tenant, h, ev, inv)
	ignored(t, "invoice object", err, "not_a_refund_object")
	e.fail("Lock.LockTenant", 0)
	_, err = cb("re_refund:"+r1.RefundID, "succeeded")
	isInjected(t, "lock", err)
	e.fail("Money.RefundByProvider", 0)
	_, err = cb("re_refund:"+r1.RefundID, "succeeded")
	isInjected(t, "refund read", err)
	_, err = cb("re_dashboard_1", "succeeded")
	ignored(t, "dashboard refund", err, "unknown_refund")
	_, err = cb("re_refund:"+r1.RefundID, "pending")
	ignored(t, "still pending", err, "refund_still_pending")
	if out, err := cb("re_refund:"+r1.RefundID, "canceled"); err != nil || out != "applied:refund_failed" {
		t.Fatalf("canceled %s %v", out, err)
	}
	_, err = cb("re_refund:"+r1.RefundID, "succeeded")
	ignored(t, "settled refund", err, "refund_failed")
	e.fail("Invoices.AddRefunded", 0)
	_, err = cb("re_refund:"+r2.RefundID, "succeeded")
	isInjected(t, "refunded total", err)
	if out, err := cb("re_refund:"+r2.RefundID, "succeeded"); err != nil || out != "applied:refund_succeeded" {
		t.Fatalf("succeeded %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT (SELECT amount_refunded_minor FROM invoices WHERE invoice_id = $1)||'/'||
		(SELECT string_agg(status, ',' ORDER BY amount_minor) FROM refunds WHERE tenant_id = $2)`, paid, tenant); got != "2000/failed,succeeded" {
		t.Fatalf("state %s", got)
	}
}

func disputeState(id string, kind domain.ProviderObjectKind, fields map[string]any) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: kind, ID: id}, Status: "needs_response", Fields: fields}
}

// §8.2 disputes: an early fraud warning on the conversion payment within the
// revert window asks for a decision (SEV-2 BillingReversalDecisionNeeded);
// storage failures roll the record back.
func TestDisputePaths(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	exec(t, `UPDATE subscriptions SET converted_at = $2 WHERE billing_subscription_id = $1::uuid`, sub, e.now)
	e.mirror(t, tenant, sub, "in_conv_"+sub, "subscription_create", "paid", 20700)
	h := app.DisputeOpened{S: e.svc}
	ev := callback("evt_efw", "radar.early_fraud_warning.created", domain.KindDisputeOpened)
	efw := disputeState("issfr_"+sub, domain.ObjectEarlyFraudWarning, map[string]any{"payment_ref": "ch_1", "invoice": "in_conv_" + sub})

	for _, f := range []string{"Lock.LockTenant", "Invoices.ByProvider", "Subs.Live", "Money.UpsertDispute"} {
		e.fail(f, 0)
		_, err := e.handle(tenant, h, ev, efw)
		isInjected(t, f, err)
	}
	if n := count(t, `SELECT count(*) FROM disputes WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("disputes after failures %d", n)
	}
	if out, err := e.handle(tenant, h, ev, efw); err != nil || out != "applied:dispute_recorded" {
		t.Fatalf("efw %s %v", out, err)
	}
	got := scalar[string](t, `SELECT kind||'/'||revert_decision||'/'||is_conversion_payment||'/'||within_revert_window||'/'||(opened_at = $2)::text||'/'||(reason IS NULL)::text
		FROM disputes WHERE tenant_id = $1`, tenant, e.now)
	if got != "early_fraud_warning/pending/true/true/true/true" {
		t.Fatalf("dispute %s", got)
	}
	if !e.log.has("ERROR", "SEV-2", "BillingReversalDecisionNeeded", tenant) {
		t.Fatalf("decision alert not logged: %v", e.log.lines)
	}
}

// B-O8 (§8.2): refusals before RP-4; RP-4 answers; provider failures; then
// each commit step failing rolls the whole commit back, and a retry with
// the same RP-4 key completes it. A lost dispute already returned the funds:
// no refund.
func TestReversalPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	req := app.ReversalRequest{Reason: "Early fraud warning on conversion payment"}
	_, err := e.svc.RevertConversion(ctx, operator, newTenant(), req)
	isCode(t, "no subscription", err, domain.ErrSubscriptionNotActive)

	tenant, sub := e.activeSubscription(t, "pro", 3)
	exec(t, `UPDATE subscriptions SET converted_at = $2 WHERE billing_subscription_id = $1::uuid`, sub, e.now)
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1::uuid`, tenant)
	e.mirror(t, tenant, sub, "in_conv_"+sub, "subscription_create", "paid", 20700)
	saga := scalar[string](t, `SELECT saga_id::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub)
	exec(t, `UPDATE conversion_sagas SET state = 'awaiting_tenant_converted' WHERE saga_id = $1`, saga)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
	isCode(t, "conversion not confirmed", err, domain.ErrSubscriptionNotActive)
	exec(t, `UPDATE conversion_sagas SET state = 'confirmed', confirmed_at = now() WHERE saga_id = $1`, saga)

	e.fail("Subs.Live", 0)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
	isInjected(t, "live read", err)
	e.fail("Sagas.BySubscription", 0)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
	isInjected(t, "saga read", err)
	e.absent("Sagas.BySubscription", 0)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
	isCode(t, "no saga", err, domain.ErrSagaNotFound)
	foreign := req
	foreign.DisputeID = newTenant()
	e.fail("Money.Dispute", 0)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, foreign)
	isInjected(t, "dispute read", err)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, foreign)
	isCode(t, "foreign dispute", err, domain.ErrValidation)
	if len(e.rp.keys) != 0 {
		t.Fatalf("RP-4 called before the checks passed: %v", e.rp.keys)
	}

	e.rp.reverts = []domain.RP3Answer{{Status: 409, ErrorCode: "revert_window_expired"}, {Status: 503}, {Status: 0},
		{Status: 409, ErrorCode: "operation_in_progress"}, {Status: 409, ErrorCode: "optimistic_lock_conflict"},
		{Status: 422, ErrorCode: "tenant_not_converted"}, {Status: 400, ErrorCode: "invalid_request"},
		{Status: 403, ErrorCode: "insufficient_role"}, {Status: 403}, {Status: 404, ErrorCode: "tenant_realm_not_found"}}
	for i, want := range []domain.ErrorCode{domain.ErrRevertWindowExpired, domain.ErrIAMUnavailable, domain.ErrIAMUnavailable,
		domain.ErrIAMUnavailable, domain.ErrIAMUnavailable, domain.ErrInternal, domain.ErrInternal, domain.ErrInternal, domain.ErrInternal,
		domain.ErrInternal} {
		e.now = e.now.Add(10 * time.Minute) // past the previous answer's backoff
		_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
		isCode(t, "RP-4 answer "+string(rune('0'+i)), err, want)
	}
	// Each refusal above dropped the request: the saga is confirmed again.
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, saga); got != "confirmed" {
		t.Fatalf("after a terminal RP-4 answer: %s", got)
	}
	// From here RP-4 answers 200 and a provider step fails: the request stays
	// recorded (reverting) and a repeat of the same body continues it.
	withRefund := req
	withRefund.Refund = true
	e.reg.hidden["stripe"] = true
	_, err = e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	if got := scalar[string](t, `SELECT state::text||'/'||reversal_refund::text FROM conversion_sagas WHERE saga_id = $1`, saga); got != "reverting/true" {
		t.Fatalf("request not recorded: %s", got)
	}
	_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
	isCode(t, "a different request while one is recorded", err, domain.ErrRequestInProgress)
	// RP-4 answered 200: recorded, so RP-4 is never called again for it.
	if n := count(t, `SELECT count(*) FROM conversion_sagas WHERE saga_id = $1 AND reversal_rp_done_at IS NOT NULL`, saga); n != 1 {
		t.Fatal("RP-4 200 not recorded")
	}
	_, err = e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	isCode(t, "repeat inside the backoff", err, domain.ErrIAMUnavailable)
	rpCalls := len(e.rp.keys)
	e.p.on("CancelNow", answer{err: errProvider})
	e.now = e.now.Add(10 * time.Minute)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	isCode(t, "cancel", err, domain.ErrProviderUnavailable)
	e.fail("Invoices.ConversionInvoice", 0)
	e.now = e.now.Add(10 * time.Minute)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	isInjected(t, "conversion invoice", err)
	e.p.on("Refund", answer{err: errProvider})
	e.now = e.now.Add(10 * time.Minute)
	_, err = e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	isCode(t, "refund", err, domain.ErrProviderUnavailable)
	if len(e.rp.keys) != rpCalls {
		t.Fatalf("RP-4 called again after its 200: %d calls", len(e.rp.keys)-rpCalls)
	}
	// B-O9 refund, resolved_manually and a late TenantConverted leave the
	// recorded reversal alone (§9.3.1).
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, app.SagaActionRequest{Action: "refund", Reason: "Signup could not complete"})
	isCode(t, "B-O9 refund while reverting", err, domain.ErrSagaAlreadyConfirmed)
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, app.SagaActionRequest{Action: "resolved_manually", Reason: "Checked by hand in RP"})
	isCode(t, "resolved_manually while reverting", err, domain.ErrSagaAlreadyConfirmed)
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, saga); got != "reverting" {
		t.Fatalf("saga %s", got)
	}
	for _, k := range e.rp.keys {
		if k != "revert:"+tenant+":"+sub {
			t.Fatalf("RP-4 key %s", k)
		}
	}

	// A lost fraudulent dispute: the funds went back with it.
	d := disputeState("dp_lost_"+sub, domain.ObjectDispute, map[string]any{"reason": "fraudulent", "payment_ref": "ch_1", "invoice": "in_conv_" + sub})
	d.Status = "lost"
	if _, err := e.handle(tenant, app.DisputeOpened{S: e.svc}, callback("evt_dp_lost", "charge.dispute.created", domain.KindDisputeOpened), d); err != nil {
		t.Fatal(err)
	}
	withRefund.DisputeID = scalar[string](t, `SELECT dispute_id::text FROM disputes WHERE tenant_id = $1`, tenant)
	// The recorded request had no dispute: a request naming one waits for it.
	_, err = e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	isCode(t, "dispute added while a request is recorded", err, domain.ErrRequestInProgress)
	exec(t, `UPDATE conversion_sagas SET state = 'confirmed', next_attempt_at = NULL, reversal_requested_at = NULL,
		reversal_requested_by = NULL, reversal_reason = NULL, reversal_refund = NULL, reversal_rp_done_at = NULL WHERE saga_id = $1`, saga)
	e.now = e.now.Add(10 * time.Minute)
	for _, f := range []string{"Subs.Get", "Subs.Transition", "Subs.AppendHistory", "Sagas.Get", "Sagas.Update", "Accounts.ReturnToTrial",
		"Money.DecideDispute", "Audit.Write"} {
		e.fail(f, 0)
		e.now = e.now.Add(10 * time.Minute)
		_, err := e.svc.RevertConversion(ctx, operator, tenant, withRefund)
		isInjected(t, f, err)
		if got := scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "active" {
			t.Fatalf("%s: commit not rolled back (%s)", f, got)
		}
	}
	refunds := len(e.p.called("Refund "))
	e.now = e.now.Add(10 * time.Minute)
	res, err := e.svc.RevertConversion(ctx, operator, tenant, withRefund)
	if err != nil || res.ReversalStatus != "reverted" || res.RefundID != nil {
		t.Fatalf("reversal %+v %v", res, err)
	}
	if n := len(e.p.called("Refund ")); n != refunds {
		t.Fatalf("refund issued although the dispute returned the funds")
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||s.cancel_reason::text||'/'||g.state::text||'/'||d.revert_decision||'/'||a.iam_lifecycle::text
		FROM subscriptions s JOIN conversion_sagas g USING (billing_subscription_id) JOIN disputes d ON d.tenant_id = s.tenant_id
		JOIN billing_accounts a ON a.tenant_id = s.tenant_id WHERE s.billing_subscription_id = $1`, sub); got != "ended/operator_reversal/reverted/reverted/trial" {
		t.Fatalf("state %s", got)
	}
	if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.conversion.reverted'`, tenant); n != 1 {
		t.Fatalf("audit %d", n)
	}
}

// B-O8 answered 503 while Realm Provisioner was still reverting (RP-D30):
// the request stays recorded, the saga worker retries RP-4 with the same key
// and completes the reversal (provider cancel, one refund, subscription
// ended, account back on trial, one AL-5 entry), even after the 7-day window;
// a later repeat of B-O8 finds nothing left to revert.
func TestReversalCompletedBySagaWorker(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	exec(t, `UPDATE subscriptions SET converted_at = $2 WHERE billing_subscription_id = $1::uuid`, sub, e.now)
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1::uuid`, tenant)
	e.mirror(t, tenant, sub, "in_conv_"+sub, "subscription_create", "paid", 20700)
	saga := scalar[string](t, `SELECT saga_id::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub)
	exec(t, `UPDATE conversion_sagas SET state = 'confirmed', confirmed_at = now() WHERE saga_id = $1`, saga)
	req := app.ReversalRequest{Refund: true, Reason: "Early fraud warning on conversion payment"}

	e.rp.reverts = []domain.RP3Answer{{Status: 409, ErrorCode: "operation_in_progress"}, {Status: 0}}
	_, err := e.svc.RevertConversion(ctx, operator, tenant, req)
	isCode(t, "RP-4 still running", err, domain.ErrIAMUnavailable)
	if got := scalar[string](t, `SELECT state::text||'/'||last_error_code FROM conversion_sagas WHERE saga_id = $1`, saga); got != "reverting/operation_in_progress" {
		t.Fatalf("after 503: %s", got)
	}
	// Past the window: the recorded request is still completed.
	e.now = e.now.Add(8 * 24 * time.Hour)
	if _, err := e.svc.RunSagas(ctx, 100); err != nil { // RP-4 transport error: retried later
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, saga); got != "reverting" {
		t.Fatalf("after a retryable worker attempt: %s", got)
	}
	e.now = e.now.Add(10 * time.Minute)
	if _, err := e.svc.RunSagas(ctx, 100); err != nil { // RP-4 200: completed
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||s.cancel_reason::text||'/'||g.state::text||'/'||a.iam_lifecycle::text
		FROM subscriptions s JOIN conversion_sagas g USING (billing_subscription_id)
		JOIN billing_accounts a ON a.tenant_id = s.tenant_id WHERE s.billing_subscription_id = $1`, sub); got != "ended/operator_reversal/reverted/trial" {
		t.Fatalf("state %s", got)
	}
	if n := count(t, `SELECT count(*) FROM refunds WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, "refund:reversal:"+sub); n != 1 {
		t.Fatalf("refunds %d", n)
	}
	if c := e.p.called("CancelNow!"); len(c) != 1 {
		t.Fatalf("provider cancels %v", c)
	}
	if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.conversion.reverted'`, tenant); n != 1 {
		t.Fatalf("audit %d", n)
	}
	for _, k := range e.rp.keys {
		if k != "revert:"+tenant+":"+sub {
			t.Fatalf("RP-4 key %s", k)
		}
	}
	_, err = e.svc.RevertConversion(ctx, operator, tenant, req)
	isCode(t, "repeat after completion", err, domain.ErrSubscriptionNotActive)
}

// RP-4 answered 200 but Billing's side failed (provider cancel, then the
// refund): the request is marked, so RP-4 is never called again (RP would
// now refuse it: past the window) and the request is never dropped. The
// saga worker completes it; a refund the provider never issued within the
// key window does not block the reversal and pages for a manual refund.
func TestReversalCompletesAfterRP4WithoutCallingItAgain(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	exec(t, `UPDATE subscriptions SET converted_at = $2 WHERE billing_subscription_id = $1::uuid`, sub, e.now)
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1::uuid`, tenant)
	e.mirror(t, tenant, sub, "in_conv_"+sub, "subscription_create", "paid", 20700)
	saga := scalar[string](t, `SELECT saga_id::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub)
	exec(t, `UPDATE conversion_sagas SET state = 'confirmed', confirmed_at = now() WHERE saga_id = $1`, saga)
	req := app.ReversalRequest{Refund: true, Reason: "Early fraud warning on conversion payment"}

	e.p.on("Refund", answer{err: errProvider})
	_, err := e.svc.RevertConversion(ctx, operator, tenant, req) // RP-4 200, refund fails
	isCode(t, "refund", err, domain.ErrProviderUnavailable)
	e.rp.reverts = []domain.RP3Answer{{Status: 409, ErrorCode: "revert_window_expired"}}
	e.p.on("Refund", answer{err: errProvider})
	e.now = e.now.Add(10 * time.Minute)
	if _, err := e.svc.RunSagas(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT state::text||'/'||last_error_code FROM conversion_sagas WHERE saga_id = $1`, saga); got != "reverting/completion_failed" {
		t.Fatalf("after a failed completion: %s", got)
	}
	// Past the provider key window the unissued refund no longer blocks.
	e.now = e.now.Add(24 * time.Hour)
	if _, err := e.svc.RunSagas(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||g.state::text FROM subscriptions s JOIN conversion_sagas g USING (billing_subscription_id)
		WHERE s.billing_subscription_id = $1`, sub); got != "ended/reverted" {
		t.Fatalf("state %s", got)
	}
	if len(e.rp.reverts) != 1 {
		t.Fatal("RP-4 called again after its 200")
	}
	if !e.log.has("ERROR", "SEV-2", "B-O3", tenant) {
		t.Fatalf("unissued refund not paged: %v", e.log.lines)
	}
}

// A reversal completed without an operator (empty caller → commercial-system's
// sentinel) is audited as the service account, not as a user with the
// sentinel id (Audit Log §10.3: non-IAM producers use {service_account, …00c1}).
func TestReversalWithoutOperatorAuditedAsServiceAccount(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	exec(t, `UPDATE subscriptions SET converted_at = $2 WHERE billing_subscription_id = $1::uuid`, sub, e.now)
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1::uuid`, tenant)
	exec(t, `UPDATE conversion_sagas SET state = 'confirmed', confirmed_at = now() WHERE billing_subscription_id = $1::uuid`, sub)
	if _, err := e.svc.RevertConversion(ctx, app.Caller{}, tenant, app.ReversalRequest{Reason: "Early fraud warning on conversion payment"}); err != nil {
		t.Fatal(err)
	}
	got := scalar[string](t, `SELECT entry->'actor'->>'type'||'/'||(entry->'actor'->>'id') FROM pending_audit_entries
		WHERE tenant_id = $1 AND entry->>'entry_type' = 'billing.conversion.reverted'`, tenant)
	if got != "service_account/"+domain.CommercialSystemPrincipal {
		t.Fatalf("actor %s", got)
	}
}

// B-O9 refund (§9.3.1): refusals; the saga is refund_pending from the first
// commit; a stub without a paid conversion invoice closes as refunded at
// once; with one, the refund is pending and the subscription ends
// (signup_failed).
func TestSagaRefundPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	act := app.SagaActionRequest{Action: "refund", Reason: "Signup could not complete"}
	tenant, sub := e.activeSubscription(t, "pro", 3)
	saga := scalar[string](t, `SELECT saga_id::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub)

	_, err := e.svc.SagaAction(ctx, operator, tenant, newTenant(), act)
	isCode(t, "unknown saga", err, domain.ErrSagaNotFound)
	e.fail("Sagas.Get", 0)
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isInjected(t, "saga read", err)
	e.fail("Subs.Get", 0)
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isInjected(t, "subscription read", err)
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, saga); got != "awaiting_tenant_converted" {
		t.Fatalf("saga after a rolled-back commit %s", got)
	}
	e.reg.hidden["stripe"] = true
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, saga); got != "refund_pending" {
		t.Fatalf("saga %s, want refund_pending from the first commit", got)
	}
	e.p.on("CancelNow", answer{err: errProvider})
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isCode(t, "cancel", err, domain.ErrProviderUnavailable)
	e.fail("Invoices.ConversionInvoice", 0)
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isInjected(t, "conversion invoice", err)
	e.fail("Subs.Latest", 0)
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isInjected(t, "saga close", err)
	v, err := e.svc.SagaAction(ctx, operator, tenant, saga, act)
	if err != nil || v.State != "refunded" {
		t.Fatalf("nothing to refund %+v %v", v, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "ended/signup_failed" {
		t.Fatalf("subscription %s", got)
	}
	if len(e.p.called("Refund ")) != 0 || len(e.p.called("CancelNow sub_"+sub+" cancel:"+sub+":signup_refund")) != 3 {
		t.Fatalf("provider %v %v", e.p.called("Refund "), e.p.called("CancelNow"))
	}
	_, err = e.svc.SagaAction(ctx, operator, tenant, saga, act)
	isCode(t, "closed saga", err, domain.ErrSagaAlreadyConfirmed)

	// With a paid conversion invoice.
	t2, s2 := e.activeSubscription(t, "pro", 3)
	saga2 := scalar[string](t, `SELECT saga_id::text FROM conversion_sagas WHERE billing_subscription_id = $1`, s2)
	e.mirror(t, t2, s2, "in_conv_"+s2, "subscription_create", "paid", 20700)
	e.p.on("Refund", answer{err: errProvider})
	_, err = e.svc.SagaAction(ctx, operator, t2, saga2, act)
	isCode(t, "refund", err, domain.ErrProviderUnavailable)
	e.fail("Subs.Get", 1)
	_, err = e.svc.SagaAction(ctx, operator, t2, saga2, act)
	isInjected(t, "final read", err)
	e.fail("Subs.Transition", 0)
	_, err = e.svc.SagaAction(ctx, operator, t2, saga2, act)
	isInjected(t, "end signup", err)
	v, err = e.svc.SagaAction(ctx, operator, t2, saga2, act)
	if err != nil || v.State != "refund_pending" {
		t.Fatalf("refund %+v %v", v, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE billing_subscription_id = $1`, s2); got != "ended/signup_failed" {
		t.Fatalf("subscription %s", got)
	}
	if n := len(e.p.called("Refund in_conv_" + s2 + " 20700 " + string(port.RefundReasonRequestedByCustomer))); n != 4 {
		t.Fatalf("refund calls %d, want 4", n)
	}
	exec(t, `UPDATE conversion_sagas SET state = 'confirmed', confirmed_at = now() WHERE saga_id = $1`, saga2)
	_, err = e.svc.SagaAction(ctx, operator, t2, saga2, act)
	isCode(t, "confirmed saga", err, domain.ErrSagaAlreadyConfirmed)
}
