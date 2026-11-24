package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// AccountRepository implements port.AccountRepo on the app pool (RLS).
type AccountRepository struct{ pool *pgcommon.Pool }

var _ port.AccountRepo = (*AccountRepository)(nil)

// NewAccountRepository returns the repository.
func NewAccountRepository(appPool *pgcommon.Pool) *AccountRepository {
	return &AccountRepository{pool: appPool}
}

const accountColumns = `tenant_id, provider, provider_customer_id, currency, billing_contact_name, billing_contact_email,
  company_name, country, tax_id, default_pm_set, iam_lifecycle, iam_suspension_source, iam_last_event_at,
  closed_at, pii_minimised_at, record_version, created_at, updated_at`

func scanAccount(row pgcommon.Row) (domain.BillingAccount, error) {
	var a domain.BillingAccount
	err := row.Scan(&a.TenantID, &a.Provider, &a.ProviderCustomerID, &a.Currency, &a.BillingContactName,
		&a.BillingContactEmail, &a.CompanyName, &a.Country, &a.TaxID, &a.DefaultPMSet, &a.IAMLifecycle,
		&a.IAMSuspensionSource, &a.IAMLastEventAt, &a.ClosedAt, &a.PIIMinimisedAt, &a.RecordVersion,
		&a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// Create inserts an account.
func (r *AccountRepository) Create(ctx context.Context, a domain.BillingAccount) error {
	if a.IAMLifecycle == "" {
		a.IAMLifecycle = domain.LifecycleTrial
	}
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO billing_accounts (tenant_id, provider, provider_customer_id, currency, billing_contact_name,
			  billing_contact_email, company_name, country, tax_id, default_pm_set, iam_lifecycle)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			a.TenantID, a.Provider, a.ProviderCustomerID, a.Currency, a.BillingContactName, a.BillingContactEmail,
			a.CompanyName, a.Country, a.TaxID, a.DefaultPMSet, a.IAMLifecycle)
		return err
	})
}

// Get returns the account; ACCOUNT not found maps to SUBSCRIPTION_NOT_FOUND
// at the API (a tenant without an account has no subscription).
func (r *AccountRepository) Get(ctx context.Context, tenantID string) (domain.BillingAccount, error) {
	var a domain.BillingAccount
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		a, err = scanAccount(q.QueryRow(ctx, `SELECT `+accountColumns+` FROM billing_accounts WHERE tenant_id = $1::uuid`, tenantID))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return a, subscriptionNotFound("billing account for tenant", tenantID)
	}
	return a, err
}

// UpdateContact sets the contact fields when record_version matches;
// otherwise 409 OPTIMISTIC_LOCK_CONFLICT with the current version (§5.1).
func (r *AccountRepository) UpdateContact(ctx context.Context, tenantID string, expectedVersion int64,
	name, email, company, country, taxID *string) (domain.BillingAccount, error) {
	var a domain.BillingAccount
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		a, err = scanAccount(q.QueryRow(ctx, `
			UPDATE billing_accounts SET billing_contact_name = $3, billing_contact_email = $4, company_name = $5,
			  country = $6, tax_id = $7
			WHERE tenant_id = $1::uuid AND record_version = $2
			RETURNING `+accountColumns, tenantID, expectedVersion, name, email, company, country, taxID))
		if !errors.Is(err, pgcommon.ErrNoRows) {
			return err
		}
		var current int64
		if err := q.QueryRow(ctx, `SELECT record_version FROM billing_accounts WHERE tenant_id = $1::uuid`, tenantID).Scan(&current); err != nil {
			if errors.Is(err, pgcommon.ErrNoRows) {
				return subscriptionNotFound("billing account for tenant", tenantID)
			}
			return err
		}
		return domain.NewError(domain.ErrOptimisticLockConflict, "billing account changed since it was read").With("record_version", current)
	})
	return a, err
}

// MinimiseContact implements port.AccountRepo (§15.3, BIL-OPS-9).
func (r *AccountRepository) MinimiseContact(ctx context.Context, tenantID string, at time.Time) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE billing_accounts SET billing_contact_name = NULL, billing_contact_email = NULL,
			pii_minimised_at = $2, record_version = record_version + 1, updated_at = now()
			WHERE tenant_id = $1::uuid AND pii_minimised_at IS NULL`, tenantID, at)
		return err
	})
}

// ReturnToTrial implements port.AccountRepo.
func (r *AccountRepository) ReturnToTrial(ctx context.Context, tenantID string, at time.Time) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE billing_accounts SET iam_lifecycle = 'trial', iam_suspension_source = NULL,
			iam_last_event_at = GREATEST(COALESCE(iam_last_event_at, $2), $2)
			WHERE tenant_id = $1::uuid AND iam_lifecycle <> 'offboarded'`, tenantID, at)
		return err
	})
}

// SetDefaultPM records whether the provider reports a default payment
// method (no payment-method data is stored, BIL-PCI-2).
func (r *AccountRepository) SetDefaultPM(ctx context.Context, tenantID string, set bool) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE billing_accounts SET default_pm_set = $2 WHERE tenant_id = $1::uuid`, tenantID, set)
		return err
	})
}
