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

// voidStale: an invoice already paid is left alone (its callback settles
// the late payment), and a purchase without an invoice voids nothing;
// both purchases expire.
func TestExpiryLeavesPaidOrMissingInvoice(t *testing.T) {
	e := newEnv(t)
	t1, _ := e.active("pro", 10)
	paid := e.pendingSeatPurchase(t1)
	t2, _ := e.active("pro", 10)
	none := e.pendingSeatPurchase(t2)
	e.exec(`UPDATE seat_purchases SET provider_invoice_id = NULL WHERE purchase_id = $1`, none)
	e.prov.invoiceStatus = "paid"
	e.later()
	if res := e.run(e.svc.ExpireStale); res.Handled != 2 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	for _, id := range []string{paid, none} {
		if st := scalar[string](t, seatStatus, id); st != "expired" {
			t.Fatalf("purchase %s: %s", id, st)
		}
	}
	if n := len(e.prov.called("VoidInvoice")); n != 0 {
		t.Fatalf("voided %v", e.prov.called("VoidInvoice"))
	}
	if n := len(e.prov.called("FetchCurrent in_seatpur:" + none)); n != 0 {
		t.Fatal("re-read an invoice that does not exist")
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
