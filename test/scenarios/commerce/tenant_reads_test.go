//go:build integration

package commerce_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// warns counts the lifecycle follow-up failures logged for tenant.
func (e *env) warns(tenant string) int {
	e.log.mu.Lock()
	defer e.log.mu.Unlock()
	n := 0
	for _, l := range e.log.lines {
		if strings.HasPrefix(l, "WARN lifecycle follow-up failed") && strings.Contains(l, tenant) {
			n++
		}
	}
	return n
}

// B-P15 / B-P16 (BIL-PCI-2, BIL-API-5) and the payment_method_set callback
// with its follow-up (§7.4): read live, nothing for a manual account,
// provider and storage failures, and the follow-up retried on each pass
// until the provider default is set and recorded.
func TestPaymentMethodPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	pm, err := e.svc.ListPaymentMethods(ctx, owner, newTenant())
	isCode(t, "B-P15 no account", err, domain.ErrSubscriptionNotFound)
	if !pm.CanManage || pm.Items == nil {
		t.Fatalf("B-P15 shape %+v", pm)
	}
	manual, _ := e.activeSubscription(t, "pro", 1)
	exec(t, `UPDATE billing_accounts SET provider = 'manual' WHERE tenant_id = $1::uuid`, manual)
	if pm, err := e.svc.ListPaymentMethods(ctx, app.Caller{Roles: []string{domain.RoleTenantOwner}}, manual); err != nil || len(pm.Items) != 0 ||
		len(e.p.called("ListPaymentMethods")) != 0 {
		t.Fatalf("manual account %+v %v", pm, err)
	}

	tenant, sub := e.activeSubscription(t, "pro", 3)
	e.reg.hidden["stripe"] = true
	_, err = e.svc.ListPaymentMethods(ctx, owner, tenant)
	isCode(t, "B-P15 provider unloaded", err, domain.ErrProviderNotEnabled)
	_, err = e.svc.CreateSetupSession(ctx, owner, tenant)
	isCode(t, "B-P16 provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("ListPaymentMethods", answer{err: errProvider})
	_, err = e.svc.ListPaymentMethods(ctx, owner, tenant)
	isCode(t, "B-P15 provider", err, domain.ErrProviderUnavailable)
	e.p.pms[0].ProviderRef = "pm_1"
	if pm, err := e.svc.ListPaymentMethods(ctx, owner, tenant); err != nil || pm.Items[0].ID != "pm_1" || pm.Items[0].Last4 != "4242" {
		t.Fatalf("short reference %+v %v", pm, err)
	}

	_, err = e.svc.CreateSetupSession(ctx, owner, newTenant())
	isCode(t, "B-P16 no account", err, domain.ErrSubscriptionNotFound)
	e.p.noHostedPM = true
	_, err = e.svc.CreateSetupSession(ctx, owner, tenant)
	isCode(t, "B-P16 unsupported", err, domain.ErrProviderOperationUnsupported)
	e.p.noHostedPM = false
	e.fail("Checkouts.Insert", 0)
	_, err = e.svc.CreateSetupSession(ctx, owner, tenant)
	isInjected(t, "B-P16 insert", err)
	e.p.on("CreatePaymentMethodSetup", answer{err: errProvider})
	_, err = e.svc.CreateSetupSession(ctx, owner, tenant)
	isCode(t, "B-P16 provider", err, domain.ErrProviderUnavailable)
	e.fail("Checkouts.SetProviderSession", 0)
	_, err = e.svc.CreateSetupSession(ctx, owner, tenant)
	isInjected(t, "B-P16 session write", err)
	if n := count(t, `SELECT count(*) FROM checkout_sessions WHERE tenant_id = $1 AND provider_session_id LIKE $2`, tenant, domain.PendingSessionPrefix+"%"); n != 2 {
		t.Fatalf("sessions left pending %d, want 2", n)
	}
	hs, err := e.svc.CreateSetupSession(ctx, owner, tenant)
	if err != nil || hs.URL != "https://checkout.example/setup" {
		t.Fatalf("B-P16 %+v %v", hs, err)
	}
	session := scalar[string](t, `SELECT session_id::text FROM checkout_sessions WHERE tenant_id = $1 AND provider_session_id LIKE 'cs_setup_%'`, tenant)
	if n := len(e.p.called("CreatePaymentMethodSetup https://app.example/billing?payment_method=updated pmsetup:" + session)); n != 1 {
		t.Fatalf("setup call %v", e.p.called("CreatePaymentMethodSetup"))
	}

	// checkout.session.completed (payment_method_set).
	h := app.PaymentMethodSet{S: e.svc}
	ev := callback("evt_pm_"+sub, "checkout.session.completed", domain.KindPaymentMethodSet)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: "cs_setup_pmsetup:" + session}, Status: "complete"}
	unknown := cur
	unknown.Ref.ID = "cs_unknown"
	_, err = e.handle(tenant, h, ev, unknown)
	ignored(t, "unknown session", err, "unknown_session")
	for _, f := range []string{"Checkouts.ByProviderSession", "Checkouts.SetStatus"} {
		e.fail(f, 0)
		_, err := e.handle(tenant, h, ev, cur)
		isInjected(t, f, err)
	}
	exec(t, `UPDATE billing_accounts SET default_pm_set = false WHERE tenant_id = $1::uuid`, tenant)
	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:payment_method_set" {
		t.Fatalf("pm set %s %v", out, err)
	}

	// The follow-up: each failure is logged and retried on the next pass.
	applied := func() string {
		return scalar[string](t, `SELECT (default_applied_at IS NOT NULL)::text||'/'||(SELECT default_pm_set FROM billing_accounts WHERE tenant_id = $2)::text
			FROM checkout_sessions WHERE session_id = $1`, session, tenant)
	}
	e.absent("Checkouts.GetSession", 0)
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if e.warns(tenant) != 0 || applied() != "false/false" {
		t.Fatalf("vanished session: warns %d, %s", e.warns(tenant), applied())
	}
	steps := []func(){
		func() { e.fail("Checkouts.GetSession", 0) },
		func() { e.fail("Accounts.Get", 0) },
		func() { e.reg.hidden["stripe"] = true },
		func() { delete(e.reg.hidden, "stripe"); e.p.on("FetchCurrent", answer{err: errProvider}) },
		func() { e.p.on("AttachDefaultPaymentMethod", answer{err: errProvider}) },
		func() { e.fail("Checkouts.MarkDefaultApplied", 0) },
	}
	for i, arm := range steps {
		arm()
		if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
			t.Fatal(err)
		}
		if e.warns(tenant) != i+1 || applied() != "false/false" {
			t.Fatalf("step %d: warns %d, %s", i, e.warns(tenant), applied())
		}
	}
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// Two provider calls succeeded (the rolled-back pass and this one), both
	// with the session's key, so the provider applies it once.
	if applied() != "true/true" || len(e.p.called("AttachDefaultPaymentMethod pm_new pmdefault:"+session)) != 2 {
		t.Fatalf("applied %s %v", applied(), e.p.called("AttachDefault"))
	}

	// A setup abandoned inside the provider page saved nothing: the session
	// is only closed.
	_, err = e.svc.CreateSetupSession(ctx, owner, tenant)
	if err != nil {
		t.Fatal(err)
	}
	session = scalar[string](t, `SELECT session_id::text FROM checkout_sessions WHERE tenant_id = $1 AND purpose = 'payment_method_setup' AND status <> 'completed'
		AND provider_session_id LIKE 'cs_setup_%'`, tenant)
	exec(t, `UPDATE checkout_sessions SET status = 'completed' WHERE session_id = $1::uuid`, session)
	exec(t, `UPDATE billing_accounts SET default_pm_set = false WHERE tenant_id = $1::uuid`, tenant)
	e.p.setupPM = ""
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if applied() != "true/false" || len(e.p.called("AttachDefaultPaymentMethod pm_new")) != 2 {
		t.Fatalf("nothing saved: %s %v", applied(), e.p.called("AttachDefaultPaymentMethod"))
	}
}

// B-P17 / B-P18 (§5.4.1): failures and pagination; B-P24: cursor
// validation, tenant-facing cause labels, actors without identities, plan
// moves, pages; B-P26: provider first, the account unchanged on failure.
func TestInvoiceHistoryAndContactPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	start := e.now.Truncate(time.Second)
	for i := range 2 {
		id := "in_open_" + string(rune('a'+i)) + "_" + sub
		if _, err := e.handle(tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_"+id, "invoice.finalized", domain.KindInvoiceUpdated),
			invoiceState(id, sub, "open", "subscription_cycle", start.AddDate(0, i, 0), 3)); err != nil {
			t.Fatal(err)
		}
	}
	e.fail("Invoices.List", 0)
	_, err := e.svc.ListInvoices(ctx, owner, tenant, app.InvoiceFilter{})
	isInjected(t, "B-P17", err)
	page, err := e.svc.ListInvoices(ctx, owner, tenant, app.InvoiceFilter{Page: app.Page{Limit: 1}})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != page.Items[0].InvoiceID {
		t.Fatalf("B-P17 page %+v %v", page, err)
	}
	_, err = e.svc.InvoiceDownloadURL(ctx, owner, tenant, newTenant())
	isCode(t, "B-P18 unknown", err, domain.ErrInvoiceNotFound)
	e.reg.hidden["stripe"] = true
	_, err = e.svc.InvoiceDownloadURL(ctx, owner, tenant, page.Items[0].InvoiceID)
	isCode(t, "B-P18 provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("InvoicePDFLink", answer{err: errProvider})
	_, err = e.svc.InvoiceDownloadURL(ctx, owner, tenant, page.Items[0].InvoiceID)
	isCode(t, "B-P18 provider", err, domain.ErrProviderUnavailable)
	// F-3 (rev 0.71): no PDF at the provider is 404, not PROVIDER_PRICE_MISMATCH.
	e.p.on("InvoicePDFLink", answer{err: port.ErrNotFound})
	_, err = e.svc.InvoiceDownloadURL(ctx, owner, tenant, page.Items[0].InvoiceID)
	isCode(t, "B-P18 no PDF", err, domain.ErrInvoiceNotFound)

	// B-P24.
	for _, c := range []string{"abc", "-1"} {
		_, err = e.svc.SubscriptionHistory(ctx, owner, tenant, app.Page{Cursor: c})
		isCode(t, "cursor "+c, err, domain.ErrValidation)
	}
	if h, err := e.svc.SubscriptionHistory(ctx, owner, newTenant(), app.Page{}); err != nil || len(h.Items) != 0 || h.NextCursor != nil {
		t.Fatalf("no subscription %+v %v", h, err)
	}
	for _, f := range []string{"Subs.Latest", "Subs.History"} {
		e.fail(f, 0)
		_, err := e.svc.SubscriptionHistory(ctx, owner, tenant, app.Page{})
		isInjected(t, f, err)
	}
	for i, r := range []struct{ cause, actor string }{
		{"operator:B-O16", operator.UserID}, {"iam:TenantSuspended", domain.CommercialSystemPrincipal},
		{"job:seat-renewal", domain.CommercialSystemPrincipal}, {"migration", domain.CommercialSystemPrincipal},
		{"webhook:invoice.paid", owner.UserID},
	} {
		exec(t, `INSERT INTO subscription_status_history (tenant_id, billing_subscription_id, from_status, to_status, from_plan, to_plan, cause, actor_id, occurred_at)
			VALUES ($1::uuid, $2::uuid, 'active', 'active', 'pro', 'enterprise', $3, $4::uuid, now() + make_interval(secs => $5))`, tenant, sub, r.cause, r.actor, i+1)
	}
	var all []app.HistoryView
	cursor := ""
	for pages := 0; ; pages++ {
		h, err := e.svc.SubscriptionHistory(ctx, owner, tenant, app.Page{Limit: 2, Cursor: cursor})
		if err != nil || len(h.Items) == 0 || pages > 5 {
			t.Fatalf("B-P24 page %d %+v %v", pages, h, err)
		}
		all = append(all, h.Items...)
		if h.NextCursor == nil {
			break
		}
		cursor = *h.NextCursor
	}
	labels := map[string]string{}
	for _, v := range all {
		if v.FromPlan != nil && v.ToPlan != nil && *v.ToPlan == "enterprise" {
			labels[v.Cause] = v.Actor
		}
	}
	want := map[string]string{"support": "an administrator", "account": "system", "renewal": "system", "system": "system", "payment": "you"}
	for c, a := range want {
		if labels[c] != a {
			t.Fatalf("history labels %v, want %v", labels, want)
		}
	}
	if total := count(t, `SELECT count(*) FROM subscription_status_history WHERE billing_subscription_id = $1`, sub); len(all) != total {
		t.Fatalf("paged %d of %d rows", len(all), total)
	}

	// B-P26.
	_, err = e.svc.UpdateBillingContact(ctx, owner, newTenant(), app.BillingContact{RecordVersion: 1})
	isCode(t, "B-P26 no account", err, domain.ErrSubscriptionNotFound)
	c, err := e.svc.GetBillingContact(ctx, owner, tenant)
	if err != nil {
		t.Fatal(err)
	}
	upd := app.BillingContact{BillingContactEmail: ptr("billing@acme.example"), RecordVersion: c.RecordVersion}
	e.reg.hidden["stripe"] = true
	_, err = e.svc.UpdateBillingContact(ctx, owner, tenant, upd)
	isCode(t, "B-P26 provider unloaded", err, domain.ErrProviderNotEnabled)
	delete(e.reg.hidden, "stripe")
	e.p.on("UpdateCustomer", answer{err: errProvider})
	_, err = e.svc.UpdateBillingContact(ctx, owner, tenant, upd)
	isCode(t, "B-P26 provider", err, domain.ErrProviderUnavailable)
	if got, _ := e.svc.GetBillingContact(ctx, owner, tenant); got.RecordVersion != c.RecordVersion || got.BillingContactEmail != nil && *got.BillingContactEmail == "billing@acme.example" {
		t.Fatalf("account changed although the provider refused: %+v", got)
	}
}
