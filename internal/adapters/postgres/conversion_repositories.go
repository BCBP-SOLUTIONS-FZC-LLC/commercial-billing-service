package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// CheckoutRepository is port.CheckoutRepo (§4.2.7).
type CheckoutRepository struct{ pool *pgcommon.Pool }

var _ port.CheckoutRepo = (*CheckoutRepository)(nil)

// NewCheckoutRepository wraps the app pool.
func NewCheckoutRepository(appPool *pgcommon.Pool) *CheckoutRepository {
	return &CheckoutRepository{pool: appPool}
}

const checkoutColumns = `session_id::text, tenant_id::text, purpose, provider_session_id, billing_subscription_id::text,
  addon_purchase_id::text, requested_plan, requested_interval, requested_seats, status, requested_by::text, expires_at, created_at`

func scanCheckout(row pgcommon.Row) (domain.CheckoutSession, error) {
	var c domain.CheckoutSession
	err := row.Scan(&c.SessionID, &c.TenantID, &c.Purpose, &c.ProviderSessionID, &c.BillingSubscriptionID, &c.AddonPurchaseID,
		&c.RequestedPlan, &c.RequestedInterval, &c.RequestedSeats, &c.Status, &c.RequestedBy, &c.ExpiresAt, &c.CreatedAt)
	return c, err
}

// Insert implements port.CheckoutRepo.
func (r *CheckoutRepository) Insert(ctx context.Context, c domain.CheckoutSession) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO checkout_sessions (session_id, tenant_id, purpose, provider_session_id, billing_subscription_id,
			  addon_purchase_id, requested_plan, requested_interval, requested_seats, status, requested_by, expires_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6::uuid, $7, $8, $9, 'open', $10::uuid, $11)`,
			c.SessionID, c.TenantID, c.Purpose, c.ProviderSessionID, c.BillingSubscriptionID, c.AddonPurchaseID,
			c.RequestedPlan, c.RequestedInterval, c.RequestedSeats, c.RequestedBy, c.ExpiresAt)
		return err
	})
}

// Open implements port.CheckoutRepo: the tenant's open session for purpose.
func (r *CheckoutRepository) Open(ctx context.Context, tenantID, purpose string) (domain.CheckoutSession, bool, error) {
	return r.one(ctx, `WHERE tenant_id = $1::uuid AND purpose = $2 AND status = 'open' ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, tenantID, purpose)
}

// ByProviderSession implements port.CheckoutRepo.
func (r *CheckoutRepository) ByProviderSession(ctx context.Context, providerSessionID string) (domain.CheckoutSession, bool, error) {
	return r.one(ctx, `WHERE provider_session_id = $1 FOR UPDATE`, providerSessionID)
}

func (r *CheckoutRepository) one(ctx context.Context, where string, args ...any) (domain.CheckoutSession, bool, error) {
	var c domain.CheckoutSession
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		c, err = scanCheckout(q.QueryRow(ctx, `SELECT `+checkoutColumns+` FROM checkout_sessions `+where, args...))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// SetProviderSession implements port.CheckoutRepo.
func (r *CheckoutRepository) SetProviderSession(ctx context.Context, sessionID, providerSessionID string, expiresAt time.Time) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE checkout_sessions SET provider_session_id = $2, expires_at = $3 WHERE session_id = $1::uuid`,
			sessionID, providerSessionID, expiresAt)
		return err
	})
}

// SetStatus implements port.CheckoutRepo (open → completed | expired).
func (r *CheckoutRepository) SetStatus(ctx context.Context, sessionID, status string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE checkout_sessions SET status = $2 WHERE session_id = $1::uuid AND status = 'open'`, sessionID, status)
		return err
	})
}

// ExpireAll implements port.CheckoutRepo: every open session of the tenant
// (offboarding, §8.8.4).
func (r *CheckoutRepository) ExpireAll(ctx context.Context, tenantID string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE checkout_sessions SET status = 'expired' WHERE tenant_id = $1::uuid AND status = 'open'`, tenantID)
		return err
	})
}

// MarkDefaultApplied implements port.CheckoutRepo: the setup session's
// payment method is now the provider default.
func (r *CheckoutRepository) MarkDefaultApplied(ctx context.Context, sessionID string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE checkout_sessions SET default_applied_at = now() WHERE session_id = $1::uuid`, sessionID)
		return err
	})
}

// GetSession implements port.CheckoutRepo.
func (r *CheckoutRepository) GetSession(ctx context.Context, sessionID string) (domain.CheckoutSession, bool, error) {
	return r.one(ctx, `WHERE session_id = $1::uuid`, sessionID)
}

// SagaRepository is port.SagaRepo (§4.2.8).
type SagaRepository struct{ app, sys *pgcommon.Pool }

var _ port.SagaRepo = (*SagaRepository)(nil)

// NewSagaRepository wraps both pools: tenant work on the app pool, the
// cross-tenant due scan on the system pool.
func NewSagaRepository(appPool, sysPool *pgcommon.Pool) *SagaRepository {
	return &SagaRepository{app: appPool, sys: sysPool}
}

const sagaColumns = `saga_id::text, tenant_id::text, billing_subscription_id::text, kind, state, payment_confirmed_at,
  rp_idempotency_key, attempts, next_attempt_at, last_error_code, last_error_at, rp_accepted_at,
  tenant_converted_event::text, confirmed_at, alerted_at, resolution, resolution_reason, resolved_by::text,
  reversal_requested_at, reversal_requested_by::text, reversal_reason, reversal_refund, reversal_dispute_id::text,
  reversal_rp_done_at, record_version, created_at, updated_at`

func scanSaga(row pgcommon.Row) (domain.ConversionSaga, error) {
	var s domain.ConversionSaga
	err := row.Scan(&s.SagaID, &s.TenantID, &s.BillingSubscriptionID, &s.Kind, &s.State, &s.PaymentConfirmedAt,
		&s.RPIdempotencyKey, &s.Attempts, &s.NextAttemptAt, &s.LastErrorCode, &s.LastErrorAt, &s.RPAcceptedAt,
		&s.TenantConvertedEvent, &s.ConfirmedAt, &s.AlertedAt, &s.Resolution, &s.ResolutionReason, &s.ResolvedBy,
		&s.ReversalRequestedAt, &s.ReversalRequestedBy, &s.ReversalReason, &s.ReversalRefund, &s.ReversalDisputeID, &s.ReversalRPDoneAt,
		&s.RecordVersion, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// Insert implements port.SagaRepo.
func (r *SagaRepository) Insert(ctx context.Context, s domain.ConversionSaga) error {
	return inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO conversion_sagas (saga_id, tenant_id, billing_subscription_id, kind, state, payment_confirmed_at,
			  rp_idempotency_key, next_attempt_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)`,
			s.SagaID, s.TenantID, s.BillingSubscriptionID, s.Kind, s.State, s.PaymentConfirmedAt, s.RPIdempotencyKey, s.NextAttemptAt)
		return err
	})
}

// BySubscription implements port.SagaRepo (locks the row).
func (r *SagaRepository) BySubscription(ctx context.Context, billingSubscriptionID string) (domain.ConversionSaga, bool, error) {
	return r.one(ctx, `WHERE billing_subscription_id = $1::uuid FOR UPDATE`, billingSubscriptionID)
}

// Get implements port.SagaRepo (locks the row).
func (r *SagaRepository) Get(ctx context.Context, sagaID string) (domain.ConversionSaga, bool, error) {
	return r.one(ctx, `WHERE saga_id = $1::uuid FOR UPDATE`, sagaID)
}

func (r *SagaRepository) one(ctx context.Context, where string, args ...any) (domain.ConversionSaga, bool, error) {
	var s domain.ConversionSaga
	err := inTx(ctx, r.app, func(q querier) error {
		var err error
		s, err = scanSaga(q.QueryRow(ctx, `SELECT `+sagaColumns+` FROM conversion_sagas `+where, args...))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

// Update implements port.SagaRepo: writes every mutable column.
func (r *SagaRepository) Update(ctx context.Context, s domain.ConversionSaga) error {
	if !domain.CanSagaTransitionOrStay(s) {
		return domain.NewError(domain.ErrInternal, "saga state not allowed")
	}
	return inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `
			UPDATE conversion_sagas SET state = $2, attempts = $3, next_attempt_at = $4, last_error_code = $5, last_error_at = $6,
			  rp_accepted_at = $7, tenant_converted_event = $8::uuid, confirmed_at = $9, alerted_at = $10, resolution = $11,
			  resolution_reason = $12, resolved_by = $13::uuid, reversal_requested_at = $14, reversal_requested_by = $15::uuid,
			  reversal_reason = $16, reversal_refund = $17, reversal_dispute_id = $18::uuid, reversal_rp_done_at = $19
			WHERE saga_id = $1::uuid`,
			s.SagaID, s.State, s.Attempts, s.NextAttemptAt, s.LastErrorCode, s.LastErrorAt, s.RPAcceptedAt, s.TenantConvertedEvent,
			s.ConfirmedAt, s.AlertedAt, s.Resolution, s.ResolutionReason, s.ResolvedBy, s.ReversalRequestedAt, s.ReversalRequestedBy,
			s.ReversalReason, s.ReversalRefund, s.ReversalDisputeID, s.ReversalRPDoneAt)
		return err
	})
}

// Due implements port.SagaRepo: sagas whose RP-3 (or, reverting, RP-4) call is due, across
// tenants (system pool, no lock: each is claimed under its tenant lock).
func (r *SagaRepository) Due(ctx context.Context, now time.Time, limit int) ([]port.SagaRef, error) {
	var out []port.SagaRef
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT saga_id::text, tenant_id::text FROM conversion_sagas
			WHERE state IN ('rp_convert_pending', 'reverting') AND next_attempt_at <= $1
			ORDER BY next_attempt_at LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (port.SagaRef, error) {
			var s port.SagaRef
			return s, row.Scan(&s.SagaID, &s.TenantID)
		})
		return err
	})
	return out, err
}
