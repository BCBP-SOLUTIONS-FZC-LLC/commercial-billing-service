package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// SeatPurchaseRepository is port.SeatPurchaseRepo (§4.2.10).
type SeatPurchaseRepository struct{ pool *pgcommon.Pool }

var _ port.SeatPurchaseRepo = (*SeatPurchaseRepository)(nil)

// NewSeatPurchaseRepository wraps the app pool.
func NewSeatPurchaseRepository(appPool *pgcommon.Pool) *SeatPurchaseRepository {
	return &SeatPurchaseRepository{pool: appPool}
}

const seatPurchaseColumns = `purchase_id::text, tenant_id::text, billing_subscription_id::text, reason, seats_added, seats_before,
  seats_after, proration_instant, amount_minor, currency, amount_source, unit_amount_minor, remaining_seconds, period_seconds,
  amount_unrounded_minor::text, deferred_amount_minor, deferral_reason, status, provider_invoice_id, client_action::text,
  requested_by::text, idempotency_key, confirmed_at, created_at, updated_at`

func scanSeatPurchase(row pgcommon.Row) (domain.SeatPurchase, error) {
	var p domain.SeatPurchase
	var action *string
	err := row.Scan(&p.PurchaseID, &p.TenantID, &p.BillingSubscriptionID, &p.Reason, &p.SeatsAdded, &p.SeatsBefore, &p.SeatsAfter,
		&p.ProrationInstant, &p.AmountMinor, &p.Currency, &p.AmountSource, &p.UnitAmountMinor, &p.RemainingSeconds, &p.PeriodSeconds,
		&p.AmountUnroundedMinor, &p.DeferredAmountMinor, &p.DeferralReason, &p.Status, &p.ProviderInvoiceID, &action,
		&p.RequestedBy, &p.IdempotencyKey, &p.ConfirmedAt, &p.CreatedAt, &p.UpdatedAt)
	if action != nil {
		p.ClientAction = []byte(*action)
	}
	return p, err
}

// Insert implements port.SeatPurchaseRepo; uq_sp_one_open → 409
// SEAT_PURCHASE_IN_PROGRESS (BIL-SEAT-1).
func (r *SeatPurchaseRepository) Insert(ctx context.Context, p domain.SeatPurchase) error {
	err := inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO seat_purchases (purchase_id, tenant_id, billing_subscription_id, reason, seats_added, seats_before, seats_after,
			  proration_instant, amount_minor, currency, amount_source, unit_amount_minor, remaining_seconds, period_seconds,
			  amount_unrounded_minor, deferred_amount_minor, deferral_reason, status, requested_by, idempotency_key)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::numeric, $16, $17, $18, $19::uuid, $20)`,
			p.PurchaseID, p.TenantID, p.BillingSubscriptionID, p.Reason, p.SeatsAdded, p.SeatsBefore, p.SeatsAfter, p.ProrationInstant,
			p.AmountMinor, p.Currency, p.AmountSource, p.UnitAmountMinor, p.RemainingSeconds, p.PeriodSeconds, p.AmountUnroundedMinor,
			p.DeferredAmountMinor, p.DeferralReason, p.Status, p.RequestedBy, p.IdempotencyKey)
		return err
	})
	if pgcommon.IsUniqueViolation(err) {
		return domain.NewError(domain.ErrSeatPurchaseInProgress, "a seat purchase is already in progress")
	}
	return err
}

// Get implements port.SeatPurchaseRepo (locked).
func (r *SeatPurchaseRepository) Get(ctx context.Context, purchaseID string) (domain.SeatPurchase, bool, error) {
	return r.one(ctx, `WHERE purchase_id = $1::uuid FOR UPDATE`, purchaseID)
}

// Open implements port.SeatPurchaseRepo: the subscription's open purchase.
func (r *SeatPurchaseRepository) Open(ctx context.Context, subID string) (domain.SeatPurchase, bool, error) {
	return r.one(ctx, `WHERE billing_subscription_id = $1::uuid AND status IN ('pending_payment','requires_action') FOR UPDATE`, subID)
}

// ByProviderInvoice implements port.SeatPurchaseRepo.
func (r *SeatPurchaseRepository) ByProviderInvoice(ctx context.Context, tenantID, providerInvoiceID string) (domain.SeatPurchase, bool, error) {
	return r.one(ctx, `WHERE tenant_id = $1::uuid AND provider_invoice_id = $2 FOR UPDATE`, tenantID, providerInvoiceID)
}

func (r *SeatPurchaseRepository) one(ctx context.Context, where string, args ...any) (domain.SeatPurchase, bool, error) {
	var p domain.SeatPurchase
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		p, err = scanSeatPurchase(q.QueryRow(ctx, `SELECT `+seatPurchaseColumns+` FROM seat_purchases `+where, args...))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// List implements port.SeatPurchaseRepo: newest first, keyset on
// (created_at, purchase_id) after the cursor row.
func (r *SeatPurchaseRepository) List(ctx context.Context, tenantID, afterID string, limit int) ([]domain.SeatPurchase, error) {
	var out []domain.SeatPurchase
	err := inTx(ctx, r.pool, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+seatPurchaseColumns+` FROM seat_purchases p WHERE tenant_id = $1::uuid
			AND ($2 = '' OR (created_at, purchase_id) < (SELECT created_at, purchase_id FROM seat_purchases WHERE purchase_id = NULLIF($2, '')::uuid))
			ORDER BY created_at DESC, purchase_id DESC LIMIT $3`, tenantID, afterID, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (domain.SeatPurchase, error) { return scanSeatPurchase(row) })
		return err
	})
	return out, err
}

// Update implements port.SeatPurchaseRepo.
func (r *SeatPurchaseRepository) Update(ctx context.Context, p domain.SeatPurchase) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE seat_purchases SET status = $2, provider_invoice_id = $3, client_action = $4::jsonb,
			confirmed_at = $5, updated_at = now() WHERE purchase_id = $1::uuid`,
			p.PurchaseID, p.Status, p.ProviderInvoiceID, jsonTextOrNil(p.ClientAction), p.ConfirmedAt)
		return err
	})
}

// ScheduleRepository is port.ScheduleRepo (§4.2.11).
type ScheduleRepository struct{ app, sys *pgcommon.Pool }

var _ port.ScheduleRepo = (*ScheduleRepository)(nil)

// NewScheduleRepository wraps both pools (the renewal job scans on sys).
func NewScheduleRepository(appPool, sysPool *pgcommon.Pool) *ScheduleRepository {
	return &ScheduleRepository{app: appPool, sys: sysPool}
}

const scheduleColumns = `schedule_id::text, tenant_id::text, billing_subscription_id::text, target_seats, effective_at, status,
  seats_in_use_at_apply, renewal_seats, final_seats, provider_schedule_id, skip_reason, requested_by::text, record_version,
  created_at, updated_at`

func scanSchedule(row pgcommon.Row) (domain.SeatChangeSchedule, error) {
	var s domain.SeatChangeSchedule
	err := row.Scan(&s.ScheduleID, &s.TenantID, &s.BillingSubscriptionID, &s.TargetSeats, &s.EffectiveAt, &s.Status,
		&s.SeatsInUseAtApply, &s.RenewalSeats, &s.FinalSeats, &s.ProviderScheduleID, &s.SkipReason, &s.RequestedBy,
		&s.RecordVersion, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// Insert implements port.ScheduleRepo.
func (r *ScheduleRepository) Insert(ctx context.Context, s domain.SeatChangeSchedule) error {
	return inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO seat_change_schedules (schedule_id, tenant_id, billing_subscription_id, target_seats,
			effective_at, status, requested_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7::uuid)`,
			s.ScheduleID, s.TenantID, s.BillingSubscriptionID, s.TargetSeats, s.EffectiveAt, s.Status, s.RequestedBy)
		return err
	})
}

// Open implements port.ScheduleRepo (locked).
func (r *ScheduleRepository) Open(ctx context.Context, subID string) (domain.SeatChangeSchedule, bool, error) {
	return r.one(ctx, `WHERE billing_subscription_id = $1::uuid AND status IN ('scheduled','applying') FOR UPDATE`, subID)
}

// Latest implements port.ScheduleRepo: the most recent schedule.
func (r *ScheduleRepository) Latest(ctx context.Context, subID string) (domain.SeatChangeSchedule, bool, error) {
	return r.one(ctx, `WHERE billing_subscription_id = $1::uuid ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, subID)
}

// Get implements port.ScheduleRepo (locked).
func (r *ScheduleRepository) Get(ctx context.Context, scheduleID string) (domain.SeatChangeSchedule, bool, error) {
	return r.one(ctx, `WHERE schedule_id = $1::uuid FOR UPDATE`, scheduleID)
}

func (r *ScheduleRepository) one(ctx context.Context, where string, args ...any) (domain.SeatChangeSchedule, bool, error) {
	var s domain.SeatChangeSchedule
	err := inTx(ctx, r.app, func(q querier) error {
		var err error
		s, err = scanSchedule(q.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM seat_change_schedules `+where, args...))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

// Update implements port.ScheduleRepo.
func (r *ScheduleRepository) Update(ctx context.Context, s domain.SeatChangeSchedule) error {
	return inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE seat_change_schedules SET status = $2, seats_in_use_at_apply = $3, renewal_seats = $4,
			final_seats = $5, provider_schedule_id = $6, skip_reason = $7 WHERE schedule_id = $1::uuid`,
			s.ScheduleID, s.Status, s.SeatsInUseAtApply, s.RenewalSeats, s.FinalSeats, s.ProviderScheduleID, s.SkipReason)
		return err
	})
}

// Due implements port.ScheduleRepo: open schedules effective before
// until, across tenants (system pool).
func (r *ScheduleRepository) Due(ctx context.Context, until time.Time, limit int) ([]domain.SeatChangeSchedule, error) {
	var out []domain.SeatChangeSchedule
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+scheduleColumns+` FROM seat_change_schedules
			WHERE status IN ('scheduled','applying') AND effective_at <= $1 ORDER BY effective_at LIMIT $2`, until, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (domain.SeatChangeSchedule, error) { return scanSchedule(row) })
		return err
	})
	return out, err
}
