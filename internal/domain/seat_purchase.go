package domain

import "time"

// Seat purchase reasons (§4.2.10).
const (
	SeatReasonPurchase     = "purchase"
	SeatReasonRenewalTopUp = "renewal_topup"

	AmountSourceProvider   = "provider_preview"
	AmountSourceCalculator = "billing_calculator"
)

// SeatPurchase is §4.2.10.
type SeatPurchase struct {
	PurchaseID            string
	TenantID              string
	BillingSubscriptionID string
	Reason                string
	SeatsAdded            int
	SeatsBefore           int
	SeatsAfter            int
	ProrationInstant      time.Time
	AmountMinor           int64
	Currency              string
	AmountSource          string
	UnitAmountMinor       *int64
	RemainingSeconds      *int64
	PeriodSeconds         *int64
	AmountUnroundedMinor  *string
	DeferredAmountMinor   *int64
	DeferralReason        *string
	Status                PurchaseStatus
	ProviderInvoiceID     *string
	ClientAction          []byte
	RequestedBy           string
	IdempotencyKey        string
	ConfirmedAt           *time.Time
	CreatedAt, UpdatedAt  time.Time
}

// SeatChangeSchedule is §4.2.11.
type SeatChangeSchedule struct {
	ScheduleID            string
	TenantID              string
	BillingSubscriptionID string
	TargetSeats           int
	EffectiveAt           time.Time
	Status                ScheduleStatus
	SeatsInUseAtApply     *int
	RenewalSeats          *int
	FinalSeats            *int
	ProviderScheduleID    *string
	SkipReason            *string
	RequestedBy           string
	RecordVersion         int64
	CreatedAt, UpdatedAt  time.Time
}
