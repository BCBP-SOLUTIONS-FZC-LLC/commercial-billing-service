package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// AdminRepository is port.AdminRepo: the global price book, the provider
// webhook ledger and provider certifications, written as billing_system
// (billing_app has SELECT only on these tables).
type AdminRepository struct{ sys *pgcommon.Pool }

var _ port.AdminRepo = (*AdminRepository)(nil)

// NewAdminRepository wraps the system pool.
func NewAdminRepository(sysPool *pgcommon.Pool) *AdminRepository {
	return &AdminRepository{sys: sysPool}
}

// overlap maps the price-book exclusion constraints (excl_*_overlap, SQLSTATE
// 23P01, which pgcommon has no Is* helper for: matched by constraint name)
// and the unique keys to PRICE_OVERLAP.
func overlap(err error) error {
	if pgcommon.IsUniqueViolation(err) || strings.HasPrefix(pgcommon.ConstraintName(err), "excl_") {
		return domain.NewError(domain.ErrPriceOverlap, "the new row overlaps an existing one for this key")
	}
	return err
}

// AddPlanPrice implements port.AdminRepo: the current row for the key ends
// where the new one starts (B-O11).
func (r *AdminRepository) AddPlanPrice(ctx context.Context, p domain.PlanPrice) (domain.PlanPrice, error) {
	err := inTx(ctx, r.sys, func(q querier) error {
		if _, err := q.Exec(ctx, `UPDATE plan_prices SET effective_to = $5 WHERE plan = $1 AND interval = $2 AND currency = $3 AND provider = $4
			AND effective_to IS NULL AND effective_from < $5`, p.Plan, p.Interval, p.Currency, p.Provider, p.EffectiveFrom); err != nil {
			return err
		}
		return q.QueryRow(ctx, `INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from,
			created_by, reason) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid, $9) RETURNING price_id::text`,
			p.Plan, p.Interval, p.Currency, p.UnitAmountMinor, p.Provider, p.ProviderPriceID, p.EffectiveFrom, p.CreatedBy, p.Reason).Scan(&p.PriceID)
	})
	return p, overlap(err)
}

// AddTokenPack implements port.AdminRepo (B-O12): closes the current row
// for (pack_code, currency) and inserts the new one.
func (r *AdminRepository) AddTokenPack(ctx context.Context, p domain.TokenPack, by, reason string) error {
	return overlap(inTx(ctx, r.sys, func(q querier) error {
		if _, err := q.Exec(ctx, `UPDATE token_packs SET effective_to = $3 WHERE pack_code = $1 AND currency = $2
			AND effective_to IS NULL AND effective_from < $3`, p.PackCode, p.Currency, p.EffectiveFrom); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `INSERT INTO token_packs (pack_code, tokens, currency, amount_minor, provider, provider_price_id,
			monthly_cap_tokens, active, effective_from, created_by, reason) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid, $11)`,
			p.PackCode, p.Tokens, p.Currency, p.AmountMinor, p.Provider, p.ProviderPriceID, p.MonthlyCapTokens, p.Active,
			p.EffectiveFrom, by, reason)
		return err
	}))
}

// ReplayLedger implements port.AdminRepo (B-O13): the row goes back to
// received with attempts reset; found=false for an unknown event.
func (r *AdminRepository) ReplayLedger(ctx context.Context, provider, eventID, note string) (bool, error) {
	var ok bool
	err := inTx(ctx, r.sys, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE payment_event_ledger SET status = 'received', attempts = 0, next_attempt_at = now(),
			claimed_until = NULL, outcome = $3 WHERE provider = $1 AND provider_event_id = $2`, provider, eventID, note)
		ok = tag.RowsAffected() == 1
		return err
	})
	return ok, err
}

const certColumns = `id::text, provider, adapter_version, provider_api_version, suite_version, status, proration_mode, certified_by::text,
  certified_at, reviewed_by::text, reviewed_at`

func scanCert(row pgcommon.Row) (domain.Certification, error) {
	var c domain.Certification
	err := row.Scan(&c.ID, &c.Provider, &c.AdapterVersion, &c.ProviderAPIVersion, &c.SuiteVersion, &c.Status, &c.ProrationMode,
		&c.CertifiedBy, &c.CertifiedAt, &c.ReviewedBy, &c.ReviewedAt)
	return c, err
}

// InsertCertification implements port.AdminRepo; uq_pc_run → 409
// CERTIFICATION_EXISTS.
func (r *AdminRepository) InsertCertification(ctx context.Context, c port.CertificationRun) (domain.Certification, error) {
	var out domain.Certification
	err := inTx(ctx, r.sys, func(q querier) error {
		var err error
		out, err = scanCert(q.QueryRow(ctx, `INSERT INTO provider_certifications (provider, adapter_version, provider_api_version,
			suite_version, status, proration_mode, capabilities, results, provider_account_ref, run_started_at, run_finished_at, certified_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11, $12::uuid) RETURNING `+certColumns,
			c.Provider, c.AdapterVersion, c.ProviderAPIVersion, c.SuiteVersion, c.Status, c.ProrationMode, string(c.Capabilities),
			string(c.Results), c.ProviderAccountRef, c.RunStartedAt, c.RunFinishedAt, c.CertifiedBy))
		return err
	})
	if pgcommon.IsUniqueViolation(err) {
		return out, domain.NewError(domain.ErrCertificationExists, "this run is already pending, passed or exempt; decide or revoke it first")
	}
	return out, err
}

// Certification implements port.AdminRepo.
func (r *AdminRepository) Certification(ctx context.Context, id string) (domain.Certification, bool, error) {
	var c domain.Certification
	err := inTx(ctx, r.sys, func(q querier) error {
		var err error
		c, err = scanCert(q.QueryRow(ctx, `SELECT `+certColumns+` FROM provider_certifications WHERE id = $1::uuid`, id))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// ReviewCertification implements port.AdminRepo (pending_review → passed
// | rejected; trg_pc_guard and chk_pc_two_people also enforce it).
func (r *AdminRepository) ReviewCertification(ctx context.Context, id string, status domain.CertificationStatus, by, reason string, at time.Time) (domain.Certification, error) {
	var out domain.Certification
	err := inTx(ctx, r.sys, func(q querier) error {
		var err error
		out, err = scanCert(q.QueryRow(ctx, `UPDATE provider_certifications SET status = $2, reviewed_by = $3::uuid, reviewed_at = $4,
			review_reason = $5 WHERE id = $1::uuid AND status = 'pending_review' RETURNING `+certColumns, id, status, by, at, reason))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return out, domain.NewError(domain.ErrCertificationNotPending, "the record is not pending review")
	}
	return out, err
}

// RevokeCertification implements port.AdminRepo (passed → revoked).
func (r *AdminRepository) RevokeCertification(ctx context.Context, id, by, reason string, at time.Time) (domain.Certification, error) {
	var out domain.Certification
	err := inTx(ctx, r.sys, func(q querier) error {
		var err error
		out, err = scanCert(q.QueryRow(ctx, `UPDATE provider_certifications SET status = 'revoked', revoked_at = $3, revoked_by = $2::uuid,
			revoked_reason = $4 WHERE id = $1::uuid AND status = 'passed' RETURNING `+certColumns, id, by, at, reason))
		return err
	})
	if errors.Is(err, pgcommon.ErrNoRows) {
		return out, domain.NewError(domain.ErrCertificationNotActive, "only a passed certification can be revoked")
	}
	return out, err
}
