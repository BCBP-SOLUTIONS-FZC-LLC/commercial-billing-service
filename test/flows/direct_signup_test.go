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

var bff = app.Caller{Principal: "cluster.local/ns/bff/sa/bff-signup"}

func charge(tenant string) app.AuthorizeChargeRequest {
	return app.AuthorizeChargeRequest{TenantID: tenant, PlanCode: "pro", LicensedSeats: 10, PaymentMethodRef: "pm_1",
		BillingContact: &app.ContactInput{Name: "Dana Owner", Email: "owner@acme.example"}, CompanyName: "Acme", Country: "AE"}
}

// §8.3 paid path: account with contact, active subscription, saga
// direct_signup in awaiting_tenant_converted, no billing.events, provider
// keys derived from the subscription id (BIL-IDEM-2).
func TestDirectSignupPaid(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	res, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(tenant))
	if err != nil || res.Status != "active" || res.Provider != "stripe" || res.CurrentPeriodEnd.IsZero() {
		t.Fatalf("%+v %v", res, err)
	}
	sub := res.BillingSubscriptionID
	for _, want := range []string{"EnsureCustomer bsub:" + sub + ":customer", "AttachDefaultPaymentMethod pm_1 bsub:" + sub + ":pm",
		"CreateSubscription bsub:" + sub + ":create", "ConfirmInitialPayment bsub:" + sub + ":confirm"} {
		if len(e.stripe.called(want)) != 1 {
			t.Errorf("missing call %q in %v", want, e.stripe.calls)
		}
	}
	if st := scalar[string](t, `SELECT kind::text||'/'||state::text FROM conversion_sagas WHERE billing_subscription_id = $1`, sub); st != "direct_signup/awaiting_tenant_converted" {
		t.Fatalf("saga %s", st)
	}
	if st := scalar[string](t, `SELECT coalesce(billing_contact_email,'')||'/'||coalesce(country,'') FROM billing_accounts WHERE tenant_id = $1`, tenant); st != "owner@acme.example/AE" {
		t.Fatalf("account %s", st)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("events %d", n)
	}
	// Same tenant, another key → 409 with the id (the middleware's replay
	// covers the same key).
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(tenant)); !domain.HasCode(err, domain.ErrSubscriptionAlreadyExists) {
		t.Fatalf("second: %v", err)
	}
}

// 3-D Secure: 402 with the client secret, then the retry continues the
// same subscription and succeeds; never a second subscription or customer.
func TestDirectSignup3DS(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	e.stripe.confirm = []confirmAnswer{{err: port.ErrActionRequired{ClientSecret: "pi_secret_1"}}}
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(tenant))
	var de *domain.DomainError
	if !domain.HasCode(err, domain.ErrPaymentActionRequired) || !asDomain(err, &de) || de.Extensions["billing_subscription_id"] == nil {
		t.Fatalf("3ds: %v", err)
	}
	if de.Extensions["action"].(map[string]string)["client_secret"] != "pi_secret_1" || de.Extensions["expires_at"] == nil {
		t.Fatalf("extensions %+v", de.Extensions)
	}
	sub := de.Extensions["billing_subscription_id"].(string)
	res, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(tenant))
	if err != nil || res.BillingSubscriptionID != sub {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if len(e.stripe.called("EnsureCustomer")) != 1 || len(e.stripe.called("CreateSubscription")) != 1 {
		t.Fatalf("duplicate provider objects: %v", e.stripe.calls)
	}
	// A different request for the same tenant while 3-D Secure is pending → 409.
	other := newTenant()
	e.stripe.confirm = []confirmAnswer{{err: port.ErrActionRequired{ClientSecret: "x"}}}
	_, _ = e.svc.AuthorizeAndCharge(ctx, bff, charge(other))
	diff := charge(other)
	diff.LicensedSeats = 11
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, diff); !domain.HasCode(err, domain.ErrSubscriptionAlreadyExists) {
		t.Fatalf("different body during 3ds: %v", err)
	}
	// Abandoned past SCA_ABANDON_AFTER → 410; provider subscription cancelled; ended.
	e.now = e.now.Add(31 * time.Minute)
	if _, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(other)); !domain.HasCode(err, domain.ErrChargeExpired) {
		t.Fatalf("expired: %v", err)
	}
	if st := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE tenant_id = $1`, other); st != "ended/signup_failed" {
		t.Fatalf("expired subscription %s", st)
	}
	if len(e.stripe.called("CancelNow")) != 1 {
		t.Fatalf("cancel calls %v", e.stripe.called("CancelNow"))
	}
}

// A decline: 402 PAYMENT_DECLINED with the mapped reason; provider
// subscription cancelled; Billing's ended (signup_failed).
func TestDirectSignupDeclined(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	e.stripe.confirm = []confirmAnswer{{err: port.ErrDeclined{Code: "insufficient_funds"}}}
	_, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(tenant))
	var de *domain.DomainError
	if !asDomain(err, &de) || de.Code != domain.ErrPaymentDeclined || de.Extensions["decline_reason"] != "insufficient_funds" {
		t.Fatalf("declined: %v", err)
	}
	if st := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE tenant_id = $1`, tenant); st != "ended/signup_failed" {
		t.Fatalf("subscription %s", st)
	}
	if len(e.stripe.called("CancelNow")) != 1 {
		t.Fatal("provider subscription not cancelled")
	}
	// The tenant can sign up again with a new key.
	if res, err := e.svc.AuthorizeAndCharge(ctx, bff, charge(tenant)); err != nil || res.Status != "active" {
		t.Fatalf("after decline: %+v %v", res, err)
	}
}
