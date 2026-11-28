//go:build integration

package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

var stubReq = app.StubConversionRequest{Plan: "pro", Interval: "month", LicensedSeats: 3, Reason: "Signed order form; invoiced offline"}

// B-O1 (§8.1.4): guards and failures; each failure commits nothing (no
// subscription, saga or AL-5 entry); an abandoned incomplete checkout is
// ended and replaced by the stub.
func TestStubConversionEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	nothing := func(t *testing.T, tenant string) {
		t.Helper()
		if n := count(t, `SELECT (SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND origin = 'operator_stub')
			+ (SELECT count(*) FROM conversion_sagas WHERE tenant_id = $1) + (SELECT count(*) FROM pending_audit_entries WHERE tenant_id = $1)`, tenant); n != 0 {
			t.Fatalf("%d rows written", n)
		}
	}
	tenant := e.tenant()
	var err error
	e.withoutProvider("manual", func() { _, err = e.svc.StubConversion(ctx, operator, tenant, stubReq) })
	wantCode(t, err, domain.ErrProviderNotEnabled)
	e.f.on("Manual.CreateSubscription", 0)
	_, err = e.svc.StubConversion(ctx, operator, tenant, stubReq)
	wantCode(t, err, domain.ErrInternal)
	for _, c := range []struct {
		name string
		skip int
	}{{"Accounts.Get", 0}, {"Accounts.Create", 0}, {"Subs.Live", 0}, {"Prices.PriceAt", 0}, {"Subs.Create", 0}, {"Subs.AppendHistory", 0},
		{"Sagas.BySubscription", 0}, {"Sagas.BySubscription", 1}, {"Audit.Write", 0}} {
		e.f.on(c.name, c.skip)
		_, err := e.svc.StubConversion(ctx, operator, tenant, stubReq)
		injected(t, err, c.name)
		nothing(t, tenant)
	}
	e.svc.Settings.DefaultCurrency = "GBP" // a new account in a currency the manual book lacks
	_, err = e.svc.StubConversion(ctx, operator, tenant, stubReq)
	e.svc.Settings.DefaultCurrency = "USD"
	wantCode(t, err, domain.ErrPriceNotAvailable)
	nothing(t, tenant)

	// A trial tenant with an abandoned checkout: the incomplete one is ended.
	t2 := e.tenant()
	co, err := e.svc.StartCheckout(ctx, owner, t2, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.f.on("Subs.Transition", 0)
	_, err = e.svc.StubConversion(ctx, operator, t2, stubReq)
	injected(t, err, "Subs.Transition")
	res, err := e.svc.StubConversion(ctx, operator, t2, stubReq)
	if err != nil || res.State != "rp_convert_pending" {
		t.Fatalf("stub %+v %v", res, err)
	}
	if status(t, co.BillingSubscriptionID) != "ended" || status(t, res.BillingSubscriptionID) != "active" {
		t.Fatal("abandoned checkout not replaced")
	}
	if got := scalar[string](t, `SELECT cause||'/'||actor_id FROM subscription_status_history WHERE billing_subscription_id = $1`, co.BillingSubscriptionID); got != "operator:B-O1/"+operator.UserID {
		t.Fatalf("history %s", got)
	}

	// An account no longer on trial.
	t3 := e.tenant()
	if _, err := e.svc.StartCheckout(ctx, owner, t3, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2}); err != nil {
		t.Fatal(err)
	}
	setLifecycle(t, t3, "suspended", "operator")
	_, err = e.svc.StubConversion(ctx, operator, t3, stubReq)
	wantCode(t, err, domain.ErrSubscriptionAlreadyExists)
}

// B-O9 (§9.3.1): retry only a failed or overdue pending saga;
// resolved_manually confirms (and schedules the seat emission) unless
// confirmed or closed; failures commit nothing.
func TestSagaActionEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	retry := app.SagaActionRequest{Action: "retry", Reason: "RP fixed the tenant record"}
	resolve := app.SagaActionRequest{Action: "resolved_manually", Reason: "Converted by hand in RP"}
	sagaState := func(id string) string {
		return scalar[string](t, `SELECT state::text||'/'||coalesce(resolution,'') FROM conversion_sagas WHERE saga_id = $1`, id)
	}

	tenant, res := e.stub(t)
	e.f.on("Sagas.Get", 0)
	_, err := e.svc.SagaAction(ctx, operator, tenant, res.SagaID, retry)
	injected(t, err, "Sagas.Get")
	// Pending and not overdue: not retryable.
	_, err = e.svc.SagaAction(ctx, operator, tenant, res.SagaID, retry)
	wantCode(t, err, domain.ErrSagaAlreadyConfirmed)
	// Overdue (> 30 min since payment): retry allowed.
	e.now = e.now.Add(31 * time.Minute)
	if v, err := e.svc.SagaAction(ctx, operator, tenant, res.SagaID, retry); err != nil || v.State != "rp_convert_pending" {
		t.Fatalf("overdue retry %+v %v", v, err)
	}
	// §9.3.1 (rev 0.69, F-8): resolved_manually only from
	// awaiting_tenant_converted; a pending RP-3 is retried, not resolved.
	_, err = e.svc.SagaAction(ctx, operator, tenant, res.SagaID, resolve)
	wantCode(t, err, domain.ErrSagaAlreadyConfirmed)
	exec(t, `UPDATE conversion_sagas SET state = 'awaiting_tenant_converted' WHERE saga_id = $1`, res.SagaID)
	for _, name := range []string{"Subs.Get", "Emissions.Schedule", "Sagas.Update", "Audit.Write"} {
		e.f.on(name, 0)
		_, err := e.svc.SagaAction(ctx, operator, tenant, res.SagaID, resolve)
		injected(t, err, name)
		if got := sagaState(res.SagaID); got != "awaiting_tenant_converted/retry" {
			t.Fatalf("%s: %s", name, got)
		}
	}
	v, err := e.svc.SagaAction(ctx, operator, tenant, res.SagaID, resolve)
	if err != nil || v.State != "confirmed" || v.ConfirmedAt == nil {
		t.Fatalf("resolved %+v %v", v, err)
	}
	if n := count(t, `SELECT count(*) FROM scheduled_emissions WHERE ref_id = $1 AND payload_builder = 'seats_after_conversion'`, res.BillingSubscriptionID); n != 1 {
		t.Fatalf("emissions %d", n)
	}
	_, err = e.svc.SagaAction(ctx, operator, tenant, res.SagaID, resolve)
	wantCode(t, err, domain.ErrSagaAlreadyConfirmed)

	// A closed (refunded) saga cannot be resolved.
	t2, r2 := e.stub(t)
	exec(t, `UPDATE conversion_sagas SET state = 'refunded', next_attempt_at = NULL WHERE saga_id = $1`, r2.SagaID)
	_, err = e.svc.SagaAction(ctx, operator, t2, r2.SagaID, resolve)
	wantCode(t, err, domain.ErrSagaAlreadyConfirmed)
	if got := sagaState(r2.SagaID); got != "refunded/" {
		t.Fatalf("closed saga changed: %s", got)
	}
}
