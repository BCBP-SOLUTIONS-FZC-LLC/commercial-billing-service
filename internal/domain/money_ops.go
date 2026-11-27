package domain

import "time"

// Adjustment is §4.2.16.
type Adjustment struct {
	AdjustmentID   string
	TenantID       string
	Kind           string // credit | debit
	AmountMinor    int64
	Currency       string
	Description    string
	Reason         string
	ProviderRef    *string
	ActorID        string
	IdempotencyKey string
	CreatedAt      time.Time
}

// Refund is §4.2.17.
type Refund struct {
	RefundID         string
	TenantID         string
	InvoiceID        string
	AmountMinor      int64
	Currency         string
	Category         string
	Reason           string
	Status           string // pending | succeeded | failed
	ProviderRefundID *string
	AddonPurchaseID  *string
	AddonTokens      *int64
	AddonEventID     *string
	ActorID          string
	IdempotencyKey   string
	CreatedAt        time.Time
}

// Dispute is §4.2.18.
type Dispute struct {
	DisputeID           string
	TenantID            string
	Provider            string
	ProviderDisputeID   string
	Kind                string // dispute | early_fraud_warning
	PaymentRef          string
	InvoiceID           *string
	IsConversionPayment bool
	WithinRevertWindow  bool
	Reason              *string
	ProviderStatus      string
	RevertDecision      string // none | pending | reverted | declined
	DecidedBy           *string
	OpenedAt            time.Time
}

// Refund statuses and categories set by flows (§8.11, §8.2, B-O9).
const (
	RefundPending   = "pending"
	RefundSucceeded = "succeeded"
	RefundFailed    = "failed"

	RefundCategorySignupFailed       = "signup_failed"
	RefundCategoryConversionReversal = "conversion_reversal"
)

// RevertWindow is CD-10's 7-day reversal window after converted_at.
const RevertWindow = 7 * 24 * time.Hour
