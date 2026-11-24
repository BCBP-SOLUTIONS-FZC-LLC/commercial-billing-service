package domain

import (
	"strings"
	"time"
)

// Checkout session purposes and statuses (§4.2.7).
const (
	PurposeConversion   = "conversion"
	PurposeReactivation = "reactivation"
	PurposePMSetup      = "payment_method_setup"
	PurposeAddonPayment = "addon_payment"

	SessionOpen      = "open"
	SessionCompleted = "completed"
	SessionExpired   = "expired"
)

// PendingSessionPrefix marks a checkout_sessions row whose provider session
// is not created yet: the row is committed before the provider call
// (BIL-CONC-2) and provider_session_id is NOT NULL and unique.
const PendingSessionPrefix = "pending:"

// CheckoutSession is §4.2.7.
type CheckoutSession struct {
	SessionID             string
	TenantID              string
	Purpose               string
	ProviderSessionID     string
	BillingSubscriptionID *string
	AddonPurchaseID       *string
	RequestedPlan         *PlanCode
	RequestedInterval     *BillingInterval
	RequestedSeats        *int
	Status                string
	RequestedBy           string
	ExpiresAt             time.Time
	CreatedAt             time.Time
}

// ProviderCreated reports whether the provider session exists.
func (c CheckoutSession) ProviderCreated() bool {
	return !strings.HasPrefix(c.ProviderSessionID, PendingSessionPrefix)
}

// SameRequest reports a conversion request for the same plan, interval and
// seats (B-P2 is idempotent by content).
func (c CheckoutSession) SameRequest(plan PlanCode, interval BillingInterval, seats int) bool {
	return c.RequestedPlan != nil && *c.RequestedPlan == plan && c.RequestedInterval != nil && *c.RequestedInterval == interval &&
		c.RequestedSeats != nil && *c.RequestedSeats == seats
}

// ConversionSaga is §4.2.8.
type ConversionSaga struct {
	SagaID                string
	TenantID              string
	BillingSubscriptionID string
	Kind                  SagaKind
	State                 SagaState
	PaymentConfirmedAt    time.Time
	RPIdempotencyKey      *string
	Attempts              int
	NextAttemptAt         *time.Time
	LastErrorCode         *string
	LastErrorAt           *time.Time
	RPAcceptedAt          *time.Time
	TenantConvertedEvent  *string
	ConfirmedAt           *time.Time
	AlertedAt             *time.Time
	Resolution            *string
	ResolutionReason      *string
	ResolvedBy            *string
	RecordVersion         int64
	CreatedAt, UpdatedAt  time.Time
}

// SagaBackoff is §8.1.2 step 2: 2 s, 4 s, 8 s … capped.
func SagaBackoff(attempts int, maxBackoff time.Duration) time.Duration {
	d := 2 * time.Second
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}
