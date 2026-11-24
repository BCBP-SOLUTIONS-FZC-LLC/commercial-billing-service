package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// SubscriptionRepository implements port.SubscriptionRepo on the app pool.
type SubscriptionRepository struct{ pool *pgcommon.Pool }

var _ port.SubscriptionRepo = (*SubscriptionRepository)(nil)

// NewSubscriptionRepository returns the repository.
func NewSubscriptionRepository(appPool *pgcommon.Pool) *SubscriptionRepository {
	return &SubscriptionRepository{pool: appPool}
}

const subscriptionColumns = `billing_subscription_id, tenant_id, provider, provider_subscription_id, origin, plan, interval,
  status, auto_renew, current_period_start, current_period_end, collection_paused, collection_paused_by,
  payment_confirmed_at, converted_at, past_due_since, cancelled_at, cancel_reason, ended_at, record_version,
  created_at, updated_at`

func scanSubscription(row pgx.Row) (domain.Subscription, error) {
	var s domain.Subscription
	err := row.Scan(&s.BillingSubscriptionID, &s.TenantID, &s.Provider, &s.ProviderSubscriptionID, &s.Origin, &s.Plan,
		&s.Interval, &s.Status, &s.AutoRenew, &s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.CollectionPaused,
		&s.CollectionPausedBy, &s.PaymentConfirmedAt, &s.ConvertedAt, &s.PastDueSince, &s.CancelledAt,
		&s.CancelReason, &s.EndedAt, &s.RecordVersion, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// Create inserts the subscription and its seat item in the caller's
// transaction. A second live subscription violates uq_sub_one_live and is
// returned as 409 SUBSCRIPTION_ALREADY_EXISTS (BIL-SUB-3).
func (r *SubscriptionRepository) Create(ctx context.Context, s domain.Subscription, item domain.SubscriptionItem) error {
	if s.Status == "" {
		s.Status = domain.StatusIncomplete
	}
	err := inTx(ctx, r.pool, func(q querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO subscriptions (billing_subscription_id, tenant_id, provider, provider_subscription_id, origin, plan,
			  interval, status, auto_renew, current_period_start, current_period_end, payment_confirmed_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			s.BillingSubscriptionID, s.TenantID, s.Provider, s.ProviderSubscriptionID, s.Origin, s.Plan, s.Interval,
			s.Status, s.AutoRenew, s.CurrentPeriodStart, s.CurrentPeriodEnd, s.PaymentConfirmedAt); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO subscription_items (tenant_id, billing_subscription_id, kind, price_id, quantity, provider_item_id)
			VALUES ($1::uuid, $2::uuid, 'seats', $3::uuid, $4, $5)`,
			s.TenantID, s.BillingSubscriptionID, item.PriceID, item.Quantity, item.ProviderItemID)
		return err
	})
	if sqlState(err) == "23505" {
		return domain.NewError(domain.ErrSubscriptionAlreadyExists, "tenant already has a live subscription")
	}
	return err
}

// Get returns one subscription.
func (r *SubscriptionRepository) Get(ctx context.Context, id string) (domain.Subscription, error) {
	var s domain.Subscription
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		s, err = scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE billing_subscription_id = $1::uuid`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, subscriptionNotFound("subscription", id)
	}
	return s, err
}

// Live returns the tenant's non-ended subscription, if any (BIL-SUB-3).
func (r *SubscriptionRepository) Live(ctx context.Context, tenantID string) (domain.Subscription, bool, error) {
	var s domain.Subscription
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		s, err = scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE tenant_id = $1::uuid AND status <> 'ended'`, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

// Item returns the subscription's seat item.
func (r *SubscriptionRepository) Item(ctx context.Context, id string) (domain.SubscriptionItem, error) {
	var it domain.SubscriptionItem
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT item_id, tenant_id, billing_subscription_id, kind, price_id, quantity, provider_item_id,
			  scheduled_price_id, scheduled_price_at, record_version, updated_at
			FROM subscription_items WHERE billing_subscription_id = $1::uuid AND kind = 'seats'`, id).
			Scan(&it.ItemID, &it.TenantID, &it.BillingSubscriptionID, &it.Kind, &it.PriceID, &it.Quantity, &it.ProviderItemID,
				&it.ScheduledPriceID, &it.ScheduledPriceAt, &it.RecordVersion, &it.UpdatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return it, subscriptionNotFound("seat item of subscription", id)
	}
	return it, err
}

// Transition moves the status along the §8.14 table (BIL-SUB-4) with
// optimistic locking. A move the table forbids returns 500 INTERNAL (a code
// path) before touching the row; callbacks check CanTransition first and
// record ignored instead (BIL-PAY-4).
func (r *SubscriptionRepository) Transition(ctx context.Context, id string, expectedVersion int64, to domain.SubscriptionStatus,
	mutate func(*domain.Subscription)) (domain.Subscription, error) {
	var out domain.Subscription
	err := inTx(ctx, r.pool, func(q querier) error {
		cur, err := scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE billing_subscription_id = $1::uuid FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return subscriptionNotFound("subscription", id)
		}
		if err != nil {
			return err
		}
		if expectedVersion != 0 && cur.RecordVersion != expectedVersion { // 0: background writer under the tenant lock (BIL-CONC-3)
			return domain.NewError(domain.ErrOptimisticLockConflict, "subscription changed since it was read").With("record_version", cur.RecordVersion)
		}
		if !domain.CanTransition(cur.Status, to) {
			return domain.NewError(domain.ErrInternal, fmt.Sprintf("subscription transition %s → %s is not allowed (BIL-SUB-4)", cur.Status, to))
		}
		next := cur
		next.Status = to
		if mutate != nil {
			mutate(&next)
		}
		out, err = writeSubscription(ctx, q, next)
		return err
	})
	return out, err
}

func writeSubscription(ctx context.Context, q querier, next domain.Subscription) (domain.Subscription, error) {
	return scanSubscription(q.QueryRow(ctx, `
		UPDATE subscriptions SET status = $2, auto_renew = $3, current_period_start = $4, current_period_end = $5,
		  collection_paused = $6, collection_paused_by = $7, payment_confirmed_at = $8, converted_at = $9,
		  past_due_since = $10, cancelled_at = $11, cancel_reason = $12, ended_at = $13, provider_subscription_id = $14,
		  plan = $15, interval = $16
		WHERE billing_subscription_id = $1::uuid
		RETURNING `+subscriptionColumns,
		next.BillingSubscriptionID, next.Status, next.AutoRenew, next.CurrentPeriodStart, next.CurrentPeriodEnd, next.CollectionPaused,
		next.CollectionPausedBy, next.PaymentConfirmedAt, next.ConvertedAt, next.PastDueSince, next.CancelledAt,
		next.CancelReason, next.EndedAt, next.ProviderSubscriptionID, next.Plan, next.Interval))
}

// Update changes fields other than status (period, provider id, pause,
// plan, converted_at …) under the tenant lock; expectedVersion 0 skips the
// version check (background writers, BIL-CONC-3).
func (r *SubscriptionRepository) Update(ctx context.Context, id string, expectedVersion int64, mutate func(*domain.Subscription)) (domain.Subscription, error) {
	var out domain.Subscription
	err := inTx(ctx, r.pool, func(q querier) error {
		cur, err := scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE billing_subscription_id = $1::uuid FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return subscriptionNotFound("subscription", id)
		}
		if err != nil {
			return err
		}
		if expectedVersion != 0 && cur.RecordVersion != expectedVersion {
			return domain.NewError(domain.ErrOptimisticLockConflict, "subscription changed since it was read").With("record_version", cur.RecordVersion)
		}
		next := cur
		mutate(&next)
		next.Status = cur.Status
		out, err = writeSubscription(ctx, q, next)
		return err
	})
	return out, err
}

// ByProvider returns the subscription with this provider subscription id.
func (r *SubscriptionRepository) ByProvider(ctx context.Context, provider, providerSubscriptionID string) (domain.Subscription, bool, error) {
	var s domain.Subscription
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		s, err = scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE provider = $1 AND provider_subscription_id = $2`,
			provider, providerSubscriptionID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

// Latest returns the tenant's live subscription, else its most recent ended
// one (B-P1 shows an ended subscription rather than 404).
func (r *SubscriptionRepository) Latest(ctx context.Context, tenantID string) (domain.Subscription, bool, error) {
	var s domain.Subscription
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		s, err = scanSubscription(q.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE tenant_id = $1::uuid
			ORDER BY (status <> 'ended') DESC, created_at DESC LIMIT 1`, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

// SetItem updates the seat item's quantity, price and provider item id.
func (r *SubscriptionRepository) SetItem(ctx context.Context, it domain.SubscriptionItem) error {
	return inTx(ctx, r.pool, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE subscription_items SET quantity = $2, price_id = $3::uuid, provider_item_id = $4,
			  scheduled_price_id = $5::uuid, scheduled_price_at = $6
			WHERE billing_subscription_id = $1::uuid AND kind = 'seats'`,
			it.BillingSubscriptionID, it.Quantity, it.PriceID, it.ProviderItemID, it.ScheduledPriceID, it.ScheduledPriceAt)
		if err == nil && tag.RowsAffected() != 1 {
			return subscriptionNotFound("seat item of subscription", it.BillingSubscriptionID)
		}
		return err
	})
}

// AppendHistory writes one §4.2.6 row in the caller's transaction.
func (r *SubscriptionRepository) AppendHistory(ctx context.Context, h domain.StatusHistoryEntry) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO subscription_status_history (tenant_id, billing_subscription_id, from_status, to_status, from_plan, to_plan,
			  from_seats, to_seats, cause, payment_event_ref, actor_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11::uuid)`,
			h.TenantID, h.BillingSubscriptionID, h.FromStatus, h.ToStatus, h.FromPlan, h.ToPlan, h.FromSeats, h.ToSeats,
			h.Cause, h.PaymentEventRef, h.ActorID)
		return err
	})
}

// History returns the newest entries first.
func (r *SubscriptionRepository) History(ctx context.Context, id string, limit int) ([]domain.StatusHistoryEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []domain.StatusHistoryEntry
	err := inTx(ctx, r.pool, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT tenant_id, billing_subscription_id, from_status, to_status, from_plan, to_plan, from_seats, to_seats,
			  cause, payment_event_ref, actor_id, occurred_at
			FROM subscription_status_history WHERE billing_subscription_id = $1::uuid
			ORDER BY occurred_at DESC, id DESC LIMIT $2`, id, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h domain.StatusHistoryEntry
			if err := rows.Scan(&h.TenantID, &h.BillingSubscriptionID, &h.FromStatus, &h.ToStatus, &h.FromPlan, &h.ToPlan,
				&h.FromSeats, &h.ToSeats, &h.Cause, &h.PaymentEventRef, &h.ActorID, &h.OccurredAt); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}
