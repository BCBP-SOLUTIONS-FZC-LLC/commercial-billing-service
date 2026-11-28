//go:build integration

package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func chargeReq(tenant string) app.AuthorizeChargeRequest {
	return app.AuthorizeChargeRequest{TenantID: tenant, PlanCode: "pro", LicensedSeats: 4, PaymentMethodRef: "pm_1"}
}

func subStatus(t *testing.T, tenant string) string {
	t.Helper()
	return scalar[string](t, `SELECT coalesce(string_agg(status::text||'/'||coalesce(cancel_reason::text,'')||'/'||coalesce(provider_subscription_id,''), ',' ORDER BY created_at), '')
		FROM subscriptions WHERE tenant_id = $1`, tenant)
}

// B-I1 (§5.4.2, §8.3) failures before the provider subscription exists:
// nothing is left that the BFF's retry would trip over.
func TestDirectSignupGuardsAndEarlyFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	t.Run("new-account provider not enabled", func(t *testing.T) {
		tenant := e.tenant()
		e.svc.Settings.NewAccountProvider = "paypal"
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
		e.svc.Settings.NewAccountProvider = "stripe"
		wantCode(t, err, domain.ErrProviderNotEnabled)
	})
	t.Run("provider cannot charge off-session", func(t *testing.T) {
		tenant := e.tenant()
		e.svc.Settings.NewAccountProvider = "manual"
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
		e.svc.Settings.NewAccountProvider = "stripe"
		wantCode(t, err, domain.ErrProviderOperationUnsupported)
		if subStatus(t, tenant) != "" {
			t.Fatal("subscription written")
		}
	})
	t.Run("no price for the currency", func(t *testing.T) {
		tenant := e.tenant()
		r := chargeReq(tenant)
		r.Currency = "eur"
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, r)
		wantCode(t, err, domain.ErrPriceNotAvailable)
		if n := count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant); n != 0 {
			t.Fatal("account committed without a subscription")
		}
	})
	for _, c := range []struct {
		name string
		skip int
	}{{"Accounts.Get", 0}, {"Provider.EnsureCustomer", 0}, {"Accounts.Create", 0}, {"Prices.PriceAt", 0}, {"Subs.Live", 1}, {"Subs.Create", 0}} {
		t.Run(c.name+" fails", func(t *testing.T) {
			tenant := e.tenant()
			e.f.on(c.name, c.skip)
			_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
			if c.name == "Provider.EnsureCustomer" {
				wantCode(t, err, domain.ErrInternal)
			} else {
				injected(t, err, c.name)
			}
			if subStatus(t, tenant) != "" || len(e.stripe.calls("CreateSubscription bsub:")) != 0 {
				t.Fatal("partial signup")
			}
		})
	}
}

// A provider failure after the account and incomplete subscription were
// committed: the BFF's retry continues the same subscription with the same
// provider keys (BIL-IDEM-2) and never creates a second one.
func TestDirectSignupProviderFailuresThenRetry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	for _, name := range []string{"Provider.AttachDefaultPaymentMethod", "Provider.CreateSubscription", "Subs.Update", "Provider.FetchCurrent"} {
		e.f.on(name, 0)
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
		if errors.Is(err, errInjected) {
			injected(t, err, name)
		} else {
			wantCode(t, err, domain.ErrInternal)
		}
	}
	sub := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, tenant)
	// Paid answer not yet settled: 503 with the same key, still incomplete.
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentPending}}
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	wantCode(t, err, domain.ErrProviderUnavailable)
	if got := subStatus(t, tenant); got != "incomplete//sub_"+sub {
		t.Fatalf("after pending %s", got)
	}
	// Failures inside transaction 2 roll back: still incomplete, no saga.
	for _, name := range []string{"Subs.Get", "Subs.Transition", "Subs.AppendHistory", "Sagas.BySubscription", "Sagas.Insert"} {
		e.f.on(name, 0)
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
		injected(t, err, name)
		if got := subStatus(t, tenant); got != "incomplete//sub_"+sub || count(t, `SELECT count(*) FROM conversion_sagas WHERE tenant_id = $1`, tenant) != 0 {
			t.Fatalf("%s: %s", name, got)
		}
	}
	res, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	if err != nil || res.BillingSubscriptionID != sub || res.Status != "active" || !res.CurrentPeriodEnd.Equal(e.now.Truncate(time.Second).AddDate(0, 1, 0)) {
		t.Fatalf("retry: %+v %v", res, err)
	}
	for prefix, want := range map[string]int{"EnsureCustomer bsub:" + sub + ":customer": 1, "CreateSubscription bsub:" + sub + ":create": 3,
		"AttachDefaultPaymentMethod pm_1 bsub:" + sub + ":pm": 4, "ConfirmInitialPayment bsub:" + sub + ":confirm": 8} {
		if got := len(e.stripe.calls(prefix)); got != want {
			t.Errorf("%s: %d calls, want %d", prefix, got, want)
		}
	}
	if got := scalar[string](t, `SELECT kind::text||'/'||state::text FROM conversion_sagas WHERE tenant_id = $1`, tenant); got != "direct_signup/awaiting_tenant_converted" {
		t.Fatalf("saga %s", got)
	}
}

// Outcomes the provider reports as a status rather than an error: 3-D
// Secure (402 with the client secret) and a decline with an unmapped code
// (decline_reason "other", §5.4.2 enum); a decline whose provider cancel or
// Billing write fails is retried, and the retry ends the subscription.
func TestDirectSignupOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	tenant := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi_sec_9"}}
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.ErrPaymentActionRequired || de.Extensions["action"].(map[string]string)["client_secret"] != "pi_sec_9" {
		t.Fatalf("3ds: %v", err)
	}
	// The retry reads the item of the pending subscription.
	e.f.on("Subs.Item", 0)
	_, err = e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	injected(t, err, "Subs.Item")

	t2 := e.tenant()
	for _, name := range []string{"Provider.CancelNow", "Subs.Get", "Subs.Transition"} {
		e.f.on(name, 0)
		e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentDeclined, DeclineCode: "Lost_Card"}}
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(t2))
		if name == "Provider.CancelNow" {
			wantCode(t, err, domain.ErrInternal)
		} else {
			injected(t, err, name)
		}
		if got := subStatus(t, t2); !strings.HasPrefix(got, "incomplete/") {
			t.Fatalf("%s: %s", name, got)
		}
	}
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentDeclined, DeclineCode: "Lost_Card"}}
	_, err = e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(t2))
	if !errors.As(err, &de) || de.Code != domain.ErrPaymentDeclined || de.Extensions["decline_reason"] != "other" {
		t.Fatalf("declined: %v", err)
	}
	sub := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, t2)
	if got := subStatus(t, t2); got != "ended/signup_failed/sub_"+sub {
		t.Fatalf("after decline %s", got)
	}
	if got := e.stripe.calls("CancelNow sub_" + sub + " cancel:" + sub + ":1"); len(got) != 4 {
		t.Fatalf("cancel calls %v", got)
	}

	// The account stays (USD): a new signup in another currency is refused.
	r := chargeReq(t2)
	r.Currency = "AED"
	_, err = e.svc.AuthorizeAndCharge(ctx, bff, r)
	wantCode(t, err, domain.ErrCurrencyMismatch)
}

// 410 CHARGE_EXPIRED (§8.3): 3-D Secure abandoned past SCA_ABANDON_AFTER;
// a failed provider cancel or Billing write leaves the subscription for the
// next retry, which ends it.
func TestDirectSignupExpiry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi_x"}}
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant)); !domain.HasCode(err, domain.ErrPaymentActionRequired) {
		t.Fatal(err)
	}
	sub := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, tenant)
	e.now = e.now.Add(31 * time.Minute)
	e.stripe.subStatus = "incomplete" // 3-D Secure abandoned: the first invoice is unpaid
	e.f.on("Provider.FetchCurrent", 0)
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	wantCode(t, err, domain.ErrInternal) // the expiry re-read failed: retried, nothing cancelled
	if status(t, sub) != "incomplete" {
		t.Fatal("cancelled without reading the payment")
	}
	for _, name := range []string{"Provider.CancelNow", "Subs.Get", "Subs.Transition"} {
		e.f.on(name, 0)
		_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
		if name == "Provider.CancelNow" {
			wantCode(t, err, domain.ErrInternal)
		} else {
			injected(t, err, name)
		}
		if status(t, sub) != "incomplete" {
			t.Fatalf("%s: committed", name)
		}
	}
	_, err = e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	wantCode(t, err, domain.ErrChargeExpired)
	if got := subStatus(t, tenant); got != "ended/signup_failed/sub_"+sub {
		t.Fatalf("expired %s", got)
	}
	if n := count(t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1 AND to_status = 'ended' AND cause = 'api:B-I1'`, sub); n != 1 {
		t.Fatalf("history %d", n)
	}
}

// 3-D Secure completed just before SCA_ABANDON_AFTER, retry (or the
// purchase-expiry job) after it: the paid first invoice is activated as on
// the paid path (201, active, direct-signup saga), never cancelled with a
// 410 (rev 0.68). A first invoice paid after Billing ended the signup anyway
// is parked for an operator, not ignored.
func TestDirectSignupPaidJustBeforeExpiry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi_x"}}
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant)); !domain.HasCode(err, domain.ErrPaymentActionRequired) {
		t.Fatal(err)
	}
	sub := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, tenant)
	e.now = e.now.Add(31 * time.Minute) // past the window; the provider says paid (default "active")
	res, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	if err != nil || res.BillingSubscriptionID != sub || res.Status != "active" {
		t.Fatalf("late payment: %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); got != "awaiting_tenant_converted" {
		t.Fatalf("saga %s", got)
	}
	if len(e.stripe.calls("CancelNow")) != 0 {
		t.Fatal("a paid signup was cancelled")
	}

	// The job path: same answer, no cancel.
	t2 := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi_y"}}
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(t2)); !domain.HasCode(err, domain.ErrPaymentActionRequired) {
		t.Fatal(err)
	}
	s2 := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, t2)
	e.now = e.now.Add(31 * time.Minute)
	if _, err := e.svc.ExpireStale(ctx); err != nil {
		t.Fatal(err)
	}
	if status(t, s2) != "active" || len(e.stripe.calls("CancelNow")) != 0 {
		t.Fatalf("job: %s", status(t, s2))
	}

	// Paid after Billing ended the signup (the re-read said unpaid, then the
	// customer paid before the cancel landed): parked, SEV-2.
	t3 := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi_z"}}
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(t3)); !domain.HasCode(err, domain.ErrPaymentActionRequired) {
		t.Fatal(err)
	}
	s3 := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, t3)
	e.now = e.now.Add(31 * time.Minute)
	e.stripe.subStatus = "incomplete"
	_, err = e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(t3))
	wantCode(t, err, domain.ErrChargeExpired)
	e.stripe.subStatus = ""
	paid := invoiceState("in_late_"+s3, s3, "paid", "subscription_create", e.now, 3)
	_, err = e.handle(t3, app.CycleInvoicePaid{S: e.svc}, callback("evt_late_"+s3, "invoice.paid", domain.KindInvoicePaid), paid)
	var parked *port.ParkedEvent
	if !errors.As(err, &parked) || parked.Reason != "paid_after_end" {
		t.Fatalf("paid after end: %v", err)
	}
}

// A conversion checkout committed by a concurrent request between B-I1's
// read and its transaction 1 (account already present): 409 with the id of
// the subscription that won (BIL-SUB-3), no second subscription.
func TestDirectSignupConcurrentCheckout(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentDeclined, DeclineCode: "card_declined"}}
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant)); !domain.HasCode(err, domain.ErrPaymentDeclined) {
		t.Fatal(err)
	}
	var winner app.CheckoutResult
	e.beforeTx(1, func() {
		var err error
		if winner, err = e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 4}); err != nil {
			t.Errorf("concurrent checkout: %v", err)
		}
	})
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.ErrSubscriptionAlreadyExists || de.Extensions["billing_subscription_id"] != winner.BillingSubscriptionID {
		t.Fatalf("concurrent: %v (winner %s)", err, winner.BillingSubscriptionID)
	}
	if n := count(t, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND status <> 'ended'`, tenant); n != 1 {
		t.Fatalf("%d live subscriptions", n)
	}
}

// A first B-I1 whose account is created by a concurrent checkout between
// B-I1's read (no account) and its transaction 1: the account is re-read
// under the tenant lock instead of colliding on insert, so the request
// fails cleanly with 409 SUBSCRIPTION_ALREADY_EXISTS naming the winner —
// not with an error from a transaction the unique violation had aborted —
// and the winner's account is kept, not duplicated.
func TestDirectSignupAccountCreatedConcurrently(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	var winner app.CheckoutResult
	e.beforeTx(1, func() {
		var err error
		if winner, err = e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 4}); err != nil {
			t.Errorf("concurrent checkout: %v", err)
		}
	})
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(tenant))
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.ErrSubscriptionAlreadyExists || de.Extensions["billing_subscription_id"] != winner.BillingSubscriptionID {
		t.Fatalf("B-I1 after a concurrent account create: %v (winner %s)", err, winner.BillingSubscriptionID)
	}
	if n := count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}
