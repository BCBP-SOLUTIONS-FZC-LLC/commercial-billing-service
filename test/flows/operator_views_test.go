//go:build integration

package flows_test

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// B-O4 shows drafts with refunds; B-O7 carries saga, purchases, ledger
// excerpt and account.
func TestOperatorViews(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	_, _ = e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_d1", "invoice.created", domain.KindInvoiceUpdated),
		invoiceState("in_d_"+sub, sub, "draft", "subscription_cycle", e.now, 3))
	l, err := e.svc.ListInvoicesForOperator(ctx, operator, tenant, app.InvoiceFilter{})
	if err != nil || len(l.Items) != 1 || l.Items[0].Status != "draft" {
		t.Fatalf("B-O4 %+v %v", l, err)
	}
	mustNoErr(t, sql(ctx, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, tenant_id, livemode,
		provider_created_at, payload, status) VALUES ('stripe', $1, 'invoice.paid', 'invoice_paid', $2::uuid, false, now(), '{"secret":1}', 'processed')`,
		"evt_ex_"+sub, tenant))
	d, err := e.svc.GetSubscriptionDetail(ctx, operator, tenant)
	if err != nil || d.Saga == nil || len(d.Ledger) != 1 || d.Ledger[0].ProviderEventID != "evt_ex_"+sub || d.Account == nil {
		t.Fatalf("B-O7 %+v %v", d, err)
	}
}

// §8.13: one run per provider; status mismatch and missing ledger event
// reported; completed with counts and an AL-5 cross-tenant entry.
func TestReconciliation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	start := time.Now().Add(-24 * time.Hour)
	req := app.ReconciliationRequest{Provider: "stripe", WindowStart: start, WindowEnd: time.Now(), Reason: "Monthly reconciliation"}
	run, err := e.svc.StartReconciliation(ctx, operator, req)
	if err != nil || run.Status != "queued" {
		t.Fatalf("queue %+v %v", run, err)
	}
	if _, err := e.svc.StartReconciliation(ctx, operator, req); !domain.HasCode(err, domain.ErrReconciliationRunning) {
		t.Fatalf("second run: %v", err)
	}
	e.stripe.recon = map[port.ReconKind][]domain.ProviderObjectState{
		port.ReconSubscriptions: {{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + sub}, Status: "past_due"}},
		port.ReconEvents:        {{Ref: domain.ProviderObjectRef{Kind: "event", ID: "evt_missing_" + sub}, Status: "invoice.paid"}},
	}
	// Other tests' queued runs may be claimed first; run until this one is done.
	for range 10 {
		if n, err := e.svc.RunReconciliation(ctx, 1); err != nil || n == 0 {
			break
		}
	}
	v, err := e.svc.GetReconciliationRun(ctx, operator, run.RunID)
	if err != nil || v.Status != "completed" || v.Counts["subscriptions"] != 1 {
		t.Fatalf("run %+v %v", v, err)
	}
	kinds := map[string]bool{}
	for _, f := range v.Findings {
		kinds[f.Kind] = true
		if f.Kind == "status_mismatch" && f.TenantID != tenant {
			t.Errorf("finding tenant %s", f.TenantID)
		}
	}
	if !kinds["status_mismatch"] || !kinds["missing_ledger_event"] {
		t.Fatalf("findings %+v", v.Findings)
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE entry->>'entry_type' = 'commercial.cross_tenant_job.ran'
		AND entry->'target'->>'id' = $1`, run.RunID); n != 1 {
		t.Fatalf("audit %d", n)
	}
}

// §8.8.4: offboarding voids the tenant's open invoices.
func TestOffboardingVoidsOpenInvoices(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	_, _ = e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_o1", "invoice.finalized", domain.KindInvoiceUpdated),
		invoiceState("in_open_"+sub, sub, "open", "subscription_cycle", e.now, 3))
	setLifecycle(t, tenant, "offboarded", nil)
	_, _ = e.svc.RunLifecycleFollowUps(ctx, 100)
	if len(e.stripe.called("VoidInvoice in_open_"+sub)) != 1 {
		t.Fatalf("void: %v", e.stripe.called("VoidInvoice"))
	}
}
