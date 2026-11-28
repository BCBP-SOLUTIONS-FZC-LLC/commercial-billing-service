//go:build integration

package flows_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// §13.2 CronJobs, run through the real use cases against Postgres.

func periodEnd(t *testing.T, sub string) time.Time {
	t.Helper()
	return scalar[time.Time](t, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func quantity(t *testing.T, sub string) int {
	t.Helper()
	return scalar[int](t, `SELECT quantity FROM subscription_items WHERE billing_subscription_id = $1`, sub)
}

// §8.6.2 – §8.6.3 (HLD §8.5.1 example): target 60, 55 in use → renew at
// 60; 75 in use at renewal → top-up of 15 charged with the seats kept
// even if unpaid; final 75 and TenantSeatsChanged {renewal_topup}.
func TestSeatRenewalJob(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 100)
	e.core.usage = port.SeatUsage{ActiveUsers: 55, LicensedSeats: 100}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 60}); err != nil {
		t.Fatal(err)
	}
	sch := scalar[string](t, `SELECT schedule_id::text FROM seat_change_schedules WHERE billing_subscription_id = $1`, sub)
	r := periodEnd(t, sub)

	e.now = r.Add(-48 * time.Hour) // outside SEAT_RENEWAL_LEAD
	mustJob(t, e.svc.RunSeatRenewal)
	if st := scalar[string](t, `SELECT status FROM seat_change_schedules WHERE schedule_id = $1`, sch); st != "scheduled" {
		t.Fatalf("outside the lead: %s", st)
	}
	e.now = r.Add(-2 * time.Hour)
	mustJob(t, e.svc.RunSeatRenewal)
	if got := scalar[string](t, `SELECT status||'/'||renewal_seats||'/'||seats_in_use_at_apply FROM seat_change_schedules WHERE schedule_id = $1`, sch); got != "applying/60/55" {
		t.Fatalf("applied ahead: %s", got)
	}
	if len(e.stripe.called("ScheduleAtPeriodEnd price_pro_month sched:"+sch+":apply")) != 1 {
		t.Fatalf("provider schedule %v", e.stripe.called("ScheduleAtPeriodEnd"))
	}
	// Before the renewal nothing more happens.
	mustJob(t, e.svc.RunSeatRenewal)
	if quantity(t, sub) != 100 {
		t.Fatal("changed before the renewal")
	}

	// The renewal: the cycle invoice rolls the period; members were added.
	paid := invoiceState("in_renew_"+sub, sub, "paid", "subscription_cycle", r, 60)
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_renew_"+sub, "invoice.paid", domain.KindInvoicePaid), paid); err != nil || out != "applied:renewed" {
		t.Fatalf("renewal: %s %v", out, err)
	}
	e.now = r.Add(10 * time.Minute)
	e.core.usage = port.SeatUsage{ActiveUsers: 75, LicensedSeats: 100}
	e.stripe.change = []confirmAnswer{{err: port.ErrDeclined{Code: "insufficient_funds"}}}
	mustJob(t, e.svc.RunSeatRenewal)
	if len(e.stripe.called("ChangeQuantity 60->75 invoice_now=true keep=true topup:"+sch)) != 1 {
		t.Fatalf("top-up %v", e.stripe.called("ChangeQuantity"))
	}
	if q := quantity(t, sub); q != 75 {
		t.Fatalf("quantity %d", q)
	}
	if got := scalar[string](t, `SELECT status||'/'||final_seats FROM seat_change_schedules WHERE schedule_id = $1`, sch); got != "applied/75" {
		t.Fatalf("schedule %s", got)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||seats_added FROM seat_purchases WHERE billing_subscription_id = $1 AND reason = 'renewal_topup'`, sub); got != "confirmed/15" {
		t.Fatalf("top-up row %s", got)
	}
	if got := scalar[string](t, `SELECT payload->'data'->>'reason'||'/'||(payload->'data'->>'licensed_seats') FROM outbox_events
		WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged' ORDER BY ordering_seq DESC LIMIT 1`, tenant); got != "renewal_topup/75" {
		t.Fatalf("event %s", got)
	}
	if e.metrics.get("seat_change:renewal_topup:unpaid") != 1 {
		t.Fatal("top-up metric")
	}

	// I-11 down within the last hour → skipped (iam_unavailable), never blind.
	tenant2, sub2 := e.activeSubscription(t, "pro", 10)
	e.core.usage, e.core.err = port.SeatUsage{ActiveUsers: 2}, nil
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant2, app.ReductionRequest{TargetSeats: 5}); err != nil {
		t.Fatal(err)
	}
	e.core.err = domain.NewError(domain.ErrIAMUnavailable, "down")
	e.now = periodEnd(t, sub2).Add(-30 * time.Minute)
	mustJob(t, e.svc.RunSeatRenewal)
	if got := scalar[string](t, `SELECT status||'/'||skip_reason FROM seat_change_schedules WHERE billing_subscription_id = $1`, sub2); got != "skipped/iam_unavailable" {
		t.Fatalf("I-11 down: %s", got)
	}
}

// §8.4.2 step 2: the re-check keeps an allowed downgrade, releases a
// blocked one, and blocks it when I-18 is still down 15 min before.
func TestDowngradeApplyJob(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	schedule := func() (string, string, time.Time) {
		tenant, sub := e.activeSubscription(t, "enterprise", 5)
		e.core.err, e.core.check = nil, port.PlanChangeCheck{Allowed: true}
		if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-dg-"+sub[:8]), tenant, app.PlanChangeRequest{TargetPlan: "pro", RecordVersion: e.version(t, sub)}); err != nil {
			t.Fatal(err)
		}
		return tenant, sub, periodEnd(t, sub)
	}
	tenant, _, r := schedule()
	change := scalar[string](t, `SELECT change_id::text FROM plan_change_requests WHERE tenant_id = $1`, tenant)
	e.now = r.Add(-time.Hour)
	mustJob(t, e.svc.RunDowngradeApply)
	if st := scalar[string](t, `SELECT status::text FROM plan_change_requests WHERE change_id = $1`, change); st != "scheduled" {
		t.Fatalf("allowed: %s", st)
	}
	e.core.check = port.PlanChangeCheck{Allowed: false, Blockers: []port.PlanChangeBlock{{Code: "sso_idp_configured"}}}
	mustJob(t, e.svc.RunDowngradeApply)
	if got := scalar[string](t, `SELECT status::text||'/'||array_to_string(blocker_codes, ',') FROM plan_change_requests WHERE change_id = $1`, change); got != "blocked/sso_idp_configured" {
		t.Fatalf("blocked: %s", got)
	}
	if len(e.stripe.called("ReleaseSchedule sched_planchg:"+change+" planchg:"+change+":release")) != 1 {
		t.Fatalf("release %v", e.stripe.called("ReleaseSchedule"))
	}
	if e.metrics.get("downgrade_blocked:sso_idp_configured") != 1 {
		t.Fatal("metric")
	}

	tenant2, _, r2 := schedule()
	change2 := scalar[string](t, `SELECT change_id::text FROM plan_change_requests WHERE tenant_id = $1`, tenant2)
	e.core.err = domain.NewError(domain.ErrIAMUnavailable, "down")
	e.now = r2.Add(-time.Hour)
	mustJob(t, e.svc.RunDowngradeApply)
	if st := scalar[string](t, `SELECT status::text FROM plan_change_requests WHERE change_id = $1`, change2); st != "scheduled" {
		t.Fatalf("I-18 down early: %s", st)
	}
	e.now = r2.Add(-10 * time.Minute)
	mustJob(t, e.svc.RunDowngradeApply)
	if got := scalar[string](t, `SELECT status::text||'/'||array_to_string(blocker_codes, ',') FROM plan_change_requests WHERE change_id = $1`, change2); got != "blocked/iam_unavailable" {
		t.Fatalf("I-18 down late: %s", got)
	}
}

// purchase-expiry: an abandoned seat purchase expires and its open invoice
// is voided; an expired checkout session ends its incomplete subscription.
func TestPurchaseExpiryJob(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.activeSubscription(t, "pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.stripe.change = []confirmAnswer{{outcome: port.PaymentOutcome{Status: port.PaymentPending}}}
	p, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-exp-seat"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	if err != nil || p.Status != "pending_payment" {
		t.Fatalf("pending %+v %v", p, err)
	}
	trial := newTenant()
	e.core.usage = port.SeatUsage{ActiveUsers: 1}
	if _, err := e.svc.StartCheckout(ctx, owner, trial, app.CheckoutRequest{Plan: "pro", Interval: "month", LicensedSeats: 2}); err != nil {
		t.Fatal(err)
	}
	e.now = time.Now().UTC().Add(2 * time.Hour)
	mustJob(t, e.svc.ExpireStale)
	if st := scalar[string](t, `SELECT status::text FROM seat_purchases WHERE purchase_id = $1`, p.PurchaseID); st != "expired" {
		t.Fatalf("purchase %s", st)
	}
	if len(e.stripe.called("VoidInvoice in_seatpur:"+p.PurchaseID+" seatpur:"+p.PurchaseID+":expire")) != 1 {
		t.Fatalf("void %v", e.stripe.called("VoidInvoice"))
	}
	if got := scalar[string](t, `SELECT c.status||'/'||s.status::text FROM checkout_sessions c JOIN subscriptions s USING (billing_subscription_id)
		WHERE c.tenant_id = $1`, trial); got != "expired/ended" {
		t.Fatalf("checkout %s", got)
	}
}

// §8.6.4: Core's licensed_seats differs from Billing's for longer than
// SEAT_RESYNC_AFTER → a seats_resync emission is queued.
func TestSeatProjectionJob(t *testing.T) {
	e := newEnv(t)
	tenant, _ := e.activeSubscription(t, "pro", 10)
	e.core.usage = port.SeatUsage{ActiveUsers: 3, LicensedSeats: 8}
	e.now = time.Now().UTC().Add(time.Minute) // recent change: not yet
	mustJob(t, e.svc.CheckSeatProjection)
	if n := scalar[int](t, `SELECT count(*) FROM scheduled_emissions WHERE tenant_id = $1 AND payload_builder = 'seats_resync'`, tenant); n != 0 {
		t.Fatalf("too early: %d", n)
	}
	e.now = time.Now().UTC().Add(11 * time.Minute)
	mustJob(t, e.svc.CheckSeatProjection)
	if n := scalar[int](t, `SELECT count(*) FROM scheduled_emissions WHERE tenant_id = $1 AND payload_builder = 'seats_resync' AND status = 'pending'`, tenant); n != 1 {
		t.Fatalf("resync queued: %d", n)
	}
	if e.metrics.get("seat_projection_mismatch") == 0 {
		t.Fatal("metric")
	}
}

// §15.3, BIL-OPS-9: contact name and email nulled 90 days after
// offboarding, provider email cleared; no row deleted, company kept.
func TestBIL_OPS_9_PIIMinimisationJob(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	mustNoErr(t, sql(context.Background(), `UPDATE billing_accounts SET billing_contact_name = 'Asha Rao', billing_contact_email = 'billing@acme.example',
		company_name = 'Acme', iam_lifecycle = 'offboarded', closed_at = now() - interval '91 days' WHERE tenant_id = $1`, tenant))
	e.now = time.Now().UTC()
	mustJob(t, e.svc.MinimisePII)
	got := scalar[string](t, `SELECT coalesce(billing_contact_name,'∅')||'/'||coalesce(billing_contact_email,'∅')||'/'||company_name||'/'||(pii_minimised_at IS NOT NULL)
		FROM billing_accounts WHERE tenant_id = $1`, tenant)
	if got != "∅/∅/Acme/true" {
		t.Fatalf("account %s", got)
	}
	if len(e.stripe.called("UpdateCustomer  pii:"+tenant)) != 1 {
		t.Fatalf("provider email %v", e.stripe.called("UpdateCustomer"))
	}
	if n := scalar[int](t, `SELECT count(*) FROM subscriptions WHERE billing_subscription_id = $1`, sub); n != 1 {
		t.Fatal("row deleted")
	}
}

// §8.16: the job schedules the row effective at the renewal (cutoff and
// idempotency respected); the renewal's cycle invoice moves the item.
func TestRepricingJob(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "starter", 4)
	r := periodEnd(t, sub)
	// A test-only GBP price book, so no other test sees the change.
	mustNoErr(t, sql(ctx, `INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, effective_to, created_by, reason)
		VALUES ('starter','month','GBP',1000,'stripe','price_gbp_old','2026-01-01',$1,'00000000-0000-0000-0000-0000000000c1','test GBP row'),
		       ('starter','month','GBP',1200,'stripe','price_gbp_new',$1,NULL,'00000000-0000-0000-0000-0000000000c1','test GBP row')`, r.Add(-24*time.Hour)))
	oldID := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_gbp_old'`)
	newID := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_gbp_new'`)
	mustNoErr(t, sql(ctx, `UPDATE subscription_items SET price_id = $2 WHERE billing_subscription_id = $1`, sub, oldID))

	e.now = r.Add(-30 * time.Minute) // inside REPRICE_CUTOFF: next renewal
	mustJob(t, e.svc.RunRepricing)
	if len(e.stripe.called("ScheduleAtPeriodEnd price_gbp_new")) != 0 {
		t.Fatal("scheduled inside the cutoff")
	}
	e.now = r.Add(-5 * time.Hour)
	mustJob(t, e.svc.RunRepricing)
	mustJob(t, e.svc.RunRepricing) // idempotent
	if n := len(e.stripe.called("ScheduleAtPeriodEnd price_gbp_new reprice:" + sub + ":" + newID)); n != 1 {
		t.Fatalf("schedule calls %d", n)
	}
	if got := scalar[string](t, `SELECT scheduled_price_id::text FROM subscription_items WHERE billing_subscription_id = $1`, sub); got != newID {
		t.Fatalf("scheduled %s", got)
	}
	if e.metrics.get("repricing:increase") != 1 {
		t.Fatal("metric")
	}
	paid := invoiceState("in_rp_"+sub, sub, "paid", "subscription_cycle", r, 4)
	if out, err := e.handle(t, tenant, paidChain(e.svc), callback("evt_rp_"+sub, "invoice.paid", domain.KindInvoicePaid), paid); err != nil || out != "applied:renewed" {
		t.Fatalf("renewal %s %v", out, err)
	}
	if got := scalar[string](t, `SELECT price_id::text||'/'||(scheduled_price_id IS NULL) FROM subscription_items WHERE billing_subscription_id = $1`, sub); got != newID+"/true" {
		t.Fatalf("item after renewal %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type IN ('TenantPlanChanged','TenantSeatsChanged')`, tenant); n != 0 {
		t.Fatalf("bus event on repricing: %d", n)
	}
}

// BIL-OPS-5: an overdue saga raises its alert once (alerted_at).
func TestSagaSweeperJob(t *testing.T) {
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	mustNoErr(t, sql(context.Background(), `UPDATE conversion_sagas SET state = 'awaiting_tenant_converted', confirmed_at = NULL,
		payment_confirmed_at = now() - interval '1 hour', alerted_at = NULL WHERE billing_subscription_id = $1`, sub))
	e.now = time.Now().UTC()
	mustJob(t, e.svc.SweepSagas)
	first := scalar[time.Time](t, `SELECT alerted_at FROM conversion_sagas WHERE tenant_id = $1`, tenant)
	mustJob(t, e.svc.SweepSagas)
	if again := scalar[time.Time](t, `SELECT alerted_at FROM conversion_sagas WHERE tenant_id = $1`, tenant); !again.Equal(first) {
		t.Fatal("alerted twice")
	}
}

// idempotency-prune removes expired records and stored previews only.
func TestIdempotencyPruneJob(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, _ := e.activeSubscription(t, "pro", 3)
	if _, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1}); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, `SELECT count(*) FROM idempotency_records WHERE tenant_id = $1 AND scope = 'preview'`, tenant); n != 1 {
		t.Fatalf("preview stored %d", n)
	}
	e.now = time.Now().UTC().Add(time.Hour)
	mustJob(t, e.svc.PruneIdempotency)
	if n := scalar[int](t, `SELECT count(*) FROM idempotency_records WHERE tenant_id = $1 AND scope = 'preview'`, tenant); n != 0 {
		t.Fatalf("preview not pruned %d", n)
	}
}

// §8.13 with auto_repair: a missing event is ledgered, an ended provider
// subscription is queued for re-read, a missing invoice is mirrored.
func TestReconciliationAutoRepair(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 3)
	cus := scalar[string](t, `SELECT provider_customer_id FROM billing_accounts WHERE tenant_id = $1`, tenant)
	payload, _ := json.Marshal(map[string]any{"id": "evt_rep_" + sub, "type": "invoice.paid"})
	e.stripe.recon = map[port.ReconKind][]domain.ProviderObjectState{
		port.ReconSubscriptions: {{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_" + sub}, Status: "canceled"}},
		port.ReconEvents: {{Ref: domain.ProviderObjectRef{Kind: "event", ID: "evt_rep_" + sub}, Status: "invoice.paid",
			Fields: map[string]any{"event": domain.PaymentEvent{Provider: "stripe", ProviderEventID: "evt_rep_" + sub, EventType: "invoice.paid",
				Kind: domain.KindInvoicePaid, CustomerRef: cus, Object: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_rep_" + sub},
				ProviderCreatedAt: time.Now().UTC(), Payload: payload}}}},
		port.ReconInvoices: {{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_rep_" + sub}, Status: "open",
			Metadata: map[string]string{"tenant_id": tenant, "billing_subscription_id": sub}, Fields: map[string]any{"customer": cus}}},
	}
	run, err := e.svc.StartReconciliation(ctx, operator, app.ReconciliationRequest{Provider: "stripe", WindowStart: time.Now().Add(-time.Hour),
		WindowEnd: time.Now(), AutoRepair: true, Reason: "Repair run for the auto-repair test"})
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
	repaired := map[string]bool{}
	for _, f := range v.Findings {
		repaired[f.Kind] = f.AutoRepaired
	}
	if !repaired["missing_ledger_event"] || !repaired["status_mismatch"] || !repaired["invoice_missing"] {
		t.Fatalf("findings %+v", v.Findings)
	}
	if got := scalar[string](t, `SELECT status::text||'/'||tenant_id::text FROM payment_event_ledger WHERE provider_event_id = $1`, "evt_rep_"+sub); got != "received/"+tenant {
		t.Fatalf("ledgered %s", got)
	}
	if got := scalar[string](t, `SELECT normalised_kind FROM payment_event_ledger WHERE provider_event_id = $1`, "recon:"+run.RunID+":sub_"+sub); got != string(domain.KindSubscriptionEnded) {
		t.Fatalf("re-read %s", got)
	}
	if n := scalar[int](t, `SELECT count(*) FROM invoices WHERE provider_invoice_id = $1`, "in_rep_"+sub); n != 1 {
		t.Fatalf("mirror %d", n)
	}
	if v.Counts["repaired"] != 3 {
		t.Fatalf("counts %+v", v.Counts)
	}
}

func mustJob(t *testing.T, job func(context.Context) (app.JobResult, error)) {
	t.Helper()
	if _, err := job(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// outbox-prune and processed-events-prune run as billing_app.
func TestPruneJobs(t *testing.T) {
	ctx := context.Background()
	if _, err := pgadapter.PruneOutbox(ctx, appPool, 8*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	inbox, err := pgadapter.NewInboxRunner(appPool, app.ConsumerTenantBilling, app.ConsumerUsageBilling)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Prune(ctx, 8*24*time.Hour); err != nil {
		t.Fatal(err)
	}
}

// The exporter snapshot reads the §11.2 gauges as billing_system.
func TestMetricsSnapshot(t *testing.T) {
	e := newEnv(t)
	_, _ = e.activeSubscription(t, "pro", 3)
	snap, err := pgadapter.NewSnapshotRepository(systemPool).MetricsSnapshot(context.Background(), time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Subscriptions[[3]string{"active", "pro", "stripe"}] == 0 {
		t.Fatalf("subscriptions %+v", snap.Subscriptions)
	}
}

// Use cases record their §11.2 metrics.
func TestUseCaseMetrics(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-met-seat"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: pv.PreviewID}); err != nil {
		t.Fatal(err)
	}
	if e.metrics.get("purchase_to_event:seats") != 1 || e.metrics.get("seat_change:purchase:confirmed") != 1 {
		t.Fatalf("seat metrics %v", e.metrics.counts)
	}
	failed := invoiceState("in_met_"+sub, sub, "open", "subscription_cycle", time.Now().Add(24*time.Hour), 11)
	if _, err := e.handle(t, tenant, app.PaymentFailed{S: e.svc}, callback("evt_met_"+sub, "invoice.payment_failed", domain.KindInvoicePaymentFailed), failed); err != nil {
		t.Fatal(err)
	}
	if e.metrics.get("dunning:past_due") != 1 || e.metrics.get("payment_failure:payment_failed") != 1 {
		t.Fatalf("dunning metrics %v", e.metrics.counts)
	}
}

// F-12 (rev 0.71, BIL-SUB-8): a downgrade takes effect at the renewal R, so
// its ScheduleAtPeriodEnd uses the target plan's row effective at R, not the
// one effective when the owner asked.
func TestBIL_SUB_8_DowngradeSchedulesThePriceEffectiveAtRenewal(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 4)
	r := periodEnd(t, sub)
	// A test-only CHF price book, so no other test sees it.
	mustNoErr(t, sql(ctx, `INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, effective_to, created_by, reason)
		VALUES ('pro','month','CHF',6900,'stripe','price_chf_pro','2026-01-01',NULL,'00000000-0000-0000-0000-0000000000c1','test CHF row'),
		       ('starter','month','CHF',2900,'stripe','price_chf_starter_old','2026-01-01',$1,'00000000-0000-0000-0000-0000000000c1','test CHF row'),
		       ('starter','month','CHF',3400,'stripe','price_chf_starter_new',$1,NULL,'00000000-0000-0000-0000-0000000000c1','test CHF row')`, r.Add(-24*time.Hour)))
	pro := scalar[string](t, `SELECT price_id::text FROM plan_prices WHERE provider_price_id = 'price_chf_pro'`)
	mustNoErr(t, sql(ctx, `UPDATE subscription_items SET price_id = $2 WHERE billing_subscription_id = $1`, sub, pro))

	pv, err := e.svc.PreviewPlanChange(ctx, owner, tenant, app.PlanChangePreviewRequest{TargetPlan: "starter"})
	if err != nil || pv.NextRenewalAmount.AmountMinor != 4*3400 {
		t.Fatalf("B-P3 renewal amount %+v %v", pv.NextRenewalAmount, err)
	}
	e.core.check = port.PlanChangeCheck{Allowed: true}
	res, err := e.svc.ChangePlan(ctx, withKey(owner, "k-down-chf"), tenant, app.PlanChangeRequest{TargetPlan: "starter", RecordVersion: e.version(t, sub)})
	if err != nil || res.Status != "scheduled" {
		t.Fatalf("downgrade %+v %v", res, err)
	}
	if len(e.stripe.called("ScheduleAtPeriodEnd price_chf_starter_new")) != 1 || len(e.stripe.called("ScheduleAtPeriodEnd price_chf_starter_old")) != 0 {
		t.Fatalf("schedule %v", e.stripe.called("ScheduleAtPeriodEnd"))
	}
}
