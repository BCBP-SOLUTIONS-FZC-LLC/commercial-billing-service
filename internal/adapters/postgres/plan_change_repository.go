package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// PlanChangeRepository is port.PlanChangeRepo (§4.2.9).
type PlanChangeRepository struct{ app, sys *pgcommon.Pool }

var _ port.PlanChangeRepo = (*PlanChangeRepository)(nil)

// NewPlanChangeRepository wraps both pools (the downgrade re-check scans
// across tenants on the system pool).
func NewPlanChangeRepository(appPool, sysPool *pgcommon.Pool) *PlanChangeRepository {
	return &PlanChangeRepository{app: appPool, sys: sysPool}
}

const changeColumns = `change_id::text, tenant_id::text, billing_subscription_id::text, from_plan, to_plan, direction, status,
  effective_at, amount_minor, currency, i18_checked_at, blocker_codes, provider_invoice_id, provider_schedule_id,
  client_action::text, requested_by::text, idempotency_key, created_at, updated_at`

func scanChange(row pgcommon.Row) (domain.PlanChangeRequest, error) {
	var c domain.PlanChangeRequest
	var action *string
	err := row.Scan(&c.ChangeID, &c.TenantID, &c.BillingSubscriptionID, &c.FromPlan, &c.ToPlan, &c.Direction, &c.Status,
		&c.EffectiveAt, &c.AmountMinor, &c.Currency, &c.I18CheckedAt, &c.BlockerCodes, &c.ProviderInvoiceID, &c.ProviderScheduleID,
		&action, &c.RequestedBy, &c.IdempotencyKey, &c.CreatedAt, &c.UpdatedAt)
	if action != nil {
		c.ClientAction = []byte(*action)
	}
	return c, err
}

// Insert implements port.PlanChangeRepo; uq_pcr_one_open → 409
// PLAN_CHANGE_PENDING (§9.5).
func (r *PlanChangeRepository) Insert(ctx context.Context, c domain.PlanChangeRequest) error {
	err := inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO plan_change_requests (change_id, tenant_id, billing_subscription_id, from_plan, to_plan, direction, status,
			  effective_at, amount_minor, currency, i18_checked_at, blocker_codes, requested_by, idempotency_key)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::uuid, $14)`,
			c.ChangeID, c.TenantID, c.BillingSubscriptionID, c.FromPlan, c.ToPlan, c.Direction, c.Status, c.EffectiveAt,
			c.AmountMinor, c.Currency, c.I18CheckedAt, c.BlockerCodes, c.RequestedBy, c.IdempotencyKey)
		return err
	})
	if sqlState(err) == "23505" {
		return domain.NewError(domain.ErrPlanChangePending, "a plan change is already pending")
	}
	return err
}

// Open implements port.PlanChangeRepo: the tenant's pending_payment or
// scheduled request, locked.
func (r *PlanChangeRepository) Open(ctx context.Context, tenantID string) (domain.PlanChangeRequest, bool, error) {
	return r.one(ctx, `WHERE tenant_id = $1::uuid AND status IN ('pending_payment','scheduled') FOR UPDATE`, tenantID)
}

// ByProviderInvoice implements port.PlanChangeRepo.
func (r *PlanChangeRepository) ByProviderInvoice(ctx context.Context, tenantID, providerInvoiceID string) (domain.PlanChangeRequest, bool, error) {
	return r.one(ctx, `WHERE tenant_id = $1::uuid AND provider_invoice_id = $2 FOR UPDATE`, tenantID, providerInvoiceID)
}

func (r *PlanChangeRepository) one(ctx context.Context, where string, args ...any) (domain.PlanChangeRequest, bool, error) {
	var c domain.PlanChangeRequest
	err := inTx(ctx, r.app, func(q querier) error {
		var err error
		c, err = scanChange(q.QueryRow(ctx, `SELECT `+changeColumns+` FROM plan_change_requests `+where, args...))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// Update implements port.PlanChangeRepo: writes the mutable columns.
func (r *PlanChangeRepository) Update(ctx context.Context, c domain.PlanChangeRequest) error {
	return inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `
			UPDATE plan_change_requests SET status = $2, effective_at = $3, amount_minor = $4, currency = $5, i18_checked_at = $6,
			  blocker_codes = $7, provider_invoice_id = $8, provider_schedule_id = $9, client_action = $10::jsonb, updated_at = now()
			WHERE change_id = $1::uuid`,
			c.ChangeID, c.Status, c.EffectiveAt, c.AmountMinor, c.Currency, c.I18CheckedAt, c.BlockerCodes, c.ProviderInvoiceID,
			c.ProviderScheduleID, jsonTextOrNil(c.ClientAction))
		return err
	})
}

// DueDowngrades implements port.PlanChangeRepo: scheduled downgrades
// taking effect before until (system pool, across tenants).
func (r *PlanChangeRepository) DueDowngrades(ctx context.Context, until time.Time, limit int) ([]domain.PlanChangeRequest, error) {
	var out []domain.PlanChangeRequest
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+changeColumns+` FROM plan_change_requests
			WHERE status = 'scheduled' AND direction = 'downgrade' AND effective_at <= $1 ORDER BY effective_at LIMIT $2`, until, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (domain.PlanChangeRequest, error) { return scanChange(row) })
		return err
	})
	return out, err
}

func jsonTextOrNil(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	return &s
}

// PreviewRepository is port.PreviewRepo: previews live in
// idempotency_records with scope 'preview' and key = preview_id, so any
// replica can honour them (§4.2.19).
type PreviewRepository struct{ pool *pgcommon.Pool }

var _ port.PreviewRepo = (*PreviewRepository)(nil)

// NewPreviewRepository wraps the app pool.
func NewPreviewRepository(appPool *pgcommon.Pool) *PreviewRepository {
	return &PreviewRepository{pool: appPool}
}

// Save implements port.PreviewRepo.
func (r *PreviewRepository) Save(ctx context.Context, tenantID, previewID string, body []byte, expiresAt time.Time) error {
	h := sha256.Sum256(body)
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO idempotency_records (tenant_id, scope, key, request_hash, state, status_code, response_body, expires_at)
			VALUES ($1::uuid, 'preview', $2, $3, 'completed', 200, $4::jsonb, $5)`, tenantID, previewID, h[:], jsonText(body), expiresAt)
		return err
	})
}

// Load implements port.PreviewRepo.
func (r *PreviewRepository) Load(ctx context.Context, tenantID, previewID string) ([]byte, time.Time, bool, error) {
	var body string
	var exp time.Time
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `SELECT response_body::text, expires_at FROM idempotency_records
			WHERE tenant_id = $1::uuid AND scope = 'preview' AND key = $2`, tenantID, previewID).Scan(&body, &exp)
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return nil, exp, false, nil
	}
	return []byte(body), exp, err == nil, err
}
