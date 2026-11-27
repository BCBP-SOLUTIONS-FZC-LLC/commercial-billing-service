//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

const testUser = "00000000-0000-0000-0000-0000000000c1"

// gbpBook gives sub a test-only GBP price book whose row changes at R-24h
// (old → new, new cheaper when decrease) and moves the item to the old row.
func (e *env) gbpBook(sub string, r time.Time, oldMinor, newMinor int64) (oldID, newID string) {
	e.t.Helper()
	e.exec(`INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, effective_to, created_by, reason)
		VALUES ('starter','month','GBP',$2,'stripe','price_gbp_old','2026-01-01',$1,$4,'test GBP row'),
		       ('starter','month','GBP',$3,'stripe','price_gbp_new',$1,NULL,$4,'test GBP row')`, r.Add(-24*time.Hour), oldMinor, newMinor, testUser)
	oldID = scalar[string](e.t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_gbp_old'`)
	newID = scalar[string](e.t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_gbp_new'`)
	e.exec(`UPDATE subscription_items SET price_id = $2 WHERE billing_subscription_id = $1`, sub, oldID)
	return oldID, newID
}

func (e *env) scheduledPrice(sub string) string {
	e.t.Helper()
	return scalar[string](e.t, `SELECT coalesce(scheduled_price_id::text,'-') FROM subscription_items WHERE billing_subscription_id = $1`, sub)
}

// §8.16 / BIL-SUB-8: every failure scheduling the renewal price (reads in
// the transaction, the provider, the writes under the lock) leaves nothing
// scheduled and is retried; the run then schedules a decrease with the
// renewal quantity of the open seat reduction.
func TestRepricingRetriesEachFailureAndUsesReductionQuantity(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.f.fail("Jobs.LiveSubscriptions", 0)
	e.runErr(e.svc.RunRepricing)
	e.f.clear()

	tenant, sub := e.active("starter", 10)
	r := e.periodEnd(sub)
	_, newID := e.gbpBook(sub, r, 1200, 1000)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 3, LicensedSeats: 10}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 6}); err != nil {
		t.Fatal(err)
	}
	e.now = r.Add(-5 * time.Hour)
	e.run(e.svc.RunSeatRenewal) // applied ahead: renewal quantity 6

	for _, step := range []struct {
		name string
		skip int
	}{
		{"Subs.Get", 0}, {"Subs.Item", 0}, {"Prices.Get", 0}, {"Prices.PriceAt", 0}, {"Schedules.Open", 0},
		{"prov.ScheduleAtPeriodEnd", 0}, {"Subs.Item", 1}, {"Subs.SetItem", 0}, {"Subs.Get", 1},
	} {
		e.f.fail(step.name, step.skip)
		if res := e.run(e.svc.RunRepricing); res.Failed != 1 {
			t.Fatalf("step %s: %+v", step.name, res)
		}
		if got := e.scheduledPrice(sub); got != "-" {
			t.Fatalf("step %s: scheduled %s", step.name, got)
		}
		e.f.clear()
	}
	stripe := e.svc.Providers
	e.unload()
	if res := e.run(e.svc.RunRepricing); res.Failed != 1 {
		t.Fatalf("unloaded: %+v", res)
	}
	e.svc.Providers = stripe
	if !e.log.has("WARN renewal-repricing: subscription not repriced") {
		t.Fatal("failures not logged")
	}

	e.run(e.svc.RunRepricing)
	if got := e.scheduledPrice(sub); got != newID {
		t.Fatalf("scheduled %s", got)
	}
	if n := len(e.prov.called("ScheduleAtPeriodEnd price_gbp_new x6 reprice:" + sub + ":" + newID)); n < 1 {
		t.Fatalf("schedule %v", e.prov.called("ScheduleAtPeriodEnd"))
	}
	// Recorded on each attempt that reached the provider schedule.
	if e.met.get("repricing:decrease") == 0 {
		t.Fatalf("metric %v", e.met.counts)
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1 AND cause = 'job:renewal-repricing'`, sub); n != 1 {
		t.Fatalf("history %d", n)
	}
}

// §8.16: a scheduled price that is no longer the row effective at R (the
// current row is) is cleared; a subscription that does not renew is
// skipped; the job pages past a full batch and stays idempotent.
func TestRepricingClearsStaleScheduleSkipsNonRenewingAndPages(t *testing.T) {
	e := newEnv(t)
	_, sub := e.active("starter", 4)
	r := e.periodEnd(sub)
	ent := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_enterprise_month'`)
	e.exec(`UPDATE subscription_items SET scheduled_price_id = $2, scheduled_price_at = $3 WHERE billing_subscription_id = $1`, sub, ent, r)
	e.now = r.Add(-5 * time.Hour)
	e.jobs.firstPage = fullPage(scalar[string](t, `SELECT tenant_id::text FROM subscriptions WHERE billing_subscription_id = $1`, sub), sub, e.now, true)
	res := e.run(e.svc.RunRepricing)
	if res.Handled != 200 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if len(e.jobs.afters) != 2 || e.jobs.afters[1] != sub {
		t.Fatalf("pages after %q", e.jobs.afters)
	}
	if got := e.scheduledPrice(sub); got != "-" {
		t.Fatalf("stale schedule kept: %s", got)
	}
	if n := len(e.prov.called("ScheduleAtPeriodEnd price_starter_month")); n != 1 {
		t.Fatalf("provider schedule back to the current row %v", e.prov.called("ScheduleAtPeriodEnd"))
	}

	_, other := e.active("starter", 4)
	e.gbpBook(other, e.periodEnd(other), 1000, 1200)
	e.exec(`UPDATE subscriptions SET auto_renew = false WHERE billing_subscription_id = $1`, other)
	e.run(e.svc.RunRepricing)
	if got := e.scheduledPrice(other); got != "-" {
		t.Fatalf("non-renewing subscription repriced: %s", got)
	}
}

// §8.16 step 4 for the manual provider: no provider invoice drives the
// renewal, so once R has passed the job moves the item to the scheduled
// row (same plan) with a history row; a scheduled row of another plan is
// cleared without moving the price. Failures are retried.
func TestRepricingAppliesDueManualSchedule(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	stub := func(plan string) (string, string) {
		tenant := newTenant()
		res, err := e.svc.StubConversion(ctx, operator, tenant, app.StubConversionRequest{Plan: plan, Interval: "month", LicensedSeats: 5,
			Reason: "manual invoicing agreed with the customer"})
		if err != nil {
			t.Fatal(err)
		}
		return tenant, res.BillingSubscriptionID
	}
	_, sub := stub("pro")
	e.exec(`INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, created_by, reason)
		VALUES ('pro','month','SGD',9900,'manual',NULL,'2026-01-01',$1,'test manual SGD row')`, testUser)
	next := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider = 'manual' AND currency = 'SGD'`)
	e.now = time.Now().UTC()
	e.exec(`UPDATE subscription_items SET scheduled_price_id = $2, scheduled_price_at = $3 WHERE billing_subscription_id = $1`, sub, next, e.now.Add(-time.Hour))
	item := func() string {
		return scalar[string](t, `SELECT price_id::text||'/'||coalesce(scheduled_price_id::text,'-') FROM subscription_items WHERE billing_subscription_id = $1`, sub)
	}
	before := item()
	for _, step := range []struct {
		name string
		skip int
	}{
		{"Prices.Get", 0}, {"Subs.Get", 1}, {"Subs.SetItem", 0},
	} {
		e.f.fail(step.name, step.skip)
		if res := e.run(e.svc.RunRepricing); res.Failed != 1 {
			t.Fatalf("step %s: %+v", step.name, res)
		}
		if got := item(); got != before {
			t.Fatalf("step %s: item %s", step.name, got)
		}
		e.f.clear()
	}
	e.run(e.svc.RunRepricing)
	if got := item(); got != next+"/-" {
		t.Fatalf("item %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1 AND cause = 'job:renewal-repricing'`, sub); n != 1 {
		t.Fatalf("history %d", n)
	}

	_, sub2 := stub("starter")
	price2 := scalar[string](t, `SELECT price_id::text FROM subscription_items WHERE billing_subscription_id = $1`, sub2)
	e.exec(`UPDATE subscription_items SET scheduled_price_id = $2, scheduled_price_at = $3 WHERE billing_subscription_id = $1`, sub2, next, e.now.Add(-time.Hour))
	e.run(e.svc.RunRepricing)
	if got := scalar[string](t, `SELECT price_id::text||'/'||coalesce(scheduled_price_id::text,'-') FROM subscription_items WHERE billing_subscription_id = $1`, sub2); got != price2+"/-" {
		t.Fatalf("other plan's row: %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1 AND cause = 'job:renewal-repricing'`, sub2); n != 0 {
		t.Fatalf("history for an unchanged price %d", n)
	}
}
