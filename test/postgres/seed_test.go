//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// newID returns a random UUID v4 string.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// tenantScopedTables are the §4.3 tables with tenant_isolation (BIL-RLS-1),
// pending_audit_entries excepted (it is platform-audit's and tested apart).
var tenantScopedTables = []string{
	"billing_accounts", "subscriptions", "subscription_items", "subscription_status_history",
	"checkout_sessions", "conversion_sagas", "plan_change_requests", "seat_purchases",
	"seat_change_schedules", "quota_addon_purchases", "invoices", "invoice_links", "adjustments",
	"refunds", "disputes", "idempotency_records", "scheduled_emissions", "usage_period_records",
}

// seeded holds the ids created for one tenant.
type seeded struct {
	Tenant, Sub, Invoice, AddonPurchase string
}

// seedTenant writes one valid row into every tenant-scoped table for a new
// tenant, as the superuser (RLS does not apply), and returns the ids.
func seedTenant(t *testing.T) seeded {
	t.Helper()
	ctx := context.Background()
	s := seeded{Tenant: newID(), Sub: newID(), Invoice: newID(), AddonPurchase: newID()}
	actor := "00000000-0000-0000-0000-0000000000c1"
	err := pgcommon.RunInTx(ctx, superPool, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		var priceID string
		if err := tx.QueryRow(ctx, `SELECT price_id FROM plan_prices WHERE provider='manual' AND plan='pro' AND interval='month'`).Scan(&priceID); err != nil {
			return fmt.Errorf("seed price: %w", err)
		}
		stmts := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO billing_accounts (tenant_id, provider, provider_customer_id, currency, iam_lifecycle)
			  VALUES ($1, 'stripe', 'cus_'||$1::uuid::text, 'USD', 'active')`, []any{s.Tenant}},
			{`INSERT INTO subscriptions (billing_subscription_id, tenant_id, provider, provider_subscription_id, origin, plan, interval, status,
			    current_period_start, current_period_end, payment_confirmed_at)
			  VALUES ($1, $2, 'stripe', 'sub_'||$1::uuid::text, 'trial_conversion', 'pro', 'month', 'active', now(), now() + interval '30 days', now())`, []any{s.Sub, s.Tenant}},
			{`INSERT INTO subscription_items (tenant_id, billing_subscription_id, kind, price_id, quantity) VALUES ($1, $2, 'seats', $3, 10)`, []any{s.Tenant, s.Sub, priceID}},
			{`INSERT INTO subscription_status_history (tenant_id, billing_subscription_id, from_status, to_status, cause, actor_id)
			  VALUES ($1, $2, 'incomplete', 'active', 'webhook:invoice.paid', $3)`, []any{s.Tenant, s.Sub, actor}},
			{`INSERT INTO checkout_sessions (session_id, tenant_id, purpose, provider_session_id, billing_subscription_id, requested_by, expires_at, status)
			  VALUES ($1, $2, 'payment_method_setup', 'cs_'||$1::uuid::text, $3, $4, now() + interval '1 hour', 'completed')`, []any{newID(), s.Tenant, s.Sub, actor}},
			{`INSERT INTO conversion_sagas (saga_id, tenant_id, billing_subscription_id, kind, state, payment_confirmed_at, rp_idempotency_key)
			  VALUES ($1, $2, $3, 'trial_conversion', 'rp_convert_pending', now(), 'convert:'||$2::uuid::text||':'||$3::uuid::text)`, []any{newID(), s.Tenant, s.Sub}},
			{`INSERT INTO plan_change_requests (change_id, tenant_id, billing_subscription_id, from_plan, to_plan, direction, status, requested_by, idempotency_key)
			  VALUES ($1, $2, $3, 'pro', 'enterprise', 'upgrade', 'applied', $4, 'key-'||$1::uuid::text)`, []any{newID(), s.Tenant, s.Sub, actor}},
			{`INSERT INTO seat_purchases (purchase_id, tenant_id, billing_subscription_id, seats_added, seats_before, seats_after, proration_instant,
			    amount_minor, currency, amount_source, status, requested_by, idempotency_key)
			  VALUES ($1, $2, $3, 2, 10, 12, now(), 4600, 'USD', 'provider_preview', 'confirmed', $4, 'key-'||$1::uuid::text)`, []any{newID(), s.Tenant, s.Sub, actor}},
			{`INSERT INTO seat_change_schedules (schedule_id, tenant_id, billing_subscription_id, target_seats, effective_at, requested_by)
			  VALUES ($1, $2, $3, 8, now() + interval '30 days', $4)`, []any{newID(), s.Tenant, s.Sub, actor}},
			{`INSERT INTO quota_addon_purchases (purchase_id, tenant_id, pack_code, pack_quantity, tokens_total, amount_minor, currency, status,
			    requested_by, idempotency_key, confirmed_at)
			  VALUES ($1, $2, 'tokens_5m', 1, 5000000, 1500, 'USD', 'confirmed', $3, 'key-'||$1::uuid::text, now())`, []any{s.AddonPurchase, s.Tenant, actor}},
			{`INSERT INTO invoices (invoice_id, tenant_id, provider, provider_invoice_id, kind, status, currency, amount_due_minor, amount_paid_minor, provider_updated_at)
			  VALUES ($1, $2, 'stripe', 'in_'||$1::uuid::text, 'token_addon', 'paid', 'USD', 1500, 1500, now())`, []any{s.Invoice, s.Tenant}},
			{`INSERT INTO invoice_links (invoice_id, tenant_id, ref_type, ref_id) VALUES ($1, $2, 'quota_addon_purchase', $3)`, []any{s.Invoice, s.Tenant, s.AddonPurchase}},
			{`INSERT INTO adjustments (adjustment_id, tenant_id, kind, amount_minor, currency, description, reason, actor_id, idempotency_key)
			  VALUES ($1, $2, 'credit', 500, 'USD', 'Goodwill', 'Service credit for outage', $3, 'key-'||$1::uuid::text)`, []any{newID(), s.Tenant, actor}},
			{`INSERT INTO refunds (refund_id, tenant_id, invoice_id, amount_minor, currency, category, reason, status, addon_purchase_id, addon_tokens, actor_id, idempotency_key)
			  VALUES ($1, $2, $3, 1500, 'USD', 'duplicate_charge', 'Second add-on charge refunded', 'pending', $4, 5000000, $5, 'key-'||$1::uuid::text)`, []any{newID(), s.Tenant, s.Invoice, s.AddonPurchase, actor}},
			{`INSERT INTO disputes (dispute_id, tenant_id, provider, provider_dispute_id, kind, payment_ref, invoice_id, is_conversion_payment,
			    within_revert_window, reason, provider_status, opened_at)
			  VALUES ($1, $2, 'stripe', 'dp_'||$1::uuid::text, 'dispute', 'pi_1', $3, false, false, 'general', 'needs_response', now())`, []any{newID(), s.Tenant, s.Invoice}},
			{`INSERT INTO idempotency_records (tenant_id, scope, key, request_hash, state, expires_at)
			  VALUES ($1, 'B-P7', 'key-12345678', '\x00', 'completed', now() + interval '72 hours')`, []any{s.Tenant}},
			{`INSERT INTO scheduled_emissions (emission_id, tenant_id, event_type, payload_builder, ref_id, due_at)
			  VALUES ($1, $2, 'TenantSeatsChanged', 'seats_resync', $3, now())`, []any{newID(), s.Tenant, s.Sub}},
			{`INSERT INTO usage_period_records (tenant_id, period_start, period_end, tokens_total, api_requests_total, limits, source_event_id, source_event_time)
			  VALUES ($1, date_trunc('month', now()), date_trunc('month', now()) + interval '1 month', 10, 2, '{}', $2, now())`, []any{s.Tenant, newID()}},
		}
		for _, st := range stmts {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return fmt.Errorf("%s: %w", firstLine(st.sql), err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return s
}
