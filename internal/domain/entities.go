package domain

import "time"

// Persistent entities of LLD §4.2 that the Phase 2 repositories read and
// write. Flow-specific records (sagas, purchases, schedules, invoices,
// refunds, disputes) are added with their flows.

// BillingAccount is one tenant's billing account (§4.2.1). Contact fields
// are personal data (§15.2) and are nulled by pii-minimisation.
type BillingAccount struct {
	TenantID             string
	Provider             string
	ProviderCustomerID   *string // nil only for the manual provider
	Currency             string  // fixed while a subscription is live (BIL-SUB-6)
	BillingContactName   *string
	BillingContactEmail  *string
	CompanyName          *string
	Country              *string
	TaxID                *string
	DefaultPMSet         bool
	IAMLifecycle         IAMLifecycle
	IAMSuspensionSource  *string // billing_lapse | operator, set iff suspended
	IAMLastEventAt       *time.Time
	ClosedAt             *time.Time
	PIIMinimisedAt       *time.Time
	RecordVersion        int64
	CreatedAt, UpdatedAt time.Time
}

// Subscription is §4.2.4. Status moves only along the §8.14 table.
type Subscription struct {
	BillingSubscriptionID  string
	TenantID               string
	Provider               string
	ProviderSubscriptionID *string
	Origin                 SubscriptionOrigin
	Plan                   PlanCode
	Interval               BillingInterval
	Status                 SubscriptionStatus
	AutoRenew              bool
	CurrentPeriodStart     *time.Time
	CurrentPeriodEnd       *time.Time
	CollectionPaused       bool
	CollectionPausedBy     *string // auto | operator
	PaymentConfirmedAt     *time.Time
	ConvertedAt            *time.Time
	PastDueSince           *time.Time
	CancelledAt            *time.Time
	CancelReason           *CancelReason
	EndedAt                *time.Time
	RecordVersion          int64
	CreatedAt, UpdatedAt   time.Time
}

// SubscriptionItem is the seat line of a subscription (§4.2.5).
type SubscriptionItem struct {
	ItemID                string
	TenantID              string
	BillingSubscriptionID string
	Kind                  string // "seats"
	PriceID               string
	Quantity              int
	ProviderItemID        *string
	ScheduledPriceID      *string    // §8.16 renewal repricing
	ScheduledPriceAt      *time.Time // renewal instant R
	RecordVersion         int64
	UpdatedAt             time.Time
}

// StatusHistoryEntry is one append-only row of §4.2.6, written in the same
// transaction as the change it records.
type StatusHistoryEntry struct {
	TenantID              string
	BillingSubscriptionID string
	FromStatus            *SubscriptionStatus
	ToStatus              SubscriptionStatus
	FromPlan, ToPlan      *PlanCode
	FromSeats, ToSeats    *int
	Cause                 string // webhook:<type> | api:<route> | operator:<route> | iam:<event> | job:<name>
	PaymentEventRef       *string
	ActorID               string
	OccurredAt            time.Time
}

// PlanPrice is a price-book row (§4.2.2): per seat per interval.
type PlanPrice struct {
	PriceID         string
	Plan            PlanCode
	Interval        BillingInterval
	Currency        string
	UnitAmountMinor int64
	Provider        string
	ProviderPriceID *string
	EffectiveFrom   time.Time
	EffectiveTo     *time.Time
	CreatedBy       string
	Reason          string
}

// CertificationStatus is a provider_certifications status (§4.2.24).
type CertificationStatus string

// Certification statuses.
const (
	CertPendingReview CertificationStatus = "pending_review"
	CertPassed        CertificationStatus = "passed"
	CertFailed        CertificationStatus = "failed"
	CertRejected      CertificationStatus = "rejected"
	CertRevoked       CertificationStatus = "revoked"
	CertExempt        CertificationStatus = "exempt"
)

// Enables reports whether the status lets a provider take new accounts (BIL-PROV-1).
func (s CertificationStatus) Enables() bool { return s == CertPassed || s == CertExempt }

// Certification is a provider capability certification record.
type Certification struct {
	ID                 string
	Provider           string
	AdapterVersion     string
	ProviderAPIVersion *string
	SuiteVersion       string
	Status             CertificationStatus
	ProrationMode      string // native | fallback | n/a
	CertifiedBy        string
	CertifiedAt        time.Time
	ReviewedBy         *string
	ReviewedAt         *time.Time
}

// IdempotencyState is an idempotency record state (§9.2).
type IdempotencyState string

// States.
const (
	IdempotencyInFlight  IdempotencyState = "in_flight"
	IdempotencyCompleted IdempotencyState = "completed"
)

// IdempotencyRecord is §4.2.19.
type IdempotencyRecord struct {
	TenantID     string
	Scope        string // route id, e.g. B-P7
	Key          string
	RequestHash  []byte
	State        IdempotencyState
	StatusCode   *int
	ResponseBody []byte // JSON
	LockedUntil  *time.Time
	ExpiresAt    time.Time
}

// LedgerEntry is one provider callback in payment_event_ledger (§4.2.15).
// It is written before any domain change (BIL-PAY-1).
type LedgerEntry struct {
	Provider          string
	ProviderEventID   string
	EventType         string
	NormalisedKind    string
	TenantID          *string
	ProviderObjectRef *string
	Livemode          bool
	ProviderCreatedAt time.Time
	ReceivedAt        time.Time
	Payload           []byte // allow-listed fields only (BIL-PCI-3)
	Status            LedgerStatus
	Attempts          int
	NextAttemptAt     time.Time
	Outcome           *string
}
