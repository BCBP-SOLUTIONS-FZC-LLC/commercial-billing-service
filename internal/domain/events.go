package domain

import (
	"errors"
	"fmt"
	"time"
)

// Payload types of billing.events (LLD §7.3). Every payload carries
// tenant_id in data as well as in the envelope (D-1, HLD v0.65 §9.4).
// Builders validate the rules the JSON Schemas state, so an invalid payload
// is never enqueued. The envelope itself is platform-events' (Phase 4).

// EventType is a billing.events type name.
type EventType string

// Produced event types (§7.3.2). BillingPeriodRolled is not emitted.
const (
	EventTenantPlanChanged           EventType = "TenantPlanChanged"
	EventTenantSeatsChanged          EventType = "TenantSeatsChanged"
	EventTenantPaymentPastDue        EventType = "TenantPaymentPastDue"
	EventTenantSubscriptionCancelled EventType = "TenantSubscriptionCancelled"
	EventTenantReactivated           EventType = "TenantReactivated"
	EventTenantQuotaAddOnChanged     EventType = "TenantQuotaAddOnChanged"
)

// ProducedEventTypes lists the six registered types.
var ProducedEventTypes = []EventType{
	EventTenantPlanChanged, EventTenantSeatsChanged, EventTenantPaymentPastDue,
	EventTenantSubscriptionCancelled, EventTenantReactivated, EventTenantQuotaAddOnChanged,
}

// ErrInvalidPayload wraps every payload validation failure.
var ErrInvalidPayload = errors.New("invalid event payload")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPayload, fmt.Sprintf(format, a...))
}

// TenantPlanChanged (§7.3.3).
type TenantPlanChanged struct {
	TenantID              string              `json:"tenant_id"`
	Plan                  PlanCode            `json:"plan"`
	PreviousPlan          PlanCode            `json:"previous_plan"`
	Direction             PlanChangeDirection `json:"direction,omitempty"`
	EffectiveAt           time.Time           `json:"effective_at"`
	BillingRef            string              `json:"billing_ref"`
	BillingSubscriptionID string              `json:"billing_subscription_id,omitempty"`
}

// Validate applies the schema rules.
func (p TenantPlanChanged) Validate() error {
	if p.TenantID == "" || p.BillingRef == "" || p.EffectiveAt.IsZero() {
		return invalid("TenantPlanChanged: tenant_id, billing_ref and effective_at are required")
	}
	if !p.Plan.Valid() || !p.PreviousPlan.Valid() {
		return invalid("TenantPlanChanged: plan and previous_plan must be starter|pro|enterprise")
	}
	if p.Direction != "" && p.Direction != DirectionUpgrade && p.Direction != DirectionDowngrade {
		return invalid("TenantPlanChanged: direction must be upgrade|downgrade")
	}
	return nil
}

// SeatsChangeReason is TenantSeatsChanged.reason.
type SeatsChangeReason string

// Reasons (§7.3.4).
const (
	SeatsConversion   SeatsChangeReason = "conversion"
	SeatsPurchase     SeatsChangeReason = "purchase"
	SeatsRenewal      SeatsChangeReason = "renewal"
	SeatsRenewalTopUp SeatsChangeReason = "renewal_topup"
	SeatsReactivation SeatsChangeReason = "reactivation"
	SeatsResync       SeatsChangeReason = "resync"
)

// TenantSeatsChanged (§7.3.4). licensed_seats is an absolute snapshot.
type TenantSeatsChanged struct {
	TenantID              string            `json:"tenant_id"`
	LicensedSeats         int               `json:"licensed_seats"`
	PreviousLicensedSeats *int              `json:"previous_licensed_seats"`
	Reason                SeatsChangeReason `json:"reason"`
	BillingRef            *string           `json:"billing_ref"`
	BillingSubscriptionID string            `json:"billing_subscription_id,omitempty"`
	// Additive (BL-OQ-6): set together, purchase only (BIL-SEAT-5).
	DeferredAmountMinor *int64 `json:"deferred_amount_minor,omitempty"`
	DeferralReason      string `json:"deferral_reason,omitempty"`
}

// Validate applies the schema rules.
func (p TenantSeatsChanged) Validate() error {
	if p.TenantID == "" {
		return invalid("TenantSeatsChanged: tenant_id is required")
	}
	if p.LicensedSeats < 1 || p.LicensedSeats > MaxLicensedSeats {
		return invalid("TenantSeatsChanged: licensed_seats must be 1–%d", MaxLicensedSeats)
	}
	if p.PreviousLicensedSeats != nil && *p.PreviousLicensedSeats < 0 {
		return invalid("TenantSeatsChanged: previous_licensed_seats must be ≥ 0")
	}
	switch p.Reason {
	case SeatsConversion, SeatsPurchase, SeatsRenewal, SeatsRenewalTopUp, SeatsReactivation, SeatsResync:
	default:
		return invalid("TenantSeatsChanged: unknown reason %q", p.Reason)
	}
	hasAmount, hasReason := p.DeferredAmountMinor != nil, p.DeferralReason != ""
	if hasAmount != hasReason {
		return invalid("TenantSeatsChanged: deferred_amount_minor and deferral_reason go together")
	}
	if hasAmount {
		if p.Reason != SeatsPurchase {
			return invalid("TenantSeatsChanged: a deferral is only reported on reason=purchase")
		}
		if *p.DeferredAmountMinor < 1 || p.DeferralReason != DeferralBelowProviderMinimum {
			return invalid("TenantSeatsChanged: deferred_amount_minor ≥ 1 with deferral_reason below_provider_minimum")
		}
	}
	return nil
}

// TenantPaymentPastDue (§7.3.5).
type TenantPaymentPastDue struct {
	TenantID     string     `json:"tenant_id"`
	InvoiceRef   string     `json:"invoice_ref"` // Billing invoice_id, never a provider id
	AmountDue    *Money     `json:"amount_due,omitempty"`
	PastDueSince time.Time  `json:"past_due_since"`
	NextRetryAt  *time.Time `json:"next_retry_at"`
}

// Validate applies the schema rules.
func (p TenantPaymentPastDue) Validate() error {
	if p.TenantID == "" || p.InvoiceRef == "" || p.PastDueSince.IsZero() {
		return invalid("TenantPaymentPastDue: tenant_id, invoice_ref and past_due_since are required")
	}
	if p.AmountDue != nil && !ValidCurrency(p.AmountDue.Currency) {
		return invalid("TenantPaymentPastDue: amount_due.currency must be ISO 4217")
	}
	return nil
}

// TenantSubscriptionCancelled (§7.3.5).
type TenantSubscriptionCancelled struct {
	TenantID    string       `json:"tenant_id"`
	CancelledAt time.Time    `json:"cancelled_at"`
	Reason      CancelReason `json:"reason"`
}

// Validate applies the schema rules: reason is one of the four the schema
// allows (offboarded and signup_failed end a subscription without this event).
func (p TenantSubscriptionCancelled) Validate() error {
	if p.TenantID == "" || p.CancelledAt.IsZero() {
		return invalid("TenantSubscriptionCancelled: tenant_id and cancelled_at are required")
	}
	switch p.Reason {
	case CancelDunningExhausted, CancelCustomer, CancelAutoRenewOff, CancelOperatorReversal:
		return nil
	case CancelOffboarded, CancelSignupFailed:
		// These end a subscription without a cancellation event (§8.8.4, §8.3).
		return invalid("TenantSubscriptionCancelled: reason %q ends a subscription without this event", p.Reason)
	}
	return invalid("TenantSubscriptionCancelled: reason %q not allowed", p.Reason)
}

// ReactivationSource is the only TenantReactivated.source Billing sends.
const ReactivationSource = "billing"

// TenantReactivated (§7.3.5). It carries no reason field.
type TenantReactivated struct {
	TenantID       string             `json:"tenant_id"`
	Source         string             `json:"source"`
	ReactivatedAt  time.Time          `json:"reactivated_at"`
	PreviousStatus SubscriptionStatus `json:"previous_status,omitempty"`
}

// Validate applies the schema rules.
func (p TenantReactivated) Validate() error {
	if p.TenantID == "" || p.ReactivatedAt.IsZero() {
		return invalid("TenantReactivated: tenant_id and reactivated_at are required")
	}
	if p.Source != ReactivationSource {
		return invalid("TenantReactivated: source must be %q", ReactivationSource)
	}
	if p.PreviousStatus != "" && p.PreviousStatus != StatusPastDue && p.PreviousStatus != StatusCancelled {
		return invalid("TenantReactivated: previous_status must be past_due|cancelled")
	}
	return nil
}

// AddonChangeReason is TenantQuotaAddOnChanged.reason: Billing-owned
// commercial reasons only (BL-OQ-20).
type AddonChangeReason string

// Reasons (§7.3.6).
const (
	AddonPurchase AddonChangeReason = "purchase"
	AddonRefund   AddonChangeReason = "refund"
)

// ResourceLLMTokens is the only resource Billing emits in v1 (CD-55).
const ResourceLLMTokens = "llm_tokens"

// TenantQuotaAddOnChanged (§7.3.6).
type TenantQuotaAddOnChanged struct {
	TenantID     string            `json:"tenant_id"`
	Resource     string            `json:"resource"`
	Delta        int64             `json:"delta"`
	Reason       AddonChangeReason `json:"reason"`
	BillingRef   string            `json:"billing_ref"`
	PurchaseRef  string            `json:"purchase_ref,omitempty"`
	PurchasedAt  *time.Time        `json:"purchased_at,omitempty"`
	PackCode     string            `json:"pack_code,omitempty"`
	PackQuantity int               `json:"pack_quantity,omitempty"`
}

// Validate applies the schema rules and BIL-ADD-2 / BIL-ADD-4:
// a purchase adds tokens (delta > 0) with purchased_at; a refund reverses
// them (delta < 0) and names the purchase it reverses.
func (p TenantQuotaAddOnChanged) Validate() error {
	if p.TenantID == "" || p.BillingRef == "" {
		return invalid("TenantQuotaAddOnChanged: tenant_id and billing_ref are required")
	}
	if p.Resource != ResourceLLMTokens {
		return invalid("TenantQuotaAddOnChanged: Billing emits resource llm_tokens only in v1")
	}
	switch p.Reason {
	case AddonPurchase:
		if p.Delta <= 0 || p.PurchasedAt == nil {
			return invalid("TenantQuotaAddOnChanged purchase: delta > 0 and purchased_at are required")
		}
		if p.PurchaseRef != "" {
			return invalid("TenantQuotaAddOnChanged purchase: purchase_ref is refund-only")
		}
	case AddonRefund:
		if p.Delta >= 0 || p.PurchaseRef == "" {
			return invalid("TenantQuotaAddOnChanged refund: delta < 0 and purchase_ref are required")
		}
		// Entitlements decodes purchase_ref as a UUID and its consumed schema
		// declares format uuid (contract test, LLD §14.4): canonical form only.
		if !canonicalUUID(p.PurchaseRef) {
			return invalid("TenantQuotaAddOnChanged refund: purchase_ref must be a UUID")
		}
	default:
		return invalid("TenantQuotaAddOnChanged: reason must be purchase|refund")
	}
	if p.PackQuantity < 0 {
		return invalid("TenantQuotaAddOnChanged: pack_quantity must be ≥ 1 when set")
	}
	return nil
}

// EventPayload is implemented by every produced payload: its type name, the
// tenant it is about (data.tenant_id, equal to the envelope's), and its
// schema rules.
type EventPayload interface {
	EventType() EventType
	Tenant() string
	Validate() error
}

// EventType implementations.
func (TenantPlanChanged) EventType() EventType           { return EventTenantPlanChanged }
func (TenantSeatsChanged) EventType() EventType          { return EventTenantSeatsChanged }
func (TenantPaymentPastDue) EventType() EventType        { return EventTenantPaymentPastDue }
func (TenantSubscriptionCancelled) EventType() EventType { return EventTenantSubscriptionCancelled }
func (TenantReactivated) EventType() EventType           { return EventTenantReactivated }
func (TenantQuotaAddOnChanged) EventType() EventType     { return EventTenantQuotaAddOnChanged }

// Tenant implementations.
func (p TenantPlanChanged) Tenant() string           { return p.TenantID }
func (p TenantSeatsChanged) Tenant() string          { return p.TenantID }
func (p TenantPaymentPastDue) Tenant() string        { return p.TenantID }
func (p TenantSubscriptionCancelled) Tenant() string { return p.TenantID }
func (p TenantReactivated) Tenant() string           { return p.TenantID }
func (p TenantQuotaAddOnChanged) Tenant() string     { return p.TenantID }

// canonicalUUID reports whether s is a UUID in canonical 8-4-4-4-12 hex form
// (standard library only, LLD §3.2; C-8 rev 0.73).
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			hex := '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
			if !hex {
				return false
			}
		}
	}
	return true
}
