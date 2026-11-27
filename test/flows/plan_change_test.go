//go:build integration

package flows_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func withKey(c app.Caller, key string) app.Caller { c.IdempotencyKey = key; return c }

// §8.4.1 upgrade: preview → change → paid now → applied, TenantPlanChanged
// {direction: upgrade, billing_ref: change_id}; stale and expired previews.
func TestUpgrade(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)

	pv, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if err != nil || pv.Direction != "upgrade" || pv.PreviewID == "" || pv.AmountDueNow.AmountMinor != 28400 {
		t.Fatalf("preview %+v %v", pv, err)
	}
	// Wrong target for the preview → 409 PREVIEW_STALE; unknown preview → 410.
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-stale-01"), tenant, app.PlanChangeRequest{TargetPlan: "starter", RecordVersion: e.version(t, sub), PreviewID: pv.PreviewID}); err == nil {
		t.Fatal("downgrade with an upgrade preview accepted without I-18")
	}
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-exp-0001"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: e.version(t, sub), PreviewID: newTenant()}); !domain.HasCode(err, domain.ErrPreviewExpired) {
		t.Fatalf("unknown preview: %v", err)
	}
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-ver-0001"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: 999, PreviewID: pv.PreviewID}); !domain.HasCode(err, domain.ErrOptimisticLockConflict) {
		t.Fatalf("version: %v", err)
	}
	res, err := e.svc.ChangePlan(ctx, withKey(owner, "k-upgrade-1"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: e.version(t, sub), PreviewID: pv.PreviewID})
	if err != nil || res.Status != "applied" || res.Direction != "upgrade" {
		t.Fatalf("upgrade %+v %v", res, err)
	}
	if len(e.stripe.called("ChangePrice price_enterprise_month planchg:"+res.ChangeID)) != 1 {
		t.Fatalf("provider key: %v", e.stripe.called("ChangePrice"))
	}
	if got := scalar[string](t, `SELECT s.plan::text||'/'||p.plan::text FROM subscriptions s JOIN subscription_items i USING (billing_subscription_id)
		JOIN plan_prices p ON p.price_id = i.price_id WHERE s.billing_subscription_id = $1`, sub); got != "enterprise/enterprise" {
		t.Fatalf("plan/price %s", got)
	}
	ev := scalar[string](t, `SELECT payload->'data'->>'direction'||'/'||(payload->'data'->>'billing_ref')||'/'||(payload->'data'->>'previous_plan')
		FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantPlanChanged'`, tenant)
	if ev != "upgrade/"+res.ChangeID+"/pro" {
		t.Fatalf("event %s", ev)
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1 AND to_plan = 'enterprise'`, sub); n != 1 {
		t.Fatalf("history %d", n)
	}
	// Same plan → 409.
	if _, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"}); !domain.HasCode(err, domain.ErrSamePlan) {
		t.Fatalf("same plan: %v", err)
	}
}

// 3-D Secure on the proration: pending_payment with client_action, then
// invoice.paid applies it; a decline leaves the plan unchanged.
func TestUpgradeAsyncAndDeclined(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "starter", 3)
	pv, _ := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "pro"})
	e.stripe.change = []confirmAnswer{{err: port.ErrActionRequired{ClientSecret: "pi_s"}}}
	res, err := e.svc.ChangePlan(ctx, withKey(owner, "k-3ds-0001"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub), PreviewID: pv.PreviewID})
	if err != nil || res.Status != "pending_payment" || res.ClientAction == nil || res.ClientAction.ClientSecret != "pi_s" {
		t.Fatalf("3ds %+v %v", res, err)
	}
	// Another change while pending → 409 PLAN_CHANGE_PENDING.
	pv2, _ := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "enterprise"})
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-second-1"), tenant, app.PlanChangeRequest{TargetPlan: "enterprise", RecordVersion: e.version(t, sub), PreviewID: pv2.PreviewID}); !domain.HasCode(err, domain.ErrPlanChangePending) {
		t.Fatalf("pending: %v", err)
	}
	// The proration invoice is paid later.
	inv := scalar[string](t, `SELECT provider_invoice_id FROM plan_change_requests WHERE change_id = $1`, res.ChangeID)
	paid := domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_up", EventType: "invoice.paid", Kind: domain.KindInvoicePaid}
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: inv}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_update", "currency": "USD",
			"amount_paid_minor": scalar[int64](t, `SELECT amount_minor FROM plan_change_requests WHERE change_id = $1`, res.ChangeID)}}
	h := app.FirstInvoicePaid{S: e.svc, Next: app.UpgradeInvoicePaid{S: e.svc, Next: app.DowngradeOnCycle{S: e.svc}}}
	if out, err := e.handle(t, tenant, h, paid, cur); err != nil || out != "applied:plan_upgrade" {
		t.Fatalf("invoice.paid: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT plan::text FROM subscriptions WHERE billing_subscription_id = $1`, sub); got != "pro" {
		t.Fatalf("plan %s", got)
	}

	// Decline: 402, request failed, plan unchanged, no event.
	tenant2, sub2 := e.activeSubscription(t, "starter", 3)
	pv3, _ := e.svc.PreviewPlanChange(ctx, owner, tenant2, app.PlanChangePreviewRequest{TargetPlan: "pro"})
	e.stripe.change = []confirmAnswer{{err: port.ErrDeclined{Code: "card_declined"}}}
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-decl-001"), tenant2, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub2), PreviewID: pv3.PreviewID}); !domain.HasCode(err, domain.ErrPaymentDeclined) {
		t.Fatalf("declined: %v", err)
	}
	if got := scalar[string](t, `SELECT plan::text FROM subscriptions WHERE billing_subscription_id = $1`, sub2); got != "starter" {
		t.Fatalf("plan after decline %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantPlanChanged'`, tenant2); n != 0 {
		t.Fatalf("events after decline %d", n)
	}
}

// §8.4.2 downgrade: I-18 blocked → 409 with blockers; I-18 down → 503;
// allowed → scheduled at period end, no event; B-P5 cancels; the cycle
// invoice.paid of the new period applies it with TenantPlanChanged.
func TestDowngrade(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "enterprise", 5)
	e.core.check = port.PlanChangeCheck{Allowed: false, Blockers: []port.PlanChangeBlock{{Code: "sso_idp_configured", Detail: "1 IdP"}}}
	_, err := e.svc.ChangePlan(ctx, withKey(owner, "k-blk-0001"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub)})
	if !domain.HasCode(err, domain.ErrPlanChangeBlocked) {
		t.Fatalf("blocked: %v", err)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||array_to_string(blocker_codes, ',') FROM plan_change_requests WHERE tenant_id = $1`, tenant); got != "blocked/sso_idp_configured" {
		t.Fatalf("blocked row %s", got)
	}
	e.core.err = domain.NewError(domain.ErrIAMUnavailable, "down")
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-down-001"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub)}); !domain.HasCode(err, domain.ErrIAMUnavailable) {
		t.Fatalf("I-18 down: %v", err)
	}
	e.core.err, e.core.check = nil, port.PlanChangeCheck{Allowed: true}

	res, err := e.svc.ChangePlan(ctx, withKey(owner, "k-dg-00001"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub)})
	if err != nil || res.Status != "scheduled" || res.EffectiveAt == nil {
		t.Fatalf("scheduled %+v %v", res, err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("event on schedule %d", n)
	}
	// B-P5 cancels it (release on the provider).
	if err := e.svc.CancelPlanChange(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	if len(e.stripe.called("ReleaseSchedule")) != 1 {
		t.Fatal("schedule not released")
	}
	if err := e.svc.CancelPlanChange(ctx, owner, tenant); !domain.HasCode(err, domain.ErrPlanChangeNotFound) {
		t.Fatalf("nothing to cancel: %v", err)
	}

	// Schedule again; the renewal's cycle invoice applies it.
	res, err = e.svc.ChangePlan(ctx, withKey(owner, "k-dg-00002"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub)})
	if err != nil {
		t.Fatal(err)
	}
	cycle := domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_cycle", EventType: "invoice.paid", Kind: domain.KindInvoicePaid}
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_cycle"}, Status: "paid",
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": "subscription_cycle", "line_period_start": *res.EffectiveAt,
			"line_period_end": res.EffectiveAt.AddDate(0, 1, 0)}}
	h := app.FirstInvoicePaid{S: e.svc, Next: app.UpgradeInvoicePaid{S: e.svc, Next: app.DowngradeOnCycle{S: e.svc}}}
	if out, err := e.handle(t, tenant, h, cycle, cur); err != nil || out != "applied:plan_downgrade" {
		t.Fatalf("cycle: %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'direction'||'/'||(payload->'data'->>'plan') FROM outbox_events WHERE tenant_id = $1`, tenant); got != "downgrade/pro" {
		t.Fatalf("event %s", got)
	}
}
