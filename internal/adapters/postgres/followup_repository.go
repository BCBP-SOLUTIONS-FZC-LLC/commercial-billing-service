package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// FollowUpRepository is port.FollowUpRepo on the system pool.
type FollowUpRepository struct{ sys *pgcommon.Pool }

var _ port.FollowUpRepo = (*FollowUpRepository)(nil)

// NewFollowUpRepository wraps the system pool.
func NewFollowUpRepository(sysPool *pgcommon.Pool) *FollowUpRepository {
	return &FollowUpRepository{sys: sysPool}
}

// Due implements port.FollowUpRepo: operator suspension → pause (when
// AUTO_PAUSE_ON_OPERATOR_SUSPENSION), reactivation → resume an automatic
// pause, offboarded → close the live subscription.
func (r *FollowUpRepository) Due(ctx context.Context, autoPause bool, limit int) ([]port.FollowUp, error) {
	var out []port.FollowUp
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT a.tenant_id::text, s.billing_subscription_id::text,
			  CASE WHEN a.iam_lifecycle = 'offboarded' THEN 'close'
			       WHEN a.iam_lifecycle = 'suspended' THEN 'pause'
			       ELSE 'resume' END
			FROM billing_accounts a JOIN subscriptions s ON s.tenant_id = a.tenant_id AND s.status <> 'ended'
			WHERE (a.iam_lifecycle = 'offboarded')
			   OR ($1 AND a.iam_lifecycle = 'suspended' AND a.iam_suspension_source = 'operator'
			       AND s.status IN ('active','past_due') AND NOT s.collection_paused)
			   OR (a.iam_lifecycle = 'active' AND s.collection_paused AND s.collection_paused_by = 'auto')
			ORDER BY a.updated_at LIMIT $2`, autoPause, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.FollowUp, error) {
			var f port.FollowUp
			return f, row.Scan(&f.TenantID, &f.BillingSubscriptionID, &f.Action)
		})
		return err
	})
	return out, err
}
