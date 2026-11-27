//go:build integration

package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// injectedOrProvider accepts the scripted failure itself or, for a provider
// call, its §17 mapping (an unclassified adapter error is INTERNAL).
func injectedOrProvider(t *testing.T, err error, name string) {
	t.Helper()
	if len(name) > 9 && name[:9] == "Provider." {
		wantCode(t, err, domain.ErrInternal)
		if !errors.Is(err, errInjected) {
			t.Fatalf("cause lost: %v", err)
		}
		return
	}
	injected(t, err, name)
}

// downgrade schedules enterprise → pro at period end (§8.4.2) and returns
// the change id; the provider schedule id is recorded on the row.
func (e *env) downgrade(t *testing.T, tenant, sub, key string) string {
	t.Helper()
	res, err := e.svc.ChangePlan(context.Background(), withKey(owner, key), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: version(t, sub)})
	if err != nil || res.Status != "scheduled" {
		t.Fatalf("downgrade %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT coalesce(provider_schedule_id, '') FROM plan_change_requests WHERE change_id = $1`, res.ChangeID); got == "" {
		t.Fatal("no provider schedule")
	}
	return res.ChangeID
}

// reduction schedules a seat reduction (B-P10) and gives it a provider
// schedule, as a provider that phases quantity changes would.
func (e *env) reduction(t *testing.T, tenant, target string) string {
	t.Helper()
	v, err := e.svc.ScheduleReduction(context.Background(), owner, tenant, app.ReductionRequest{TargetSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, `UPDATE seat_change_schedules SET provider_schedule_id = $2 WHERE schedule_id = $1`, v.ScheduleID, target)
	return v.ScheduleID
}

// B-P12 / B-P13 (§8.8.2): guards, idempotent resume, failures before and
// after the provider call (BIL-CONC-2: provider outside the transaction),
// and the release of a scheduled downgrade and seat reduction.
func TestCancelAndResumeEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	trial := e.tenant()
	_, err := e.svc.CancelSubscription(ctx, owner, trial, app.CancelRequest{RecordVersion: 1, Reason: "too_expensive"})
	wantCode(t, err, domain.ErrSubscriptionNotActive)
	e.f.on("Subs.Live", 0)
	_, err = e.svc.CancelSubscription(ctx, owner, trial, app.CancelRequest{RecordVersion: 1})
	injected(t, err, "Subs.Live")

	stubTenant, stub := e.stub(t)
	_, err = e.svc.CancelSubscription(ctx, owner, stubTenant, app.CancelRequest{RecordVersion: version(t, stub.BillingSubscriptionID)})
	wantCode(t, err, domain.ErrProviderOperationUnsupported)

	tenant, sub := e.signup(t, "pro", 4)
	// Resume while auto-renewing: nothing to do, no provider call.
	if v, err := e.svc.ResumeSubscription(ctx, owner, tenant, app.ResumeRequest{RecordVersion: version(t, sub)}); err != nil || !v.AutoRenew {
		t.Fatalf("resume no-op %+v %v", v, err)
	}
	e.withoutProvider("stripe", func() {
		_, err = e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: version(t, sub)})
	})
	wantCode(t, err, domain.ErrProviderNotEnabled)
	for _, name := range []string{"Changes.Open", "Schedules.Open", "Provider.SetCancelAtPeriodEnd", "Subs.Update", "Subs.AppendHistory"} {
		e.f.on(name, 0)
		_, err := e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: version(t, sub)})
		injectedOrProvider(t, err, name)
		if !scalar[bool](t, `SELECT auto_renew FROM subscriptions WHERE billing_subscription_id = $1`, sub) {
			t.Fatalf("%s: cancellation committed", name)
		}
	}
	if len(e.stripe.calls("SetCancelAtPeriodEnd")) != 3 {
		t.Fatalf("provider calls %v", e.stripe.calls("SetCancelAtPeriodEnd"))
	}
	if _, err := e.svc.CancelSubscription(ctx, owner, tenant, app.CancelRequest{RecordVersion: version(t, sub)}); err != nil {
		t.Fatal(err)
	}
	// The period is over: B-P13 is refused (reactivate instead, §8.9).
	end := scalar[time.Time](t, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	saved := e.now
	e.now = end
	_, err = e.svc.ResumeSubscription(ctx, owner, tenant, app.ResumeRequest{RecordVersion: version(t, sub)})
	e.now = saved
	wantCode(t, err, domain.ErrSubscriptionNotActive)

	// A scheduled downgrade is released on the provider and cancelled.
	t2, s2 := e.signup(t, "enterprise", 4)
	change := e.downgrade(t, t2, s2, "k-dg-cancel-1")
	schedID := scalar[string](t, `SELECT provider_schedule_id FROM plan_change_requests WHERE change_id = $1`, change)
	for _, name := range []string{"Provider.ReleaseSchedule", "Changes.Update"} {
		e.f.on(name, 0)
		_, err := e.svc.CancelSubscription(ctx, owner, t2, app.CancelRequest{RecordVersion: version(t, s2)})
		injectedOrProvider(t, err, name)
	}
	if _, err := e.svc.CancelSubscription(ctx, owner, t2, app.CancelRequest{RecordVersion: version(t, s2)}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT status::text FROM plan_change_requests WHERE change_id = $1`, change); got != "cancelled" {
		t.Fatalf("change %s", got)
	}
	if got := e.stripe.calls("ReleaseSchedule " + schedID + " planchg:" + change + ":release"); len(got) != 3 {
		t.Fatalf("release calls %v", e.stripe.calls("ReleaseSchedule"))
	}

	// A scheduled reduction with a provider schedule likewise.
	t3, s3 := e.signup(t, "pro", 4)
	sched := e.reduction(t, t3, "sched_seat_"+s3)
	for _, name := range []string{"Provider.ReleaseSchedule", "Schedules.Update"} {
		e.f.on(name, 0)
		_, err := e.svc.CancelSubscription(ctx, owner, t3, app.CancelRequest{RecordVersion: version(t, s3)})
		injectedOrProvider(t, err, name)
	}
	if _, err := e.svc.CancelSubscription(ctx, owner, t3, app.CancelRequest{RecordVersion: version(t, s3)}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT status::text FROM seat_change_schedules WHERE schedule_id = $1`, sched); got != "cancelled" {
		t.Fatalf("schedule %s", got)
	}
	if got := e.stripe.calls("ReleaseSchedule sched_seat_" + s3 + " sched:" + sched + ":release"); len(got) != 3 {
		t.Fatalf("reduction release calls %v", got)
	}
	if len(events(t, t3)) != 0 {
		t.Fatal("B-P12 emits nothing (TenantSubscriptionCancelled comes with the provider's end)")
	}
}

// invoice.payment_failed (§8.8.1, BIL-EVT-7): only cycle invoices of a
// known subscription; every failure rolls back (still active, no event);
// the decline code is counted by reason.
func TestPaymentFailedEdges(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.signup(t, "pro", 4)
	start := e.now.AddDate(0, 1, 0).Truncate(time.Second)
	h := app.PaymentFailed{S: e.svc}
	ev := callback("evt_pf_"+sub, "invoice.payment_failed", domain.KindInvoicePaymentFailed)
	failed := func() domain.ProviderObjectState {
		cur := invoiceState("in_pf_"+sub, sub, "open", "subscription_cycle", start, int64(4))
		cur.Fields["decline_code"] = "insufficient_funds"
		return cur
	}

	cur := failed()
	cur.Fields["billing_reason"] = "subscription_update"
	_, err := e.handle(tenant, h, ev, cur)
	ignored(t, err, "not_a_cycle_invoice")
	cur = failed()
	delete(cur.Fields, "subscription")
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, err, "no_subscription")
	_, err = e.handle(tenant, h, ev, invoiceState("in_x", "unknown", "open", "subscription_cycle", start, int64(4)))
	ignored(t, err, "unknown_subscription")

	for _, name := range []string{"Lock.LockTenant", "Subs.ByProvider", "Invoices.Mirror", "Subs.Transition", "Subs.AppendHistory", "Events.Enqueue"} {
		e.f.on(name, 0)
		_, err := e.handle(tenant, h, ev, failed())
		injected(t, err, name)
		if status(t, sub) != "active" || len(events(t, tenant)) != 0 || count(t, `SELECT count(*) FROM invoices WHERE tenant_id = $1`, tenant) != 0 {
			t.Fatalf("%s: partial commit", name)
		}
	}
	if out, err := e.handle(tenant, h, ev, failed()); err != nil || out != "applied:past_due" {
		t.Fatalf("past due: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT (payload->'data'->'amount_due'->>'amount_minor')||'/'||(payload->'data'->>'next_retry_at' IS NOT NULL)
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantPaymentPastDue'`, tenant); got != "69000/true" {
		t.Fatalf("event %s", got)
	}
	// (Counted from the first attempt that got past the transition: metrics
	// are recorded before the callback's transaction commits.)
	if e.metrics.get("payment_failure:insufficient_funds") == 0 || e.metrics.get("payment_failure:payment_failed") != 0 {
		t.Fatal("failure metrics")
	}
}

// customer.subscription.deleted (§8.8.2): the reason follows the state
// (past_due → dunning_exhausted, auto-renew on → auto_renew_off); open
// downgrade and reduction are cancelled; failures roll back; a redelivery
// is ignored.
func TestSubscriptionEndedEdges(t *testing.T) {
	e := newEnv(t)
	h := app.SubscriptionEnded{S: e.svc}
	ended := func(sub string) domain.ProviderObjectState {
		return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + sub}, Status: "canceled"}
	}
	ev := callback("evt_del", "customer.subscription.deleted", domain.KindSubscriptionEnded)

	tenant, sub := e.signup(t, "enterprise", 4)
	change := e.downgrade(t, tenant, sub, "k-dg-ended-1")
	if _, err := e.handle(tenant, app.PaymentFailed{S: e.svc}, callback("evt_pf", "invoice.payment_failed", domain.KindInvoicePaymentFailed),
		invoiceState("in_pf", sub, "open", "subscription_cycle", e.now, int64(4))); err != nil {
		t.Fatal(err)
	}
	active := ended(sub)
	active.Status = "active"
	_, err := e.handle(tenant, h, ev, active)
	ignored(t, err, "subscription_not_ended")
	_, err = e.handle(tenant, h, ev, ended("unknown"))
	ignored(t, err, "unknown_subscription")
	for _, name := range []string{"Lock.LockTenant", "Subs.Transition", "Changes.Open", "Changes.Update", "Schedules.Open", "Subs.AppendHistory", "Events.Enqueue"} {
		e.f.on(name, 0)
		_, err := e.handle(tenant, h, ev, ended(sub))
		injected(t, err, name)
		if status(t, sub) != "past_due" {
			t.Fatalf("%s: committed", name)
		}
	}
	if out, err := e.handle(tenant, h, ev, ended(sub)); err != nil || out != "applied:cancelled" {
		t.Fatalf("ended: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT s.status::text||'/'||s.cancel_reason::text||'/'||(s.past_due_since IS NULL)||'/'||c.status::text
		FROM subscriptions s JOIN plan_change_requests c ON c.billing_subscription_id = s.billing_subscription_id WHERE c.change_id = $1`, change); got != "cancelled/dunning_exhausted/true/cancelled" {
		t.Fatalf("after end %s", got)
	}
	if got := events(t, tenant); !slices.Equal(got, []string{"TenantPaymentPastDue", "TenantSubscriptionCancelled"}) {
		t.Fatalf("events %v", got)
	}
	_, err = e.handle(tenant, h, ev, ended(sub)) // redelivery
	ignored(t, err, "transition_not_allowed")

	t2, s2 := e.signup(t, "pro", 4)
	sched := e.reduction(t, t2, "sched_seat_"+s2)
	if out, err := e.handle(t2, h, ev, ended(s2)); err != nil || out != "applied:cancelled" {
		t.Fatalf("ended: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT s.cancel_reason::text||'/'||c.status::text FROM subscriptions s JOIN seat_change_schedules c USING (billing_subscription_id)
		WHERE c.schedule_id = $1`, sched); got != "auto_renew_off/cancelled" {
		t.Fatalf("after end %s", got)
	}
}

// Cycle invoice.paid (§8.6.3 step 4, §8.9): recovery from past_due,
// reactivation of a cancelled subscription with a seat change (seats event
// first, BIL-EVT-5), renewal of an active one; every failure rolls back;
// an incomplete subscription is not this handler's.
func TestCycleInvoicePaidEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	h := app.CycleInvoicePaid{S: e.svc}
	ev := callback("evt_paid", "invoice.paid", domain.KindInvoicePaid)

	tenant, sub := e.signup(t, "pro", 4)
	start := scalar[time.Time](t, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	paid := func(qty any) domain.ProviderObjectState {
		return invoiceState("in_cy_"+sub, sub, "paid", "subscription_cycle", start, qty)
	}

	open := paid(int64(4))
	open.Status = "open"
	_, err := e.handle(tenant, h, ev, open)
	ignored(t, err, "invoice_not_paid")
	noSub := paid(int64(4))
	delete(noSub.Fields, "subscription")
	_, err = e.handle(tenant, h, ev, noSub)
	ignored(t, err, "no_subscription")

	// Active: the renewal moves the period; failures roll back.
	for _, c := range []struct {
		name string
		skip int
	}{{"Lock.LockTenant", 0}, {"Invoices.Mirror", 0}, {"Subs.Update", 0}, {"Subs.Item", 0}} {
		e.f.on(c.name, c.skip)
		_, err := e.handle(tenant, h, ev, paid(int64(4)))
		injected(t, err, c.name)
		if got := scalar[time.Time](t, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub); !got.Equal(start) {
			t.Fatalf("%s: period moved", c.name)
		}
	}
	if out, err := e.handle(tenant, h, ev, paid(int64(4))); err != nil || out != "applied:renewed" {
		t.Fatalf("renewed: %s %v", out, err)
	}

	// past_due → active with TenantReactivated {previous_status: past_due}.
	if _, err := e.handle(tenant, app.PaymentFailed{S: e.svc}, callback("evt_pf", "invoice.payment_failed", domain.KindInvoicePaymentFailed),
		invoiceState("in_cy2_"+sub, sub, "open", "subscription_cycle", start.AddDate(0, 1, 0), int64(4))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Subs.Item", "Subs.Transition", "Subs.AppendHistory", "Events.Enqueue"} {
		e.f.on(name, 0)
		_, err := e.handle(tenant, h, ev, paid(int64(4)))
		injected(t, err, name)
		if status(t, sub) != "past_due" {
			t.Fatalf("%s: committed", name)
		}
	}
	if out, err := e.handle(tenant, h, ev, paid(int64(4))); err != nil || out != "applied:reactivated" {
		t.Fatalf("recovered: %s %v", out, err)
	}

	// cancelled → active with 6 seats: TenantSeatsChanged, then TenantReactivated.
	t2, s2 := e.signup(t, "pro", 4)
	if _, err := e.handle(t2, app.SubscriptionEnded{S: e.svc}, callback("evt_del", "customer.subscription.deleted", domain.KindSubscriptionEnded),
		domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + s2}, Status: "canceled"}); err != nil {
		t.Fatal(err)
	}
	// A late payment of an old cycle invoice of the deleted provider
	// subscription (dunning exhausted) does not reactivate (§8.9): the
	// invoice is mirrored, no transition, no TenantReactivated.
	stale := invoiceState("in_old_"+s2, s2, "paid", "subscription_cycle", start, int64(4))
	if out, err := e.handle(t2, h, ev, stale); err != nil || out != "applied:invoice_mirrored" {
		t.Fatalf("old invoice: %s %v", out, err)
	}
	if status(t, s2) != "cancelled" || len(events(t, t2)) != 1 {
		t.Fatalf("old invoice reactivated: %s %v", status(t, s2), events(t, t2))
	}
	if got := scalar[string](t, `SELECT status::text FROM invoices WHERE provider_invoice_id = $1`, "in_old_"+s2); got != "paid" {
		t.Fatalf("old invoice mirror %s", got)
	}
	react := invoiceState("in_re_"+s2, s2, "paid", "subscription_create", e.now, int64(6))
	for _, c := range []struct {
		name string
		skip int
	}{{"Subs.SetItem", 0}, {"Events.Enqueue", 0}, {"Events.Enqueue", 1}} {
		e.f.on(c.name, c.skip)
		_, err := e.handle(t2, h, ev, react)
		injected(t, err, c.name)
		if status(t, s2) != "cancelled" || len(events(t, t2)) != 1 {
			t.Fatalf("%s/%d: committed", c.name, c.skip)
		}
	}
	if out, err := e.handle(t2, h, ev, react); err != nil || out != "applied:reactivated" {
		t.Fatalf("reactivated: %s %v", out, err)
	}
	if got := events(t, t2); !slices.Equal(got, []string{"TenantSubscriptionCancelled", "TenantSeatsChanged", "TenantReactivated"}) {
		t.Fatalf("events %v", got)
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'licensed_seats'||'/'||(payload->'data'->>'previous_licensed_seats') FROM outbox_events
		WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'`, t2); got != "6/4" {
		t.Fatalf("seats event %s", got)
	}

	// An incomplete subscription (3-D Secure pending) is not reactivated.
	t3 := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentRequiresAction, ClientSecret: "pi"}}
	_, _ = e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(t3))
	s3 := scalar[string](t, `SELECT billing_subscription_id::text FROM subscriptions WHERE tenant_id = $1`, t3)
	_, err = e.handle(t3, h, ev, invoiceState("in_inc", s3, "paid", "subscription_cycle", e.now, int64(4)))
	ignored(t, err, "subscription_incomplete")
}

// B-P14 (§8.9): guards, the open-invoice path for past_due and the hosted
// checkout for cancelled (seats ≥ in use from a fresh I-11; the price for
// the requested interval); failures before the checkout leave no session,
// a second request replaces the open one.
func TestReactivateEdges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	// No live subscription (a declined signup) and an offboarded account.
	declined := e.tenant()
	e.stripe.confirm = []port.PaymentOutcome{{Status: port.PaymentDeclined, DeclineCode: "card_declined"}}
	_, _ = e.svc.AuthorizeAndCharge(ctx, bff, chargeReq(declined))
	_, err := e.svc.Reactivate(ctx, owner, declined, app.ReactivateRequest{})
	wantCode(t, err, domain.ErrSubscriptionNotFound)
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'offboarded', closed_at = now() WHERE tenant_id = $1`, declined)
	_, err = e.svc.Reactivate(ctx, owner, declined, app.ReactivateRequest{})
	wantCode(t, err, domain.ErrAccountClosed)

	// past_due.
	tenant, sub := e.signup(t, "pro", 4)
	if _, err := e.handle(tenant, app.PaymentFailed{S: e.svc}, callback("evt_pf", "invoice.payment_failed", domain.KindInvoicePaymentFailed),
		invoiceState("in_open_"+sub, sub, "open", "subscription_cycle", e.now, int64(4))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Accounts.Get", "Subs.Live", "Subs.Item", "Provider.PayOpenInvoiceLink"} {
		e.f.on(name, 0)
		_, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{})
		injectedOrProvider(t, err, name)
	}
	e.withoutProvider("stripe", func() { _, err = e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{}) })
	wantCode(t, err, domain.ErrProviderNotEnabled)
	exec(t, `UPDATE subscriptions SET collection_paused = true, collection_paused_by = 'operator' WHERE billing_subscription_id = $1`, sub)
	_, err = e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{})
	wantCode(t, err, domain.ErrCollectionPaused)
	exec(t, `UPDATE subscriptions SET collection_paused = false, collection_paused_by = NULL WHERE billing_subscription_id = $1`, sub)
	if r, err := e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{}); err != nil || r.Kind != "open_invoice" || r.PaymentURL != "https://invoice.example/in_open_"+sub {
		t.Fatalf("open invoice %+v %v", r, err)
	}
	// past_due with nothing open (the invoice was paid or voided meanwhile).
	exec(t, `UPDATE invoices SET status = 'void' WHERE tenant_id = $1`, tenant)
	_, err = e.svc.Reactivate(ctx, owner, tenant, app.ReactivateRequest{})
	wantCode(t, err, domain.ErrInvoiceNotFound)

	// cancelled.
	t2, s2 := e.signup(t, "pro", 4)
	if _, err := e.handle(t2, app.SubscriptionEnded{S: e.svc}, callback("evt_del", "customer.subscription.deleted", domain.KindSubscriptionEnded),
		domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + s2}, Status: "canceled"}); err != nil {
		t.Fatal(err)
	}
	e.core.Err = domain.NewError(domain.ErrIAMUnavailable, "I-11 down")
	_, err = e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{})
	e.core.Err = nil
	wantCode(t, err, domain.ErrIAMUnavailable)
	// The yearly Stripe price was retired (B-O11 closes a row): no price for
	// the requested interval. (The account currency cannot change while a
	// subscription is live, BIL-SUB-6.)
	exec(t, `UPDATE plan_prices SET effective_to = $1 WHERE provider = 'stripe' AND plan = 'pro' AND interval = 'year'`, e.now.Add(-time.Hour))
	_, err = e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{Interval: "year"})
	exec(t, `UPDATE plan_prices SET effective_to = NULL WHERE provider = 'stripe' AND plan = 'pro' AND interval = 'year'`)
	wantCode(t, err, domain.ErrPriceNotAvailable)
	for _, name := range []string{"Checkouts.Open", "Checkouts.Insert"} {
		e.f.on(name, 0)
		_, err := e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{})
		injected(t, err, name)
		if n := count(t, `SELECT count(*) FROM checkout_sessions WHERE tenant_id = $1`, t2); n != 0 {
			t.Fatalf("%s: session written", name)
		}
	}
	first, err := e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{Interval: "year", LicensedSeats: ptr(5)})
	if err != nil || first.Kind != "checkout" {
		t.Fatalf("checkout %+v %v", first, err)
	}
	if got := e.stripe.calls("CreateHostedCheckout price_pro_year bsub:" + s2 + ":reactivate:" + first.CheckoutSessionID); len(got) != 1 {
		t.Fatalf("hosted checkout %v", e.stripe.calls("CreateHostedCheckout"))
	}
	if got := scalar[string](t, `SELECT requested_interval::text||'/'||requested_seats FROM checkout_sessions WHERE session_id = $1`, first.CheckoutSessionID); got != "year/5" {
		t.Fatalf("session %s", got)
	}
	// A second request replaces the open session; failures roll back.
	e.f.on("Checkouts.SetStatus", 0)
	_, err = e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{})
	injected(t, err, "Checkouts.SetStatus")
	for _, name := range []string{"Provider.CreateHostedCheckout", "Checkouts.SetProviderSession"} {
		e.f.on(name, 0)
		_, err := e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{})
		injectedOrProvider(t, err, name)
	}
	second, err := e.svc.Reactivate(ctx, owner, t2, app.ReactivateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT string_agg(status::text, ',' ORDER BY created_at) FROM checkout_sessions WHERE tenant_id = $1 AND session_id IN ($2, $3)`,
		t2, first.CheckoutSessionID, second.CheckoutSessionID); got != "expired,open" {
		t.Fatalf("sessions %s", got)
	}
	if n := count(t, `SELECT count(*) FROM checkout_sessions WHERE tenant_id = $1 AND status = 'open'`, t2); n != 1 {
		t.Fatalf("%d open sessions", n)
	}
}
