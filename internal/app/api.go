package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// The use-case surface of the HTTP API (LLD §5). Request types carry the
// §10.4 validation as `binding` tags (checked by the HTTP adapter before a
// use case is called); view types are the §5.4 response bodies. Each method
// names its endpoint id.

// Money is the API money object (§5.1): integer minor units, ISO 4217.
type Money struct {
	AmountMinor int64  `json:"amount_minor" binding:"min=0"`
	Currency    string `json:"currency" binding:"required,iso4217"`
}

// MoneyFrom converts a domain amount.
func MoneyFrom(m domain.Money) Money { return Money{AmountMinor: m.AmountMinor, Currency: m.Currency} }

// Page is cursor pagination (§5.1): limit 1–100 (default 20), opaque cursor.
type Page struct {
	Limit  int    `form:"limit" binding:"omitempty,min=1,max=100"`
	Cursor string `form:"cursor" binding:"omitempty,max=512"`
}

// List is a paginated response.
type List[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// ── Common views (§5.4) ────────────────────────────────────────────────────

// SubscriptionView is B-P1, B-P4, B-P12, B-P13, B-O7.
type SubscriptionView struct {
	BillingSubscriptionID string           `json:"billing_subscription_id,omitempty"`
	TenantID              string           `json:"tenant_id,omitempty"`
	Provider              string           `json:"provider,omitempty"`
	Origin                string           `json:"origin,omitempty"`
	Plan                  string           `json:"plan,omitempty"`
	Interval              string           `json:"interval,omitempty"`
	LicensedSeats         int              `json:"licensed_seats,omitempty"`
	UnitPrice             *Money           `json:"unit_price,omitempty"`
	ScheduledUnitPrice    *ScheduledPrice  `json:"scheduled_unit_price"`
	Status                string           `json:"status"`
	Trial                 bool             `json:"trial,omitempty"`
	AutoRenew             bool             `json:"auto_renew"`
	CurrentPeriodStart    *time.Time       `json:"current_period_start,omitempty"`
	CurrentPeriodEnd      *time.Time       `json:"current_period_end,omitempty"`
	RenewalEstimate       *RenewalEstimate `json:"renewal_estimate,omitempty"`
	CollectionPaused      bool             `json:"collection_paused"`
	PastDueSince          *time.Time       `json:"past_due_since"`
	CancelledAt           *time.Time       `json:"cancelled_at"`
	PendingPlanChange     *PlanChangeView  `json:"pending_plan_change"`
	ScheduledReduction    *ScheduleView    `json:"scheduled_reduction"`
	OpenPurchases         []PurchaseView   `json:"open_purchases"`
	RecordVersion         int64            `json:"record_version,omitempty"`
	CallerPermissions     []string         `json:"caller_permissions"`
	ActionsAvailable      []string         `json:"actions_available"`
}

// ScheduledPrice is a renewal repricing (§8.16).
type ScheduledPrice struct {
	Money
	EffectiveAt time.Time `json:"effective_at"`
}

// RenewalEstimate is the next renewal's seats and amount.
type RenewalEstimate struct {
	Seats  int   `json:"seats"`
	Amount Money `json:"amount"`
}

// PlanChangeView is a plan change request.
type PlanChangeView struct {
	ChangeID    string     `json:"change_id"`
	Direction   string     `json:"direction"`
	FromPlan    string     `json:"from_plan"`
	ToPlan      string     `json:"to_plan"`
	Status      string     `json:"status"`
	EffectiveAt *time.Time `json:"effective_at,omitempty"`
	Blockers    []Blocker  `json:"blockers"`
}

// Blocker is an I-18 downgrade blocker.
type Blocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// ScheduleView is a scheduled seat reduction (B-P10).
type ScheduleView struct {
	ScheduleID           string    `json:"schedule_id"`
	TargetSeats          int       `json:"target_seats"`
	EffectiveAt          time.Time `json:"effective_at"`
	Status               string    `json:"status"`
	SeatsInUse           *int      `json:"seats_in_use"`
	RenewalEstimateSeats *int      `json:"renewal_estimate_seats"`
	Note                 string    `json:"note,omitempty"`
}

// ClientAction is what the browser must do to finish a payment (3-D Secure
// or a hosted payment session). client_secret only reaches the caller that
// started the payment (BIL-API-4).
type ClientAction struct {
	Type         string     `json:"type"`
	ClientSecret string     `json:"client_secret,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// PurchaseView is a seat or token add-on purchase.
type PurchaseView struct {
	PurchaseID   string        `json:"purchase_id"`
	Kind         string        `json:"kind"`
	Status       string        `json:"status"`
	Amount       Money         `json:"amount"`
	SeatsAfter   *int          `json:"seats_after,omitempty"`
	SeatUpdate   string        `json:"seat_update,omitempty"`
	PackCode     string        `json:"pack_code,omitempty"`
	TokensTotal  *int64        `json:"tokens_total,omitempty"`
	Activation   string        `json:"activation,omitempty"`
	ClientAction *ClientAction `json:"client_action"`
	CreatedAt    time.Time     `json:"created_at"`
	ConfirmedAt  *time.Time    `json:"confirmed_at"`
}

// ── Tenant requests and results ────────────────────────────────────────────

// CheckoutRequest is B-P2.
type CheckoutRequest struct {
	Plan          string `json:"plan" binding:"required,oneof=starter pro enterprise"`
	Interval      string `json:"interval" binding:"required,oneof=month year"`
	LicensedSeats int    `json:"licensed_seats" binding:"required,min=1,max=1000"`
}

// CheckoutResult is B-P2's 201.
type CheckoutResult struct {
	CheckoutSessionID     string    `json:"checkout_session_id"`
	CheckoutURL           string    `json:"checkout_url"`
	ExpiresAt             time.Time `json:"expires_at"`
	BillingSubscriptionID string    `json:"billing_subscription_id"`
	Price                 Money     `json:"price"`
}

// PlanChangePreviewRequest is B-P3.
type PlanChangePreviewRequest struct {
	TargetPlan string `json:"target_plan" binding:"required,oneof=starter pro enterprise"`
}

// PlanChangePreview is B-P3's 200 (upgrade or downgrade shape).
type PlanChangePreview struct {
	Direction         string      `json:"direction"`
	TargetPlan        string      `json:"target_plan"`
	Effective         string      `json:"effective"`
	AmountDueNow      *Money      `json:"amount_due_now,omitempty"`
	NextRenewalAmount Money       `json:"next_renewal_amount"`
	ProrationInstant  *time.Time  `json:"proration_instant,omitempty"`
	PreviewID         string      `json:"preview_id,omitempty"`
	ExpiresAt         *time.Time  `json:"expires_at,omitempty"`
	EffectiveAt       *time.Time  `json:"effective_at,omitempty"`
	Check             *PlanChange `json:"check,omitempty"`
}

// PlanChange is the I-18 check result in a downgrade preview.
type PlanChange struct {
	Allowed  bool      `json:"allowed"`
	Blockers []Blocker `json:"blockers"`
}

// PlanChangeRequest is B-P4.
type PlanChangeRequest struct {
	TargetPlan    string `json:"target_plan" binding:"required,oneof=starter pro enterprise"`
	RecordVersion int64  `json:"record_version" binding:"required,min=1"`
	PreviewID     string `json:"preview_id" binding:"omitempty,uuid"`
}

// PlanChangeResult is B-P4's 202 (upgrade) or 200 (downgrade scheduled).
type PlanChangeResult struct {
	ChangeID     string        `json:"change_id"`
	Direction    string        `json:"direction"`
	Status       string        `json:"status"`
	Amount       *Money        `json:"amount,omitempty"`
	EffectiveAt  *time.Time    `json:"effective_at,omitempty"`
	ClientAction *ClientAction `json:"client_action"`
}

// SeatPreviewRequest is B-P6.
type SeatPreviewRequest struct {
	AddSeats int `json:"add_seats" binding:"required,min=1,max=1000"`
}

// SeatPreview is B-P6's 200.
type SeatPreview struct {
	PreviewID            string    `json:"preview_id"`
	SeatsBefore          int       `json:"seats_before"`
	SeatsAfter           int       `json:"seats_after"`
	AmountDueNow         Money     `json:"amount_due_now"`
	AmountSource         string    `json:"amount_source"`
	AmountUnroundedMinor string    `json:"amount_unrounded_minor,omitempty"`
	DeferredAmount       *Money    `json:"deferred_amount,omitempty"`
	DeferralReason       string    `json:"deferral_reason,omitempty"`
	ProrationInstant     time.Time `json:"proration_instant"`
	CurrentPeriodEnd     time.Time `json:"current_period_end"`
	NextRenewalAmount    Money     `json:"next_renewal_amount"`
	ExpiresAt            time.Time `json:"expires_at"`
}

// SeatPurchaseRequest is B-P7.
type SeatPurchaseRequest struct {
	AddSeats  int    `json:"add_seats" binding:"required,min=1,max=1000"`
	PreviewID string `json:"preview_id" binding:"required,uuid"`
}

// ReductionRequest is B-P10.
type ReductionRequest struct {
	TargetSeats int `json:"target_seats" binding:"required,min=1,max=100000"`
}

// CancelRequest is B-P12.
type CancelRequest struct {
	RecordVersion int64  `json:"record_version" binding:"required,min=1"`
	Reason        string `json:"reason" binding:"required,oneof=too_expensive missing_features switching_vendor no_longer_needed other"`
	Comment       string `json:"comment" binding:"omitempty,max=500"`
}

// ResumeRequest is B-P13.
type ResumeRequest struct {
	RecordVersion int64 `json:"record_version" binding:"required,min=1"`
}

// ReactivateRequest is B-P14.
type ReactivateRequest struct {
	LicensedSeats *int   `json:"licensed_seats" binding:"omitempty,min=1,max=1000"`
	Interval      string `json:"interval" binding:"omitempty,oneof=month year"`
}

// ReactivationResult is B-P14's 201.
type ReactivationResult struct {
	PaymentURL        string    `json:"payment_url"`
	Kind              string    `json:"kind"`
	CheckoutSessionID string    `json:"checkout_session_id,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// PaymentMethodView is one B-P15 item: display data only (BIL-PCI-2).
type PaymentMethodView struct {
	ID        string `json:"id"`
	Brand     string `json:"brand"`
	Last4     string `json:"last4"`
	ExpMonth  int    `json:"exp_month"`
	ExpYear   int    `json:"exp_year"`
	IsDefault bool   `json:"is_default"`
}

// PaymentMethodList is B-P15's 200.
type PaymentMethodList struct {
	Items     []PaymentMethodView `json:"items"`
	CanManage bool                `json:"can_manage"`
}

// HostedSession is B-P16's and B-P23's 201.
type HostedSession struct {
	URL       string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InvoiceFilter is B-P17's and B-O4's query.
type InvoiceFilter struct {
	Page
	Status string `form:"status" binding:"omitempty,oneof=draft open paid void uncollectible"`
	Kind   string `form:"kind" binding:"omitempty,oneof=subscription_create subscription_cycle proration token_addon manual"`
}

// InvoiceView is one B-P17 item.
type InvoiceView struct {
	InvoiceID        string     `json:"invoice_id"`
	Number           *string    `json:"number"`
	Kind             string     `json:"kind"`
	Status           string     `json:"status"`
	AmountDue        Money      `json:"amount_due"`
	AmountPaid       Money      `json:"amount_paid"`
	PeriodStart      *time.Time `json:"period_start"`
	PeriodEnd        *time.Time `json:"period_end"`
	IssuedAt         *time.Time `json:"issued_at"`
	PaidAt           *time.Time `json:"paid_at"`
	HostedInvoiceURL *string    `json:"hosted_invoice_url"`
}

// TokenPackCatalog is B-P19's 200.
type TokenPackCatalog struct {
	Resource                  string          `json:"resource"`
	Eligible                  bool            `json:"eligible"`
	IneligibleReason          *string         `json:"ineligible_reason"`
	MonthlyCapRemainingTokens *int64          `json:"monthly_cap_remaining_tokens"`
	Items                     []TokenPackView `json:"items"`
	Terms                     AddonTerms      `json:"terms"`
}

// TokenPackView is one pack.
type TokenPackView struct {
	PackCode string `json:"pack_code"`
	Tokens   int64  `json:"tokens"`
	Price    Money  `json:"price"`
}

// AddonTerms are shown before purchase.
type AddonTerms struct {
	NonRefundable      bool   `json:"non_refundable"`
	ExpiresAfterMonths int    `json:"expires_after_months"`
	UsableAfter        string `json:"usable_after"`
}

// AddonPurchaseRequest is B-P20.
type AddonPurchaseRequest struct {
	PackCode     string `json:"pack_code" binding:"required,max=64"`
	PackQuantity int    `json:"pack_quantity" binding:"required,min=1,max=100"`
	AcceptTerms  bool   `json:"accept_terms"`
}

// HistoryView is one B-P24 row: no operator identity or free text.
type HistoryView struct {
	OccurredAt time.Time `json:"occurred_at"`
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	FromPlan   *string   `json:"from_plan"`
	ToPlan     *string   `json:"to_plan"`
	FromSeats  *int      `json:"from_seats"`
	ToSeats    *int      `json:"to_seats"`
	Cause      string    `json:"cause"`
	Actor      string    `json:"actor"` // you | an administrator | system
}

// BillingContact is B-P25's 200 and B-P26's body and 200.
type BillingContact struct {
	BillingContactName  *string `json:"billing_contact_name" binding:"omitempty,min=1,max=200"`
	BillingContactEmail *string `json:"billing_contact_email" binding:"omitempty,email,max=254"`
	CompanyName         *string `json:"company_name" binding:"omitempty,min=1,max=200"`
	Country             *string `json:"country" binding:"omitempty,iso3166_1_alpha2"`
	TaxID               *string `json:"tax_id" binding:"omitempty,max=64,taxid"`
	RecordVersion       int64   `json:"record_version" binding:"required,min=1"`
}

// TenantAPI is the tenant route surface (§5.4.1).
type TenantAPI interface {
	GetSubscription(ctx context.Context, c Caller, tenantID string) (SubscriptionView, error)                                // B-P1
	StartCheckout(ctx context.Context, c Caller, tenantID string, r CheckoutRequest) (CheckoutResult, error)                 // B-P2
	PreviewPlanChange(ctx context.Context, c Caller, tenantID string, r PlanChangePreviewRequest) (PlanChangePreview, error) // B-P3
	ChangePlan(ctx context.Context, c Caller, tenantID string, r PlanChangeRequest) (PlanChangeResult, error)                // B-P4
	CancelPlanChange(ctx context.Context, c Caller, tenantID string) error                                                   // B-P5
	PreviewSeats(ctx context.Context, c Caller, tenantID string, r SeatPreviewRequest) (SeatPreview, error)                  // B-P6
	PurchaseSeats(ctx context.Context, c Caller, tenantID string, r SeatPurchaseRequest) (PurchaseView, error)               // B-P7
	GetSeatPurchase(ctx context.Context, c Caller, tenantID, purchaseID string) (PurchaseView, error)                        // B-P8
	ListSeatPurchases(ctx context.Context, c Caller, tenantID string, p Page) (List[PurchaseView], error)                    // B-P9
	ScheduleReduction(ctx context.Context, c Caller, tenantID string, r ReductionRequest) (ScheduleView, error)              // B-P10
	CancelReduction(ctx context.Context, c Caller, tenantID string) error                                                    // B-P11
	CancelSubscription(ctx context.Context, c Caller, tenantID string, r CancelRequest) (SubscriptionView, error)            // B-P12
	ResumeSubscription(ctx context.Context, c Caller, tenantID string, r ResumeRequest) (SubscriptionView, error)            // B-P13
	Reactivate(ctx context.Context, c Caller, tenantID string, r ReactivateRequest) (ReactivationResult, error)              // B-P14
	ListPaymentMethods(ctx context.Context, c Caller, tenantID string) (PaymentMethodList, error)                            // B-P15
	CreateSetupSession(ctx context.Context, c Caller, tenantID string) (HostedSession, error)                                // B-P16
	ListInvoices(ctx context.Context, c Caller, tenantID string, f InvoiceFilter) (List[InvoiceView], error)                 // B-P17
	InvoiceDownloadURL(ctx context.Context, c Caller, tenantID, invoiceID string) (string, error)                            // B-P18
	ListTokenPacks(ctx context.Context, c Caller, tenantID string) (TokenPackCatalog, error)                                 // B-P19
	PurchaseTokenAddon(ctx context.Context, c Caller, tenantID string, r AddonPurchaseRequest) (PurchaseView, error)         // B-P20
	GetAddonPurchase(ctx context.Context, c Caller, tenantID, purchaseID string) (PurchaseView, error)                       // B-P21
	ListAddonPurchases(ctx context.Context, c Caller, tenantID string, p Page) (List[PurchaseView], error)                   // B-P22
	AddonPaymentSession(ctx context.Context, c Caller, tenantID, purchaseID string) (HostedSession, error)                   // B-P23
	SubscriptionHistory(ctx context.Context, c Caller, tenantID string, p Page) (List[HistoryView], error)                   // B-P24
	GetBillingContact(ctx context.Context, c Caller, tenantID string) (BillingContact, error)                                // B-P25
	UpdateBillingContact(ctx context.Context, c Caller, tenantID string, r BillingContact) (BillingContact, error)           // B-P26
}

// ── Internal route (§5.4.2) ────────────────────────────────────────────────

// ContactInput is B-I1's optional billing contact.
type ContactInput struct {
	Name  string `json:"name" binding:"omitempty,max=200"`
	Email string `json:"email" binding:"omitempty,email,max=254"`
}

// AuthorizeChargeRequest is B-I1 (unknown fields tolerated).
type AuthorizeChargeRequest struct {
	TenantID         string        `json:"tenant_id" binding:"required,uuid"`
	PlanCode         string        `json:"plan_code" binding:"required,oneof=starter pro enterprise"`
	LicensedSeats    int           `json:"licensed_seats" binding:"required,min=1,max=1000"`
	PaymentMethodRef string        `json:"payment_method_ref" binding:"required,max=255"`
	Interval         string        `json:"interval" binding:"omitempty,oneof=month year"`
	Currency         string        `json:"currency" binding:"omitempty,iso4217"`
	BillingContact   *ContactInput `json:"billing_contact"`
	CompanyName      string        `json:"company_name" binding:"omitempty,max=200"`
	Country          string        `json:"country" binding:"omitempty,iso3166_1_alpha2"`
}

// ChargeResult is B-I1's 201.
type ChargeResult struct {
	BillingSubscriptionID string    `json:"billing_subscription_id"`
	Status                string    `json:"status"`
	PlanCode              string    `json:"plan_code"`
	LicensedSeats         int       `json:"licensed_seats"`
	Interval              string    `json:"interval"`
	CurrentPeriodEnd      time.Time `json:"current_period_end"`
	Provider              string    `json:"provider"`
}

// InternalAPI is the internal route surface.
type InternalAPI interface {
	// AuthorizeAndCharge is B-I1. PAYMENT_ACTION_REQUIRED is returned as a
	// DomainError carrying action, billing_subscription_id and expires_at,
	// and with KeepInFlight so the BFF's retry is processed (§5.4.2 step 5).
	AuthorizeAndCharge(ctx context.Context, c Caller, r AuthorizeChargeRequest) (ChargeResult, error)
}

// ── Operator routes (§5.4.4) ───────────────────────────────────────────────

// OperatorReason is the mandatory reason on every operator mutation.
type OperatorReason struct {
	Reason string `json:"reason" binding:"required,min=10,max=1000"`
}

// StubConversionRequest is B-O1.
type StubConversionRequest struct {
	Plan          string `json:"plan" binding:"required,oneof=starter pro enterprise"`
	Interval      string `json:"interval" binding:"required,oneof=month year"`
	LicensedSeats int    `json:"licensed_seats" binding:"required,min=1,max=1000"`
	Reason        string `json:"reason" binding:"required,min=10,max=1000"`
}

// StubConversionResult is B-O1's 202.
type StubConversionResult struct {
	BillingSubscriptionID string `json:"billing_subscription_id"`
	SagaID                string `json:"saga_id"`
	State                 string `json:"state"`
	Provider              string `json:"provider"`
}

// AdjustmentRequest is B-O2.
type AdjustmentRequest struct {
	Kind        string `json:"kind" binding:"required,oneof=credit debit"`
	Amount      Money  `json:"amount" binding:"required"`
	Description string `json:"description" binding:"required,min=3,max=500"`
	Reason      string `json:"reason" binding:"required,min=10,max=1000"`
}

// AdjustmentResult is B-O2's 201.
type AdjustmentResult struct {
	AdjustmentID string `json:"adjustment_id"`
	Kind         string `json:"kind"`
	Amount       Money  `json:"amount"`
	ProviderRef  string `json:"provider_ref"`
}

// RefundRequest is B-O3.
type RefundRequest struct {
	InvoiceID string `json:"invoice_id" binding:"required,uuid"`
	Amount    Money  `json:"amount" binding:"required"`
	// signup_failed and conversion_reversal are set by B-O9 / B-O8, not here.
	Category string `json:"category" binding:"required,oneof=billing_error duplicate_charge fraud goodwill other"`
	Reason   string `json:"reason" binding:"required,min=10,max=1000"`
}

// RefundResult is B-O3's 201.
type RefundResult struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"`
	Amount   Money  `json:"amount"`
}

// OperatorInvoiceView is one B-O4 item: B-P17's plus links and refunds.
type OperatorInvoiceView struct {
	InvoiceView
	Links   []InvoiceLink `json:"links"`
	Refunds []RefundView  `json:"refunds"`
}

// InvoiceLink is a typed reference from an invoice.
type InvoiceLink struct {
	RefType string `json:"ref_type"`
	RefID   string `json:"ref_id"`
}

// RefundView is a refund on an invoice.
type RefundView struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"`
	Amount   Money  `json:"amount"`
	Category string `json:"category"`
}

// ReconciliationRequest is B-O5.
type ReconciliationRequest struct {
	Provider    string    `json:"provider" binding:"required,oneof=stripe manual"`
	WindowStart time.Time `json:"window_start" binding:"required"`
	WindowEnd   time.Time `json:"window_end" binding:"required,gtfield=WindowStart"`
	AutoRepair  bool      `json:"auto_repair"`
	Reason      string    `json:"reason" binding:"required,min=10,max=1000"`
}

// ReconciliationRunView is B-O5's 202 and B-O6's 200.
type ReconciliationRunView struct {
	RunID    string                  `json:"run_id"`
	Status   string                  `json:"status"`
	Counts   map[string]int          `json:"counts,omitempty"`
	Findings []ReconciliationFinding `json:"findings,omitempty"`
}

// ReconciliationFinding is one B-O6 finding.
type ReconciliationFinding struct {
	TenantID      string          `json:"tenant_id"`
	Kind          string          `json:"kind"`
	BillingValue  json.RawMessage `json:"billing_value"`
	ProviderValue json.RawMessage `json:"provider_value"`
	AutoRepaired  bool            `json:"auto_repaired"`
	RepairNote    string          `json:"repair_note,omitempty"`
}

// OperatorSubscriptionView is B-O7: the subscription plus everything an
// operator needs; ledger rows never carry payloads.
type OperatorSubscriptionView struct {
	SubscriptionView
	Saga                json.RawMessage   `json:"saga"`
	PlanChanges         []PlanChangeView  `json:"plan_changes"`
	SeatPurchases       []PurchaseView    `json:"seat_purchases"`
	SeatChangeSchedules []ScheduleView    `json:"seat_change_schedules"`
	QuotaAddonPurchases []PurchaseView    `json:"quota_addon_purchases"`
	Disputes            []json.RawMessage `json:"disputes"`
	Ledger              []LedgerRowView   `json:"ledger"`
	Account             json.RawMessage   `json:"account"`
}

// LedgerRowView is a ledger excerpt row (B-O7).
type LedgerRowView struct {
	ProviderEventID string    `json:"provider_event_id"`
	EventType       string    `json:"event_type"`
	Status          string    `json:"status"`
	Outcome         *string   `json:"outcome"`
	ReceivedAt      time.Time `json:"received_at"`
}

// ReversalRequest is B-O8.
type ReversalRequest struct {
	DisputeID string `json:"dispute_id" binding:"omitempty,uuid"`
	Refund    bool   `json:"refund"`
	Reason    string `json:"reason" binding:"required,min=10,max=1000"`
}

// ReversalResult is B-O8's 202.
type ReversalResult struct {
	ReversalStatus string          `json:"reversal_status"`
	RPResponse     json.RawMessage `json:"rp_response,omitempty"`
	RefundID       *string         `json:"refund_id"`
}

// SagaActionRequest is B-O9.
type SagaActionRequest struct {
	Action string `json:"action" binding:"required,oneof=retry refund resolved_manually"`
	Reason string `json:"reason" binding:"required,min=10,max=1000"`
}

// SagaView is B-O9's 200.
type SagaView struct {
	SagaID                string     `json:"saga_id"`
	Kind                  string     `json:"kind"`
	State                 string     `json:"state"`
	BillingSubscriptionID string     `json:"billing_subscription_id"`
	NextAttemptAt         *time.Time `json:"next_attempt_at"`
	ConfirmedAt           *time.Time `json:"confirmed_at"`
}

// CollectionPauseRequest is B-O10.
type CollectionPauseRequest struct {
	Paused *bool  `json:"paused" binding:"required"`
	Reason string `json:"reason" binding:"required,min=10,max=1000"`
}

// PlanPriceRequest is B-O11.
type PlanPriceRequest struct {
	Plan            string    `json:"plan" binding:"required,oneof=starter pro enterprise"`
	Interval        string    `json:"interval" binding:"required,oneof=month year"`
	Currency        string    `json:"currency" binding:"required,iso4217"`
	UnitAmountMinor int64     `json:"unit_amount_minor" binding:"required,min=1"`
	Provider        string    `json:"provider" binding:"required,oneof=stripe manual"`
	ProviderPriceID string    `json:"provider_price_id" binding:"required_unless=Provider manual,max=255"`
	EffectiveFrom   time.Time `json:"effective_from" binding:"required"`
	Reason          string    `json:"reason" binding:"required,min=10,max=1000"`
}

// TokenPackRequest is B-O12.
type TokenPackRequest struct {
	PackCode         string    `json:"pack_code" binding:"required,max=64"`
	Tokens           int64     `json:"tokens" binding:"required,min=1"`
	Currency         string    `json:"currency" binding:"required,iso4217"`
	AmountMinor      int64     `json:"amount_minor" binding:"required,min=1"`
	Provider         string    `json:"provider" binding:"required,oneof=stripe manual"`
	ProviderPriceID  string    `json:"provider_price_id" binding:"required_unless=Provider manual,max=255"`
	MonthlyCapTokens *int64    `json:"monthly_cap_tokens" binding:"omitempty,min=1"`
	Active           *bool     `json:"active" binding:"required"`
	EffectiveFrom    time.Time `json:"effective_from" binding:"required"`
	Reason           string    `json:"reason" binding:"required,min=10,max=1000"`
}

// CertificationResult is one §14.7 case result in a B-O14 run.
type CertificationResult struct {
	Capability  string `json:"capability" binding:"required,max=64"`
	Case        string `json:"case" binding:"required,max=64"`
	Passed      bool   `json:"passed"`
	EvidenceRef string `json:"evidence_ref" binding:"required,max=1000"`
}

// CertificationRequest is B-O14: the record file cmd/certify writes.
type CertificationRequest struct {
	Provider           string                `json:"provider" binding:"required,oneof=stripe manual"`
	AdapterVersion     string                `json:"adapter_version" binding:"required,max=200"`
	ProviderAPIVersion string                `json:"provider_api_version" binding:"required,max=100"`
	SuiteVersion       string                `json:"suite_version" binding:"required,max=50"`
	ProrationMode      string                `json:"proration_mode" binding:"required,oneof=native fallback"`
	Capabilities       json.RawMessage       `json:"capabilities" binding:"required"`
	Results            []CertificationResult `json:"results" binding:"required,min=1,dive"`
	ProviderAccountRef string                `json:"provider_account_ref" binding:"required,max=255"`
	RunStartedAt       time.Time             `json:"run_started_at" binding:"required"`
	RunFinishedAt      time.Time             `json:"run_finished_at" binding:"required,gtefield=RunStartedAt"`
	Reason             string                `json:"reason" binding:"required,min=10,max=1000"`
}

// CertificationView is B-O14's 201 and B-O15/B-O17's 200.
type CertificationView struct {
	ID                    string     `json:"id"`
	Provider              string     `json:"provider"`
	AdapterVersion        string     `json:"adapter_version"`
	SuiteVersion          string     `json:"suite_version"`
	Status                string     `json:"status"`
	CertifiedBy           string     `json:"certified_by,omitempty"`
	ReviewedBy            *string    `json:"reviewed_by,omitempty"`
	ReviewedAt            *time.Time `json:"reviewed_at,omitempty"`
	RevokedAt             *time.Time `json:"revoked_at,omitempty"`
	EnabledForNewAccounts bool       `json:"enabled_for_new_accounts"`
}

// ReviewRequest is B-O17.
type ReviewRequest struct {
	Decision string `json:"decision" binding:"required,oneof=approve reject"`
	Reason   string `json:"reason" binding:"required,min=10,max=1000"`
}

// DispositionRequest is B-O16.
type DispositionRequest struct {
	Action string `json:"action" binding:"required,oneof=finalize void"`
	Reason string `json:"reason" binding:"required,min=10,max=1000"`
}

// DispositionResult is B-O16's 200.
type DispositionResult struct {
	InvoiceID   string    `json:"invoice_id"`
	Status      string    `json:"status"`
	Disposition string    `json:"disposition"`
	DisposedBy  string    `json:"disposed_by"`
	DisposedAt  time.Time `json:"disposed_at"`
}

// OperatorAPI is the operator route surface (§5.4.4). Every call is made
// after RequirePlatformOperator; actor = Caller.UserID.
type OperatorAPI interface {
	StubConversion(ctx context.Context, c Caller, tenantID string, r StubConversionRequest) (StubConversionResult, error)          // B-O1
	IssueAdjustment(ctx context.Context, c Caller, tenantID string, r AdjustmentRequest) (AdjustmentResult, error)                 // B-O2
	IssueRefund(ctx context.Context, c Caller, tenantID string, r RefundRequest) (RefundResult, error)                             // B-O3
	ListInvoicesForOperator(ctx context.Context, c Caller, tenantID string, f InvoiceFilter) (List[OperatorInvoiceView], error)    // B-O4
	StartReconciliation(ctx context.Context, c Caller, r ReconciliationRequest) (ReconciliationRunView, error)                     // B-O5
	GetReconciliationRun(ctx context.Context, c Caller, runID string) (ReconciliationRunView, error)                               // B-O6
	GetSubscriptionDetail(ctx context.Context, c Caller, tenantID string) (OperatorSubscriptionView, error)                        // B-O7
	RevertConversion(ctx context.Context, c Caller, tenantID string, r ReversalRequest) (ReversalResult, error)                    // B-O8
	SagaAction(ctx context.Context, c Caller, tenantID, sagaID string, r SagaActionRequest) (SagaView, error)                      // B-O9
	SetCollectionPause(ctx context.Context, c Caller, tenantID string, r CollectionPauseRequest) (SubscriptionView, error)         // B-O10
	AddPlanPrice(ctx context.Context, c Caller, r PlanPriceRequest) (domain.PlanPrice, error)                                      // B-O11
	UpsertTokenPack(ctx context.Context, c Caller, r TokenPackRequest) (json.RawMessage, error)                                    // B-O12
	ReplayWebhookEvent(ctx context.Context, c Caller, provider, providerEventID string, r OperatorReason) error                    // B-O13
	SubmitCertification(ctx context.Context, c Caller, r CertificationRequest) (CertificationView, error)                          // B-O14
	RevokeCertification(ctx context.Context, c Caller, id string, r OperatorReason) (CertificationView, error)                     // B-O15
	DisposeHeldInvoice(ctx context.Context, c Caller, tenantID, invoiceID string, r DispositionRequest) (DispositionResult, error) // B-O16
	ReviewCertification(ctx context.Context, c Caller, id string, r ReviewRequest) (CertificationView, error)                      // B-O17
}
