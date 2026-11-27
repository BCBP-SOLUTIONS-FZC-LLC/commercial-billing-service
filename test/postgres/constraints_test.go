//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"
)

const commercialSystem = "00000000-0000-0000-0000-0000000000c1"

// want asserts that sql fails with the SQLSTATE.
func want(t *testing.T, code, what, sql string, args ...any) {
	t.Helper()
	if got := errCode(exec(context.Background(), superPool, sql, args...)); got != code {
		t.Errorf("%s: SQLSTATE %q, want %q", what, got, code)
	}
}

// BIL-SUB-3: at most one non-ended subscription per tenant.
func TestBIL_SUB_3_OneLiveSubscription(t *testing.T) {
	s := seedTenant(t)
	want(t, "23505", "second live subscription", `
		INSERT INTO subscriptions (billing_subscription_id, tenant_id, provider, origin, plan, interval, status)
		VALUES ($1, $2, 'stripe', 'direct_signup', 'pro', 'month', 'incomplete')`, newID(), s.Tenant)
	// An ended one may coexist.
	mustExec(t, superPool, `
		INSERT INTO subscriptions (billing_subscription_id, tenant_id, provider, origin, plan, interval, status, ended_at)
		VALUES ($1, $2, 'stripe', 'direct_signup', 'pro', 'month', 'ended', now())`, newID(), s.Tenant)
}

func TestSubscriptionStatusConstraints(t *testing.T) {
	s := seedTenant(t)
	want(t, "23514", "past_due without past_due_since", `UPDATE subscriptions SET status='past_due' WHERE billing_subscription_id=$1`, s.Sub)
	want(t, "23514", "ended without ended_at", `UPDATE subscriptions SET status='ended' WHERE billing_subscription_id=$1`, s.Sub)
	want(t, "23514", "pause without who", `UPDATE subscriptions SET collection_paused=true WHERE billing_subscription_id=$1`, s.Sub)
	mustExec(t, superPool, `UPDATE subscriptions SET collection_paused=true, collection_paused_by='auto' WHERE billing_subscription_id=$1`, s.Sub)
}

// 000008 / 000009 (§8.2, rev 0.63): a reverting saga carries the B-O8
// request it completes, and the due scan's index covers it.
func TestSagaReversalRequestConstraint(t *testing.T) {
	s := seedTenant(t)
	mustExec(t, superPool, `UPDATE conversion_sagas SET state = 'confirmed', confirmed_at = now() WHERE billing_subscription_id = $1`, s.Sub)
	want(t, "23514", "reverting without its request", `UPDATE conversion_sagas SET state = 'reverting' WHERE billing_subscription_id = $1`, s.Sub)
	want(t, "23514", "reverting without the refund flag", `UPDATE conversion_sagas SET state = 'reverting', reversal_requested_at = now(),
		reversal_requested_by = $2::uuid, reversal_reason = 'fraud on the conversion payment' WHERE billing_subscription_id = $1`, s.Sub, commercialSystem)
	mustExec(t, superPool, `UPDATE conversion_sagas SET state = 'reverting', reversal_requested_at = now(), reversal_requested_by = $2::uuid,
		reversal_reason = 'fraud on the conversion payment', reversal_refund = true WHERE billing_subscription_id = $1`, s.Sub, commercialSystem)
	n, err := count(context.Background(), superPool, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_saga_due'
		AND indexdef LIKE '%rp_convert_pending%' AND indexdef LIKE '%reverting%'`)
	if err != nil || n != 1 {
		t.Errorf("idx_saga_due does not cover reverting sagas (%d, %v)", n, err)
	}
}

// BIL-SEAT-1 / BIL-SEAT-2: one open purchase and one open schedule.
func TestBIL_SEAT_1_and_2_OneOpen(t *testing.T) {
	s := seedTenant(t)
	ins := `INSERT INTO seat_purchases (purchase_id, tenant_id, billing_subscription_id, seats_added, seats_before, seats_after, proration_instant,
	          amount_minor, currency, amount_source, status, requested_by, idempotency_key)
	        VALUES ($1, $2, $3, 1, 10, 11, now(), 100, 'USD', 'provider_preview', 'pending_payment', $4, $5)`
	mustExec(t, superPool, ins, newID(), s.Tenant, s.Sub, commercialSystem, "key-open-1")
	want(t, "23505", "second open seat purchase", ins, newID(), s.Tenant, s.Sub, commercialSystem, "key-open-2")
	// The seed already has an open schedule.
	want(t, "23505", "second open schedule", `
		INSERT INTO seat_change_schedules (schedule_id, tenant_id, billing_subscription_id, target_seats, effective_at, requested_by)
		VALUES ($1, $2, $3, 5, now(), $4)`, newID(), s.Tenant, s.Sub, commercialSystem)
}

// BIL-SEAT-5: calculator inputs and deferral are stored together.
func TestBIL_SEAT_5_StoredCalculation(t *testing.T) {
	s := seedTenant(t)
	base := `INSERT INTO seat_purchases (purchase_id, tenant_id, billing_subscription_id, seats_added, seats_before, seats_after, proration_instant,
	   amount_minor, currency, amount_source, unit_amount_minor, remaining_seconds, period_seconds, amount_unrounded_minor,
	   deferred_amount_minor, deferral_reason, status, requested_by, idempotency_key)
	 VALUES ($1, $2, $3, 1, 10, 11, now(), $4, 'USD', $5, $6, $7, $8, $9, $10, $11, 'confirmed', $12, $13)`
	// The §8.5.3 below-minimum example is accepted.
	mustExec(t, superPool, base, newID(), s.Tenant, s.Sub, 0, "billing_calculator", 6900, 43200, 2592000, "115.000000000000", 115, "below_provider_minimum", commercialSystem, "key-def-1")
	cases := []struct {
		name string
		args []any
	}{
		{"calculator without inputs", []any{100, "billing_calculator", nil, nil, nil, nil, nil, nil}},
		{"provider preview with calculator inputs", []any{100, "provider_preview", 6900, 1, 2, "1.0", nil, nil}},
		{"deferral with a charge", []any{100, "billing_calculator", 6900, 43200, 2592000, "115.0", 115, "below_provider_minimum"}},
		{"deferral without reason", []any{0, "billing_calculator", 6900, 43200, 2592000, "115.0", 115, nil}},
		{"unknown deferral reason", []any{0, "billing_calculator", 6900, 43200, 2592000, "115.0", 115, "goodwill"}},
	}
	for i, c := range cases {
		args := append([]any{newID(), s.Tenant, s.Sub}, c.args...)
		args = append(args, commercialSystem, "key-def-bad-"+string(rune('a'+i)))
		want(t, "23514", c.name, base, args...)
	}
}

// BIL-SUB-7: an invoice disposition is all-or-nothing and only on held drafts.
func TestBIL_SUB_7_InvoiceDisposition(t *testing.T) {
	s := seedTenant(t)
	want(t, "23514", "disposition on an invoice not held", `
		UPDATE invoices SET disposition='void', disposition_reason='Voided by operator after review', disposed_by=$2, disposed_at=now()
		WHERE invoice_id=$1`, s.Invoice, commercialSystem)
	mustExec(t, superPool, `UPDATE invoices SET held_for_review=true, status='draft' WHERE invoice_id=$1`, s.Invoice)
	want(t, "23514", "partial disposition", `UPDATE invoices SET disposition='void' WHERE invoice_id=$1`, s.Invoice)
	mustExec(t, superPool, `
		UPDATE invoices SET disposition='void', disposition_reason='Voided by operator after review', disposed_by=$2, disposed_at=now()
		WHERE invoice_id=$1`, s.Invoice, commercialSystem)
}

// BIL-SUB-6: currency is fixed while a subscription is live.
func TestBIL_SUB_6_CurrencyFixed(t *testing.T) {
	s := seedTenant(t)
	want(t, "P0001", "currency change with a live subscription", `UPDATE billing_accounts SET currency='AED' WHERE tenant_id=$1`, s.Tenant)
	mustExec(t, superPool, `UPDATE subscriptions SET status='ended', ended_at=now() WHERE billing_subscription_id=$1`, s.Sub)
	mustExec(t, superPool, `UPDATE billing_accounts SET currency='AED' WHERE tenant_id=$1`, s.Tenant)
}

// §4.5 touch_row: updated_at and record_version move only on a real change.
func TestTouchRowTrigger(t *testing.T) {
	s := seedTenant(t)
	ctx := context.Background()
	version := func() int {
		n, err := count(ctx, superPool, `SELECT record_version FROM subscriptions WHERE billing_subscription_id=$1`, s.Sub)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	v0 := version()
	mustExec(t, superPool, `UPDATE subscriptions SET auto_renew = auto_renew WHERE billing_subscription_id=$1`, s.Sub)
	if version() != v0 {
		t.Error("no-op update must not bump record_version")
	}
	mustExec(t, superPool, `UPDATE subscriptions SET auto_renew = NOT auto_renew WHERE billing_subscription_id=$1`, s.Sub)
	if version() != v0+1 {
		t.Error("real update must bump record_version by 1")
	}
}

// Price book: no overlapping ranges for the same key (excl_pp_overlap).
func TestPriceOverlapExcluded(t *testing.T) {
	want(t, "23P01", "overlapping manual price", `
		INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, effective_from, created_by, reason)
		VALUES ('pro', 'month', 'USD', 7900, 'manual', '2026-06-01T00:00:00Z', $1, 'overlapping row must fail')`, commercialSystem)
	want(t, "23514", "stripe row without provider price", `
		INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, effective_from, created_by, reason)
		VALUES ('pro', 'month', 'AED', 25300, 'stripe', now(), $1, 'stripe needs a price id')`, commercialSystem)
}

// BIL-PROV-4: trg_pc_guard and the two-person rule.
func TestBIL_PROV_4_CertificationGuard(t *testing.T) {
	ctx := context.Background()
	submitter, reviewer := newID(), newID()
	id := newID()
	ins := `INSERT INTO provider_certifications (id, provider, adapter_version, provider_api_version, suite_version, status, proration_mode,
	          capabilities, results, certified_by)
	        VALUES ($1, 'stripe', $2, '2026-10-28.acacia', 'cert-1.0', 'pending_review', 'native', '{}', '[{"case":"create","passed":true}]', $3)`
	ver := "1.0.0+test." + time.Now().Format("150405.000000")
	if err := exec(ctx, systemPool, ins, id, ver, submitter); err != nil {
		t.Fatalf("billing_system submits a run: %v", err)
	}
	// Self-review is refused by the table.
	if code := errCode(exec(ctx, systemPool, `UPDATE provider_certifications SET status='passed', reviewed_by=$2, reviewed_at=now(), review_reason='self approval attempt' WHERE id=$1`, id, submitter)); code != "23514" {
		t.Errorf("self review: SQLSTATE %q, want 23514", code)
	}
	// Run data is immutable.
	if code := errCode(exec(ctx, systemPool, `UPDATE provider_certifications SET results='[]' WHERE id=$1`, id)); code != "P0001" {
		t.Errorf("results edit: SQLSTATE %q, want P0001", code)
	}
	// A different person approves.
	if err := exec(ctx, systemPool, `UPDATE provider_certifications SET status='passed', reviewed_by=$2, reviewed_at=now(), review_reason='Evidence checked for every case' WHERE id=$1`, id, reviewer); err != nil {
		t.Fatalf("review by a second person: %v", err)
	}
	// A duplicate live run for the same version is refused.
	if code := errCode(exec(ctx, systemPool, ins, newID(), ver, submitter)); code != "23505" {
		t.Errorf("duplicate live run: SQLSTATE %q, want 23505", code)
	}
	// The review decision cannot be rewritten.
	if code := errCode(exec(ctx, systemPool, `UPDATE provider_certifications SET review_reason='rewritten after approval' WHERE id=$1`, id)); code != "P0001" {
		t.Errorf("review rewrite: SQLSTATE %q, want P0001", code)
	}
	// Revocation is allowed; then the same version can be submitted again.
	if err := exec(ctx, systemPool, `UPDATE provider_certifications SET status='revoked', revoked_at=now(), revoked_by=$2, revoked_reason='api version change' WHERE id=$1`, id, reviewer); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := exec(ctx, systemPool, ins, newID(), ver, submitter); err != nil {
		t.Errorf("re-submission after revocation: %v", err)
	}
	// Never deleted, even by the owner.
	if code := errCode(exec(ctx, superPool, `DELETE FROM provider_certifications WHERE id=$1`, id)); code != "P0001" {
		t.Errorf("delete: SQLSTATE %q, want P0001", code)
	}
	// The manual adapter's exempt record exists (000004).
	if n, _ := count(ctx, appPool, `SELECT count(*) FROM provider_certifications WHERE provider='manual' AND status='exempt' AND adapter_version='1.0.0+manual'`); n != 1 {
		t.Errorf("manual exempt record: %d", n)
	}
}

// pending_audit_entries: RLS on, drainer policy present (000005).
func TestAuditOutboxRLS(t *testing.T) {
	ctx := context.Background()
	n, err := count(ctx, superPool, `
		SELECT count(*) FROM pg_policies WHERE tablename='pending_audit_entries'
		AND policyname IN ('tenant_isolation','audit_drainer_all')`)
	if err != nil || n != 2 {
		t.Fatalf("pending_audit_entries policies: %d (%v), want 2", n, err)
	}
	f, err := count(ctx, superPool, `SELECT count(*) FROM pg_class WHERE relname='pending_audit_entries' AND relrowsecurity AND relforcerowsecurity`)
	if err != nil || f != 1 {
		t.Fatalf("pending_audit_entries must FORCE RLS: %d (%v)", f, err)
	}
}
