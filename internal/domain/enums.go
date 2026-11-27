package domain

// Enumerations mirroring the Postgres types of LLD §4.1. The string values
// are the database and wire values; they never change once published.

// PlanCode is a Catalog plan code (HLD §2.3).
type PlanCode string

// Plan codes.
const (
	PlanStarter    PlanCode = "starter"
	PlanPro        PlanCode = "pro"
	PlanEnterprise PlanCode = "enterprise"
)

// Valid reports whether p is a known plan.
func (p PlanCode) Valid() bool { _, ok := planRank[p]; return ok }

// BillingInterval is the subscription billing interval.
type BillingInterval string

// Intervals.
const (
	IntervalMonth BillingInterval = "month"
	IntervalYear  BillingInterval = "year"
)

// Valid reports whether i is a known interval.
func (i BillingInterval) Valid() bool { return i == IntervalMonth || i == IntervalYear }

// SubscriptionOrigin records how a subscription started.
type SubscriptionOrigin string

// Origins.
const (
	OriginTrialConversion SubscriptionOrigin = "trial_conversion"
	OriginDirectSignup    SubscriptionOrigin = "direct_signup"
	OriginOperatorStub    SubscriptionOrigin = "operator_stub"
)

// IAMLifecycle is the tenant lifecycle Billing projects from IAM events (§7.1).
type IAMLifecycle string

// Lifecycles.
const (
	LifecycleTrial      IAMLifecycle = "trial"
	LifecycleActive     IAMLifecycle = "active"
	LifecycleSuspended  IAMLifecycle = "suspended"
	LifecycleOffboarded IAMLifecycle = "offboarded"
)

// PurchaseStatus is the status of a seat or add-on purchase.
type PurchaseStatus string

// Purchase statuses.
const (
	PurchasePendingPayment PurchaseStatus = "pending_payment"
	PurchaseRequiresAction PurchaseStatus = "requires_action"
	PurchaseConfirmed      PurchaseStatus = "confirmed"
	PurchaseFailed         PurchaseStatus = "failed"
	PurchaseExpired        PurchaseStatus = "expired"
	PurchaseCancelled      PurchaseStatus = "cancelled"
)

// Open reports whether the purchase is still in flight (BIL-SEAT-1).
func (s PurchaseStatus) Open() bool {
	return s == PurchasePendingPayment || s == PurchaseRequiresAction
}

// ScheduleStatus is the status of a seat change schedule.
type ScheduleStatus string

// Schedule statuses.
const (
	ScheduleScheduled ScheduleStatus = "scheduled"
	ScheduleApplying  ScheduleStatus = "applying"
	ScheduleApplied   ScheduleStatus = "applied"
	ScheduleSkipped   ScheduleStatus = "skipped"
	ScheduleCancelled ScheduleStatus = "cancelled"
)

// Open reports whether the schedule still awaits its renewal (BIL-SEAT-2).
func (s ScheduleStatus) Open() bool { return s == ScheduleScheduled || s == ScheduleApplying }

// ChangeStatus is the status of a plan change request.
type ChangeStatus string

// Plan change statuses.
const (
	ChangePendingPayment ChangeStatus = "pending_payment"
	ChangeScheduled      ChangeStatus = "scheduled"
	ChangeApplied        ChangeStatus = "applied"
	ChangeBlocked        ChangeStatus = "blocked"
	ChangeFailed         ChangeStatus = "failed"
	ChangeCancelled      ChangeStatus = "cancelled"
)

// InvoiceKind classifies invoices.
type InvoiceKind string

// Invoice kinds.
const (
	InvoiceSubscriptionCreate InvoiceKind = "subscription_create"
	InvoiceSubscriptionCycle  InvoiceKind = "subscription_cycle"
	InvoiceProration          InvoiceKind = "proration"
	InvoiceTokenAddon         InvoiceKind = "token_addon"
	InvoiceManual             InvoiceKind = "manual"
)

// InvoiceStatus mirrors the provider invoice status.
type InvoiceStatus string

// Invoice statuses.
const (
	InvoiceDraft         InvoiceStatus = "draft"
	InvoiceOpen          InvoiceStatus = "open"
	InvoicePaid          InvoiceStatus = "paid"
	InvoiceVoid          InvoiceStatus = "void"
	InvoiceUncollectible InvoiceStatus = "uncollectible"
)

// LedgerStatus is the processing status of a payment_event_ledger row (§8.15).
type LedgerStatus string

// Ledger statuses.
const (
	LedgerReceived   LedgerStatus = "received"
	LedgerProcessing LedgerStatus = "processing"
	LedgerProcessed  LedgerStatus = "processed"
	LedgerIgnored    LedgerStatus = "ignored"
	LedgerFailed     LedgerStatus = "failed"
)

// CancelReason records why a subscription was cancelled or ended.
type CancelReason string

// Cancel reasons.
const (
	CancelDunningExhausted CancelReason = "dunning_exhausted"
	CancelCustomer         CancelReason = "customer"
	CancelAutoRenewOff     CancelReason = "auto_renew_off"
	CancelOperatorReversal CancelReason = "operator_reversal"
	CancelOffboarded       CancelReason = "offboarded"
	CancelSignupFailed     CancelReason = "signup_failed"
)
