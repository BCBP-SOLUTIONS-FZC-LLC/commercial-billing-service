package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// InvoiceRepository is port.InvoiceRepo: the provider invoice mirror
// (§4.2.13) and its links (§4.2.14).
type InvoiceRepository struct{ pool *pgcommon.Pool }

var _ port.InvoiceRepo = (*InvoiceRepository)(nil)

// NewInvoiceRepository wraps the app pool.
func NewInvoiceRepository(appPool *pgcommon.Pool) *InvoiceRepository {
	return &InvoiceRepository{pool: appPool}
}

// Mirror implements port.InvoiceRepo: insert, or update only when the
// provider's version is newer (BIL-PAY-4 ordering guard). Returns the
// Billing invoice id either way.
func (r *InvoiceRepository) Mirror(ctx context.Context, tenantID, provider string, v port.InvoiceView, providerUpdatedAt time.Time) (string, error) {
	var id string
	err := inTx(ctx, r.pool, func(q querier) error {
		var number *string
		if v.Number != "" {
			number = &v.Number
		}
		var hosted *string
		if v.HostedURL != "" {
			hosted = &v.HostedURL
		}
		return q.QueryRow(ctx, `
			INSERT INTO invoices (invoice_id, tenant_id, provider, provider_invoice_id, number, kind, status, currency,
			  amount_due_minor, amount_paid_minor, period_start, period_end, hosted_invoice_url, issued_at, paid_at, provider_updated_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (provider, provider_invoice_id) DO UPDATE SET
			  number = EXCLUDED.number, status = EXCLUDED.status, amount_due_minor = EXCLUDED.amount_due_minor,
			  amount_paid_minor = EXCLUDED.amount_paid_minor, period_start = EXCLUDED.period_start, period_end = EXCLUDED.period_end,
			  hosted_invoice_url = EXCLUDED.hosted_invoice_url, issued_at = EXCLUDED.issued_at, paid_at = EXCLUDED.paid_at,
			  provider_updated_at = EXCLUDED.provider_updated_at, updated_at = now()
			  WHERE invoices.provider_updated_at <= EXCLUDED.provider_updated_at
			RETURNING invoice_id::text`,
			uuid.Must(uuid.NewV7()).String(), tenantID, provider, v.Ref.ProviderInvoiceID, number, string(v.Kind), v.Status,
			v.AmountDue.Currency, v.AmountDue.AmountMinor, v.AmountPaid.AmountMinor, v.PeriodStart, v.PeriodEnd, hosted,
			v.IssuedAt, v.PaidAt, providerUpdatedAt).Scan(&id)
	})
	if err == nil || !isNoRows(err) {
		return id, err
	}
	// Older version: unchanged; return the stored id.
	err = inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `SELECT invoice_id::text FROM invoices WHERE provider = $1 AND provider_invoice_id = $2`,
			provider, v.Ref.ProviderInvoiceID).Scan(&id)
	})
	return id, err
}

// Link implements port.InvoiceRepo.
func (r *InvoiceRepository) Link(ctx context.Context, invoiceID, tenantID, refType, refID string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `INSERT INTO invoice_links (invoice_id, tenant_id, ref_type, ref_id) VALUES ($1::uuid, $2::uuid, $3, $4::uuid)
			ON CONFLICT DO NOTHING`, invoiceID, tenantID, refType, refID)
		return err
	})
}

// OldestOpen implements port.InvoiceRepo: the provider id of the oldest open
// invoice for the subscription's provider subscriptions (B-P14), or "".
func (r *InvoiceRepository) OldestOpen(ctx context.Context, tenantID, _ string) (string, error) {
	var id string
	err := inTx(ctx, r.pool, func(q querier) error {
		err := q.QueryRow(ctx, `SELECT provider_invoice_id FROM invoices WHERE tenant_id = $1::uuid AND status = 'open'
			AND kind IN ('subscription_cycle','subscription_create') ORDER BY issued_at NULLS LAST, provider_updated_at LIMIT 1`, tenantID).Scan(&id)
		if isNoRows(err) {
			return nil
		}
		return err
	})
	return id, err
}
