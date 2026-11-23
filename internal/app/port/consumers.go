package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// InboxRunner runs fn once per (consumer, event id): in one tenant
// transaction that also records the id in processed_events (pkg/inbox), so
// a redelivered or duplicated message changes nothing (§7.1). A duplicate
// returns nil without calling fn.
type InboxRunner interface {
	RunOnce(ctx context.Context, consumer, eventID, eventType, tenantID string, fn func(ctx context.Context) error) error
}

// TenantLocker takes the per-tenant advisory lock for the rest of the
// transaction in ctx (BIL-CONC-1).
type TenantLocker interface {
	LockTenant(ctx context.Context, tenantID string) error
}

// LifecycleRepo projects IAM lifecycle events onto billing_accounts.
type LifecycleRepo interface {
	// AccountForUpdate locks and returns the tenant's account; found=false
	// for a tenant without one (a trial tenant).
	AccountForUpdate(ctx context.Context, tenantID string) (a domain.BillingAccount, found bool, err error)
	ApplyLifecycle(ctx context.Context, tenantID string, c domain.LifecycleChange) error
}

// UsageRecordRepo stores UsagePeriodClosed totals.
type UsageRecordRepo interface {
	// Upsert inserts the record, or replaces the stored one for the same
	// period only when SourceEventTime is newer; applied=false otherwise.
	Upsert(ctx context.Context, r domain.UsagePeriodRecord) (applied bool, err error)
}

// EmissionRepo is the scheduled_emissions queue (BIL-EVT-6).
type EmissionRepo interface {
	// Schedule inserts a pending emission; an existing pending one for the
	// same (tenant, builder, ref) is kept (inserted=false).
	Schedule(ctx context.Context, e domain.ScheduledEmission) (inserted bool, err error)
	// DueTenants lists tenants with a pending emission due at or before now
	// (system pool, across tenants).
	DueTenants(ctx context.Context, now time.Time, limit int) ([]string, error)
	// ClaimDue locks the tenant's due pending emissions (SKIP LOCKED) in the
	// transaction in ctx.
	ClaimDue(ctx context.Context, tenantID string, now time.Time) ([]domain.ScheduledEmission, error)
	// Finish marks an emission emitted (with its event id) or cancelled.
	Finish(ctx context.Context, emissionID string, emittedEventID *string) error
}
