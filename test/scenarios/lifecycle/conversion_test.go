//go:build integration

package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// B-P1 (§5.4.1): every read failure surfaces; a scheduled renewal price
// (§8.16, BIL-SUB-8) is shown with its date and drives the renewal
// estimate, and is left out when it cannot be read.
func TestGetSubscriptionReadsAndScheduledPrice(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.signup(t, "pro", 4)
	for _, name := range []string{"Accounts.Get", "Subs.Latest", "Subs.Item", "Prices.Get"} {
		e.f.on(name, 0)
		_, err := e.svc.GetSubscription(ctx, owner, tenant)
		injected(t, err, name)
	}

	at := e.now.Add(10 * 24 * time.Hour).Truncate(time.Second)
	next := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider = 'stripe' AND plan = 'pro' AND interval = 'year' AND currency = 'USD'`)
	exec(t, `UPDATE subscription_items SET scheduled_price_id = $2, scheduled_price_at = $3 WHERE billing_subscription_id = $1`, sub, next, at)
	v, err := e.svc.GetSubscription(ctx, owner, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if v.ScheduledUnitPrice == nil || v.ScheduledUnitPrice.AmountMinor != 70380 || !v.ScheduledUnitPrice.EffectiveAt.Equal(at) {
		t.Fatalf("scheduled price %+v", v.ScheduledUnitPrice)
	}
	if v.UnitPrice.AmountMinor != 6900 || v.RenewalEstimate == nil || v.RenewalEstimate.Amount.AmountMinor != 4*70380 {
		t.Fatalf("renewal estimate %+v (unit %+v)", v.RenewalEstimate, v.UnitPrice)
	}
	// The scheduled row unreadable: shown without it, estimate at today's price.
	e.f.on("Prices.Get", 1)
	if v, err = e.svc.GetSubscription(ctx, owner, tenant); err != nil || v.ScheduledUnitPrice != nil || v.RenewalEstimate.Amount.AmountMinor != 4*6900 {
		t.Fatalf("without scheduled price: %+v %v", v, err)
	}
}

// B-P2 (§8.1.1) guards and failures. Each failure leaves no account,
// subscription or session behind (transaction 1 is atomic), and the
// provider is never called inside it (BIL-CONC-2).
func TestStartCheckoutGuardsAndFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	req := app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2}
	nothing := func(t *testing.T, tenant string) {
		t.Helper()
		if n := count(t, `SELECT (SELECT count(*) FROM billing_accounts WHERE tenant_id = $1) + (SELECT count(*) FROM subscriptions WHERE tenant_id = $1)
			+ (SELECT count(*) FROM checkout_sessions WHERE tenant_id = $1)`, tenant); n != 0 {
			t.Fatalf("%d rows written", n)
		}
	}

	t.Run("above MAX_SEATS_PER_PURCHASE", func(t *testing.T) {
		tenant := e.tenant()
		_, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 1001})
		wantCode(t, err, domain.ErrValidation)
		nothing(t, tenant)
	})
	t.Run("account read fails", func(t *testing.T) {
		tenant := e.tenant()
		e.f.on("Accounts.Get", 0)
		_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		injected(t, err, "Accounts.Get")
		nothing(t, tenant)
	})
	t.Run("new-account provider not enabled", func(t *testing.T) {
		tenant := e.tenant()
		e.svc.Settings.NewAccountProvider = "paypal"
		_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		e.svc.Settings.NewAccountProvider = "stripe"
		wantCode(t, err, domain.ErrProviderNotEnabled)
		nothing(t, tenant)
	})
	t.Run("provider without hosted checkout", func(t *testing.T) {
		tenant := e.tenant()
		e.svc.Settings.NewAccountProvider = "manual"
		_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		e.svc.Settings.NewAccountProvider = "stripe"
		wantCode(t, err, domain.ErrProviderOperationUnsupported)
		nothing(t, tenant)
	})
	t.Run("customer creation fails", func(t *testing.T) {
		tenant := e.tenant()
		e.f.on("Provider.EnsureCustomer", 0)
		_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		wantCode(t, err, domain.ErrInternal) // an unclassified adapter error is a bug (§17)
		nothing(t, tenant)
	})
	for _, name := range []string{"Accounts.Create", "Prices.PriceAt", "Subs.Live", "Checkouts.Open", "Subs.Create", "Checkouts.SetProviderSession"} {
		t.Run(name+" fails", func(t *testing.T) {
			tenant := e.tenant()
			e.f.on(name, 0)
			_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
			injected(t, err, name)
			if name != "Checkouts.SetProviderSession" {
				nothing(t, tenant)
				return
			}
			// Transaction 1 committed before the hosted session was created:
			// the session keeps its pending placeholder; a retry reuses it.
			if !scalar[bool](t, `SELECT starts_with(provider_session_id, $2) FROM checkout_sessions WHERE tenant_id = $1`, tenant, domain.PendingSessionPrefix) {
				t.Fatal("session lost its pending placeholder")
			}
			res, err := e.svc.StartCheckout(ctx, owner, tenant, req)
			if err != nil || count(t, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1`, tenant) != 1 {
				t.Fatalf("retry: %+v %v", res, err)
			}
		})
	}
	t.Run("no price for the account currency", func(t *testing.T) {
		tenant := e.tenant()
		e.svc.Settings.DefaultCurrency = "EUR"
		_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		e.svc.Settings.DefaultCurrency = "USD"
		wantCode(t, err, domain.ErrPriceNotAvailable)
		nothing(t, tenant)
	})
	t.Run("account no longer on trial", func(t *testing.T) {
		tenant := e.tenant()
		first, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		if err != nil {
			t.Fatal(err)
		}
		exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'suspended', iam_suspension_source = 'operator' WHERE tenant_id = $1`, tenant)
		_, err = e.svc.StartCheckout(ctx, owner, tenant, req)
		wantCode(t, err, domain.ErrSubscriptionAlreadyExists)
		if status(t, first.BillingSubscriptionID) != "incomplete" {
			t.Fatal("the earlier checkout was touched")
		}
	})
	t.Run("hosted checkout fails", func(t *testing.T) {
		tenant := e.tenant()
		e.f.on("Provider.CreateHostedCheckout", 0)
		_, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		wantCode(t, err, domain.ErrInternal)
		// The incomplete subscription stays for the retry with the same
		// key bsub:<id>:checkout (idempotent on the subscription id).
		sub := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, tenant)
		res, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		if err != nil || res.BillingSubscriptionID != sub {
			t.Fatalf("retry %+v %v", res, err)
		}
		if got := e.stripe.calls("CreateHostedCheckout price_pro_month bsub:" + sub + ":checkout"); len(got) != 2 {
			t.Fatalf("checkout calls %v", got)
		}
	})
	t.Run("replacing an open checkout fails", func(t *testing.T) {
		tenant := e.tenant()
		first, err := e.svc.StartCheckout(ctx, owner, tenant, req)
		if err != nil {
			t.Fatal(err)
		}
		other := app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 3}
		e.f.on("Checkouts.SetStatus", 0)
		_, err = e.svc.StartCheckout(ctx, owner, tenant, other)
		injected(t, err, "Checkouts.SetStatus")
		// §8.1.3: ending the abandoned incomplete subscription fails.
		e.f.on("Subs.Transition", 0)
		_, err = e.svc.StartCheckout(ctx, owner, tenant, other)
		injected(t, err, "Subs.Transition")
		if status(t, first.BillingSubscriptionID) != "incomplete" ||
			count(t, `SELECT count(*) FROM checkout_sessions WHERE tenant_id = $1 AND status = 'open'`, tenant) != 1 {
			t.Fatal("a failed replacement left partial writes")
		}
	})
}

// A concurrent first checkout creates the account between B-P2's read and
// its transaction 1. Billing must not create a second account or
// subscription. (The recovery branch at conversion.go:155-160 re-reads the
// account inside a transaction the unique violation already aborted, so
// the request fails instead of continuing; see the report.)
func TestStartCheckoutConcurrentFirstCheckout(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	var winner app.CheckoutResult
	e.stripe.onEnsureCustomer = func() {
		var err error
		if winner, err = e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2}); err != nil {
			t.Errorf("concurrent checkout: %v", err)
		}
	}
	// The losing checkout re-reads the account under the tenant lock and
	// continues as a second checkout would (§8.1.3): the winner's
	// incomplete subscription is abandoned (ended) and the loser's is the
	// one live subscription (BIL-SUB-3). Before the fix it failed: its
	// recovery read ran on the transaction the unique violation had aborted.
	loser, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 3})
	if err != nil {
		t.Fatalf("losing checkout: %v", err)
	}
	if n := count(t, `SELECT count(*) FROM billing_accounts WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("%d accounts", n)
	}
	if n := count(t, `SELECT count(*) FROM subscriptions WHERE tenant_id = $1 AND status <> 'ended'`, tenant); n != 1 {
		t.Fatalf("%d live subscriptions, want 1", n)
	}
	if st := status(t, winner.BillingSubscriptionID); st != "ended" {
		t.Errorf("winner's subscription %s, want ended (abandoned by the later checkout)", st)
	}
	if st := status(t, loser.BillingSubscriptionID); st != "incomplete" {
		t.Errorf("loser's subscription %s, want incomplete", st)
	}
	if loser.CheckoutURL == "" {
		t.Error("loser got no checkout URL")
	}
}

func checkoutSession(sub string, fields map[string]any) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: "cs_" + sub}, Status: "complete", Fields: fields}
}

// checkout.session.completed (§8.1.1 step 3): failures roll back (the
// session stays open for the redelivery); unknown, foreign and already
// completed sessions are ignored; a session without a provider
// subscription is only marked completed.
func TestCheckoutCompletedEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	res, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	sub := res.BillingSubscriptionID
	ev := callback("evt_cs_"+sub, "checkout.session.completed", domain.KindCheckoutCompleted)
	cur := checkoutSession(sub, map[string]any{"subscription": "sub_" + sub})
	h := app.CheckoutCompleted{S: e.svc}

	for _, name := range []string{"Lock.LockTenant", "Checkouts.ByProviderSession", "Checkouts.SetStatus", "Subs.Update"} {
		e.f.on(name, 0)
		_, err := e.handle(tenant, h, ev, cur)
		injected(t, err, name)
	}
	if got := scalar[string](t, `SELECT status::text FROM checkout_sessions WHERE session_id = $1`, res.CheckoutSessionID); got != "open" {
		t.Fatalf("session after failures %s", got)
	}
	_, err = e.handle(tenant, h, ev, checkoutSession("unknown", nil))
	ignored(t, err, "unknown_session")
	_, err = e.handle(e.tenant(), h, ev, cur) // another tenant's session (BIL-PAY-3)
	ignored(t, err, "unknown_session")

	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:subscription_linked" {
		t.Fatalf("linked: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT provider_subscription_id FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "sub_"+sub {
		t.Fatalf("provider subscription %s", got)
	}
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "already_completed")

	// A completed session without a provider subscription only closes it.
	t2 := e.tenant()
	r2, err := e.svc.StartCheckout(ctx, owner, t2, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := e.handle(t2, h, ev, checkoutSession(r2.BillingSubscriptionID, map[string]any{})); err != nil || out != "applied:session_completed" {
		t.Fatalf("no subscription: %s %v", out, err)
	}
	if got := scalar[*string](t, `SELECT provider_subscription_id FROM subscriptions WHERE billing_subscription_id = $1`, r2.BillingSubscriptionID); got != nil {
		t.Fatalf("linked to %s", *got)
	}
}

// checkout.session.expired (§8.1.3): failures roll back; a session that
// is not open is ignored; the expiry ends the incomplete subscription.
func TestCheckoutExpiredEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	res, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	sub := res.BillingSubscriptionID
	ev := callback("evt_exp_"+sub, "checkout.session.expired", domain.KindCheckoutExpired)
	cur := checkoutSession(sub, nil)
	h := app.CheckoutExpired{S: e.svc}
	for _, name := range []string{"Lock.LockTenant", "Checkouts.ByProviderSession", "Checkouts.SetStatus", "Subs.Get", "Subs.Transition"} {
		e.f.on(name, 0)
		_, err := e.handle(tenant, h, ev, cur)
		injected(t, err, name)
	}
	if status(t, sub) != "incomplete" {
		t.Fatal("a failed expiry committed")
	}
	_, err = e.handle(tenant, h, ev, checkoutSession("unknown", nil))
	ignored(t, err, "session_not_open")
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:checkout_expired" {
		t.Fatalf("expired: %s %v", out, err)
	}
	if status(t, sub) != "ended" {
		t.Fatal("incomplete subscription not ended")
	}
	_, err = e.handle(tenant, h, ev, cur) // redelivery: the session is no longer open
	ignored(t, err, "session_not_open")
}

func firstPaid(sub string, qty any) (domain.PaymentEvent, domain.ProviderObjectState) {
	return callback("evt_in_"+sub, "invoice.paid", domain.KindInvoicePaid),
		invoiceState("in_"+sub, sub, "paid", "subscription_create", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), qty)
}

// invoice.paid for a conversion (§8.1.1 step 4, BIL-SAGA-1): only a paid
// re-read applies; every failure rolls back (subscription stays
// incomplete, no saga); the purchased quantity wins over the requested
// one, whatever numeric type the adapter normalised it to.
func TestFirstInvoicePaidEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := e.tenant()
	res, err := e.svc.StartCheckout(ctx, owner, tenant, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 5})
	if err != nil {
		t.Fatal(err)
	}
	sub := res.BillingSubscriptionID
	if _, err := e.handle(tenant, app.CheckoutCompleted{S: e.svc}, callback("evt_cs", "checkout.session.completed", domain.KindCheckoutCompleted),
		checkoutSession(sub, map[string]any{"subscription": "sub_" + sub})); err != nil {
		t.Fatal(err)
	}
	h := app.FirstInvoicePaid{S: e.svc}

	ev, cur := firstPaid(sub, int64(5))
	cur.Status = "open"
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "invoice_not_paid")
	ev, cur = firstPaid(sub, int64(5))
	delete(cur.Fields, "subscription") // not a subscription invoice, no next handler
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "no_handler")
	// A provider subscription without (usable) billing_subscription_id
	// metadata was not created by Billing, so no Billing subscription can
	// ever be linked to it: ignored, never retried to SEV-3 (rev 0.60);
	// metadata naming no Billing subscription is ignored too.
	ev, cur = firstPaid("unknown", int64(5))
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "unknown_subscription")
	cur.Fields["billing_subscription_id"] = "0193aaaa-0000-7000-8000-00000000dead"
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "unknown_subscription")
	cur.Fields["billing_subscription_id"] = "not-a-uuid"
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "unknown_subscription")

	for _, c := range []struct {
		name string
		skip int
		qty  any
	}{
		{"Lock.LockTenant", 0, int64(7)}, {"Subs.ByProvider", 0, int64(7)}, {"Subs.Transition", 0, int64(7)}, {"Subs.Item", 0, int64(7)},
		{"Subs.SetItem", 0, 7},             // int
		{"Subs.AppendHistory", 0, "seven"}, // not a number: the item keeps its quantity
		{"Sagas.BySubscription", 0, int64(5)}, {"Sagas.Insert", 0, int64(5)},
	} {
		e.f.on(c.name, c.skip)
		ev, cur := firstPaid(sub, c.qty)
		_, err := e.handle(tenant, h, ev, cur)
		injected(t, err, c.name)
		if status(t, sub) != "incomplete" || count(t, `SELECT count(*) FROM conversion_sagas WHERE billing_subscription_id = $1`, sub) != 0 {
			t.Fatalf("%s: partial commit", c.name)
		}
	}
	ev, cur = firstPaid(sub, float64(7)) // JSON numbers decode as float64
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:conversion_payment" {
		t.Fatalf("paid: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||i.quantity||'/'||g.state::text FROM subscriptions s JOIN subscription_items i USING (billing_subscription_id)
		JOIN conversion_sagas g USING (billing_subscription_id) WHERE s.billing_subscription_id = $1`, sub); got != "active/7/rp_convert_pending" {
		t.Fatalf("after payment %s", got)
	}
	if got := scalar[string](t, `SELECT payment_event_ref||'/'||to_seats FROM subscription_status_history WHERE billing_subscription_id = $1`, sub); got != "stripe:evt_in_"+sub+"/7" {
		t.Fatalf("history %s", got)
	}
	if len(events(t, tenant)) != 0 {
		t.Fatal("a conversion payment emits nothing (TenantConverted comes from IAM)")
	}
}

// §8.1.2 saga worker: failures before and after the RP-3 call are logged
// and retried on the next pass; a stale ref another replica already ran is
// skipped; a TenantConverted that arrives during the call wins; RP-3
// answers map to retry (http_<status> when RP gives no code), advance (409
// realm_already_dedicated counts as in_progress) and fail (SEV-2).
func TestSagaWorkerEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sagaState := func(id string) string {
		return scalar[string](t, `SELECT state::text||'/'||attempts||'/'||coalesce(last_error_code,'') FROM conversion_sagas WHERE saga_id = $1`, id)
	}

	e.f.on("Sagas.Due", 0)
	_, err := e.svc.RunSagas(ctx, 10)
	injected(t, err, "Sagas.Due")

	tenant, res := e.stub(t)
	for _, name := range []string{"Sagas.Get", "Sagas.Update", "Subs.Get"} {
		e.f.on(name, 0)
		if n, err := e.svc.RunSagas(ctx, 10); err != nil || n != 1 {
			t.Fatalf("%s: %d %v", name, n, err)
		}
		if got := sagaState(res.SagaID); got != "rp_convert_pending/0/" || len(e.rp.keys) != 0 {
			t.Fatalf("%s: claimed %s, RP calls %d", name, got, len(e.rp.keys))
		}
	}
	if !e.log.has("WARN", "saga attempt failed") {
		t.Fatal("failed attempt not logged")
	}

	// RP-3 503 without a problem body: retry with backoff, code http_503.
	e.rp.answers = []domain.RP3Answer{{Status: 503}}
	_, _ = e.svc.RunSagas(ctx, 10)
	if got := sagaState(res.SagaID); got != "rp_convert_pending/1/http_503" || e.metrics.get("rp_attempt:retryable") != 1 {
		t.Fatalf("503: %s", got)
	}
	if e.rp.keys[0] != domain.RP3IdempotencyKey(tenant, res.BillingSubscriptionID) {
		t.Fatalf("RP-3 key %s", e.rp.keys[0])
	}
	// 409 realm_already_dedicated: advance, the code kept (BIL-SAGA-2 replay).
	e.now = e.now.Add(time.Minute)
	e.rp.answers = []domain.RP3Answer{{Status: 409, ErrorCode: "realm_already_dedicated"}}
	_, _ = e.svc.RunSagas(ctx, 10)
	if got := sagaState(res.SagaID); got != "awaiting_tenant_converted/2/realm_already_dedicated" || e.metrics.get("rp_attempt:in_progress") != 1 {
		t.Fatalf("409 dedicated: %s", got)
	}
	// A replica holding the stale ref: the saga is no longer pending → skipped.
	e.sagas.stale = []port.SagaRef{{SagaID: res.SagaID, TenantID: tenant}}
	if n, err := e.svc.RunSagas(ctx, 10); err != nil || n != 1 || len(e.rp.keys) != 2 {
		t.Fatalf("stale ref: %d %v, RP calls %d", n, err, len(e.rp.keys))
	}
	e.sagas.stale = nil

	// TenantConverted arrives while RP-3 runs: the confirmation stands.
	t2, r2 := e.stub(t)
	e.rp.during = func() {
		if err := e.inTx(t2, func(ctx context.Context) error {
			_, err := e.svc.ConfirmConversion(ctx, app.TenantLifecycleEvent{EventID: "0193aaaa-0000-7000-8000-0000000000c2", Type: app.EventTenantConverted,
				Time: e.now, TenantID: t2, BillingSubscriptionID: r2.BillingSubscriptionID, Plan: domain.PlanPro})
			return err
		}); err != nil {
			t.Errorf("confirm during RP-3: %v", err)
		}
	}
	_, _ = e.svc.RunSagas(ctx, 10)
	if got := sagaState(r2.SagaID); got != "confirmed/0/" {
		t.Fatalf("confirmed meanwhile: %s", got)
	}

	// RP-3 404: failed, SEV-2 logged (§20.1), no next attempt.
	_, r3 := e.stub(t)
	e.rp.answers = []domain.RP3Answer{{Status: 404, ErrorCode: "tenant_not_found"}}
	_, _ = e.svc.RunSagas(ctx, 10)
	if got := sagaState(r3.SagaID); got != "failed/1/tenant_not_found" || !e.log.has("ERROR", "conversion saga failed (SEV-2") || e.metrics.get("rp_attempt:terminal") != 1 {
		t.Fatalf("404: %s", got)
	}
	if n := count(t, `SELECT count(*) FROM conversion_sagas WHERE saga_id = $1 AND next_attempt_at IS NULL`, r3.SagaID); n != 1 {
		t.Fatal("failed saga still scheduled")
	}
}

// TenantConverted (§7.1): unmatched events are counted and logged SEV-3
// (no Billing-initiated saga); a confirmed saga is a no-op; a saga that
// cannot be confirmed is left as is; write failures surface; a plan that
// differs from Billing's is logged (Billing's plan wins) and the delayed
// seat emission is still scheduled.
func TestConfirmConversionEdges(t *testing.T) {
	e := newEnv(t)
	tenant, res := e.stub(t)
	conv := app.TenantLifecycleEvent{EventID: "0193aaaa-0000-7000-8000-0000000000d1", Type: app.EventTenantConverted, Time: e.now,
		TenantID: tenant, BillingSubscriptionID: res.BillingSubscriptionID, Plan: domain.PlanPro}
	confirm := func(ev app.TenantLifecycleEvent) (bool, error) {
		var matched bool
		err := e.inTx(ev.TenantID, func(ctx context.Context) error {
			var err error
			matched, err = e.svc.ConfirmConversion(ctx, ev)
			return err
		})
		return matched, err
	}

	noSub := conv
	noSub.BillingSubscriptionID = ""
	if ok, err := confirm(noSub); ok || err != nil {
		t.Fatalf("no id: %v %v", ok, err)
	}
	unknown := conv
	unknown.BillingSubscriptionID = e.tenant()
	if ok, err := confirm(unknown); ok || err != nil {
		t.Fatalf("no saga: %v %v", ok, err)
	}
	foreign := conv
	foreign.TenantID = e.tenant()
	if ok, err := confirm(foreign); ok || err != nil {
		t.Fatalf("foreign saga: %v %v", ok, err)
	}
	if e.metrics.get("unmatched_conversion") != 3 || !e.log.has("ERROR", "TenantConverted Billing did not initiate (SEV-3)") {
		t.Fatalf("unmatched not recorded: %d", e.metrics.get("unmatched_conversion"))
	}

	// Rev 0.75: the tenant's trial add-on cap override is deleted with the
	// confirmation (BIL-ADD-5).
	exec(t, `INSERT INTO trial_addon_caps (tenant_id, max_packs, max_amount_minor, reason, set_by)
		VALUES ($1, 5, 50000, 'Enterprise PoC approved by Sales', $1)`, tenant)
	for _, name := range []string{"Sagas.BySubscription", "Sagas.Update", "Addons.DeleteTrialCap", "Subs.Update"} {
		e.f.on(name, 0)
		_, err := confirm(conv)
		injected(t, err, name)
	}
	if n := count(t, `SELECT count(*) FROM trial_addon_caps WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("override deleted by a failed confirmation: %d", n)
	}
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, res.SagaID); got != "rp_convert_pending" {
		t.Fatalf("after failures %s", got)
	}

	mismatch := conv
	mismatch.Plan = domain.PlanEnterprise
	if ok, err := confirm(mismatch); !ok || err != nil {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	// (The Subs.Update failure above already counted one confirmation: the
	// metric is recorded before the event's transaction commits.)
	if !e.log.has("ERROR", "plan_mismatch") || e.metrics.get("saga_confirmed:operator_stub") == 0 {
		t.Fatal("plan mismatch not logged or confirmation not counted")
	}
	if got := scalar[string](t, `SELECT s.plan::text||'/'||(s.converted_at IS NOT NULL)||'/'||g.tenant_converted_event FROM subscriptions s
		JOIN conversion_sagas g USING (billing_subscription_id) WHERE g.saga_id = $1`, res.SagaID); got != "pro/true/"+conv.EventID {
		t.Fatalf("confirmed %s", got)
	}
	if n := count(t, `SELECT count(*) FROM scheduled_emissions WHERE ref_id = $1 AND payload_builder = 'seats_after_conversion'`, res.BillingSubscriptionID); n != 1 {
		t.Fatalf("emissions %d", n)
	}
	if n := count(t, `SELECT count(*) FROM trial_addon_caps WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("override kept after the confirmation: %d", n)
	}
	if ok, err := confirm(conv); !ok || err != nil { // redelivery: no-op
		t.Fatalf("redelivery: %v %v", ok, err)
	}
	if n := count(t, `SELECT count(*) FROM scheduled_emissions WHERE ref_id = $1`, res.BillingSubscriptionID); n != 1 {
		t.Fatalf("emissions after redelivery %d", n)
	}

	// A failed saga cannot be confirmed by the event (§9.3.1): left, logged.
	t2, r2 := e.stub(t)
	exec(t, `UPDATE conversion_sagas SET state = 'failed', next_attempt_at = NULL WHERE saga_id = $1`, r2.SagaID)
	ev2 := conv
	ev2.TenantID, ev2.BillingSubscriptionID = t2, r2.BillingSubscriptionID
	if ok, err := confirm(ev2); !ok || err != nil {
		t.Fatalf("failed saga: %v %v", ok, err)
	}
	if got := scalar[string](t, `SELECT state::text FROM conversion_sagas WHERE saga_id = $1`, r2.SagaID); got != "failed" || !e.log.has("WARN", "cannot be confirmed") {
		t.Fatalf("failed saga became %s", got)
	}
}
