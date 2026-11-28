package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// AddonRepository is port.AddonRepo: the token pack catalog (§4.2.3) and
// quota add-on purchases (§4.2.12).
type AddonRepository struct{ pool *pgcommon.Pool }

var _ port.AddonRepo = (*AddonRepository)(nil)

// NewAddonRepository wraps the app pool.
func NewAddonRepository(appPool *pgcommon.Pool) *AddonRepository {
	return &AddonRepository{pool: appPool}
}

const packColumns = `pack_code, tokens, currency, amount_minor, provider, provider_price_id, monthly_cap_tokens, active, effective_from`

func scanPack(row pgcommon.Row) (domain.TokenPack, error) {
	var p domain.TokenPack
	err := row.Scan(&p.PackCode, &p.Tokens, &p.Currency, &p.AmountMinor, &p.Provider, &p.ProviderPriceID, &p.MonthlyCapTokens,
		&p.Active, &p.EffectiveFrom)
	return p, err
}

// ActivePacks implements port.AddonRepo: packs effective at `at` for the
// currency and provider, active only.
func (r *AddonRepository) ActivePacks(ctx context.Context, currency, provider string, at time.Time) ([]domain.TokenPack, error) {
	var out []domain.TokenPack
	err := inTx(ctx, r.pool, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+packColumns+` FROM token_packs WHERE currency = $1 AND provider = $2 AND active
			AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3) ORDER BY tokens`, currency, provider, at)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (domain.TokenPack, error) { return scanPack(row) })
		return err
	})
	return out, err
}

const addonColumns = `purchase_id::text, tenant_id::text, pack_code, pack_quantity, tokens_total, amount_minor, currency, status,
  provider_invoice_id, client_action::text, requested_by::text, idempotency_key, confirmed_at, event_id::text, trial_purchase, created_at`

func scanAddon(row pgcommon.Row) (domain.QuotaAddonPurchase, error) {
	var p domain.QuotaAddonPurchase
	var action *string
	err := row.Scan(&p.PurchaseID, &p.TenantID, &p.PackCode, &p.PackQuantity, &p.TokensTotal, &p.AmountMinor, &p.Currency, &p.Status,
		&p.ProviderInvoiceID, &action, &p.RequestedBy, &p.IdempotencyKey, &p.ConfirmedAt, &p.EventID, &p.TrialPurchase, &p.CreatedAt)
	if action != nil {
		p.ClientAction = []byte(*action)
	}
	return p, err
}

// Insert implements port.AddonRepo; uq_qap_key → port.ErrIdempotencyKeyTaken.
func (r *AddonRepository) Insert(ctx context.Context, p domain.QuotaAddonPurchase) error {
	err := inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO quota_addon_purchases (purchase_id, tenant_id, pack_code, pack_quantity, tokens_total,
			amount_minor, currency, status, requested_by, idempotency_key, trial_purchase)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::uuid, $10, $11)`,
			p.PurchaseID, p.TenantID, p.PackCode, p.PackQuantity, p.TokensTotal, p.AmountMinor, p.Currency, p.Status, p.RequestedBy,
			p.IdempotencyKey, p.TrialPurchase)
		return err
	})
	return keyTaken(err, "uq_qap_key")
}

// ByIdempotencyKey implements port.AddonRepo (locked).
func (r *AddonRepository) ByIdempotencyKey(ctx context.Context, tenantID, key string) (domain.QuotaAddonPurchase, bool, error) {
	return r.one(ctx, `WHERE tenant_id = $1::uuid AND idempotency_key = $2 FOR UPDATE`, tenantID, key)
}

// Get implements port.AddonRepo (locked).
func (r *AddonRepository) Get(ctx context.Context, purchaseID string) (domain.QuotaAddonPurchase, bool, error) {
	return r.one(ctx, `WHERE purchase_id = $1::uuid FOR UPDATE`, purchaseID)
}

// ByProviderInvoice implements port.AddonRepo.
func (r *AddonRepository) ByProviderInvoice(ctx context.Context, tenantID, providerInvoiceID string) (domain.QuotaAddonPurchase, bool, error) {
	return r.one(ctx, `WHERE tenant_id = $1::uuid AND provider_invoice_id = $2 FOR UPDATE`, tenantID, providerInvoiceID)
}

func (r *AddonRepository) one(ctx context.Context, where string, args ...any) (domain.QuotaAddonPurchase, bool, error) {
	var p domain.QuotaAddonPurchase
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		p, err = scanAddon(q.QueryRow(ctx, `SELECT `+addonColumns+` FROM quota_addon_purchases `+where, args...))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// List implements port.AddonRepo (newest first, keyset after afterID).
func (r *AddonRepository) List(ctx context.Context, tenantID, afterID string, limit int) ([]domain.QuotaAddonPurchase, error) {
	var out []domain.QuotaAddonPurchase
	err := inTx(ctx, r.pool, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+addonColumns+` FROM quota_addon_purchases WHERE tenant_id = $1::uuid
			AND ($2 = '' OR (created_at, purchase_id) < (SELECT created_at, purchase_id FROM quota_addon_purchases WHERE purchase_id = NULLIF($2, '')::uuid))
			ORDER BY created_at DESC, purchase_id DESC LIMIT $3`, tenantID, afterID, limit)
		if err != nil {
			return err
		}
		out, err = collect(rows, func(row pgcommon.Row) (domain.QuotaAddonPurchase, error) { return scanAddon(row) })
		return err
	})
	return out, err
}

// Update implements port.AddonRepo.
func (r *AddonRepository) Update(ctx context.Context, p domain.QuotaAddonPurchase) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE quota_addon_purchases SET status = $2, provider_invoice_id = $3, client_action = $4::jsonb,
			confirmed_at = $5, event_id = $6::uuid, updated_at = now() WHERE purchase_id = $1::uuid`,
			p.PurchaseID, p.Status, p.ProviderInvoiceID, jsonTextOrNil(p.ClientAction), p.ConfirmedAt, p.EventID)
		return err
	})
}

// ConfirmedTokensSince implements port.AddonRepo: tokens confirmed for the
// tenant and pack since `since` (the 1st 00:00 UTC, monthly cap).
func (r *AddonRepository) ConfirmedTokensSince(ctx context.Context, tenantID, packCode string, since time.Time) (int64, error) {
	var n int64
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `SELECT coalesce(sum(tokens_total), 0)::bigint FROM quota_addon_purchases
			WHERE tenant_id = $1::uuid AND pack_code = $2 AND status = 'confirmed' AND confirmed_at >= $3`, tenantID, packCode, since).Scan(&n)
	})
	return n, err
}

// TrialUsage implements port.AddonRepo: the tenant's trial purchases that
// count toward the trial cap (BIL-ADD-5): trial_purchase rows not failed,
// expired or cancelled.
func (r *AddonRepository) TrialUsage(ctx context.Context, tenantID string) (domain.TrialAddonUsage, error) {
	var u domain.TrialAddonUsage
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `SELECT coalesce(sum(pack_quantity), 0)::int, coalesce(sum(amount_minor), 0)::bigint FROM quota_addon_purchases
			WHERE tenant_id = $1::uuid AND trial_purchase AND status NOT IN ('failed', 'expired', 'cancelled')`, tenantID).Scan(&u.Packs, &u.AmountMinor)
	})
	return u, err
}

const trialCapColumns = `tenant_id::text, max_packs, max_amount_minor, reason, set_by::text, set_at, record_version`

func scanTrialCap(row pgcommon.Row) (domain.TrialAddonCap, error) {
	var c domain.TrialAddonCap
	err := row.Scan(&c.TenantID, &c.MaxPacks, &c.MaxAmountMinor, &c.Reason, &c.SetBy, &c.SetAt, &c.RecordVersion)
	return c, err
}

// TrialCap implements port.TrialCapRepo (locked).
func (r *AddonRepository) TrialCap(ctx context.Context, tenantID string) (domain.TrialAddonCap, bool, error) {
	var c domain.TrialAddonCap
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		c, err = scanTrialCap(q.QueryRow(ctx, `SELECT `+trialCapColumns+` FROM trial_addon_caps WHERE tenant_id = $1::uuid FOR UPDATE`, tenantID))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// UpsertTrialCap implements port.TrialCapRepo with optimistic locking on
// record_version: expectedVersion 0 inserts, any other value updates that
// version; a row in another state is port.ErrTrialCapChanged.
func (r *AddonRepository) UpsertTrialCap(ctx context.Context, c domain.TrialAddonCap, expectedVersion int64) (domain.TrialAddonCap, error) {
	var out domain.TrialAddonCap
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		if expectedVersion == 0 {
			out, err = scanTrialCap(q.QueryRow(ctx, `INSERT INTO trial_addon_caps (tenant_id, max_packs, max_amount_minor, reason, set_by, set_at)
				VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6) ON CONFLICT (tenant_id) DO NOTHING RETURNING `+trialCapColumns,
				c.TenantID, c.MaxPacks, c.MaxAmountMinor, c.Reason, c.SetBy, c.SetAt))
		} else {
			out, err = scanTrialCap(q.QueryRow(ctx, `UPDATE trial_addon_caps SET max_packs = $2, max_amount_minor = $3, reason = $4,
				set_by = $5::uuid, set_at = $6, record_version = record_version + 1 WHERE tenant_id = $1::uuid AND record_version = $7
				RETURNING `+trialCapColumns, c.TenantID, c.MaxPacks, c.MaxAmountMinor, c.Reason, c.SetBy, c.SetAt, expectedVersion))
		}
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return out, port.ErrTrialCapChanged
	}
	return out, err
}

// DeleteTrialCap implements port.TrialCapRepo.
func (r *AddonRepository) DeleteTrialCap(ctx context.Context, tenantID string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM trial_addon_caps WHERE tenant_id = $1::uuid`, tenantID)
		return err
	})
}

// keyTaken maps a unique violation on a (tenant_id, idempotency_key)
// constraint to port.ErrIdempotencyKeyTaken (§9.2: one row per key).
func keyTaken(err error, constraint string) error {
	if pgcommon.IsUniqueViolation(err) && pgcommon.ConstraintName(err) == constraint {
		return port.ErrIdempotencyKeyTaken
	}
	return err
}
