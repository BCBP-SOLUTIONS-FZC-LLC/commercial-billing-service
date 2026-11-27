package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// PriceRepository reads the global price book (no RLS; billing_app SELECT).
type PriceRepository struct{ pool *pgcommon.Pool }

var _ port.PriceRepo = (*PriceRepository)(nil)

// NewPriceRepository returns the repository.
func NewPriceRepository(pool *pgcommon.Pool) *PriceRepository { return &PriceRepository{pool: pool} }

const priceColumns = `price_id, plan, interval, currency, unit_amount_minor, provider, provider_price_id,
  effective_from, effective_to, created_by, reason`

func scanPrice(row pgx.Row) (domain.PlanPrice, error) {
	var p domain.PlanPrice
	err := row.Scan(&p.PriceID, &p.Plan, &p.Interval, &p.Currency, &p.UnitAmountMinor, &p.Provider, &p.ProviderPriceID,
		&p.EffectiveFrom, &p.EffectiveTo, &p.CreatedBy, &p.Reason)
	return p, err
}

// PriceAt returns the row effective at instant (effective_from ≤ at < effective_to).
// excl_pp_overlap guarantees at most one.
func (r *PriceRepository) PriceAt(ctx context.Context, plan domain.PlanCode, interval domain.BillingInterval,
	currency, provider string, at time.Time) (domain.PlanPrice, bool, error) {
	var p domain.PlanPrice
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		p, err = scanPrice(q.QueryRow(ctx, `SELECT `+priceColumns+` FROM plan_prices
			WHERE plan = $1 AND interval = $2 AND currency = $3 AND provider = $4
			  AND effective_from <= $5 AND (effective_to IS NULL OR effective_to > $5)`,
			plan, interval, currency, provider, at))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// Get returns one row.
func (r *PriceRepository) Get(ctx context.Context, priceID string) (domain.PlanPrice, error) {
	var p domain.PlanPrice
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		p, err = scanPrice(q.QueryRow(ctx, `SELECT `+priceColumns+` FROM plan_prices WHERE price_id = $1::uuid`, priceID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, domain.NewError(domain.ErrPriceNotAvailable, "price "+priceID+" not found")
	}
	return p, err
}

// CertificationRepository reads provider_certifications for the registry.
type CertificationRepository struct{ pool *pgcommon.Pool }

var _ port.CertificationRepo = (*CertificationRepository)(nil)

// NewCertificationRepository returns the repository.
func NewCertificationRepository(pool *pgcommon.Pool) *CertificationRepository {
	return &CertificationRepository{pool: pool}
}

// Enabling returns the passed or exempt record for exactly this adapter and
// suite version (BIL-PROV-1).
func (r *CertificationRepository) Enabling(ctx context.Context, provider, adapterVersion, suiteVersion string) (domain.Certification, bool, error) {
	var c domain.Certification
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT id, provider, adapter_version, provider_api_version, suite_version, status, proration_mode,
			  certified_by, certified_at, reviewed_by, reviewed_at
			FROM provider_certifications
			WHERE provider = $1 AND adapter_version = $2 AND suite_version = $3 AND status IN ('passed', 'exempt')`,
			provider, adapterVersion, suiteVersion).
			Scan(&c.ID, &c.Provider, &c.AdapterVersion, &c.ProviderAPIVersion, &c.SuiteVersion, &c.Status, &c.ProrationMode,
				&c.CertifiedBy, &c.CertifiedAt, &c.ReviewedBy, &c.ReviewedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// SystemRepository is the billing_system (BYPASSRLS) reader of §4.3. Only
// the webhook processor, jobs and reconciliation receive it (BIL-RLS-3).
type SystemRepository struct{ pool *pgcommon.Pool }

var _ port.SystemReader = (*SystemRepository)(nil)

// NewSystemRepository wraps sysPool.
func NewSystemRepository(sysPool *pgcommon.Pool) *SystemRepository {
	return &SystemRepository{pool: sysPool}
}

// TenantForCustomer resolves (provider, provider_customer_id) to a tenant
// (BIL-PAY-3: never from callback metadata alone).
func (r *SystemRepository) TenantForCustomer(ctx context.Context, provider, customerID string) (string, bool, error) {
	var tenantID string
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `SELECT tenant_id::text FROM billing_accounts WHERE provider = $1 AND provider_customer_id = $2`,
			provider, customerID).Scan(&tenantID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return tenantID, err == nil, err
}

// LedgerRepository stores provider callbacks on sysPool (billing_app has no
// grant on payment_event_ledger, §4.3).
type LedgerRepository struct{ pool *pgcommon.Pool }

var _ port.LedgerRepo = (*LedgerRepository)(nil)

// NewLedgerRepository wraps sysPool.
func NewLedgerRepository(sysPool *pgcommon.Pool) *LedgerRepository {
	return &LedgerRepository{pool: sysPool}
}

// Insert records a callback before any domain write (BIL-PAY-1). A
// redelivery of the same (provider, provider_event_id) inserts nothing.
func (r *LedgerRepository) Insert(ctx context.Context, e domain.LedgerEntry) (bool, error) {
	var inserted bool
	err := inTx(ctx, r.pool, func(q querier) error {
		tag, err := q.Exec(ctx, `
			INSERT INTO payment_event_ledger (provider, provider_event_id, event_type, normalised_kind, tenant_id,
			  provider_object_ref, livemode, provider_created_at, payload)
			VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9::jsonb)
			ON CONFLICT (provider, provider_event_id) DO NOTHING`,
			e.Provider, e.ProviderEventID, e.EventType, e.NormalisedKind, e.TenantID, e.ProviderObjectRef, e.Livemode,
			e.ProviderCreatedAt, jsonText(e.Payload))
		inserted = tag.RowsAffected() == 1
		return err
	})
	return inserted, err
}

// Get returns one ledger row.
func (r *LedgerRepository) Get(ctx context.Context, provider, eventID string) (domain.LedgerEntry, error) {
	var e domain.LedgerEntry
	err := inTx(ctx, r.pool, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT provider, provider_event_id, event_type, normalised_kind, tenant_id::text, provider_object_ref, livemode,
			  provider_created_at, received_at, payload, status, attempts, next_attempt_at, outcome
			FROM payment_event_ledger WHERE provider = $1 AND provider_event_id = $2`, provider, eventID).
			Scan(&e.Provider, &e.ProviderEventID, &e.EventType, &e.NormalisedKind, &e.TenantID, &e.ProviderObjectRef, &e.Livemode,
				&e.ProviderCreatedAt, &e.ReceivedAt, &e.Payload, &e.Status, &e.Attempts, &e.NextAttemptAt, &e.Outcome)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return e, domain.NewError(domain.ErrLedgerEventNotFound, "ledger event "+provider+"/"+eventID+" not found")
	}
	return e, err
}

// IdempotencyRepository implements §9.2 records on the app pool (RLS).
type IdempotencyRepository struct{ pool *pgcommon.Pool }

var _ port.IdempotencyRepo = (*IdempotencyRepository)(nil)

// NewIdempotencyRepository returns the repository.
func NewIdempotencyRepository(appPool *pgcommon.Pool) *IdempotencyRepository {
	return &IdempotencyRepository{pool: appPool}
}

// Begin inserts an in_flight record; if the key exists it returns the
// existing record and inserts nothing. Callers compare request_hash
// (422 IDEMPOTENCY_KEY_REUSED), replay a completed response, or answer
// 409 REQUEST_IN_PROGRESS while it is locked.
func (r *IdempotencyRepository) Begin(ctx context.Context, rec domain.IdempotencyRecord) (*domain.IdempotencyRecord, error) {
	var existing *domain.IdempotencyRecord
	err := inTx(ctx, r.pool, func(q querier) error {
		tag, err := q.Exec(ctx, `
			INSERT INTO idempotency_records (tenant_id, scope, key, request_hash, state, locked_until, expires_at)
			VALUES ($1::uuid, $2, $3, $4, 'in_flight', $5, $6)
			ON CONFLICT (tenant_id, scope, key) DO NOTHING`,
			rec.TenantID, rec.Scope, rec.Key, rec.RequestHash, rec.LockedUntil, rec.ExpiresAt)
		if err != nil || tag.RowsAffected() == 1 {
			return err
		}
		var e domain.IdempotencyRecord
		if err := q.QueryRow(ctx, `
			SELECT tenant_id::text, scope, key, request_hash, state, status_code, response_body, locked_until, expires_at
			FROM idempotency_records WHERE tenant_id = $1::uuid AND scope = $2 AND key = $3`,
			rec.TenantID, rec.Scope, rec.Key).
			Scan(&e.TenantID, &e.Scope, &e.Key, &e.RequestHash, &e.State, &e.StatusCode, &e.ResponseBody, &e.LockedUntil, &e.ExpiresAt); err != nil {
			return err
		}
		existing = &e
		return nil
	})
	return existing, err
}

// Complete stores the final response for replay.
func (r *IdempotencyRepository) Complete(ctx context.Context, tenantID, scope, key string, statusCode int, body []byte) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			UPDATE idempotency_records SET state = 'completed', status_code = $4, response_body = $5::jsonb, locked_until = NULL
			WHERE tenant_id = $1::uuid AND scope = $2 AND key = $3`, tenantID, scope, key, statusCode, jsonText(body))
		return err
	})
}

// Release deletes an in_flight record after a failure that must not be
// replayed (for example a provider outage before any charge).
func (r *IdempotencyRepository) Release(ctx context.Context, tenantID, scope, key string) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM idempotency_records WHERE tenant_id = $1::uuid AND scope = $2 AND key = $3 AND state = 'in_flight'`,
			tenantID, scope, key)
		return err
	})
}

// ledgerColumns is the select list shared by Get and Claim.
const ledgerColumns = `provider, provider_event_id, event_type, normalised_kind, tenant_id::text, provider_object_ref, livemode,
  provider_created_at, received_at, payload, status, attempts, next_attempt_at, outcome`

// Claim implements the §8.15 claim: due received or failed rows, plus
// processing rows whose claim expired, never a row of a tenant that still has
// an earlier row in live processing (BIL-PAY-5), oldest first, SKIP LOCKED.
func (r *LedgerRepository) Claim(ctx context.Context, limit int, claimFor time.Duration) ([]domain.LedgerEntry, error) {
	var out []domain.LedgerEntry
	err := inTx(ctx, r.pool, func(q querier) error {
		rows, err := q.Query(ctx, `
			WITH next AS (
			  SELECT provider, provider_event_id FROM payment_event_ledger l
			  WHERE ((status IN ('received', 'failed') AND next_attempt_at <= now())
			         OR (status = 'processing' AND claimed_until < now()))
			    AND NOT EXISTS (SELECT 1 FROM payment_event_ledger e
			                    WHERE e.tenant_id = l.tenant_id AND e.status = 'processing'
			                      AND e.claimed_until >= now()
			                      AND e.provider_created_at <= l.provider_created_at
			                      AND (e.provider, e.provider_event_id) <> (l.provider, l.provider_event_id))
			  ORDER BY provider_created_at
			  LIMIT $1 FOR UPDATE SKIP LOCKED)
			UPDATE payment_event_ledger p
			SET status = 'processing', claimed_until = now() + $2::interval, attempts = attempts + 1
			FROM next WHERE p.provider = next.provider AND p.provider_event_id = next.provider_event_id
			RETURNING `+qualify("p", ledgerColumns), limit, claimFor.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.LedgerEntry
			if err := rows.Scan(&e.Provider, &e.ProviderEventID, &e.EventType, &e.NormalisedKind, &e.TenantID, &e.ProviderObjectRef,
				&e.Livemode, &e.ProviderCreatedAt, &e.ReceivedAt, &e.Payload, &e.Status, &e.Attempts, &e.NextAttemptAt, &e.Outcome); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Finish records the outcome: processed / ignored (done), or failed with a
// retry time; failed without a retry time is parked (next_attempt_at =
// 9999-12-31, which Go reads back as a real time, unlike infinity) for the
// runbook replay (§20.2, B-O13).
func (r *LedgerRepository) Finish(ctx context.Context, provider, eventID string, status domain.LedgerStatus, outcome string, nextAttempt *time.Time) error {
	return inTx(ctx, r.pool, func(q querier) error {
		_, err := q.Exec(ctx, `
			UPDATE payment_event_ledger
			SET status = $3::ledger_status, outcome = $4, claimed_until = NULL,
			    processed_at = CASE WHEN $3::ledger_status IN ('processed', 'ignored') THEN now() ELSE processed_at END,
			    next_attempt_at = CASE WHEN $3::ledger_status = 'failed' THEN COALESCE($5::timestamptz, '9999-12-31T00:00:00Z'::timestamptz) ELSE next_attempt_at END
			WHERE provider = $1 AND provider_event_id = $2`,
			provider, eventID, status, outcome, nextAttempt)
		return err
	})
}

// qualify prefixes each column of a comma-separated list with alias.
func qualify(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// jsonText passes JSON as text. Pools in PgBouncer mode use the simple query
// protocol, where a []byte argument is sent as bytea and Postgres rejects it
// as JSON; text plus an explicit ::jsonb cast works in both protocols.
func jsonText(b []byte) *string {
	if b == nil {
		return nil
	}
	s := string(b)
	return &s
}
