package domain

import "time"

// PlanChangeRequest is §4.2.9.
type PlanChangeRequest struct {
	ChangeID              string
	TenantID              string
	BillingSubscriptionID string
	FromPlan, ToPlan      PlanCode
	Direction             PlanChangeDirection
	Status                ChangeStatus
	EffectiveAt           *time.Time
	AmountMinor           *int64
	Currency              *string
	I18CheckedAt          *time.Time
	BlockerCodes          []string
	ProviderInvoiceID     *string
	ProviderScheduleID    *string
	ClientAction          []byte // JSON
	RequestedBy           string
	IdempotencyKey        string
	CreatedAt, UpdatedAt  time.Time
}
