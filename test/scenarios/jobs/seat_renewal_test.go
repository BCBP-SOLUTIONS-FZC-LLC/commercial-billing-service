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

// reduction creates an active pro subscription with seats seats and a
// scheduled reduction to target (inUse members), returning tenant,
// subscription, schedule and the renewal instant R.
func (e *env) reduction(seats, inUse, target int) (string, string, string, time.Time) {
	e.t.Helper()
	tenant, sub := e.active("pro", seats)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: inUse, LicensedSeats: seats}
	if _, err := e.svc.ScheduleReduction(context.Background(), owner, tenant, app.ReductionRequest{TargetSeats: target}); err != nil {
		e.t.Fatal(err)
	}
	sch := scalar[string](e.t, `SELECT schedule_id::text FROM seat_change_schedules WHERE billing_subscription_id = $1`, sub)
	return tenant, sub, sch, e.periodEnd(sub)
}

func (e *env) schedule(sch string) string {
	e.t.Helper()
	return scalar[string](e.t, `SELECT status||'/'||coalesce(renewal_seats::text,'-')||'/'||coalesce(final_seats::text,'-')||'/'||coalesce(skip_reason,'-')
		FROM seat_change_schedules WHERE schedule_id = $1`, sch)
}

// failOnce runs the seat-renewal job with one injected failure and checks
// the schedule is reported failed, logged and left as it was.
func (e *env) failRenewal(sch, want string) {
	e.t.Helper()
	res := e.run(e.svc.RunSeatRenewal)
	if res.Failed != 1 || res.Handled != 0 {
		e.t.Fatalf("result %+v", res)
	}
	if got := e.schedule(sch); got != want {
		e.t.Fatalf("schedule after a failed run: %s, want %s", got, want)
	}
	if !e.log.has("WARN seat-renewal: schedule not processed") {
		e.t.Fatal("failure not logged")
	}
	e.f.clear()
}

// §8.6.2: every failure while applying a reduction ahead of the renewal
// leaves the schedule for the next run (BIL-CONC-1 re-read under the lock;
// BIL-CONC-2 provider call outside it); the run then completes.
func TestSeatRenewalApplyAheadRetriesEachFailure(t *testing.T) {
	e := newEnv(t)
	_, sub, sch, r := e.reduction(10, 3, 5)
	e.now = r.Add(-2 * time.Hour)

	e.f.fail("Schedules.Due", 0)
	e.runErr(e.svc.RunSeatRenewal)
	e.f.clear()

	for _, step := range []struct {
		name string
		skip int
		want string
	}{
		{"Schedules.Get", 0, "scheduled/-/-/-"}, // first re-read under the lock
		{"Subs.Get", 0, "scheduled/-/-/-"},
		{"Subs.Item", 0, "scheduled/-/-/-"},
		{"Prices.Get", 0, "applying/-/-/-"}, // BIL-SUB-8 renewal price: current row
		{"Changes.Open", 0, "applying/-/-/-"},
		{"Prices.PriceAt", 0, "applying/-/-/-"},
		{"prov.ScheduleAtPeriodEnd", 0, "applying/-/-/-"},
		{"Schedules.Get", 1, "applying/-/-/-"}, // the final re-read
	} {
		e.f.fail(step.name, step.skip)
		e.failRenewal(sch, step.want)
		if t.Failed() {
			t.Fatalf("step %s", step.name)
		}
	}
	// I-11 down more than an hour before R: retried, never reduced blind.
	e.fakes.Core.Err = domain.NewError(domain.ErrIAMUnavailable, "down")
	e.failRenewal(sch, "applying/-/-/-")
	// The provider adapter no longer loaded.
	e.fakes.Core.Err = nil
	stripe := e.svc.Providers
	e.unload()
	e.failRenewal(sch, "applying/-/-/-")
	e.svc.Providers = stripe

	e.run(e.svc.RunSeatRenewal)
	if got := e.schedule(sch); got != "applying/5/-/-" {
		t.Fatalf("applied ahead: %s", got)
	}
	if n := len(e.prov.called("ScheduleAtPeriodEnd price_pro_month x5 sched:" + sch + ":apply")); n < 1 {
		t.Fatalf("provider schedule %v", e.prov.called("ScheduleAtPeriodEnd"))
	}
	if q := scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub); q != 10 {
		t.Fatalf("seats changed before the renewal: %d", q)
	}
}

// §8.6.2: I-11 still down within the last hour before R → the reduction is
// skipped with SEV-3 (the renewal bills the current count); a failed
// re-read leaves it for the next run.
func TestSeatRenewalSkipsWhenSeatUsageUnavailableNearRenewal(t *testing.T) {
	e := newEnv(t)
	_, _, sch, r := e.reduction(10, 3, 5)
	e.now = r.Add(-30 * time.Minute)
	e.fakes.Core.Err = domain.NewError(domain.ErrIAMUnavailable, "down")
	e.f.fail("Schedules.Get", 1) // the re-read before marking it skipped
	e.failRenewal(sch, "applying/-/-/-")
	e.run(e.svc.RunSeatRenewal)
	if got := e.schedule(sch); got != "skipped/-/-/iam_unavailable" {
		t.Fatalf("schedule %s", got)
	}
	if !e.log.has("ERROR seat reduction skipped: seat usage unavailable before renewal") {
		t.Fatal("SEV-3 not logged")
	}
}

// §8.6.2: a subscription that no longer renews (cancel at period end) skips
// its reduction with subscription_not_active; nothing is sent to the provider.
func TestSeatRenewalSkipsSubscriptionThatDoesNotRenew(t *testing.T) {
	e := newEnv(t)
	_, sub, sch, r := e.reduction(10, 3, 5)
	e.exec(`UPDATE subscriptions SET auto_renew = false WHERE billing_subscription_id = $1`, sub)
	e.now = r.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	if got := e.schedule(sch); got != "skipped/-/-/subscription_not_active" {
		t.Fatalf("schedule %s", got)
	}
	if n := len(e.prov.called("ScheduleAtPeriodEnd")); n != 0 {
		t.Fatalf("provider called %d times", n)
	}
}

// BIL-SUB-8 / §8.16: the renewal quantity is scheduled on the price row
// effective at R; with no row for the key at R the current row is kept.
func TestSeatRenewalKeepsCurrentPriceWhenNoRowAtRenewal(t *testing.T) {
	e := newEnv(t)
	_, sub, sch, r := e.reduction(10, 3, 5)
	e.exec(`INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, effective_to, created_by, reason)
		VALUES ('pro','month','CHF',2500,'stripe','price_chf_ending','2026-01-01',$1,'00000000-0000-0000-0000-0000000000c1','test CHF row ending before R')`, r.Add(-time.Hour))
	e.exec(`UPDATE subscription_items SET price_id = (SELECT price_id FROM plan_prices WHERE provider_price_id = 'price_chf_ending')
		WHERE billing_subscription_id = $1`, sub)
	e.now = r.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	if n := len(e.prov.called("ScheduleAtPeriodEnd price_chf_ending x5 sched:" + sch + ":apply")); n != 1 {
		t.Fatalf("schedule %v", e.prov.called("ScheduleAtPeriodEnd"))
	}
}

// renewed moves the subscription into the next period (what the cycle
// invoice.paid does) so completeRenewal runs.
func (e *env) renewed(sub string, r time.Time) {
	e.t.Helper()
	e.exec(`UPDATE subscriptions SET current_period_start = $2, current_period_end = $3 WHERE billing_subscription_id = $1`, sub, r, r.AddDate(0, 1, 0))
}

// §8.6.3: completing the reduction after the renewal — every failure
// (I-11, the top-up charge, each write under the lock) leaves the schedule
// applying; the run then applies the final count. Members added in the
// lead window are charged as a renewal top-up (paid here); the open top-up
// row and its key are reused on retry.
func TestSeatRenewalCompletionRetriesEachFailure(t *testing.T) {
	e := newEnv(t)
	tenant, sub, sch, r := e.reduction(10, 3, 5)
	e.now = r.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	e.renewed(sub, r)
	e.now = r.Add(10 * time.Minute)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 7, LicensedSeats: 10}

	e.f.fail("Subs.Get", 0)
	e.failRenewal(sch, "applying/5/-/-")
	e.fakes.Core.Err = domain.NewError(domain.ErrIAMUnavailable, "down")
	e.failRenewal(sch, "applying/5/-/-")
	e.fakes.Core.Err = nil
	e.f.fail("SeatPurchases.Open", 0)
	e.failRenewal(sch, "applying/5/-/-")
	e.f.fail("SeatPurchases.Insert", 0)
	e.failRenewal(sch, "applying/5/-/-")
	if n := scalar[int](t, `SELECT count(*) FROM seat_purchases WHERE billing_subscription_id = $1`, sub); n != 0 {
		t.Fatalf("top-up rows after failed inserts: %d", n)
	}
	// A provider failure that is not a payment outcome: the row stays open
	// and the retry reuses it and its key.
	e.f.fail("prov.ChangeQuantity", 0)
	e.failRenewal(sch, "applying/5/-/-")
	for _, step := range []struct {
		name string
		skip int
	}{
		{"Schedules.Get", 0}, {"Subs.Item", 0}, {"Subs.SetItem", 0}, {"Schedules.Update", 0},
		{"SeatPurchases.Get", 0}, {"SeatPurchases.Update", 0}, {"Subs.Get", 1}, {"Subs.AppendHistory", 0},
	} {
		e.f.fail(step.name, step.skip)
		e.failRenewal(sch, "applying/5/-/-")
		if t.Failed() {
			t.Fatalf("step %s", step.name)
		}
	}
	e.run(e.svc.RunSeatRenewal)
	if got := e.schedule(sch); got != "applied/5/7/-" {
		t.Fatalf("schedule %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM seat_purchases WHERE billing_subscription_id = $1 AND reason = 'renewal_topup'`, sub); n != 1 {
		t.Fatalf("top-up rows %d, want one reused row", n)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||seats_added||'/'||idempotency_key FROM seat_purchases WHERE billing_subscription_id = $1`, sub); got != "confirmed/2/topup:"+sch {
		t.Fatalf("top-up %s", got)
	}
	for _, c := range e.prov.called("ChangeQuantity") {
		if c != "ChangeQuantity 5->7 topup:"+sch {
			t.Fatalf("top-up call %s", c)
		}
	}
	if e.met.get("seat_change:renewal_topup:paid") == 0 {
		t.Fatalf("metric %v", e.met.counts)
	}
	if !e.log.has("INFO renewal above the scheduled target") {
		t.Fatal("above-target renewal not logged")
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'reason'||'/'||(payload->'data'->>'licensed_seats') FROM outbox_events
		WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged' ORDER BY ordering_seq DESC LIMIT 1`, tenant); got != "renewal_topup/7" {
		t.Fatalf("event %s", got)
	}
}

// §8.6.3: when the final count equals the seats already billed and no
// top-up was needed, the schedule is applied without a TenantSeatsChanged.
func TestSeatRenewalCompletionWithoutChangeEmitsNoEvent(t *testing.T) {
	e := newEnv(t)
	tenant, sub, sch, r := e.reduction(10, 3, 5)
	e.now = r.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	e.renewed(sub, r)
	e.exec(`UPDATE subscription_items SET quantity = 5 WHERE billing_subscription_id = $1`, sub)
	before := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'`, tenant)
	e.now = r.Add(10 * time.Minute)
	e.run(e.svc.RunSeatRenewal)
	if got := e.schedule(sch); got != "applied/5/5/-" {
		t.Fatalf("schedule %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'`, tenant); n != before {
		t.Fatalf("events %d → %d", before, n)
	}
}

// §8.6.3 step 1: an open customer seat purchase from the old period cannot
// complete any more and is cancelled when the renewal top-up is recorded;
// a failure cancelling it leaves everything for the next run.
func TestSeatRenewalTopUpCancelsOpenCustomerPurchase(t *testing.T) {
	e := newEnv(t)
	tenant, sub, sch, r := e.reduction(10, 3, 5)
	e.now = r.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	e.renewed(sub, r)
	e.exec(`INSERT INTO seat_purchases (purchase_id, tenant_id, billing_subscription_id, reason, seats_added, seats_before, seats_after,
		proration_instant, amount_minor, currency, amount_source, status, requested_by, idempotency_key)
		VALUES (gen_random_uuid(), $1, $2, 'purchase', 1, 10, 11, now(), 2300, 'USD', 'provider_preview', 'pending_payment', $3, 'k-open-cust')`,
		tenant, sub, owner.UserID)
	e.now = r.Add(10 * time.Minute)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 8, LicensedSeats: 10}
	e.f.fail("SeatPurchases.Update", 0)
	e.failRenewal(sch, "applying/5/-/-")
	if st := scalar[string](t, `SELECT status::text FROM seat_purchases WHERE idempotency_key = 'k-open-cust'`); st != "pending_payment" {
		t.Fatalf("customer purchase after a failed cancel: %s", st)
	}
	e.prov.change = []port.PaymentOutcome{{Status: port.PaymentPending}}
	e.run(e.svc.RunSeatRenewal)
	if st := scalar[string](t, `SELECT status::text FROM seat_purchases WHERE idempotency_key = 'k-open-cust'`); st != "cancelled" {
		t.Fatalf("customer purchase %s", st)
	}
	if got := e.schedule(sch); got != "applied/5/8/-" {
		t.Fatalf("schedule %s", got)
	}
}

// §8.6.3 on the manual provider: the top-up is recorded without a provider
// charge (invoiced outside the product); an unloaded provider adapter
// fails the Stripe top-up for a retry.
func TestSeatRenewalTopUpOnManualAndUnloadedProvider(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant := newTenant()
	res, err := e.svc.StubConversion(ctx, operator, tenant, app.StubConversionRequest{Plan: "pro", Interval: "month", LicensedSeats: 10,
		Reason: "manual invoicing agreed with the customer"})
	if err != nil {
		t.Fatal(err)
	}
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 3, LicensedSeats: 10}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 5}); err != nil {
		t.Fatal(err)
	}
	r := e.periodEnd(res.BillingSubscriptionID)
	e.now = r.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	e.renewed(res.BillingSubscriptionID, r)
	e.now = r.Add(10 * time.Minute)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 6, LicensedSeats: 10}
	e.run(e.svc.RunSeatRenewal)
	if got := scalar[string](t, `SELECT status||'/'||final_seats FROM seat_change_schedules WHERE billing_subscription_id = $1`, res.BillingSubscriptionID); got != "applied/6" {
		t.Fatalf("schedule %s", got)
	}
	if e.met.get("seat_change:renewal_topup:manual") != 1 || len(e.prov.called("ChangeQuantity")) != 0 {
		t.Fatalf("manual top-up %v %v", e.met.counts, e.prov.called("ChangeQuantity"))
	}

	// Stripe subscription, adapter unloaded at the top-up.
	_, sub, sch, r2 := e.reduction(10, 3, 5)
	e.now = r2.Add(-2 * time.Hour)
	e.run(e.svc.RunSeatRenewal)
	e.renewed(sub, r2)
	e.now = r2.Add(10 * time.Minute)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 6, LicensedSeats: 10}
	e.unload()
	e.failRenewal(sch, "applying/5/-/-")
}
