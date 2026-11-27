//go:build integration

package commerce_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// §8.7 / BIL-ADD-2: a provider error after the add-on invoice exists keeps
// the invoice id on the purchase (so purchase-expiry can void or settle
// it); a declined charge whose invoice cannot be voided stays pending (the
// job voids it later) instead of failing with a collectible invoice.
func TestBIL_ADD_2_AddonInvoiceKeptOnErrorAndDeclineVoid(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t, "pro", 5)
	buy := func(key string) (app.PurchaseView, error) {
		return e.svc.PurchaseTokenAddon(ctx, withKey(owner, key), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	}
	row := func(key string) string {
		return scalar[string](t, `SELECT status::text||'/'||coalesce(provider_invoice_id,'')||'/'||purchase_id::text
			FROM quota_addon_purchases WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key)
	}

	e.p.on("ChargeOneOff", answer{err: errProvider, invoice: "in_after_error"})
	_, err := buy("k-rec-err-1")
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	if got := row("k-rec-err-1"); got[:len("pending_payment/in_after_error/")] != "pending_payment/in_after_error/" {
		t.Fatalf("invoice id lost on a provider error: %s", got)
	}

	e.p.on("ChargeOneOff", answer{err: port.ErrDeclined{Code: "card_declined"}, invoice: "in_declined"})
	e.p.on("VoidInvoice", answer{err: errProvider})
	_, err = buy("k-rec-dcl-1")
	isCode(t, "declined", err, domain.ErrPaymentDeclined)
	if got := row("k-rec-dcl-1"); got[:len("pending_payment/in_declined/")] != "pending_payment/in_declined/" {
		t.Fatalf("a declined purchase whose invoice is still open must stay pending: %s", got)
	}
}

// §8.7 / BIL-ADD-2: invoice.paid for an add-on invoice Billing has no id
// for confirms the purchase its metadata names (once), recording the
// invoice id; metadata naming another tenant's purchase or no purchase is
// not matched.
func TestBIL_ADD_2_CallbackMatchesInvoiceMetadata(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.converted(t, "pro", 5)
	e.p.on("ChargeOneOff", answer{err: errProvider})
	_, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-meta-01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	id := scalar[string](t, `SELECT purchase_id::text FROM quota_addon_purchases WHERE idempotency_key = 'k-meta-01'`)
	exec(t, `UPDATE quota_addon_purchases SET provider_invoice_id = NULL WHERE purchase_id = $1`, id)

	h := app.AddonInvoicePaid{S: e.svc}
	ev := callback("evt_meta_paid", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_lost"}, Status: "paid",
		Metadata: map[string]string{"purpose": "token_addon", "purchase_id": id, "tenant_id": tenant}, Fields: paidFields(t, "quota_addon_purchases", id)}
	other, _ := e.converted(t, "pro", 5)
	_, err = e.handle(other, h, ev, cur)
	ignored(t, "another tenant's purchase", err, "no_handler")
	bad := cur
	bad.Metadata = map[string]string{"purpose": "token_addon", "purchase_id": "not-a-uuid"}
	_, err = e.handle(tenant, h, ev, bad)
	ignored(t, "malformed purchase id", err, "no_handler")

	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:addon_purchase" {
		t.Fatalf("paid %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||provider_invoice_id FROM quota_addon_purchases WHERE purchase_id = $1`, id); got != "confirmed/in_lost" {
		t.Fatalf("purchase %s", got)
	}
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, "repeat", err, "purchase_confirmed")
	if addonEvents(t, id) != 1 {
		t.Fatal("TenantQuotaAddOnChanged not exactly once")
	}
}

// §8.5 / BIL-SUB-5 and §8.4.1: invoice.paid for a proration invoice Billing
// has no id for (the provider errored after the change) confirms the open
// seat purchase or pending upgrade on that subscription with the same
// amount; another amount is not matched.
func TestBIL_SUB_5_CallbackMatchesProrationInvoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.p.on("ChangeQuantity", answer{err: errProvider})
	_, err = e.svc.PurchaseSeats(ctx, withKey(owner, "k-pror-01"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)

	h := app.SeatInvoicePaid{S: e.svc}
	ev := callback("evt_pror_paid", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_lost_seat"}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "amount_due_minor": int64(4600),
			"amount_paid_minor": int64(4600), "currency": "USD"}}
	wrong := cur
	wrong.Fields = map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "amount_due_minor": int64(999), "currency": "USD"}
	_, err = e.handle(tenant, h, ev, wrong)
	ignored(t, "other amount", err, "no_handler")
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:seat_purchase" {
		t.Fatalf("paid %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||provider_invoice_id FROM seat_purchases WHERE idempotency_key = 'k-pror-01'`); got != "confirmed/in_lost_seat" {
		t.Fatalf("seat purchase %s", got)
	}
	if n := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); n != 12 {
		t.Fatalf("seats %d", n)
	}

	up, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if err != nil {
		t.Fatal(err)
	}
	e.p.on("ChangePrice", answer{err: errProvider})
	res, err := e.svc.ChangePlan(ctx, withKey(owner, "k-pror-02"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: version(t, sub), PreviewID: up.PreviewID})
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	upCur := cur
	upCur.Ref.ID = "in_lost_up"
	upCur.Fields = map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "amount_due_minor": int64(28400),
		"amount_paid_minor": int64(28400), "currency": "USD"}
	if out, err := e.handle(tenant, app.UpgradeInvoicePaid{S: e.svc}, callback("evt_pror_up", "invoice.paid", domain.KindInvoicePaid), upCur); err != nil || out != "applied:plan_upgrade" {
		t.Fatalf("upgrade paid %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||provider_invoice_id FROM plan_change_requests WHERE change_id = $1`, res.ChangeID); got != "applied/in_lost_up" {
		t.Fatalf("upgrade %s", got)
	}
}

// §9.2 / BIL-IDEM-1, BIL-IDEM-2: a retry with the same Idempotency-Key after
// a 5xx (the record was released) resumes the row the first attempt
// created, with the same provider key, instead of failing on uq_*_key; a
// completed row is answered without another provider call; another body
// under the key is 422 IDEMPOTENCY_KEY_REUSED.
func TestBIL_IDEM_1_RetryAfter5xxResumesRow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.converted(t, "pro", 5)

	// B-P20
	buy := func(qty int) (app.PurchaseView, error) {
		return e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-idem-add"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: qty, AcceptTerms: true})
	}
	e.p.on("ChargeOneOff", answer{err: errProvider})
	_, err := buy(1)
	isCode(t, "first attempt", err, domain.ErrProviderUnavailable)
	res, err := buy(1)
	if err != nil || res.Status != "confirmed" {
		t.Fatalf("resumed purchase %+v %v", res, err)
	}
	if res2, err := buy(1); err != nil || res2.PurchaseID != res.PurchaseID || res2.Status != "confirmed" {
		t.Fatalf("completed retry %+v %v", res2, err)
	}
	if n := len(e.p.called("ChargeOneOff price_tokens_1m x1 4900 addon:" + res.PurchaseID)); n != 2 {
		t.Fatalf("charges %v", e.p.called("ChargeOneOff"))
	}
	_, err = buy(2)
	isCode(t, "another body", err, domain.ErrIdempotencyKeyReused)
	if n := count(t, `SELECT count(*) FROM quota_addon_purchases WHERE idempotency_key = 'k-idem-add'`); n != 1 {
		t.Fatalf("rows %d", n)
	}

	// B-O2
	credit := app.AdjustmentRequest{Kind: "credit", Amount: app.Money{AmountMinor: 500, Currency: "USD"}, Description: "Service credit",
		Reason: "SLA credit approved by Finance"}
	e.p.on("CreditBalance", answer{err: errProvider})
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "k-idem-adj"), tenant, credit)
	isCode(t, "first adjustment", err, domain.ErrProviderUnavailable)
	adj, err := e.svc.IssueAdjustment(ctx, withKey(operator, "k-idem-adj"), tenant, credit)
	if err != nil || adj.ProviderRef != "cbtxn_adj:"+adj.AdjustmentID {
		t.Fatalf("resumed adjustment %+v %v", adj, err)
	}
	if again, err := e.svc.IssueAdjustment(ctx, withKey(operator, "k-idem-adj"), tenant, credit); err != nil || again.AdjustmentID != adj.AdjustmentID {
		t.Fatalf("completed retry %+v %v", again, err)
	}
	if n := len(e.p.called("CreditBalance -500 adj:" + adj.AdjustmentID)); n != 2 {
		t.Fatalf("credit calls %v", e.p.called("CreditBalance"))
	}
	debit := credit
	debit.Kind = "debit"
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "k-idem-adj"), tenant, debit)
	isCode(t, "another adjustment body", err, domain.ErrIdempotencyKeyReused)

	// B-O3
	paid := e.mirror(t, tenant, sub, "in_idem_"+sub, "subscription_create", "paid", 20700)
	e.p.on("Refund", answer{err: errProvider})
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "k-idem-ref"), tenant, refundReq(paid, 100, "billing_error"))
	isCode(t, "first refund", err, domain.ErrProviderUnavailable)
	ref, err := e.svc.IssueRefund(ctx, withKey(operator, "k-idem-ref"), tenant, refundReq(paid, 100, "billing_error"))
	if err != nil {
		t.Fatalf("resumed refund %v", err)
	}
	if again, err := e.svc.IssueRefund(ctx, withKey(operator, "k-idem-ref"), tenant, refundReq(paid, 100, "billing_error")); err != nil || again.RefundID != ref.RefundID {
		t.Fatalf("completed retry %+v %v", again, err)
	}
	if n := len(e.p.called("Refund in_idem_" + sub + " 100 requested_by_customer refund:" + ref.RefundID)); n != 2 {
		t.Fatalf("refund calls %v", e.p.called("Refund"))
	}
	if n := count(t, `SELECT count(*) FROM refunds WHERE idempotency_key = 'k-idem-ref'`); n != 1 {
		t.Fatalf("refund rows %d", n)
	}
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "k-idem-ref"), tenant, refundReq(paid, 200, "billing_error"))
	isCode(t, "another refund body", err, domain.ErrIdempotencyKeyReused)
}

// §9.2 / BIL-IDEM-2: a retry under the same Idempotency-Key resumes the
// row and replays its provider key, so every body field that feeds the
// provider call must match: an adjustment with another description, or a
// refund with another category (another provider refund reason), is 422
// IDEMPOTENCY_KEY_REUSED, never a provider call with other parameters
// under the same key (a provider idempotency mismatch).
func TestBIL_IDEM_2_ResumeComparesWholeBody(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.converted(t, "pro", 5)

	credit := app.AdjustmentRequest{Kind: "credit", Amount: app.Money{AmountMinor: 500, Currency: "USD"}, Description: "Service credit",
		Reason: "SLA credit approved by Finance"}
	e.p.on("CreditBalance", answer{err: errProvider})
	_, err := e.svc.IssueAdjustment(ctx, withKey(operator, "k-body-adj"), tenant, credit)
	isCode(t, "first adjustment", err, domain.ErrProviderUnavailable)
	other := credit
	other.Description = "Another description"
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "k-body-adj"), tenant, other)
	isCode(t, "another description", err, domain.ErrIdempotencyKeyReused)
	other = credit
	other.Reason = "Another reason, long enough"
	_, err = e.svc.IssueAdjustment(ctx, withKey(operator, "k-body-adj"), tenant, other)
	isCode(t, "another adjustment reason", err, domain.ErrIdempotencyKeyReused)
	if n := len(e.p.called("CreditBalance")); n != 1 {
		t.Fatalf("credit calls %v", e.p.called("CreditBalance"))
	}

	paid := e.mirror(t, tenant, sub, "in_body_"+sub, "subscription_create", "paid", 20700)
	e.p.on("Refund", answer{err: errProvider})
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "k-body-ref"), tenant, refundReq(paid, 100, "billing_error"))
	isCode(t, "first refund", err, domain.ErrProviderUnavailable)
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "k-body-ref"), tenant, refundReq(paid, 100, "duplicate_charge"))
	isCode(t, "another category", err, domain.ErrIdempotencyKeyReused)
	r := refundReq(paid, 100, "billing_error")
	r.Reason = "Another reason, long enough"
	_, err = e.svc.IssueRefund(ctx, withKey(operator, "k-body-ref"), tenant, r)
	isCode(t, "another refund reason", err, domain.ErrIdempotencyKeyReused)
	if n := len(e.p.called("Refund")); n != 1 {
		t.Fatalf("refund calls %v", e.p.called("Refund"))
	}
}

// BIL-PAY-7: invoice.paid grants a purchase only for an invoice paid for
// its amount and currency. A paid 0 add-on invoice (an empty draft the
// provider finalised), or a seat or upgrade invoice paid for another
// amount or currency, is parked failed:invoice_amount_mismatch (no retry,
// SEV-2 operator alert); nothing is granted and the purchase stays open.
func TestBIL_PAY_7_CallbackParksMismatchedPayment(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	ev := callback("evt_mismatch", "invoice.paid", domain.KindInvoicePaid)
	parkedAs := func(what string, err error) {
		t.Helper()
		var parked *port.ParkedEvent
		if !errors.As(err, &parked) || parked.Reason != "invoice_amount_mismatch" {
			t.Fatalf("%s: %v, want parked invoice_amount_mismatch", what, err)
		}
	}

	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, tenant)
	e.p.on("ChargeOneOff", answer{outcome: port.PaymentOutcome{Status: port.PaymentPending}, invoice: "in_zero"})
	add, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-mis-add"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	zero := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: scalar[string](t,
		`SELECT provider_invoice_id FROM quota_addon_purchases WHERE purchase_id = $1`, add.PurchaseID)}, Status: "paid",
		Fields: map[string]any{"amount_paid_minor": int64(0), "total_minor": int64(0), "currency": "USD"}}
	_, err = e.handle(tenant, app.AddonInvoicePaid{S: e.svc}, ev, zero)
	parkedAs("0 add-on invoice", err)
	if st := scalar[string](t, `SELECT status::text FROM quota_addon_purchases WHERE purchase_id = $1`, add.PurchaseID); st != "pending_payment" || addonEvents(t, add.PurchaseID) != 0 {
		t.Fatalf("add-on %s granted", st)
	}

	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentPending}})
	seat, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-mis-seat"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	if err != nil {
		t.Fatal(err)
	}
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_seatpur:" + seat.PurchaseID}, Status: "paid",
		Fields: paidFields(t, "seat_purchases", seat.PurchaseID, "subscription", "sub_"+sub, "billing_reason", "subscription_update", "currency", "EUR")}
	_, err = e.handle(tenant, app.SeatInvoicePaid{S: e.svc}, ev, cur)
	parkedAs("seat invoice in another currency", err)
	if quantity(t, sub) != 10 {
		t.Fatalf("seats granted: %d", quantity(t, sub))
	}
}

// BIL-PAY-6, callback side: a proration invoice already recorded on a plan
// change never confirms an open seat purchase Billing has no invoice id
// for, even with the same subscription and amount; nor does a proration
// invoice whose charged line is for another seat count.
func TestBIL_PAY_6_CallbackFallbackSkipsClaimedInvoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	up, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if err != nil {
		t.Fatal(err)
	}
	e.p.on("ChangePrice", answer{outcome: port.PaymentOutcome{Status: port.PaymentPending}})
	change, err := e.svc.ChangePlan(ctx, withKey(owner, "k-claim-up"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: version(t, sub), PreviewID: up.PreviewID})
	if err != nil || change.Status != "pending_payment" {
		t.Fatalf("upgrade %+v %v", change, err)
	}
	claimed := scalar[string](t, `SELECT provider_invoice_id FROM plan_change_requests WHERE change_id = $1`, change.ChangeID)

	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.p.on("ChangeQuantity", answer{err: errProvider})
	_, err = e.svc.PurchaseSeats(ctx, withKey(owner, "k-claim-seat"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	id := scalar[string](t, `SELECT purchase_id::text FROM seat_purchases WHERE idempotency_key = 'k-claim-seat'`)
	amount := scalar[int64](t, `SELECT amount_minor FROM seat_purchases WHERE purchase_id = $1`, id)

	h := app.SeatInvoicePaid{S: e.svc}
	ev := callback("evt_claimed", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: claimed}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "amount_due_minor": amount,
			"amount_paid_minor": amount, "currency": "USD"}}
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, "invoice recorded on a plan change", err, "no_handler")

	other := cur
	other.Ref.ID = "in_other_count"
	other.Fields = map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "amount_due_minor": amount,
		"amount_paid_minor": amount, "currency": "USD", "charged_line_quantities": []int64{11}, "charged_line_prices": []string{"price_pro"}}
	_, err = e.handle(tenant, h, ev, other)
	ignored(t, "charged line for another seat count", err, "no_handler")
	if got := scalar[string](t, `SELECT status::text FROM seat_purchases WHERE purchase_id = $1`, id); got != "pending_payment" || quantity(t, sub) != 10 {
		t.Fatalf("seat purchase %s, seats %d", got, quantity(t, sub))
	}

	mine := other
	mine.Ref.ID = "in_mine"
	mine.Fields = map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "amount_due_minor": amount,
		"amount_paid_minor": amount, "currency": "USD", "charged_line_quantities": []int64{12}, "charged_line_prices": []string{"price_pro"}}
	if out, err := e.handle(tenant, h, ev, mine); err != nil || out != "applied:seat_purchase" {
		t.Fatalf("own invoice %s %v", out, err)
	}
}
