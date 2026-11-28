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
	// C-5 (rev 0.73): the operator's reason is kept on the run and in its audit entry.
	if got := scalar[string](t, `SELECT reason FROM reconciliation_runs WHERE run_id = $1`, run.RunID); got != "Monthly reconciliation" {
		t.Fatalf("run reason %q", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM pending_audit_entries WHERE entry->'target'->>'id' = $1
		AND entry::text LIKE '%Monthly reconciliation%'`, run.RunID); n != 1 {
		t.Fatalf("reason not in the audit entry")
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

// F-10 (rev 0.71, §8.13): a live subscription whose provider quantity or
// price differs from Billing's is reported (seats_mismatch, plan_mismatch),
// not repaired; a subscription with a change in flight is not; a mirrored
// invoice whose amounts differ from the provider's is invoice_mismatch.
func TestReconciliationSeatPlanAndInvoiceMismatch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	busy, busySub := e.activeSubscription(t, "pro", 3)
	mustNoErr(t, sql(ctx, `INSERT INTO seat_purchases (purchase_id, tenant_id, billing_subscription_id, seats_added, seats_before, seats_after,
		proration_instant, amount_minor, currency, amount_source, status, requested_by, idempotency_key)
		VALUES (gen_random_uuid(), $1, $2, 2, 3, 5, now(), 100, 'USD', 'provider_preview', 'pending_payment', '00000000-0000-0000-0000-0000000000c1', 'k-recon-busy')`,
		busy, busySub))
	inv := invoiceState("in_recon_"+sub, sub, "open", "subscription_cycle", e.now, 3)
	if _, err := e.handle(t, tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_recon_inv", "invoice.finalized", domain.KindInvoiceUpdated), inv); err != nil {
		t.Fatal(err)
	}
	due := scalar[int64](t, `SELECT amount_due_minor FROM invoices WHERE provider_invoice_id = $1`, "in_recon_"+sub)
	// F-11 (rev 0.71, §4.2.14): a cycle invoice is linked to its subscription.
	if got := scalar[string](t, `SELECT l.ref_type||'/'||l.ref_id::text FROM invoice_links l JOIN invoices i USING (invoice_id)
		WHERE i.provider_invoice_id = $1`, "in_recon_"+sub); got != "subscription/"+sub {
		t.Fatalf("invoice link %s", got)
	}
	live := func(id string) domain.ProviderObjectState {
		return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + id}, Status: "active",
			Fields: map[string]any{"quantity": int64(5), "price": "price_other"}}
	}
	e.stripe.recon = map[port.ReconKind][]domain.ProviderObjectState{
		port.ReconSubscriptions: {live(sub), live(busySub)},
		port.ReconInvoices: {{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_recon_" + sub}, Status: "open",
			Fields: map[string]any{"customer": "cus_" + tenant, "amount_due_minor": due + 100, "amount_paid_minor": int64(0), "currency": "USD"}}},
	}
	run, err := e.svc.StartReconciliation(ctx, operator, app.ReconciliationRequest{Provider: "stripe", WindowStart: time.Now().Add(-24 * time.Hour),
		WindowEnd: time.Now(), Reason: "Monthly reconciliation"})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if n, err := e.svc.RunReconciliation(ctx, 1); err != nil || n == 0 {
			break
		}
	}
	v, err := e.svc.GetReconciliationRun(ctx, operator, run.RunID)
	if err != nil || v.Status != "completed" {
		t.Fatalf("run %+v %v", v, err)
	}
	got := map[string]string{}
	for _, f := range v.Findings {
		if f.TenantID == busy {
			t.Errorf("a change in flight was reported: %+v", f)
		}
		if f.TenantID == tenant {
			got[f.Kind] = f.Kind
		}
	}
	for _, k := range []string{"seats_mismatch", "plan_mismatch", "invoice_mismatch"} {
		if got[k] == "" {
			t.Errorf("no %s finding in %+v", k, v.Findings)
		}
	}
}
