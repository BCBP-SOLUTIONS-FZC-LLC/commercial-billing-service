//go:build integration

package commerce_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func version(t *testing.T, sub string) int64 {
	t.Helper()
	return scalar[int64](t, `SELECT record_version FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func plan(t *testing.T, sub string) string {
	t.Helper()
	return scalar[string](t, `SELECT s.plan::text||'/'||p.plan::text FROM subscriptions s JOIN subscription_items i USING (billing_subscription_id)
		JOIN plan_prices p ON p.price_id = i.price_id WHERE s.billing_subscription_id = $1`, sub)
}

// ext is a §17 problem extension of err.
func ext(err error, key string) any {
	var de *domain.DomainError
	if !errors.As(err, &de) {
		return nil
	}
	return de.Extensions[key]
}

// handlerFunc is a next handler in a callback chain.
type handlerFunc func() (string, error)

func (f handlerFunc) Handle(context.Context, string, domain.PaymentEvent, domain.ProviderObjectState) (string, error) {
	return f()
}

// failPending closes a plan change left pending by a rolled-back commit.
func failPending(t *testing.T, tenant string) {
	t.Helper()
	exec(t, `UPDATE plan_change_requests SET status = 'failed' WHERE tenant_id = $1::uuid AND status IN ('pending_payment','scheduled')`, tenant)
}

// B-P3 / B-P4 refusals (§5.4.1, §8.4): no paid subscription, not active,
// no target price, provider unloaded or without schedules, I-18 answers
// in a downgrade preview, stale and missing previews. Nothing reaches the
// provider's ChangePrice.
func TestPlanChangeRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	up := app.PlanChangePreviewRequest{TargetPlan: "enterprise"}
	_, err := e.svc.PreviewPlanChange(ctx, owner, newTenant(), up)
	isCode(t, "no subscription preview", err, domain.ErrSubscriptionRequired)
	_, err = e.svc.ChangePlan(ctx, owner, newTenant(), app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: 1})
	isCode(t, "no subscription change", err, domain.ErrSubscriptionRequired)

	tenant, sub := e.activeSubscription(t, "pro", 4)
	e.fail("Subs.Live", 0)
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isInjected(t, "live read", err)
	e.fail("Subs.Item", 0)
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isInjected(t, "item read", err)
	exec(t, `UPDATE subscriptions SET status = 'past_due', past_due_since = now() WHERE billing_subscription_id = $1::uuid`, sub)
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isCode(t, "past due", err, domain.ErrSubscriptionNotActive)
	if ext(err, "status") != "past_due" {
		t.Fatalf("status extension %v", ext(err, "status"))
	}
	exec(t, `UPDATE subscriptions SET status = 'active', past_due_since = NULL WHERE billing_subscription_id = $1::uuid`, sub)
	e.absent("Prices.PriceAt", 0)
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isCode(t, "no target price", err, domain.ErrPriceNotAvailable)
	e.reg.hidden["stripe"] = true
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isCode(t, "provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")

	// Downgrade preview: I-18's answer, effective at the period end, the
	// renewal at the target price (§8.4.2 step 1).
	e.core.check = port.PlanChangeCheck{Allowed: false, Blockers: []port.PlanChangeBlock{{Code: "sso_idp_configured", Detail: "1 IdP"}}}
	pv, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "starter"})
	if err != nil || pv.Direction != "downgrade" || pv.Effective != "period_end" || pv.Check == nil || pv.Check.Allowed ||
		len(pv.Check.Blockers) != 1 || pv.Check.Blockers[0].Code != "sso_idp_configured" || pv.PreviewID != "" {
		t.Fatalf("downgrade preview %+v %v", pv, err)
	}
	unit := scalar[int64](t, `SELECT unit_amount_minor FROM plan_prices WHERE plan = 'starter' AND interval = 'month' AND provider = 'stripe' AND currency = 'USD'
		ORDER BY effective_from DESC LIMIT 1`)
	if pv.NextRenewalAmount.AmountMinor != unit*4 || !pv.EffectiveAt.Equal(scalar[time.Time](t, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub)) {
		t.Fatalf("renewal %+v at %v", pv.NextRenewalAmount, pv.EffectiveAt)
	}
	e.core.checkErr = domain.NewError(domain.ErrIAMUnavailable, "I-18 down")
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "starter"})
	isCode(t, "I-18 down", err, domain.ErrIAMUnavailable)
	e.core.checkErr, e.core.check = nil, port.PlanChangeCheck{Allowed: true}

	e.p.on("PreviewPriceChange", answer{err: errProvider})
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isCode(t, "provider preview", err, domain.ErrProviderUnavailable)
	e.fail("Previews.Save", 0)
	_, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	isInjected(t, "preview save", err)

	v := version(t, sub)
	change := func(target, preview string) error {
		_, err := e.svc.ChangePlan(ctx, withKey(owner, "k-pc-refusal"), tenant, app.PlanChangeRequest{TargetPlan: target, RecordVersion: v, PreviewID: preview})
		return err
	}
	isCode(t, "same plan", change("pro", ""), domain.ErrSamePlan)
	e.fail("Subs.Live", 0)
	isInjected(t, "change live read", change("enterprise", ""))
	e.absent("Prices.PriceAt", 0)
	isCode(t, "change target price", change("enterprise", ""), domain.ErrPriceNotAvailable)
	e.reg.hidden["stripe"] = true
	isCode(t, "change provider unloaded", change("enterprise", ""), domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.noSchedules = true
	isCode(t, "no schedules", change("starter", ""), domain.ErrProviderOperationUnsupported)
	e.p.noSchedules = false
	isCode(t, "no preview id", change("enterprise", ""), domain.ErrValidation)
	pv, err = e.svc.PreviewPlanChange(ctx, owner, tenant, up)
	if err != nil {
		t.Fatal(err)
	}
	e.fail("Previews.Load", 0)
	isInjected(t, "preview load", change("enterprise", pv.PreviewID))
	e.fail("Changes.Open", 0)
	isInjected(t, "open change read", change("enterprise", pv.PreviewID))
	// The subscription moved since the preview: 409 PREVIEW_STALE.
	exec(t, `UPDATE subscriptions SET record_version = record_version + 1 WHERE billing_subscription_id = $1::uuid`, sub)
	v = version(t, sub)
	isCode(t, "stale preview", change("enterprise", pv.PreviewID), domain.ErrPreviewStale)
	if n := len(e.p.called("ChangePrice")); n != 0 {
		t.Fatalf("ChangePrice called on a refusal: %d", n)
	}
	if n := count(t, `SELECT count(*) FROM plan_change_requests WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("change rows on refusals %d", n)
	}
}

// §8.4.1 upgrade outcomes answered in the response (no error): declined →
// failed with decline_reason; requires_action → pending_payment with the
// client secret; a provider error → failed; pending → pending_payment
// awaiting invoice.paid, which UpgradeInvoicePaid then applies.
func TestUpgradeOutcomes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "starter", 3)
	upgrade := func(key string) (app.PlanChangeResult, error) {
		t.Helper()
		pv, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "pro"})
		if err != nil {
			t.Fatal(err)
		}
		return e.svc.ChangePlan(ctx, withKey(owner, key), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: version(t, sub), PreviewID: pv.PreviewID})
	}
	status := func(id string) string {
		return scalar[string](t, `SELECT status::text||'/'||coalesce(provider_invoice_id,'')||'/'||coalesce(client_action->>'client_secret','') FROM plan_change_requests WHERE change_id = $1`, id)
	}

	e.p.on("ChangePrice", answer{outcome: port.PaymentOutcome{Status: port.PaymentDeclined, DeclineCode: "insufficient_funds"}})
	res, err := upgrade("k-up-dcl-01")
	isCode(t, "declined", err, domain.ErrPaymentDeclined)
	if ext(err, "decline_reason") != "insufficient_funds" || status(res.ChangeID) != "failed//" {
		t.Fatalf("declined %v %s", ext(err, "decline_reason"), status(res.ChangeID))
	}

	e.p.on("ChangePrice", answer{outcome: port.PaymentOutcome{Status: port.PaymentRequiresAction, ClientSecret: "pi_o_secret"}})
	res, err = upgrade("k-up-3ds-01")
	if err != nil || res.Status != "pending_payment" || res.ClientAction == nil || res.ClientAction.ClientSecret != "pi_o_secret" {
		t.Fatalf("3ds %+v %v", res, err)
	}
	if got := status(res.ChangeID); got != "pending_payment/in_planchg:"+res.ChangeID+"/pi_o_secret" {
		t.Fatalf("3ds row %s", got)
	}
	failPending(t, tenant)

	e.p.on("ChangePrice", answer{err: port.ErrActionRequired{ClientSecret: "pi_e"}})
	e.fail("Changes.Update", 0)
	res, err = upgrade("k-up-3ds-02")
	isInjected(t, "3ds record", err)
	if got := status(res.ChangeID); got != "pending_payment//" {
		t.Fatalf("3ds row after a failed record %s", got)
	}
	failPending(t, tenant)

	e.p.on("ChangePrice", answer{err: errProvider})
	res, err = upgrade("k-up-err-01")
	isCode(t, "provider error", err, domain.ErrProviderUnavailable)
	if got := status(res.ChangeID); got != "failed//" {
		t.Fatalf("provider error row %s", got)
	}

	e.p.on("ChangePrice", answer{outcome: port.PaymentOutcome{Status: port.PaymentPending}})
	res, err = upgrade("k-up-pnd-01")
	if err != nil || res.Status != "pending_payment" || res.ClientAction != nil {
		t.Fatalf("pending %+v %v", res, err)
	}
	inv := "in_planchg:" + res.ChangeID
	if got := status(res.ChangeID); got != "pending_payment/"+inv+"/" {
		t.Fatalf("pending row %s", got)
	}
	if plan(t, sub) != "starter/starter" || outbox(t, tenant, "TenantPlanChanged") != 0 {
		t.Fatal("plan moved before payment")
	}

	// invoice.paid for the proration invoice (UpgradeInvoicePaid alone).
	h := app.UpgradeInvoicePaid{S: e.svc}
	ev := callback("evt_up_paid", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: inv}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update"}}
	other := cur
	other.Ref.ID = "in_unrelated"
	_, err = e.handle(tenant, h, ev, other)
	ignored(t, "unrelated invoice", err, "no_handler")
	for _, f := range []string{"Lock.LockTenant", "Changes.ByProviderInvoice", "Subs.Get", "Subs.Item", "Prices.Get", "Subs.Update"} {
		e.fail(f, 0)
		_, err := e.handle(tenant, h, ev, cur)
		isInjected(t, f, err)
	}
	e.absent("Prices.PriceAt", 0)
	_, err = e.handle(tenant, h, ev, cur)
	isCode(t, "target price", err, domain.ErrPriceNotAvailable)
	if plan(t, sub) != "starter/starter" || status(res.ChangeID) != "pending_payment/"+inv+"/" {
		t.Fatal("a failed callback left a change behind")
	}
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:plan_upgrade" {
		t.Fatalf("paid %s %v", out, err)
	}
	if plan(t, sub) != "pro/pro" || outbox(t, tenant, "TenantPlanChanged") != 1 {
		t.Fatalf("after paid %s", plan(t, sub))
	}
	_, err = e.handle(tenant, h, ev, cur)
	ignored(t, "repeat", err, "change_not_pending")
}

// §8.4 apply step paid in the request: every write of the apply (plan,
// price item, request, history) failing rolls the whole apply back and no
// TenantPlanChanged is emitted; then it applies, with a generated key when
// the client sent none.
func TestUpgradeApplyRollback(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "starter", 2)
	pv, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if err != nil {
		t.Fatal(err)
	}
	req := app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: version(t, sub), PreviewID: pv.PreviewID}
	for _, f := range []struct {
		name string
		skip int
	}{{"Subs.Update", 0}, {"Subs.Item", 1}, {"Subs.SetItem", 0}, {"Changes.Update", 0}, {"Subs.AppendHistory", 0}} {
		e.fail(f.name, f.skip)
		_, err := e.svc.ChangePlan(ctx, withKey(owner, "k-apply-"+f.name), tenant, req)
		isInjected(t, f.name, err)
		if plan(t, sub) != "starter/starter" || outbox(t, tenant, "TenantPlanChanged") != 0 {
			t.Fatalf("%s: apply not rolled back", f.name)
		}
		failPending(t, tenant)
	}
	res, err := e.svc.ChangePlan(ctx, owner, tenant, req)
	if err != nil || res.Status != "applied" || res.EffectiveAt == nil {
		t.Fatalf("apply %+v %v", res, err)
	}
	if got := scalar[string](t, `SELECT left(idempotency_key, 7) FROM plan_change_requests WHERE change_id = $1`, res.ChangeID); got != "change:" {
		t.Fatalf("generated key %s", got)
	}
	if n := len(e.p.called("ChangePrice price_enterprise_month planchg:")); n != 6 {
		t.Fatalf("ChangePrice calls %d, want 6", n)
	}
	if plan(t, sub) != "enterprise/enterprise" || outbox(t, tenant, "TenantPlanChanged") != 1 {
		t.Fatalf("after apply %s", plan(t, sub))
	}
}

// §8.4.2 downgrade: storage and provider failures while scheduling; one
// open change at a time; B-P5 release failures; then the first cycle
// invoice.paid of the new period applies it (DowngradeOnCycle), all or
// nothing.
func TestDowngradePaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "enterprise", 5)
	down := func(key string) (app.PlanChangeResult, error) {
		return e.svc.ChangePlan(ctx, withKey(owner, key), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: version(t, sub)})
	}
	e.core.check = port.PlanChangeCheck{Allowed: false, Blockers: []port.PlanChangeBlock{{Code: "sso_idp_configured"}}}
	e.fail("Changes.Insert", 0)
	_, err := down("k-dg-blk-01")
	isInjected(t, "blocked row", err)
	e.core.check = port.PlanChangeCheck{Allowed: true}
	e.fail("Changes.Open", 0)
	_, err = down("k-dg-open-1")
	isInjected(t, "open read", err)
	e.fail("Changes.Insert", 0)
	_, err = down("k-dg-ins-01")
	isInjected(t, "insert", err)
	if n := count(t, `SELECT count(*) FROM plan_change_requests WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("rows after storage failures %d", n)
	}
	e.p.on("ScheduleAtPeriodEnd", answer{err: errProvider})
	_, err = down("k-dg-prv-01")
	isCode(t, "schedule", err, domain.ErrProviderUnavailable)
	if got := scalar[string](t, `SELECT status::text||'/'||(provider_schedule_id IS NULL)::text FROM plan_change_requests WHERE tenant_id = $1`, tenant); got != "failed/true" {
		t.Fatalf("failed schedule row %s", got)
	}
	res, err := down("k-dg-ok-001")
	if err != nil || res.Status != "scheduled" {
		t.Fatalf("schedule %+v %v", res, err)
	}
	_, err = down("k-dg-two-01")
	isCode(t, "second change", err, domain.ErrPlanChangePending)

	// B-P5.
	e.fail("Changes.Open", 0)
	isInjected(t, "cancel read", e.svc.CancelPlanChange(ctx, owner, tenant))
	e.reg.hidden["stripe"] = true
	isCode(t, "cancel provider unloaded", e.svc.CancelPlanChange(ctx, owner, tenant), domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("ReleaseSchedule", answer{err: errProvider})
	isCode(t, "release", e.svc.CancelPlanChange(ctx, owner, tenant), domain.ErrProviderUnavailable)
	e.fail("Changes.Open", 1)
	isInjected(t, "cancel commit", e.svc.CancelPlanChange(ctx, owner, tenant))
	if got := scalar[string](t, `SELECT status::text FROM plan_change_requests WHERE change_id = $1`, res.ChangeID); got != "scheduled" {
		t.Fatalf("change after failed cancels %s", got)
	}
	if err := e.svc.CancelPlanChange(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	if n := len(e.p.called("ReleaseSchedule sched_planchg:" + res.ChangeID + " planchg:" + res.ChangeID + ":release")); n != 3 {
		t.Fatalf("release calls %d, want 3 with one key", n)
	}

	res, err = down("k-dg-ok-002")
	if err != nil {
		t.Fatal(err)
	}
	ev := callback("evt_cycle_"+sub, "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_cycle_" + sub}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_cycle", "line_period_start": *res.EffectiveAt,
			"line_period_end": res.EffectiveAt.AddDate(0, 1, 0)}}
	h := app.DowngradeOnCycle{S: e.svc}
	for _, f := range []string{"Lock.LockTenant", "Changes.Open", "Subs.Get", "Subs.Item", "Prices.Get", "Subs.Update"} {
		e.fail(f, 0)
		_, err := e.handle(tenant, h, ev, cur)
		isInjected(t, f, err)
	}
	e.absent("Prices.PriceAt", 0)
	_, err = e.handle(tenant, h, ev, cur)
	isCode(t, "target price", err, domain.ErrPriceNotAvailable)
	// The rest of the chain failing undoes the downgrade with it.
	errNext := errors.New("cycle handler failed")
	_, err = e.handle(tenant, app.DowngradeOnCycle{S: e.svc, Next: handlerFunc(func() (string, error) { return "", errNext })}, ev, cur)
	if !errors.Is(err, errNext) {
		t.Fatalf("next error %v", err)
	}
	if plan(t, sub) != "enterprise/enterprise" || outbox(t, tenant, "TenantPlanChanged") != 0 {
		t.Fatal("downgrade applied by a failed callback")
	}
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:plan_downgrade" {
		t.Fatalf("cycle %s %v", out, err)
	}
	if plan(t, sub) != "pro/pro" || outbox(t, tenant, "TenantPlanChanged") != 1 {
		t.Fatalf("after cycle %s", plan(t, sub))
	}
}
