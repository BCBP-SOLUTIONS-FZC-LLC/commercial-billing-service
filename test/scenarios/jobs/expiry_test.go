//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

var pending = port.PaymentOutcome{Status: port.PaymentPending}

// later moves the clock past PURCHASE_ABANDON_AFTER, SCA_ABANDON_AFTER and
// CHECKOUT_TTL.
func (e *env) later() { e.now = time.Now().UTC().Add(2 * time.Hour) }

// failExpiry runs purchase-expiry with one injected failure: the item is
// reported failed and logged; query must still return want.
func (e *env) failExpiry(query, id, want string) {
	e.t.Helper()
	res := e.run(e.svc.ExpireStale)
	if res.Failed != 1 || res.Handled != 0 {
		e.t.Fatalf("result %+v", res)
	}
	if got := scalar[string](e.t, query, id); got != want {
		e.t.Fatalf("after a failed run: %s, want %s", got, want)
	}
	if !e.log.has("WARN purchase-expiry: item not expired") {
		e.t.Fatal("failure not logged")
	}
	e.f.clear()
}

// pendingSeatPurchase leaves a seat purchase pending_payment with an open
// provider invoice (§8.5.1).
func (e *env) pendingSeatPurchase(tenant string) string {
	e.t.Helper()
	ctx := context.Background()
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		e.t.Fatal(err)
	}
	e.prov.change = []port.PaymentOutcome{pending}
	p, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-seat-"+tenant[:8]), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	if err != nil || p.Status != "pending_payment" {
		e.t.Fatalf("pending purchase %+v %v", p, err)
	}
	return p.PurchaseID
}

const seatStatus = `SELECT status::text FROM seat_purchases WHERE purchase_id = $1`

// §13.2 purchase-expiry, seat purchase: each failure (re-read, the
// subscription's provider, the invoice re-read and void, the final re-read,
// an unloaded adapter) leaves the purchase pending; the run then expires it
// and voids its open invoice.
func TestExpirySeatPurchaseRetriesEachFailure(t *testing.T) {
	e := newEnv(t)
	tenant, _ := e.active("pro", 10)
	id := e.pendingSeatPurchase(tenant)
	e.later()

	e.f.fail("Jobs.StaleWork", 0)
	e.runErr(e.svc.ExpireStale)
	e.f.clear()

	for _, step := range []struct {
		name string
		skip int
	}{
		{"SeatPurchases.Get", 0}, {"Subs.Get", 0}, {"prov.FetchCurrent", 0}, {"prov.VoidInvoice", 0}, {"SeatPurchases.Get", 1},
	} {
		e.f.fail(step.name, step.skip)
		e.failExpiry(seatStatus, id, "pending_payment")
		if t.Failed() {
			t.Fatalf("step %s", step.name)
		}
	}
	stripe := e.svc.Providers
	e.unload()
	e.failExpiry(seatStatus, id, "pending_payment")
	e.svc.Providers = stripe

	e.run(e.svc.ExpireStale)
	if st := scalar[string](t, seatStatus, id); st != "expired" {
		t.Fatalf("purchase %s", st)
	}
	if n := len(e.prov.called("VoidInvoice in_seatpur:" + id + " seatpur:" + id + ":expire")); n < 1 {
		t.Fatalf("void %v", e.prov.called("VoidInvoice"))
	}
}

const addonStatus = `SELECT status::text FROM quota_addon_purchases WHERE purchase_id = $1`

// §8.7 / §13.2: an abandoned token add-on purchase expires (open invoice
// voided); each failure on the way is retried.
func TestExpiryAddonPurchaseRetriesEachFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.active("pro", 5)
	e.exec(`UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, tenant)
	e.prov.change = []port.PaymentOutcome{pending}
	res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-addon-exp"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	if err != nil || res.Status != "pending_payment" {
		t.Fatalf("pending add-on %+v %v", res, err)
	}
	id := res.PurchaseID
	e.later()
	for _, step := range []struct {
		name string
		skip int
	}{
		{"Addons.Get", 0}, {"Accounts.Get", 0}, {"prov.VoidInvoice", 0}, {"Addons.Get", 1},
	} {
		e.f.fail(step.name, step.skip)
		e.failExpiry(addonStatus, id, "pending_payment")
		if t.Failed() {
			t.Fatalf("step %s", step.name)
		}
	}
	e.run(e.svc.ExpireStale)
	if st := scalar[string](t, addonStatus, id); st != "expired" {
		t.Fatalf("add-on %s", st)
	}
	if n := len(e.prov.called("VoidInvoice in_addon:" + id + " addon:" + id + ":expire")); n < 1 {
		t.Fatalf("void %v", e.prov.called("VoidInvoice"))
	}
}

const changeStatus = `SELECT status::text FROM plan_change_requests WHERE change_id = $1`

// §8.4.1 / §13.2: an upgrade whose payment was abandoned fails (open
// invoice voided); each failure on the way is retried.
func TestExpiryUpgradeRetriesEachFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.active("pro", 5)
	pv, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if err != nil {
		t.Fatal(err)
	}
	e.prov.change = []port.PaymentOutcome{pending}
	res, err := e.svc.ChangePlan(ctx, withKey(owner, "k-up-exp"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: e.version(sub), PreviewID: pv.PreviewID})
	if err != nil || res.Status != "pending_payment" {
		t.Fatalf("pending upgrade %+v %v", res, err)
	}
	id := res.ChangeID
	e.later()
	for _, step := range []struct {
		name string
		skip int
	}{
		{"Changes.Open", 0}, {"Subs.Get", 0}, {"prov.VoidInvoice", 0}, {"Changes.Open", 1},
	} {
		e.f.fail(step.name, step.skip)
		e.failExpiry(changeStatus, id, "pending_payment")
		if t.Failed() {
			t.Fatalf("step %s", step.name)
		}
	}
	e.run(e.svc.ExpireStale)
	if st := scalar[string](t, changeStatus, id); st != "failed" {
		t.Fatalf("upgrade %s", st)
	}
	if n := len(e.prov.called("VoidInvoice in_planchg:" + id)); n < 1 {
		t.Fatalf("void %v", e.prov.called("VoidInvoice"))
	}
}

const sessionStatus = `SELECT status FROM checkout_sessions WHERE session_id = $1`

// §8.1.3 without a callback: an expired checkout session is marked expired
// and its incomplete subscription ended; each failure is retried. A
// payment-method setup session (no subscription) just expires.
func TestExpiryCheckoutSessions(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	trial := newTenant()
	if _, err := e.svc.StartCheckout(ctx, owner, trial, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2}); err != nil {
		t.Fatal(err)
	}
	session := scalar[string](t, `SELECT session_id::text FROM checkout_sessions WHERE tenant_id = $1`, trial)
	e.later()
	for _, name := range []string{"Checkouts.GetSession", "Checkouts.SetStatus", "Subs.Get"} {
		e.f.fail(name, 0)
		e.failExpiry(sessionStatus, session, "open")
		if t.Failed() {
			t.Fatalf("step %s", name)
		}
	}
	e.run(e.svc.ExpireStale)
	if got := scalar[string](t, `SELECT c.status||'/'||s.status::text FROM checkout_sessions c JOIN subscriptions s USING (billing_subscription_id)
		WHERE c.session_id = $1`, session); got != "expired/ended" {
		t.Fatalf("checkout %s", got)
	}

	tenant, _ := e.active("pro", 3)
	e.now = time.Now().UTC()
	if _, err := e.svc.CreateSetupSession(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	setup := scalar[string](t, `SELECT session_id::text FROM checkout_sessions WHERE tenant_id = $1 AND billing_subscription_id IS NULL`, tenant)
	e.later()
	e.run(e.svc.ExpireStale)
	if st := scalar[string](t, sessionStatus, setup); st != "expired" {
		t.Fatalf("setup session %s", st)
	}
}

const subStatus = `SELECT status::text||'/'||coalesce(cancel_reason::text,'-') FROM subscriptions WHERE billing_subscription_id = $1`

// §8.3 B-I1: a direct signup whose 3-D Secure was abandoned is ended
// (signup_failed) and the provider subscription cancelled; each failure
// is retried.
func TestExpiryAbandonedDirectSignup(t *testing.T) {
	e := newEnv(t)
	e.fakes.Stripe.Confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi_secret"}}
	tenant := newTenant()
	_, err := e.svc.AuthorizeAndCharge(context.Background(), app.Caller{Principal: "bff"}, app.AuthorizeChargeRequest{TenantID: tenant,
		PlanCode: "pro", LicensedSeats: 3, PaymentMethodRef: "pm_1"})
	if !domain.HasCode(err, domain.ErrPaymentActionRequired) {
		t.Fatalf("3-D Secure: %v", err)
	}
	sub := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, tenant)
	if got := scalar[string](t, subStatus, sub); got != "incomplete/-" {
		t.Fatalf("signup %s", got)
	}
	e.later()
	e.f.fail("Subs.Get", 0)
	e.failExpiry(subStatus, sub, "incomplete/-")
	e.f.fail("prov.CancelNow", 0)
	e.failExpiry(subStatus, sub, "incomplete/-")
	stripe := e.svc.Providers
	e.unload()
	e.failExpiry(subStatus, sub, "incomplete/-")
	e.svc.Providers = stripe

	if r := e.run(e.svc.ExpireStale); r.Handled != 1 {
		t.Fatalf("result %+v", r)
	}
	if got := scalar[string](t, subStatus, sub); got != "ended/signup_failed" {
		t.Fatalf("signup %s", got)
	}
	if n := len(e.prov.called("CancelNow sub_" + sub + " cancel:" + sub + ":1")); n < 1 {
		t.Fatalf("cancel %v", e.prov.called("CancelNow"))
	}
}

// §8.5.1 / BIL-SUB-5: purchase-expiry never expires a paid purchase. An
// invoice already paid (its callback lost or not yet processed) settles the
// purchase as the callback would: seat purchase confirmed with the seats
// and TenantSeatsChanged, add-on confirmed with TenantQuotaAddOnChanged,
// upgrade applied with TenantPlanChanged; nothing is voided. A purchase
// with no invoice at the provider (looked up, none found) expires without
// a re-read.
func TestBIL_SUB_5_ExpirySettlesPaidPurchases(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	t1, s1 := e.active("pro", 10)
	paid := e.pendingSeatPurchase(t1)
	t2, _ := e.active("pro", 10)
	none := e.pendingSeatPurchase(t2)
	e.exec(`UPDATE seat_purchases SET provider_invoice_id = NULL WHERE purchase_id = $1`, none)

	t3, _ := e.active("pro", 5)
	e.exec(`UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, t3)
	e.prov.change = []port.PaymentOutcome{pending}
	add, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-addon-paid"), t3, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
	if err != nil || add.Status != "pending_payment" {
		t.Fatalf("pending add-on %+v %v", add, err)
	}

	t4, s4 := e.active("pro", 5)
	pv, err := e.svc.PreviewPlanChange(ctx, owner, t4, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if err != nil {
		t.Fatal(err)
	}
	e.prov.change = []port.PaymentOutcome{pending}
	up, err := e.svc.ChangePlan(ctx, withKey(owner, "k-up-paid"), t4, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: e.version(s4), PreviewID: pv.PreviewID})
	if err != nil || up.Status != "pending_payment" {
		t.Fatalf("pending upgrade %+v %v", up, err)
	}

	e.prov.invoiceStatus = "paid"
	e.later()
	if res := e.run(e.svc.ExpireStale); res.Handled != 4 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if st := scalar[string](t, seatStatus, paid); st != "confirmed" {
		t.Fatalf("paid seat purchase %s, want confirmed (never expired)", st)
	}
	if n := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, s1); n != 12 {
		t.Fatalf("seats %d", n)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'
		AND payload->'data'->>'billing_ref' = $2`, t1, paid); n != 1 {
		t.Fatalf("TenantSeatsChanged %d", n)
	}
	if st := scalar[string](t, seatStatus, none); st != "expired" {
		t.Fatalf("purchase without an invoice %s", st)
	}
	if st := scalar[string](t, addonStatus, add.PurchaseID); st != "confirmed" {
		t.Fatalf("paid add-on %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE event_type = 'TenantQuotaAddOnChanged' AND payload->'data'->>'billing_ref' = $1`, add.PurchaseID); n != 1 {
		t.Fatalf("TenantQuotaAddOnChanged %d", n)
	}
	if st := scalar[string](t, changeStatus, up.ChangeID); st != "applied" {
		t.Fatalf("paid upgrade %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantPlanChanged'`, t4); n != 1 {
		t.Fatalf("TenantPlanChanged %d", n)
	}
	if n := len(e.prov.called("VoidInvoice")); n != 0 {
		t.Fatalf("voided %v", e.prov.called("VoidInvoice"))
	}
	if n := len(e.prov.called("FetchCurrent in_seatpur:" + none)); n != 0 {
		t.Fatal("re-read an invoice that does not exist")
	}
	if n := len(e.fakes.Stripe.Calls("FindInvoice sub_")); n != 1 {
		t.Fatalf("lookup %v", e.fakes.Stripe.Calls("FindInvoice"))
	}
}

// §8.7 / BIL-ADD-2: a purchase whose invoice id Billing did not record (a
// provider error after the charge) is looked up at the provider before it
// expires: an open invoice found is voided and recorded; a paid one settles
// the purchase.
func TestBIL_ADD_2_ExpiryLooksUpUnrecordedInvoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.active("pro", 5)
	e.exec(`UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, tenant)
	buy := func(key string) string {
		e.prov.change = []port.PaymentOutcome{pending}
		res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, key), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE quota_addon_purchases SET provider_invoice_id = NULL WHERE purchase_id = $1`, res.PurchaseID)
		return res.PurchaseID
	}
	open := buy("k-look-open")
	e.fakes.Stripe.Found = "in_found_open"
	e.later()
	e.run(e.svc.ExpireStale)
	if got := scalar[string](t, `SELECT status::text||'/'||provider_invoice_id FROM quota_addon_purchases WHERE purchase_id = $1`, open); got != "expired/in_found_open" {
		t.Fatalf("open invoice found: %s", got)
	}
	if n := len(e.prov.called("VoidInvoice in_found_open addon:" + open + ":expire")); n != 1 {
		t.Fatalf("void %v", e.prov.called("VoidInvoice"))
	}
	if n := len(e.fakes.Stripe.Calls("FindInvoice " + open)); n != 1 {
		t.Fatalf("lookup %v", e.fakes.Stripe.Calls("FindInvoice"))
	}

	e.now = time.Now().UTC()
	paid := buy("k-look-paid")
	e.fakes.Stripe.Found, e.prov.invoiceStatus = "in_found_paid", "paid"
	e.prov.invoicePaid = map[string]int64{"in_found_paid": scalar[int64](t, `SELECT amount_minor FROM quota_addon_purchases WHERE purchase_id = $1`, paid)}
	e.later()
	e.run(e.svc.ExpireStale)
	if got := scalar[string](t, `SELECT status::text||'/'||provider_invoice_id FROM quota_addon_purchases WHERE purchase_id = $1`, paid); got != "confirmed/in_found_paid" {
		t.Fatalf("paid invoice found: %s", got)
	}
}

// BIL-PAY-6: an invoice already recorded on another purchase never settles
// this one. P1 bought seats and was confirmed with its proration invoice;
// P2's change failed before the provider updated anything, so P2 has no
// invoice and the lookup returns P1's paid invoice (same subscription and
// amount): P2 expires, no seats are granted, P1's invoice is neither
// re-read nor voided for P2.
func TestBIL_PAY_6_ExpiryRejectsAnotherPurchasesInvoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.active("pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-pay6-p1"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	if err != nil || p1.Status != "confirmed" {
		t.Fatalf("P1 %+v %v", p1, err)
	}
	inv1 := "in_seatpur:" + p1.PurchaseID
	p2 := e.pendingSeatPurchase(tenant)
	e.exec(`UPDATE seat_purchases SET provider_invoice_id = NULL WHERE purchase_id = $1`, p2)

	e.fakes.Stripe.Found, e.prov.invoiceStatus = inv1, "paid"
	e.prov.invoicePaid = map[string]int64{inv1: scalar[int64](t, `SELECT amount_minor FROM seat_purchases WHERE purchase_id = $1`, p2)}
	e.later()
	if res := e.run(e.svc.ExpireStale); res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||coalesce(provider_invoice_id, '') FROM seat_purchases WHERE purchase_id = $1`, p2); got != "expired/" {
		t.Fatalf("P2 %s, want expired without P1's invoice", got)
	}
	if n := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); n != 12 {
		t.Fatalf("seats %d, want 12 (P1 only)", n)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'
		AND payload->'data'->>'billing_ref' = $2`, tenant, p2); n != 0 {
		t.Fatalf("TenantSeatsChanged for P2: %d", n)
	}
	if n := len(e.prov.called("FetchCurrent " + inv1)); n != 0 {
		t.Fatalf("P1's invoice re-read for P2 %v", e.prov.called("FetchCurrent"))
	}
	if n := len(e.prov.called("VoidInvoice")); n != 0 {
		t.Fatalf("voided %v", e.prov.called("VoidInvoice"))
	}
}

// BIL-PAY-7: purchase-expiry settles a purchase only with an invoice paid
// for its amount and currency. A token add-on whose invoice was paid for 0
// (an empty draft the provider finalised) is not granted: it expires with
// the SEV-2 operator alert and no TenantQuotaAddOnChanged. A draft invoice
// is deleted (the provider voids only finalised invoices), never voided.
func TestBIL_PAY_7_ExpiryNeverSettlesAMismatchedInvoice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.active("pro", 5)
	e.exec(`UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1`, tenant)
	buy := func(key string) string {
		e.prov.change = []port.PaymentOutcome{pending}
		res, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, key), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true})
		if err != nil || res.Status != "pending_payment" {
			t.Fatalf("pending add-on %+v %v", res, err)
		}
		return res.PurchaseID
	}
	zero := buy("k-pay7-zero")
	e.prov.invoiceStatus = "paid"
	e.prov.invoicePaid = map[string]int64{"in_addon:" + zero: 0}
	e.later()
	e.run(e.svc.ExpireStale)
	if st := scalar[string](t, addonStatus, zero); st != "expired" {
		t.Fatalf("0 invoice: add-on %s, want expired", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE event_type = 'TenantQuotaAddOnChanged' AND payload->'data'->>'billing_ref' = $1`, zero); n != 0 {
		t.Fatalf("tokens granted for a 0 invoice: %d events", n)
	}
	if !e.log.has("ERROR provider payment does not match the purchase") {
		t.Fatal("no SEV-2 operator alert")
	}

	e.now = time.Now().UTC()
	draft := buy("k-pay7-draft")
	e.prov.invoiceStatus = "draft"
	e.later()
	e.run(e.svc.ExpireStale)
	if st := scalar[string](t, addonStatus, draft); st != "expired" {
		t.Fatalf("draft: add-on %s", st)
	}
	if n := len(e.prov.called("DeleteDraftInvoice in_addon:" + draft)); n != 1 {
		t.Fatalf("draft deletes %v", e.prov.called("DeleteDraftInvoice"))
	}
	if n := len(e.prov.called("VoidInvoice in_addon:" + draft)); n != 0 {
		t.Fatalf("draft voided %v", e.prov.called("VoidInvoice"))
	}
}

// §8.1.3 / BIL-CONC-2: purchase-expiry expires a stale checkout session at
// the provider first, outside the transaction. A session the customer
// completed just before expires_at (its callbacks still queued) stays open
// for checkout.session.completed and invoice.paid; a provider failure
// leaves it open for the next run; otherwise it is expired and its
// incomplete subscription ended.
func TestBIL_CONC_2_ExpiryExpiresCheckoutAtProviderFirst(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	trial := newTenant()
	res, err := e.svc.StartCheckout(ctx, owner, trial, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	session := scalar[string](t, `SELECT session_id::text FROM checkout_sessions WHERE tenant_id = $1`, trial)
	want := "ExpireCheckout cs_" + res.BillingSubscriptionID + " bsub:" + res.BillingSubscriptionID + ":expire"
	e.later()

	e.prov.expireErr = port.ErrCheckoutCompleted
	if r := e.run(e.svc.ExpireStale); r.Failed != 0 {
		t.Fatalf("result %+v", r)
	}
	if got := scalar[string](t, sessionStatus, session); got != "open" {
		t.Fatalf("completed at the provider: session %s, want open", got)
	}
	if got := scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, res.BillingSubscriptionID); got != "incomplete" {
		t.Fatalf("subscription %s", got)
	}
	e.prov.expireErr = nil

	e.f.fail("prov.ExpireCheckout", 0)
	e.failExpiry(sessionStatus, session, "open")

	e.run(e.svc.ExpireStale)
	if got := scalar[string](t, sessionStatus, session); got != "expired" {
		t.Fatalf("session %s", got)
	}
	if got := e.prov.called("ExpireCheckout"); len(got) == 0 || got[len(got)-1] != want {
		t.Fatalf("expire calls %v, want %q", got, want)
	}
}
