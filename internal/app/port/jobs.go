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

// Purchase kinds for commercial_purchase_confirmation_to_event_seconds.
const (
	PurchaseKindSeats           = "seats"
	PurchaseKindTokenAddon      = "token_addon"
	PurchaseKindPlan            = "plan"
	PurchaseKindConversionSeats = "conversion_seats"
)

// Metrics records the §11.2 counters and histograms the use cases own.
// Gauges derived from the database come from the exporter (MetricsSnapshot).
// The adapter is adapters/metrics; tests use NopMetrics or a fake.
type Metrics interface {
	PurchaseToEvent(kind string, d time.Duration)
	SagaConfirmed(kind string, d time.Duration)
	RPConvertAttempt(outcome string)
	UnmatchedConversion()
	DowngradeBlocked(blocker string)
	RepricingScheduled(direction string)
	SeatChange(direction, outcome string)
	SeatProjectionMismatch()
	ProrationDeferred(currency, reason string, amountMinor int64)
	AddonPurchase(packCode, outcome string)
	PaymentFailure(reason string)
	DunningTransition(to string)
	InvoiceDisposition(action string)
	WebhookProcessed(provider, kind, outcome string, receivedToProcessed time.Duration)
	SagasOverdue(n int)
	ReconciliationFindings(counts map[[2]string]int) // (kind, repaired) → n, last run
	CrossTenantJobRun(job, outcome string)
}

// NopMetrics discards everything.
type NopMetrics struct{}

var _ Metrics = NopMetrics{}

func (NopMetrics) PurchaseToEvent(string, time.Duration)                  {}
func (NopMetrics) SagaConfirmed(string, time.Duration)                    {}
func (NopMetrics) RPConvertAttempt(string)                                {}
func (NopMetrics) UnmatchedConversion()                                   {}
func (NopMetrics) DowngradeBlocked(string)                                {}
func (NopMetrics) RepricingScheduled(string)                              {}
func (NopMetrics) SeatChange(string, string)                              {}
func (NopMetrics) SeatProjectionMismatch()                                {}
func (NopMetrics) ProrationDeferred(string, string, int64)                {}
func (NopMetrics) AddonPurchase(string, string)                           {}
func (NopMetrics) PaymentFailure(string)                                  {}
func (NopMetrics) DunningTransition(string)                               {}
func (NopMetrics) InvoiceDisposition(string)                              {}
func (NopMetrics) WebhookProcessed(string, string, string, time.Duration) {}
func (NopMetrics) SagasOverdue(int)                                       {}
func (NopMetrics) ReconciliationFindings(map[[2]string]int)               {}
func (NopMetrics) CrossTenantJobRun(string, string)                       {}

// MetricsSnapshot is the state the exporter reads from the database
// (system pool) every 30 s for the §11.2 gauges.
type MetricsSnapshot struct {
	WebhookBacklog   map[string]int    // ledger status → rows
	SagasInFlight    map[string]int    // saga state → open sagas
	SagasOverdue     int               // past SAGA_ALERT_AFTER
	Subscriptions    map[[3]string]int // (status, plan, provider) → n
	CollectionPaused map[string]int    // by → n
	InvoicesHeld     map[string]int    // currency → drafts held without a decision
	EmissionsPending map[string]int    // builder → pending
}

// SnapshotReader reads a MetricsSnapshot.
type SnapshotReader interface {
	MetricsSnapshot(ctx context.Context, sagaOverdueBefore time.Time) (MetricsSnapshot, error)
}
