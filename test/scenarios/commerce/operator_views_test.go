//go:build integration

package commerce_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// B-O4: storage failures, pagination, refunds on each invoice.
func TestOperatorInvoicePaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	paid := e.mirror(t, tenant, sub, "in_paid_"+sub, "subscription_create", "paid", 20700)
	e.mirror(t, tenant, sub, "in_draft_"+sub, "subscription_cycle", "draft", 20700)
	r, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-bo4-001"), tenant, refundReq(paid, 700, "goodwill"))
	if err != nil {
		t.Fatal(err)
	}
	e.fail("Invoices.List", 0)
	_, err = e.svc.ListInvoicesForOperator(ctx, operator, tenant, app.InvoiceFilter{})
	isInjected(t, "list", err)
	e.fail("Money.RefundsForInvoice", 0)
	_, err = e.svc.ListInvoicesForOperator(ctx, operator, tenant, app.InvoiceFilter{})
	isInjected(t, "refunds", err)
	page, err := e.svc.ListInvoicesForOperator(ctx, operator, tenant, app.InvoiceFilter{Page: app.Page{Limit: 1}})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != page.Items[0].InvoiceID {
		t.Fatalf("page %+v %v", page, err)
	}
	all, err := e.svc.ListInvoicesForOperator(ctx, operator, tenant, app.InvoiceFilter{})
	if err != nil || len(all.Items) != 2 || all.NextCursor != nil {
		t.Fatalf("all %+v %v", all, err)
	}
	for _, v := range all.Items {
		switch v.Status {
		case "paid":
			if len(v.Refunds) != 1 || v.Refunds[0].RefundID != r.RefundID || v.Refunds[0].Amount.AmountMinor != 700 || v.Refunds[0].Category != "goodwill" {
				t.Fatalf("refunds %+v", v.Refunds)
			}
		case "draft":
			if len(v.Refunds) != 0 {
				t.Fatalf("draft refunds %+v", v.Refunds)
			}
		default:
			t.Fatalf("status %s", v.Status)
		}
	}
}

// B-O7: every section of the operator view (saga, open plan change, seat
// reduction, purchases, disputes, ledger excerpt without payloads), each
// read failing on its own.
func TestSubscriptionDetailPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.converted(t, "enterprise", 10)
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-bo7-dg-01"), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: version(t, sub)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 6}); err != nil {
		t.Fatal(err)
	}
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-bo7-seat1"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv.PreviewID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PurchaseTokenAddon(ctx, withKey(owner, "k-bo7-add01"), tenant, app.AddonPurchaseRequest{PackCode: "tokens_1m", PackQuantity: 1, AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.handle(tenant, app.DisputeOpened{S: e.svc}, callback("evt_bo7_dp", "charge.dispute.created", domain.KindDisputeOpened),
		disputeState("dp_bo7_"+sub, domain.ObjectDispute, map[string]any{"reason": "product_not_received", "payment_ref": "ch_9"})); err != nil {
		t.Fatal(err)
	}
	exec(t, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, tenant_id, livemode,
		provider_created_at, payload, status) VALUES ('stripe', $1, 'invoice.paid', 'invoice_paid', $2::uuid, false, now(), '{"secret":1}', 'processed')`,
		"evt_bo7_"+sub, tenant)

	e.absent("Subs.Latest", 0)
	_, err = e.svc.GetSubscriptionDetail(ctx, operator, tenant)
	isCode(t, "no subscription", err, domain.ErrSubscriptionNotFound)
	for _, f := range []string{"Subs.Latest", "Subs.Item", "Sagas.BySubscription", "Changes.Open", "Schedules.Latest", "SeatPurchases.List",
		"Addons.List", "Money.Disputes", "Ledger.Excerpt"} {
		e.fail(f, 0)
		_, err := e.svc.GetSubscriptionDetail(ctx, operator, tenant)
		isInjected(t, f, err)
	}
	d, err := e.svc.GetSubscriptionDetail(ctx, operator, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PlanChanges) != 1 || d.PlanChanges[0].Status != "scheduled" || d.PlanChanges[0].ToPlan != "pro" {
		t.Fatalf("plan changes %+v", d.PlanChanges)
	}
	if len(d.SeatChangeSchedules) != 1 || d.SeatChangeSchedules[0].TargetSeats != 6 || d.SeatChangeSchedules[0].Status != "scheduled" {
		t.Fatalf("schedules %+v", d.SeatChangeSchedules)
	}
	if len(d.SeatPurchases) != 1 || d.SeatPurchases[0].Status != "confirmed" || len(d.QuotaAddonPurchases) != 1 || d.QuotaAddonPurchases[0].PackCode != "tokens_1m" {
		t.Fatalf("purchases %+v %+v", d.SeatPurchases, d.QuotaAddonPurchases)
	}
	var disp map[string]any
	if len(d.Disputes) != 1 || json.Unmarshal(d.Disputes[0], &disp) != nil || d.Saga == nil {
		t.Fatalf("disputes %s saga %s", d.Disputes, d.Saga)
	}
	if len(d.Ledger) != 1 || d.Ledger[0].ProviderEventID != "evt_bo7_"+sub || strings.Contains(string(mustMarshal(t, d.Ledger)), "secret") {
		t.Fatalf("ledger %+v", d.Ledger)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func provSub(id string, status string, meta map[string]string) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: id}, Status: status, Metadata: meta}
}

// provInv is a provider invoice of customer (BIL-PAY-3: reconciliation maps
// the tenant from Billing's customer mapping, not the metadata).
func provInv(id, customer string, meta map[string]string) domain.ProviderObjectState {
	st := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: id}, Status: "open", Metadata: meta, Fields: map[string]any{}}
	if customer != "" {
		st.Fields["customer"] = customer
	}
	return st
}

func provEvt(id string, body *domain.PaymentEvent) domain.ProviderObjectState {
	s := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: "event", ID: id}, Status: "invoice.paid"}
	if body != nil {
		s.Fields = map[string]any{"event": *body}
	}
	return s
}

// recon queues a run for provider and processes it.
func (e *env) recon(t *testing.T, provider string, autoRepair bool) (app.ReconciliationRunView, error) {
	t.Helper()
	ctx := context.Background()
	run, err := e.svc.StartReconciliation(ctx, operator, app.ReconciliationRequest{Provider: provider, WindowStart: time.Now().Add(-time.Hour),
		WindowEnd: time.Now().Add(time.Hour), AutoRepair: autoRepair, Reason: "Reconciliation scenario run"})
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for range 10 {
		n, err := e.svc.RunReconciliation(ctx, 1)
		if err != nil {
			runErr = err
			break
		}
		if n == 0 {
			break
		}
	}
	v, err := e.svc.GetReconciliationRun(ctx, operator, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return v, runErr
}

// §8.13 with auto_repair over a paged listing: provider statuses mapped to
// Billing's, orphan and foreign objects, known and unattributable invoices,
// events with and without a body or customer, unknown-customer ledger rows;
// repairs that fail are noted on the finding and never fail the run.
func TestReconciliationAutoRepairPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, err := e.svc.StartReconciliation(ctx, operator, app.ReconciliationRequest{Provider: "stripe", WindowStart: time.Now().Add(-36 * 24 * time.Hour),
		WindowEnd: time.Now(), Reason: "Too long a window"})
	isCode(t, "window", err, domain.ErrValidation)
	e.fail("Recon.Get", 0)
	_, err = e.svc.GetReconciliationRun(ctx, operator, newTenant())
	isInjected(t, "run read", err)
	_, err = e.svc.GetReconciliationRun(ctx, operator, newTenant())
	isCode(t, "unknown run", err, domain.ErrRunNotFound)

	var tenants, subs []string
	for range 5 {
		tn, s := e.activeSubscription(t, "pro", 1)
		tenants, subs = append(tenants, tn), append(subs, s)
	}
	a := tenants[0]
	e.mirror(t, a, subs[0], "in_known_"+subs[0], "subscription_cycle", "open", 6900)
	cus := scalar[string](t, `SELECT provider_customer_id FROM billing_accounts WHERE tenant_id = $1`, a)
	exec(t, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, livemode, provider_created_at, payload, status, outcome)
		VALUES ('stripe', $1, 'invoice.paid', 'invoice_paid', false, now(), '{}', 'failed', 'failed:unknown_customer')`, "evt_nocus_"+subs[0])
	body := func(id, customer, object string) *domain.PaymentEvent {
		return &domain.PaymentEvent{Provider: "stripe", ProviderEventID: id, EventType: "invoice.paid", Kind: domain.KindInvoicePaid, CustomerRef: customer,
			Object: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: object}, ProviderCreatedAt: time.Now().UTC(), Payload: []byte(`{}`)}
	}
	x := subs[0]
	e.p.recon = map[port.ReconKind][]port.ReconPage{
		port.ReconSubscriptions: {
			{Items: []domain.ProviderObjectState{provSub("sub_"+subs[0], "trialing", nil), provSub("sub_"+subs[1], "unpaid", nil),
				provSub("sub_orphan_"+x, "active", map[string]string{"billing_subscription_id": x, "tenant_id": a}), provSub("sub_foreign_"+x, "active", nil)},
				NextCursor: "p1"},
			{Items: []domain.ProviderObjectState{provSub("sub_"+subs[2], "canceled", nil), provSub("sub_"+subs[3], "incomplete_expired", nil),
				provSub("sub_"+subs[4], "incomplete", nil)}},
		},
		port.ReconInvoices: {{Items: []domain.ProviderObjectState{provInv("in_known_"+x, cus, map[string]string{"tenant_id": a}), provInv("in_nometa_"+x, "", nil),
			provInv("in_purchase_"+x, cus, map[string]string{"purchase_id": x}), provInv("in_fetchfail_"+x, cus, map[string]string{"tenant_id": a}),
			provInv("in_missing_"+x, cus, map[string]string{"tenant_id": newTenant()})}}}, // metadata names another tenant: ignored
		port.ReconEvents: {{Items: []domain.ProviderObjectState{provEvt("evt_nobody_"+x, nil), provEvt("evt_custfail_"+x, body("evt_custfail_"+x, cus, "")),
			provEvt("evt_cust_"+x, body("evt_cust_"+x, cus, "in_missing_"+x)), provEvt("evt_anon_"+x, body("evt_anon_"+x, "", ""))}}},
	}
	e.p.fetchErr = map[string]error{"in_fetchfail_" + x: errProvider}
	e.fail("Customers.TenantForCustomer", 3) // after the three invoice lookups: the event path's lookup fails
	v, err := e.recon(t, "stripe", true)
	if err != nil || v.Status != "completed" {
		t.Fatalf("run %+v %v", v, err)
	}
	if v.Counts["subscriptions"] != 7 || v.Counts["invoices"] != 5 || v.Counts["events"] != 4 || v.Counts["findings"] != 13 || v.Counts["repaired"] != 6 {
		t.Fatalf("counts %+v", v.Counts)
	}
	type key struct{ kind, value string }
	canonical := func(raw []byte) string { // jsonb respaces and reorders keys
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	got := map[key]app.ReconciliationFinding{}
	for _, f := range v.Findings {
		got[key{f.Kind, canonical(f.ProviderValue)}] = f
	}
	expect := func(kind string, value map[string]string, repaired bool, note string) {
		t.Helper()
		b, _ := json.Marshal(value)
		f, ok := got[key{kind, string(b)}]
		if !ok || f.AutoRepaired != repaired || !strings.Contains(f.RepairNote, note) {
			t.Fatalf("finding %s %s: %+v (present %v); all %+v", kind, b, f, ok, v.Findings)
		}
	}
	expect("status_mismatch", map[string]string{"status": "unpaid"}, false, "not auto-repaired")
	expect("status_mismatch", map[string]string{"status": "canceled"}, true, "")
	expect("status_mismatch", map[string]string{"status": "incomplete_expired"}, true, "")
	expect("status_mismatch", map[string]string{"status": "incomplete"}, false, "not auto-repaired")
	expect("orphan_provider_object", map[string]string{"subscription": "sub_orphan_" + x, "status": "active"}, false, "")
	// BIL-PAY-3 (rev 0.69): the tenant is Billing's customer mapping, so an
	// invoice whose metadata names no tenant (or another one) is still
	// attributed to the customer's tenant.
	expect("invoice_missing", map[string]string{"id": "in_purchase_" + x, "status": "open"}, true, "")
	expect("invoice_missing", map[string]string{"id": "in_fetchfail_" + x, "status": "open"}, false, "repair failed")
	expect("invoice_missing", map[string]string{"id": "in_missing_" + x, "status": "open"}, true, "")
	expect("missing_ledger_event", map[string]string{"id": "evt_nobody_" + x, "status": "invoice.paid"}, false, "did not include the event body")
	expect("missing_ledger_event", map[string]string{"id": "evt_custfail_" + x, "status": "invoice.paid"}, false, "repair failed")
	expect("missing_ledger_event", map[string]string{"id": "evt_cust_" + x, "status": "invoice.paid"}, true, "")
	expect("missing_ledger_event", map[string]string{"id": "evt_anon_" + x, "status": "invoice.paid"}, true, "")
	expect("unknown_customer", map[string]string{"event": "evt_nocus_" + x}, false, "")
	if f := got[key{"missing_ledger_event", `{"id":"evt_cust_` + x + `","status":"invoice.paid"}`}]; f.TenantID != a {
		t.Fatalf("event tenant %s", f.TenantID)
	}
	if got := scalar[string](t, `SELECT string_agg(provider_event_id||':'||normalised_kind||':'||coalesce(tenant_id::text,'-'), ',' ORDER BY provider_event_id)
		FROM payment_event_ledger WHERE provider_event_id IN ($1, $2, $3)`, "evt_cust_"+x, "evt_anon_"+x, "recon:"+v.RunID+":sub_"+subs[2]); got !=
		"evt_anon_"+x+":invoice_paid:-,evt_cust_"+x+":invoice_paid:"+a+",recon:"+v.RunID+":sub_"+subs[2]+":"+string(domain.KindSubscriptionEnded)+":"+tenants[2] {
		t.Fatalf("ledgered %s", got)
	}
	if n := count(t, `SELECT count(*) FROM invoices WHERE provider_invoice_id = $1 AND tenant_id = $2`, "in_missing_"+x, a); n != 1 {
		t.Fatalf("mirrored %d", n)
	}
	if n := count(t, `SELECT count(*) FROM invoices WHERE provider_invoice_id = $1 AND tenant_id = $2`, "in_purchase_"+x, a); n != 1 {
		t.Fatalf("purchase invoice not attributed to the customer's tenant: %d", n)
	}
	if n := len(e.p.called("ListSince subscriptions p1")); n != 1 {
		t.Fatalf("second page %v", e.p.called("ListSince"))
	}
}

// §8.13 failures fail the run (status failed, AL-5 entry), never half-done
// silently; a failed finish surfaces to the worker.
func TestReconciliationFailurePaths(t *testing.T) {
	e := newEnv(t)
	tenant, s := e.activeSubscription(t, "pro", 1)
	orphan := []port.ReconPage{{Items: []domain.ProviderObjectState{provSub("sub_orphan2_"+s, "active", map[string]string{"billing_subscription_id": s})}}}
	mismatch := []port.ReconPage{{Items: []domain.ProviderObjectState{provSub("sub_"+s, "past_due", nil)}}}
	cus := scalar[string](t, `SELECT provider_customer_id FROM billing_accounts WHERE tenant_id = $1`, tenant)
	missing := []port.ReconPage{{Items: []domain.ProviderObjectState{provInv("in_gone_"+s, cus, map[string]string{"tenant_id": tenant})}}}
	exec(t, `INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, livemode, provider_created_at, payload, status, outcome)
		VALUES ('stripe', $1, 'invoice.paid', 'invoice_paid', false, now(), '{}', 'failed', 'failed:unknown_customer')`, "evt_nocus2_"+s)
	// F-10 (rev 0.71): seats_mismatch and invoice_mismatch findings.
	seatsDiff := provSub("sub_"+s, "active", nil)
	seatsDiff.Fields = map[string]any{"quantity": int64(9)}
	seats := []port.ReconPage{{Items: []domain.ProviderObjectState{seatsDiff}}}
	e.mirror(t, tenant, s, "in_mm_"+s, "subscription_cycle", "open", 6900)
	mmInv := provInv("in_mm_"+s, cus, nil)
	mmInv.Status = "open"
	if mmInv.Fields == nil {
		mmInv.Fields = map[string]any{}
	}
	mmInv.Fields["amount_due_minor"] = int64(1)
	amounts := []port.ReconPage{{Items: []domain.ProviderObjectState{mmInv}}}
	cases := []struct {
		name string
		arm  func()
	}{
		{"provider unloaded", func() { e.reg.hidden["stripe"] = true }},
		{"seats finding", func() { e.p.recon[port.ReconSubscriptions] = seats; e.fail("Recon.AddFinding", 0) }},
		{"invoice mirrors", func() { e.fail("Recon.InvoiceMirrors", 0) }},
		{"invoice mismatch finding", func() { e.p.recon[port.ReconInvoices] = amounts; e.fail("Recon.AddFinding", 0) }},
		{"billing side", func() { e.fail("Recon.BillingSubscriptions", 0) }},
		{"provider listing", func() { e.p.on("ListSince", answer{err: errProvider}) }},
		{"orphan finding", func() { e.p.recon[port.ReconSubscriptions] = orphan; e.fail("Recon.AddFinding", 0) }},
		{"mismatch finding", func() { e.p.recon[port.ReconSubscriptions] = mismatch; e.fail("Recon.AddFinding", 0) }},
		{"known ids", func() { e.fail("Recon.KnownIDs", 0) }},
		{"invoice finding", func() { e.p.recon[port.ReconInvoices] = missing; e.fail("Recon.AddFinding", 0) }},
		{"unknown customers", func() { e.fail("Recon.UnknownCustomerEvents", 0) }},
		{"unknown customer finding", func() { e.fail("Recon.AddFinding", 0) }},
	}
	for _, c := range cases {
		e.p.recon = map[port.ReconKind][]port.ReconPage{}
		c.arm()
		v, err := e.recon(t, "stripe", false)
		delete(e.reg.hidden, "stripe")
		if err != nil || v.Status != "failed" {
			t.Fatalf("%s: %+v %v", c.name, v, err)
		}
		if n := count(t, `SELECT count(*) FROM pending_audit_entries WHERE entry->>'entry_type' = 'commercial.cross_tenant_job.ran'
			AND entry->'target'->>'id' = $1 AND entry->>'action' = 'reconciliation.failed'`, v.RunID); n != 1 {
			t.Fatalf("%s: audit %d", c.name, n)
		}
	}
	e.fail("Recon.Finish", 0)
	_, err := e.recon(t, "manual", false)
	isInjected(t, "finish", err)
}

// §8.8.4 offboarding: open invoices are voided where the provider allows;
// one that cannot be read is skipped and one the provider refuses is left
// for the provider (logged), and the subscription still ends.
func TestOffboardingVoidPaths(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, s := e.activeSubscription(t, "pro", 3)
	for _, id := range []string{"in_o1_" + s, "in_o2_" + s} {
		e.mirror(t, tenant, s, id, "subscription_cycle", "open", 6900)
	}
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'offboarded', closed_at = now() WHERE tenant_id = $1::uuid`, tenant)
	e.fail("Invoices.Get", 0)
	e.p.on("VoidInvoice", answer{err: errProvider})
	if _, err := e.svc.RunLifecycleFollowUps(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if n := len(e.p.called("VoidInvoice in_o")); n != 1 {
		t.Fatalf("void calls %v", e.p.called("VoidInvoice"))
	}
	if !e.log.has("WARN", "open invoice not voided at offboarding", tenant, "PROVIDER_UNAVAILABLE") {
		t.Fatalf("void failure not logged: %v", e.log.lines)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||cancel_reason::text FROM subscriptions WHERE billing_subscription_id = $1`, s); got != "ended/offboarded" {
		t.Fatalf("subscription %s", got)
	}
}
