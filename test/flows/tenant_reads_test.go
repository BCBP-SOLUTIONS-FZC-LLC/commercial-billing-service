//go:build integration

package flows_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// B-P15 – B-P18, B-P24 – B-P26.
func TestTenantReads(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)

	pm, err := e.svc.ListPaymentMethods(ctx, owner, tenant)
	if err != nil || len(pm.Items) != 1 || pm.Items[0].ID != "pm_…1234" || !pm.CanManage {
		t.Fatalf("B-P15 %+v %v", pm, err)
	}
	hs, err := e.svc.CreateSetupSession(ctx, owner, tenant)
	if err != nil || hs.URL != "https://checkout.example/setup" {
		t.Fatalf("B-P16 %+v %v", hs, err)
	}
	cs := scalar[string](t, `SELECT provider_session_id FROM checkout_sessions WHERE tenant_id = $1 AND purpose = 'payment_method_setup'`, tenant)
	mustNoErr(t, sql(ctx, `UPDATE billing_accounts SET default_pm_set = false WHERE tenant_id = $1::uuid`, tenant))
	cur := invoiceState("x", sub, "complete", "", e.now, 0)
	cur.Ref.ID = cs
	if out, err := e.handle(t, tenant, app.PaymentMethodSet{S: e.svc}, callback("evt_pm", "checkout.session.completed", domain.KindPaymentMethodSet), cur); err != nil || out != "applied:payment_method_set" {
		t.Fatalf("pm set: %s %v", out, err)
	}
	// The follow-up makes it the provider default, outside the callback's tx.
	if scalar[bool](t, `SELECT default_pm_set FROM billing_accounts WHERE tenant_id = $1`, tenant) {
		t.Fatal("default_pm_set before the provider default was set")
	}
	_, _ = e.svc.RunLifecycleFollowUps(ctx, 100)
	session := scalar[string](t, `SELECT session_id::text FROM checkout_sessions WHERE tenant_id = $1 AND purpose = 'payment_method_setup'`, tenant)
	if len(e.stripe.called("AttachDefaultPaymentMethod pm_new pmdefault:"+session)) != 1 {
		t.Fatalf("default not set on the provider: %v", e.stripe.called("AttachDefaultPaymentMethod"))
	}
	if !scalar[bool](t, `SELECT default_pm_set AND (SELECT default_applied_at IS NOT NULL FROM checkout_sessions WHERE session_id = $2::uuid)
		FROM billing_accounts WHERE tenant_id = $1`, tenant, session) {
		t.Fatal("default_pm_set / default_applied_at not recorded")
	}

	// Invoices: a paid cycle invoice is listed; a draft is hidden; B-P18.
	start := time.Date(2026, 12, 24, 10, 0, 0, 0, time.UTC)
	_, _ = e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_i1", "invoice.finalized", domain.KindInvoiceUpdated), invoiceState("in_open1", sub, "open", "subscription_cycle", start, 10))
	_, _ = e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_i2", "invoice.created", domain.KindInvoiceUpdated), invoiceState("in_draft1", sub, "draft", "subscription_cycle", start, 10))
	l, err := e.svc.ListInvoices(ctx, owner, tenant, app.InvoiceFilter{})
	if err != nil || len(l.Items) != 1 || l.Items[0].Status != "open" {
		t.Fatalf("B-P17 %+v %v", l, err)
	}
	if url, err := e.svc.InvoiceDownloadURL(ctx, owner, tenant, l.Items[0].InvoiceID); err != nil || url != "https://pdf.example/in_open1" {
		t.Fatalf("B-P18 %s %v", url, err)
	}
	draft := scalar[string](t, `SELECT invoice_id::text FROM invoices WHERE provider_invoice_id = 'in_draft1'`)
	if _, err := e.svc.InvoiceDownloadURL(ctx, owner, tenant, draft); !domain.HasCode(err, domain.ErrInvoiceNotFinalised) {
		t.Fatalf("draft download: %v", err)
	}

	// History: actors without identities.
	h, err := e.svc.SubscriptionHistory(ctx, owner, tenant, app.Page{Limit: 10})
	if err != nil || len(h.Items) == 0 || h.Items[0].Cause != "request" {
		t.Fatalf("B-P24 %+v %v", h, err)
	}

	// Billing contact: provider first, then the account with its version.
	c, err := e.svc.GetBillingContact(ctx, owner, tenant)
	if err != nil {
		t.Fatal(err)
	}
	email := "billing@acme.example"
	upd, err := e.svc.UpdateBillingContact(ctx, owner, tenant, app.BillingContact{BillingContactEmail: &email, Country: ptr("AE"), RecordVersion: c.RecordVersion})
	if err != nil || *upd.BillingContactEmail != email || upd.RecordVersion == c.RecordVersion {
		t.Fatalf("B-P26 %+v %v", upd, err)
	}
	if len(e.stripe.called("UpdateCustomer "+email)) != 1 {
		t.Fatal("provider customer not updated")
	}
	if _, err := e.svc.UpdateBillingContact(ctx, owner, tenant, app.BillingContact{RecordVersion: c.RecordVersion}); !domain.HasCode(err, domain.ErrOptimisticLockConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if n := len(events(t, tenant)); n != 0 {
		t.Fatalf("events %d", n)
	}
}
