package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

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

func scanPack(row pgx.Row) (domain.TokenPack, error) {
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
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.TokenPack, error) { return scanPack(row) })
		return err
	})
	return out, err
}

const addonColumns = `purchase_id::text, tenant_id::text, pack_code, pack_quantity, tokens_total, amount_minor, currency, status,
  provider_invoice_id, client_action::text, requested_by::text, idempotency_key, confirmed_at, event_id::text, created_at`

func scanAddon(row pgx.Row) (domain.QuotaAddonPurchase, error) {
	var p domain.QuotaAddonPurchase
	var action *string
	err := row.Scan(&p.PurchaseID, &p.TenantID, &p.PackCode, &p.PackQuantity, &p.TokensTotal, &p.AmountMinor, &p.Currency, &p.Status,
		&p.ProviderInvoiceID, &action, &p.RequestedBy, &p.IdempotencyKey, &p.ConfirmedAt, &p.EventID, &p.CreatedAt)
	if action != nil {
		p.ClientAction = []byte(*action)
	}
	return p, err
}

// Insert implements port.AddonRepo.
func (r *AddonRepository) Insert(ctx context.Context, p domain.QuotaAddonPurchase) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO quota_addon_purchases (purchase_id, tenant_id, pack_code, pack_quantity, tokens_total,
			amount_minor, currency, status, requested_by, idempotency_key) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::uuid, $10)`,
			p.PurchaseID, p.TenantID, p.PackCode, p.PackQuantity, p.TokensTotal, p.AmountMinor, p.Currency, p.Status, p.RequestedBy, p.IdempotencyKey)
		return err
	})
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
	if errors.Is(err, pgx.ErrNoRows) {
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
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.QuotaAddonPurchase, error) { return scanAddon(row) })
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
