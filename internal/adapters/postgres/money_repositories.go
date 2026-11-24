package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// MoneyRepository is port.MoneyRepo: adjustments (§4.2.16), refunds
// (§4.2.17) and disputes (§4.2.18).
type MoneyRepository struct{ pool *pgcommon.Pool }

var _ port.MoneyRepo = (*MoneyRepository)(nil)

// NewMoneyRepository wraps the app pool.
func NewMoneyRepository(appPool *pgcommon.Pool) *MoneyRepository {
	return &MoneyRepository{pool: appPool}
}

// InsertAdjustment implements port.MoneyRepo.
func (r *MoneyRepository) InsertAdjustment(ctx context.Context, a domain.Adjustment) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO adjustments (adjustment_id, tenant_id, kind, amount_minor, currency, description, reason,
			actor_id, idempotency_key) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::uuid, $9)`,
			a.AdjustmentID, a.TenantID, a.Kind, a.AmountMinor, a.Currency, a.Description, a.Reason, a.ActorID, a.IdempotencyKey)
		return err
	})
}

// SetAdjustmentRef implements port.MoneyRepo.
func (r *MoneyRepository) SetAdjustmentRef(ctx context.Context, id, ref string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE adjustments SET provider_ref = $2 WHERE adjustment_id = $1::uuid`, id, ref)
		return err
	})
}

const refundColumns = `refund_id::text, tenant_id::text, invoice_id::text, amount_minor, currency, category, reason, status,
  provider_refund_id, addon_purchase_id::text, addon_tokens, addon_event_id::text, actor_id::text, idempotency_key, created_at`

func scanRefund(row pgx.Row) (domain.Refund, error) {
	var f domain.Refund
	err := row.Scan(&f.RefundID, &f.TenantID, &f.InvoiceID, &f.AmountMinor, &f.Currency, &f.Category, &f.Reason, &f.Status,
		&f.ProviderRefundID, &f.AddonPurchaseID, &f.AddonTokens, &f.AddonEventID, &f.ActorID, &f.IdempotencyKey, &f.CreatedAt)
	return f, err
}

// InsertRefund implements port.MoneyRepo.
func (r *MoneyRepository) InsertRefund(ctx context.Context, f domain.Refund) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO refunds (refund_id, tenant_id, invoice_id, amount_minor, currency, category, reason, status,
			addon_purchase_id, addon_tokens, actor_id, idempotency_key)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9::uuid, $10, $11::uuid, $12)`,
			f.RefundID, f.TenantID, f.InvoiceID, f.AmountMinor, f.Currency, f.Category, f.Reason, f.Status, f.AddonPurchaseID,
			f.AddonTokens, f.ActorID, f.IdempotencyKey)
		return err
	})
}

// RefundByProvider implements port.MoneyRepo (locked).
func (r *MoneyRepository) RefundByProvider(ctx context.Context, providerRefundID string) (domain.Refund, bool, error) {
	var f domain.Refund
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		f, err = scanRefund(q.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds WHERE provider_refund_id = $1 FOR UPDATE`, providerRefundID))
		return err
	})
	if isNoRows(err) {
		return f, false, nil
	}
	return f, err == nil, err
}

// RefundsForInvoice implements port.MoneyRepo.
func (r *MoneyRepository) RefundsForInvoice(ctx context.Context, invoiceID string) ([]domain.Refund, error) {
	var out []domain.Refund
	err := inTx(ctx, r.pool, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+refundColumns+` FROM refunds WHERE invoice_id = $1::uuid ORDER BY created_at`, invoiceID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Refund, error) { return scanRefund(row) })
		return err
	})
	return out, err
}

// UpdateRefund implements port.MoneyRepo.
func (r *MoneyRepository) UpdateRefund(ctx context.Context, f domain.Refund) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE refunds SET status = $2, provider_refund_id = $3, addon_event_id = $4::uuid, updated_at = now()
			WHERE refund_id = $1::uuid`, f.RefundID, f.Status, f.ProviderRefundID, f.AddonEventID)
		return err
	})
}

const disputeColumns = `dispute_id::text, tenant_id::text, provider, provider_dispute_id, kind, payment_ref, invoice_id::text,
  is_conversion_payment, within_revert_window, reason, provider_status, revert_decision, decided_by::text, opened_at`

func scanDispute(row pgx.Row) (domain.Dispute, error) {
	var d domain.Dispute
	err := row.Scan(&d.DisputeID, &d.TenantID, &d.Provider, &d.ProviderDisputeID, &d.Kind, &d.PaymentRef, &d.InvoiceID,
		&d.IsConversionPayment, &d.WithinRevertWindow, &d.Reason, &d.ProviderStatus, &d.RevertDecision, &d.DecidedBy, &d.OpenedAt)
	return d, err
}

// UpsertDispute implements port.MoneyRepo: insert, or update the provider
// status of a known dispute. Returns the stored row.
func (r *MoneyRepository) UpsertDispute(ctx context.Context, d domain.Dispute) (domain.Dispute, error) {
	var out domain.Dispute
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		out, err = scanDispute(q.QueryRow(ctx, `
			INSERT INTO disputes (dispute_id, tenant_id, provider, provider_dispute_id, kind, payment_ref, invoice_id, is_conversion_payment,
			  within_revert_window, reason, provider_status, revert_decision, opened_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (provider, provider_dispute_id) DO UPDATE SET provider_status = EXCLUDED.provider_status, updated_at = now()
			RETURNING `+disputeColumns,
			d.DisputeID, d.TenantID, d.Provider, d.ProviderDisputeID, d.Kind, d.PaymentRef, d.InvoiceID, d.IsConversionPayment,
			d.WithinRevertWindow, d.Reason, d.ProviderStatus, d.RevertDecision, d.OpenedAt))
		return err
	})
	return out, err
}

// Dispute implements port.MoneyRepo (locked).
func (r *MoneyRepository) Dispute(ctx context.Context, disputeID string) (domain.Dispute, bool, error) {
	var d domain.Dispute
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		d, err = scanDispute(q.QueryRow(ctx, `SELECT `+disputeColumns+` FROM disputes WHERE dispute_id = $1::uuid FOR UPDATE`, disputeID))
		return err
	})
	if isNoRows(err) {
		return d, false, nil
	}
	return d, err == nil, err
}

// DecideDispute implements port.MoneyRepo.
func (r *MoneyRepository) DecideDispute(ctx context.Context, disputeID, decision, by string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE disputes SET revert_decision = $2, decided_by = $3::uuid, updated_at = now() WHERE dispute_id = $1::uuid`,
			disputeID, decision, by)
		return err
	})
}
