package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// JobRepository is port.JobRepo: the §13.2 CronJobs list work across
// tenants as billing_system (§4.3, read-only on tenant tables) and write in
// tenant transactions on the app pool.
type JobRepository struct{ app, sys *pgcommon.Pool }

var _ port.JobRepo = (*JobRepository)(nil)

// NewJobRepository wraps the app pool (tenant-scoped writes) and the
// system pool (cross-tenant selection).
func NewJobRepository(appPool, sysPool *pgcommon.Pool) *JobRepository {
	return &JobRepository{app: appPool, sys: sysPool}
}

// OverdueSagas implements port.JobRepo.
func (r *JobRepository) OverdueSagas(ctx context.Context, before time.Time) ([]port.SagaRef, int, error) {
	var out []port.SagaRef
	var overdue int
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT saga_id::text, tenant_id::text FROM conversion_sagas
			WHERE alerted_at IS NULL AND payment_confirmed_at < $1
			  AND state IN ('payment_confirmed','rp_convert_pending','awaiting_tenant_converted')`, before)
		if err != nil {
			return err
		}
		if out, err = collect(rows, func(row pgcommon.Row) (port.SagaRef, error) {
			var s port.SagaRef
			return s, row.Scan(&s.SagaID, &s.TenantID)
		}); err != nil {
			return err
		}
		return q.QueryRow(ctx, `SELECT count(*) FROM conversion_sagas WHERE payment_confirmed_at < $1
			AND state IN ('payment_confirmed','rp_convert_pending','awaiting_tenant_converted')`, before).Scan(&overdue)
	})
	return out, overdue, err
}

// MarkSagaAlerted implements port.JobRepo (tenant transaction, app pool).
func (r *JobRepository) MarkSagaAlerted(ctx context.Context, sagaID string) (bool, error) {
	var n int64
	err := inTx(ctx, r.app, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE conversion_sagas SET alerted_at = now() WHERE saga_id = $1::uuid AND alerted_at IS NULL`, sagaID)
		n = tag.RowsAffected()
		return err
	})
	return n == 1, err
}

// StaleWork implements port.JobRepo. Renewal top-ups are never expired:
// their seats are kept and the invoice follows dunning (§8.6.3).
func (r *JobRepository) StaleWork(ctx context.Context, now, abandonBefore, scaBefore time.Time, limit int) ([]port.StaleWork, error) {
	var out []port.StaleWork
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `
			(SELECT 'seat_purchase', tenant_id::text, purchase_id::text FROM seat_purchases
			  WHERE status IN ('pending_payment','requires_action') AND reason = 'purchase' AND created_at < $2)
			UNION ALL
			(SELECT 'addon_purchase', tenant_id::text, purchase_id::text FROM quota_addon_purchases
			  WHERE status IN ('pending_payment','requires_action') AND created_at < $2)
			UNION ALL
			(SELECT 'upgrade', tenant_id::text, change_id::text FROM plan_change_requests
			  WHERE status = 'pending_payment' AND created_at < $2)
			UNION ALL
			(SELECT 'checkout', tenant_id::text, session_id::text FROM checkout_sessions
			  WHERE status = 'open' AND expires_at < $1)
			UNION ALL
			(SELECT 'signup', tenant_id::text, billing_subscription_id::text FROM subscriptions
			  WHERE status = 'incomplete' AND origin = 'direct_signup' AND created_at < $3)
			LIMIT $4`, now, abandonBefore, scaBefore, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (port.StaleWork, error) {
			var w port.StaleWork
			return w, row.Scan(&w.Kind, &w.TenantID, &w.ID)
		})
		return err
	})
	return out, err
}

// LiveSubscriptions implements port.JobRepo.
func (r *JobRepository) LiveSubscriptions(ctx context.Context, afterID string, limit int) ([]port.LiveSubscription, error) {
	var out []port.LiveSubscription
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT s.tenant_id::text, s.billing_subscription_id::text, i.updated_at
			FROM subscriptions s JOIN subscription_items i ON i.billing_subscription_id = s.billing_subscription_id AND i.kind = 'seats'
			WHERE s.status IN ('active','past_due') AND s.billing_subscription_id > COALESCE(NULLIF($1,'')::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
			ORDER BY s.billing_subscription_id LIMIT $2`, afterID, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (port.LiveSubscription, error) {
			var l port.LiveSubscription
			return l, row.Scan(&l.TenantID, &l.BillingSubscriptionID, &l.ItemUpdatedAt)
		})
		return err
	})
	return out, err
}

// RecentResyncs implements port.JobRepo.
func (r *JobRepository) RecentResyncs(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := inTx(ctx, r.sys, func(q querier) error {
		return q.QueryRow(ctx, `SELECT count(*) FROM scheduled_emissions
			WHERE tenant_id = $1::uuid AND payload_builder = 'seats_resync' AND created_at >= $2`, tenantID, since).Scan(&n)
	})
	return n, err
}

// PIICandidates implements port.JobRepo (§15.3). Abandoned: no paid
// invoice, every subscription ended before abandonedBefore, and no
// checkout started since.
func (r *JobRepository) PIICandidates(ctx context.Context, offboardedBefore, abandonedBefore time.Time, limit int) ([]port.PIICandidate, error) {
	var out []port.PIICandidate
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT a.tenant_id::text, a.provider, a.provider_customer_id FROM billing_accounts a
			WHERE a.pii_minimised_at IS NULL AND (
			  (a.closed_at IS NOT NULL AND a.closed_at < $1)
			  OR (EXISTS (SELECT 1 FROM subscriptions s WHERE s.tenant_id = a.tenant_id)
			      AND NOT EXISTS (SELECT 1 FROM subscriptions s WHERE s.tenant_id = a.tenant_id AND (s.status <> 'ended' OR s.ended_at >= $2))
			      AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.tenant_id = a.tenant_id AND i.status = 'paid')
			      AND NOT EXISTS (SELECT 1 FROM checkout_sessions c WHERE c.tenant_id = a.tenant_id AND c.created_at >= $2)))
			ORDER BY a.tenant_id LIMIT $3`, offboardedBefore, abandonedBefore, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (port.PIICandidate, error) {
			var c port.PIICandidate
			return c, row.Scan(&c.TenantID, &c.Provider, &c.ProviderCustomerID)
		})
		return err
	})
	return out, err
}

// ExpiredIdempotencyTenants implements port.JobRepo.
func (r *JobRepository) ExpiredIdempotencyTenants(ctx context.Context, now time.Time, limit int) ([]string, error) {
	var out []string
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT DISTINCT tenant_id::text FROM idempotency_records
			WHERE expires_at < $1 AND state <> 'in_flight' LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, scanOne[string])
		return err
	})
	return out, err
}

// PruneIdempotency implements port.JobRepo (tenant transaction, app pool).
func (r *JobRepository) PruneIdempotency(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := inTx(ctx, r.app, func(q querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM idempotency_records WHERE expires_at < $1 AND state <> 'in_flight'`, now)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}
