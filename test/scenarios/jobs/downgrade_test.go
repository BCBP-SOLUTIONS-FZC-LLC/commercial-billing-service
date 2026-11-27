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

// downgrade schedules enterprise → pro for a new enterprise subscription
// (§8.4.2 step 1) and returns tenant, subscription, change and R.
func (e *env) downgrade(seats int) (string, string, string, time.Time) {
	e.t.Helper()
	tenant, sub := e.active("enterprise", seats)
	e.fakes.Core.Err, e.fakes.Core.Check = nil, port.PlanChangeCheck{Allowed: true}
	if _, err := e.svc.ChangePlan(context.Background(), withKey(owner, "k-dg-"+sub[:8]), tenant,
		app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(sub)}); err != nil {
		e.t.Fatal(err)
	}
	change := scalar[string](e.t, `SELECT change_id::text FROM plan_change_requests WHERE tenant_id = $1`, tenant)
	return tenant, sub, change, e.periodEnd(sub)
}

func (e *env) change(id string) string {
	e.t.Helper()
	// The last column: whether the job's re-check stamped i18_checked_at (now).
	return scalar[string](e.t, `SELECT status::text||'/'||coalesce(array_to_string(blocker_codes, ','),'-')||'/'||(i18_checked_at = $2)
		FROM plan_change_requests WHERE change_id = $1`, id, e.now)
}

// failDowngrade runs downgrade-apply with one injected failure: the request
// is reported failed, logged, and left scheduled.
func (e *env) failDowngrade(change string) {
	e.t.Helper()
	res := e.run(e.svc.RunDowngradeApply)
	if res.Failed != 1 || res.Handled != 0 {
		e.t.Fatalf("result %+v", res)
	}
	if got := e.change(change); got != "scheduled/-/false" {
		e.t.Fatalf("change after a failed run: %s", got)
	}
	if !e.log.has("WARN downgrade-apply: request not processed") {
		e.t.Fatal("failure not logged")
	}
	e.f.clear()
}

var blocked = port.PlanChangeCheck{Allowed: false, Blockers: []port.PlanChangeBlock{{Code: "sso_idp_configured"}}}

// §8.4.2 step 2: an allowed re-check records i18_checked_at; at or past R
// the job leaves the request to the cycle invoice.paid; a failed re-read
// is retried.
func TestDowngradeApplyAllowedAndPastBoundary(t *testing.T) {
	e := newEnv(t)
	_, _, change, r := e.downgrade(5)
	e.f.fail("Changes.DueDowngrades", 0)
	e.runErr(e.svc.RunDowngradeApply)
	e.f.clear()

	e.now = r
	if res := e.run(e.svc.RunDowngradeApply); res.Handled+res.Failed != 0 {
		t.Fatalf("at the boundary: %+v", res)
	}
	if got := e.change(change); got != "scheduled/-/false" {
		t.Fatalf("at the boundary: %s", got)
	}

	e.now = r.Add(-time.Hour)
	e.f.fail("Changes.Open", 0)
	e.failDowngrade(change)
	e.run(e.svc.RunDowngradeApply)
	if got := e.change(change); got != "scheduled/-/true" {
		t.Fatalf("allowed: %s", got)
	}
}

// §8.4.2 step 2, blocked: each failure on the way (subscription read,
// adapter not loaded, provider release, the final re-read and update)
// leaves the request scheduled; the run then releases the provider
// schedule, blocks the request (logged), and clears a renewal price that
// was scheduled for the downgrade's plan (§8.16 step 2).
func TestDowngradeApplyBlockedRetriesEachFailure(t *testing.T) {
	e := newEnv(t)
	_, sub, change, r := e.downgrade(5)
	e.now = r.Add(-time.Hour)
	e.fakes.Core.Check = blocked
	pro := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_pro_month'`)
	e.exec(`UPDATE subscription_items SET scheduled_price_id = $2, scheduled_price_at = $3 WHERE billing_subscription_id = $1`, sub, pro, r)

	for _, name := range []string{"Subs.Get", "prov.ReleaseSchedule", "Changes.Open", "Changes.Update"} {
		e.f.fail(name, 0)
		e.failDowngrade(change)
		if t.Failed() {
			t.Fatalf("step %s", name)
		}
	}
	stripe := e.svc.Providers
	e.unload()
	e.failDowngrade(change)
	e.svc.Providers = stripe

	e.run(e.svc.RunDowngradeApply)
	if got := e.change(change); got != "blocked/sso_idp_configured/true" {
		t.Fatalf("blocked: %s", got)
	}
	if n := len(e.prov.called("ReleaseSchedule sched_planchg:" + change)); n == 0 {
		t.Fatalf("release %v", e.prov.called("ReleaseSchedule"))
	}
	if got := scalar[bool](t, `SELECT scheduled_price_id IS NULL AND scheduled_price_at IS NULL FROM subscription_items WHERE billing_subscription_id = $1`, sub); !got {
		t.Fatal("scheduled renewal price not cleared")
	}
	if !e.log.has("INFO scheduled downgrade blocked at the re-check") {
		t.Fatal("block not logged")
	}
	if e.met.get("downgrade_blocked:sso_idp_configured") == 0 {
		t.Fatal("metric")
	}
}

// §8.4.2 with a seat reduction applied ahead: the reduction's renewal
// quantity is scheduled on the downgrade's plan price (BIL-SUB-8); when the
// downgrade is blocked, releasing the provider schedule drops it, so the
// renewal quantity is re-scheduled on the current plan (reapply). Each
// failure there is retried.
func TestDowngradeBlockedReappliesSeatReduction(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.active("enterprise", 10)
	e.fakes.Core.Usage = port.SeatUsage{ActiveUsers: 3, LicensedSeats: 10}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-dg-reapply"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(sub)}); err != nil {
		t.Fatal(err)
	}
	change := scalar[string](t, `SELECT change_id::text FROM plan_change_requests WHERE tenant_id = $1`, tenant)
	sch := scalar[string](t, `SELECT schedule_id::text FROM seat_change_schedules WHERE billing_subscription_id = $1`, sub)
	r := e.periodEnd(sub)
	e.now = r.Add(-90 * time.Minute)
	e.run(e.svc.RunSeatRenewal)
	if n := len(e.prov.called("ScheduleAtPeriodEnd price_pro_month x5 sched:" + sch + ":apply")); n != 1 {
		t.Fatalf("reduction on the downgrade's price: %v", e.prov.called("ScheduleAtPeriodEnd"))
	}

	e.fakes.Core.Check = blocked
	for _, step := range []struct {
		name string
		skip int
	}{
		{"Schedules.Open", 0}, {"Subs.Item", 0}, {"Prices.Get", 0}, {"prov.ScheduleAtPeriodEnd", 0}, {"Schedules.Get", 0},
	} {
		e.f.fail(step.name, step.skip)
		e.failDowngrade(change)
		if t.Failed() {
			t.Fatalf("step %s", step.name)
		}
	}
	e.run(e.svc.RunDowngradeApply)
	if got := e.change(change); got != "blocked/sso_idp_configured/true" {
		t.Fatalf("blocked: %s", got)
	}
	if got := scalar[string](t, `SELECT status||'/'||provider_schedule_id FROM seat_change_schedules WHERE schedule_id = $1`, sch); got != "applying/sched_sched:"+sch+":reapply" {
		t.Fatalf("reduction %s", got)
	}
	if n := len(e.prov.called("ScheduleAtPeriodEnd price_enterprise_month x5 sched:" + sch + ":reapply")); n < 1 {
		t.Fatalf("re-applied %v", e.prov.called("ScheduleAtPeriodEnd"))
	}
}

// §8.4.2: I-18 still unavailable 15 minutes before R blocks the request
// with iam_unavailable (the provider schedule is released).
func TestDowngradeBlockedWhenIAMUnavailableNearRenewal(t *testing.T) {
	e := newEnv(t)
	_, _, change, r := e.downgrade(5)
	e.fakes.Core.Err = domain.NewError(domain.ErrIAMUnavailable, "down")
	e.now = r.Add(-10 * time.Minute)
	e.run(e.svc.RunDowngradeApply)
	if got := e.change(change); got != "blocked/iam_unavailable/true" {
		t.Fatalf("blocked: %s", got)
	}
}
