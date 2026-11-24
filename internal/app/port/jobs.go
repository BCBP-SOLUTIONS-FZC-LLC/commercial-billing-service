package port

import (
	"context"
	"time"
)

// Stale work kinds for purchase-expiry (§13.2).
const (
	StaleSeatPurchase  = "seat_purchase"
	StaleAddonPurchase = "addon_purchase"
	StaleUpgrade       = "upgrade"
	StaleCheckout      = "checkout"
	StaleSignup        = "signup" // direct-signup charge whose 3-D Secure was abandoned
)

// StaleWork is one abandoned purchase, upgrade, checkout or signup.
type StaleWork struct {
	Kind, TenantID, ID string // ID: purchase, change, session or billing_subscription id
}

// LiveSubscription is one active or past_due subscription with its seat
// item, for seat-projection-check and renewal-repricing.
type LiveSubscription struct {
	TenantID, BillingSubscriptionID string
	ItemUpdatedAt                   time.Time
}

// PIICandidate is an account whose billing contact is due for
// minimisation (§15.3).
type PIICandidate struct {
	TenantID, Provider string
	ProviderCustomerID *string
}

// JobRepo selects work across tenants for the §13.2 CronJobs (system pool,
// read-only selection; writes go through the tenant repositories under
// the tenant lock).
type JobRepo interface {
	// OverdueSagas lists sagas past the alert threshold not alerted yet
	// (BIL-OPS-5), with the number overdue (alerted or not).
	OverdueSagas(ctx context.Context, confirmedBefore time.Time) (unalerted []SagaRef, overdue int, err error)
	// MarkSagaAlerted sets alerted_at once; false when already set. Tenant tx.
	MarkSagaAlerted(ctx context.Context, sagaID string) (bool, error)
	// StaleWork lists purchases and upgrades created before abandonBefore
	// with no outcome, checkout sessions past expires_at, and direct-signup
	// subscriptions still incomplete since scaBefore.
	StaleWork(ctx context.Context, now, abandonBefore, scaBefore time.Time, limit int) ([]StaleWork, error)
	// LiveSubscriptions pages active and past_due subscriptions by id.
	LiveSubscriptions(ctx context.Context, afterID string, limit int) ([]LiveSubscription, error)
	// RecentResyncs counts seats_resync emissions for a tenant since.
	RecentResyncs(ctx context.Context, tenantID string, since time.Time) (int, error)
	// PIICandidates lists accounts closed before offboardedBefore, and
	// accounts whose only subscriptions ended unpaid before abandonedBefore
	// with no checkout since, whose contact is not minimised yet.
	PIICandidates(ctx context.Context, offboardedBefore, abandonedBefore time.Time, limit int) ([]PIICandidate, error)
	// ExpiredIdempotencyTenants lists tenants holding expired records.
	ExpiredIdempotencyTenants(ctx context.Context, now time.Time, limit int) ([]string, error)
	// PruneIdempotency deletes the bound tenant's idempotency records (and
	// stored previews) past expires_at. Tenant tx.
	PruneIdempotency(ctx context.Context, now time.Time) (int64, error)
}

// Metrics records the §11.2 domain metrics the use cases own. The
// adapter registers them on gincommon's registry; tests use NopMetrics.
type Metrics interface {
	DowngradeBlocked(blocker string)
	RepricingScheduled(direction string)
	SeatChange(direction, outcome string)
	SeatProjectionMismatch()
	SagasOverdue(n int)
	ReconciliationFindings(counts map[[2]string]int) // (kind, repaired) → n
}

// NopMetrics discards everything.
type NopMetrics struct{}

// DowngradeBlocked implements Metrics.
func (NopMetrics) DowngradeBlocked(string) {}

// RepricingScheduled implements Metrics.
func (NopMetrics) RepricingScheduled(string) {}

// SeatChange implements Metrics.
func (NopMetrics) SeatChange(string, string) {}

// SeatProjectionMismatch implements Metrics.
func (NopMetrics) SeatProjectionMismatch() {}

// SagasOverdue implements Metrics.
func (NopMetrics) SagasOverdue(int) {}

// ReconciliationFindings implements Metrics.
func (NopMetrics) ReconciliationFindings(map[[2]string]int) {}
