package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/inbox"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// InboxRunner is port.InboxRunner on pkg/inbox's Store.Process: the event
// id is claimed in processed_events inside the handler's tenant transaction
// on the app pool, so the handler's writes happen exactly once.
type InboxRunner struct {
	stores map[string]*inbox.Store
}

var _ port.InboxRunner = (*InboxRunner)(nil)

// NewInboxRunner builds one inbox store per consumer name (§7.1:
// tenant-billing, usage-billing) on the app pool.
func NewInboxRunner(appPool *pgcommon.Pool, consumers ...string) (*InboxRunner, error) {
	r := &InboxRunner{stores: map[string]*inbox.Store{}}
	for _, c := range consumers {
		s, err := inbox.NewStore(appPool, c)
		if err != nil {
			return nil, err
		}
		r.stores[c] = s
	}
	return r, nil
}

// RunOnce implements port.InboxRunner.
func (r *InboxRunner) RunOnce(ctx context.Context, consumer, eventID, eventType, tenantID string, fn func(ctx context.Context) error) error {
	s, ok := r.stores[consumer]
	if !ok {
		return fmt.Errorf("inbox: unknown consumer %q", consumer)
	}
	if tenantID == "" {
		return domain.NewError(domain.ErrInternal, "inbox transaction without a tenant id")
	}
	ctx = pgcommon.WithGUCSet(ctx, pgdomain.GUCSet{TenantID: tenantID})
	env := events.Envelope[json.RawMessage]{ID: eventID, Type: eventType}
	return mapDBError(s.Process(ctx, env, func(ctx context.Context, tx pgcommon.Tx) error {
		return fn(withTx(ctx, tx))
	}))
}

// TenantLocker is port.TenantLocker.
type TenantLocker struct{}

var _ port.TenantLocker = TenantLocker{}

// LockTenant takes BIL-CONC-1's transaction-scoped advisory lock.
func (TenantLocker) LockTenant(ctx context.Context, tenantID string) error {
	tx, ok := txFrom(ctx)
	if !ok {
		return domain.NewError(domain.ErrInternal, "tenant lock outside a transaction")
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('billing:' || $1::text, 0))`, tenantID)
	return mapDBError(err)
}

// LifecycleRepository is port.LifecycleRepo on billing_accounts.
type LifecycleRepository struct{ pool *pgcommon.Pool }

var _ port.LifecycleRepo = (*LifecycleRepository)(nil)

// NewLifecycleRepository wraps the app pool.
func NewLifecycleRepository(appPool *pgcommon.Pool) *LifecycleRepository {
	return &LifecycleRepository{pool: appPool}
}

// AccountForUpdate implements port.LifecycleRepo.
func (r *LifecycleRepository) AccountForUpdate(ctx context.Context, tenantID string) (domain.BillingAccount, bool, error) {
	var a domain.BillingAccount
	err := inTx(ctx, r.pool, func(q querier) error {
		var err error
		a, err = scanAccount(q.QueryRow(ctx, `SELECT `+accountColumns+` FROM billing_accounts WHERE tenant_id = $1::uuid FOR UPDATE`, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, false, nil
	}
	return a, err == nil, err
}

// ApplyLifecycle implements port.LifecycleRepo.
func (r *LifecycleRepository) ApplyLifecycle(ctx context.Context, tenantID string, c domain.LifecycleChange) error {
	return inTx(ctx, r.pool, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE billing_accounts
			SET iam_lifecycle = $2::iam_lifecycle, iam_suspension_source = $3, closed_at = $4, iam_last_event_at = $5
			WHERE tenant_id = $1::uuid`,
			tenantID, string(c.To), c.SuspensionSource, c.ClosedAt, c.EventAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return subscriptionNotFound("billing account for tenant", tenantID)
		}
		return nil
	})
}

// UsageRecordRepository is port.UsageRecordRepo.
type UsageRecordRepository struct{ pool *pgcommon.Pool }

var _ port.UsageRecordRepo = (*UsageRecordRepository)(nil)

// NewUsageRecordRepository wraps the app pool.
func NewUsageRecordRepository(appPool *pgcommon.Pool) *UsageRecordRepository {
	return &UsageRecordRepository{pool: appPool}
}

// Upsert implements port.UsageRecordRepo (§4.2.21: a re-run close replaces
// the row only when its envelope time is newer).
func (r *UsageRecordRepository) Upsert(ctx context.Context, u domain.UsagePeriodRecord) (bool, error) {
	var applied bool
	err := inTx(ctx, r.pool, func(q querier) error {
		tag, err := q.Exec(ctx, `
			INSERT INTO usage_period_records (tenant_id, period_start, period_end, tokens_total, api_requests_total,
			  addon_consumed, limits, source_event_id, source_event_time)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb, $8::uuid, $9)
			ON CONFLICT (tenant_id, period_start) DO UPDATE SET
			  period_end = EXCLUDED.period_end, tokens_total = EXCLUDED.tokens_total,
			  api_requests_total = EXCLUDED.api_requests_total, addon_consumed = EXCLUDED.addon_consumed,
			  limits = EXCLUDED.limits, source_event_id = EXCLUDED.source_event_id,
			  source_event_time = EXCLUDED.source_event_time, received_at = now()
			WHERE usage_period_records.source_event_time < EXCLUDED.source_event_time`,
			u.TenantID, u.PeriodStart, u.PeriodEnd, u.TokensTotal, u.APIRequestsTotal, u.AddonConsumed,
			jsonText(u.Limits), u.SourceEventID, u.SourceEventTime)
		applied = tag.RowsAffected() == 1
		return err
	})
	return applied, err
}

// EmissionRepository is port.EmissionRepo: tenant reads and writes on the
// app pool, the cross-tenant due scan on the system pool.
type EmissionRepository struct{ app, sys *pgcommon.Pool }

var _ port.EmissionRepo = (*EmissionRepository)(nil)

// NewEmissionRepository wraps both pools.
func NewEmissionRepository(appPool, sysPool *pgcommon.Pool) *EmissionRepository {
	return &EmissionRepository{app: appPool, sys: sysPool}
}

// Schedule implements port.EmissionRepo.
func (r *EmissionRepository) Schedule(ctx context.Context, e domain.ScheduledEmission) (bool, error) {
	if e.EmissionID == "" {
		e.EmissionID = uuid.Must(uuid.NewV7()).String()
	}
	var inserted bool
	err := inTx(ctx, r.app, func(q querier) error {
		tag, err := q.Exec(ctx, `
			INSERT INTO scheduled_emissions (emission_id, tenant_id, event_type, payload_builder, ref_id, due_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6)
			ON CONFLICT (tenant_id, payload_builder, ref_id) WHERE status = 'pending' DO NOTHING`,
			e.EmissionID, e.TenantID, string(e.EventType), e.PayloadBuilder, e.RefID, e.DueAt)
		inserted = tag.RowsAffected() == 1
		return err
	})
	return inserted, err
}

// DueTenants implements port.EmissionRepo.
func (r *EmissionRepository) DueTenants(ctx context.Context, now time.Time, limit int) ([]string, error) {
	var out []string
	err := inTx(ctx, r.sys, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT tenant_id::text FROM scheduled_emissions
			WHERE status = 'pending' AND due_at <= $1
			GROUP BY tenant_id ORDER BY min(due_at) LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return out, err
}

// ClaimDue implements port.EmissionRepo.
func (r *EmissionRepository) ClaimDue(ctx context.Context, tenantID string, now time.Time) ([]domain.ScheduledEmission, error) {
	var out []domain.ScheduledEmission
	err := inTx(ctx, r.app, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT emission_id::text, tenant_id::text, event_type, payload_builder, ref_id::text, due_at
			FROM scheduled_emissions
			WHERE tenant_id = $1::uuid AND status = 'pending' AND due_at <= $2
			ORDER BY due_at, emission_id FOR UPDATE SKIP LOCKED`, tenantID, now)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ScheduledEmission, error) {
			var e domain.ScheduledEmission
			var typ string
			err := row.Scan(&e.EmissionID, &e.TenantID, &typ, &e.PayloadBuilder, &e.RefID, &e.DueAt)
			e.EventType = domain.EventType(typ)
			return e, err
		})
		return err
	})
	return out, err
}

// Finish implements port.EmissionRepo.
func (r *EmissionRepository) Finish(ctx context.Context, emissionID string, emittedEventID *string) error {
	status := "cancelled"
	if emittedEventID != nil {
		status = "emitted"
	}
	return inTx(ctx, r.app, func(q querier) error {
		_, err := q.Exec(ctx, `UPDATE scheduled_emissions SET status = $2, emitted_event_id = $3::uuid
			WHERE emission_id = $1::uuid AND status = 'pending'`, emissionID, status, emittedEventID)
		return err
	})
}

// Prune deletes every consumer's processed_events older than retention
// (processed-events-prune, §13.2).
func (r *InboxRunner) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	var total int64
	for _, s := range r.stores {
		n, err := s.Prune(ctx, retention, 0)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
