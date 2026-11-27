package domain

import "time"

// Provider-neutral callbacks (LLD §7.4). Webhooks are verified by the
// adapter, normalised to a PaymentEvent and written to payment_event_ledger;
// they are not bus events (HLD §9.3).

// NormalisedKind is the provider-neutral kind of a callback (§7.4 table).
type NormalisedKind string

// Kinds.
const (
	KindCheckoutCompleted     NormalisedKind = "checkout_completed"
	KindPaymentMethodSet      NormalisedKind = "payment_method_set"
	KindCheckoutExpired       NormalisedKind = "checkout_expired"
	KindInvoiceUpdated        NormalisedKind = "invoice_updated"
	KindInvoicePaid           NormalisedKind = "invoice_paid"
	KindInvoicePaymentFailed  NormalisedKind = "invoice_payment_failed"
	KindPaymentActionRequired NormalisedKind = "payment_action_required"
	KindSubscriptionUpdated   NormalisedKind = "subscription_updated"
	KindSubscriptionEnded     NormalisedKind = "subscription_ended"
	KindScheduleUpdated       NormalisedKind = "schedule_updated"
	KindRefundUpdated         NormalisedKind = "refund_updated"
	KindDisputeOpened         NormalisedKind = "dispute_opened"
	KindDisputeUpdated        NormalisedKind = "dispute_updated"
	KindDisputeClosed         NormalisedKind = "dispute_closed"
	KindFraudWarning          NormalisedKind = "fraud_warning"
	KindCustomerUpdated       NormalisedKind = "customer_updated"
	KindIgnored               NormalisedKind = "ignored"
)

// ChangesState reports whether the kind changes Billing state, so the
// processor re-reads the provider object first (BIL-PAY-4).
func (k NormalisedKind) ChangesState() bool { return k != KindIgnored }

// ProviderObjectKind names the provider object a callback is about.
type ProviderObjectKind string

// Object kinds.
const (
	ObjectCustomer          ProviderObjectKind = "customer"
	ObjectSubscription      ProviderObjectKind = "subscription"
	ObjectInvoice           ProviderObjectKind = "invoice"
	ObjectCheckoutSession   ProviderObjectKind = "checkout_session"
	ObjectSchedule          ProviderObjectKind = "subscription_schedule"
	ObjectRefund            ProviderObjectKind = "refund"
	ObjectDispute           ProviderObjectKind = "dispute"
	ObjectEarlyFraudWarning ProviderObjectKind = "early_fraud_warning"
	ObjectPaymentMethod     ProviderObjectKind = "payment_method"
)

// ObjectKindFor returns the object a kind is about, for re-reading a
// ledger row (the ledger stores the kind and the object id).
func ObjectKindFor(k NormalisedKind) ProviderObjectKind {
	switch k {
	case KindCheckoutCompleted, KindPaymentMethodSet, KindCheckoutExpired:
		return ObjectCheckoutSession
	case KindInvoiceUpdated, KindInvoicePaid, KindInvoicePaymentFailed, KindPaymentActionRequired:
		return ObjectInvoice
	case KindSubscriptionUpdated, KindSubscriptionEnded:
		return ObjectSubscription
	case KindScheduleUpdated:
		return ObjectSchedule
	case KindRefundUpdated:
		return ObjectRefund
	case KindDisputeOpened, KindDisputeUpdated, KindDisputeClosed:
		return ObjectDispute
	case KindFraudWarning:
		return ObjectEarlyFraudWarning
	case KindCustomerUpdated:
		return ObjectCustomer
	default:
		return ""
	}
}

// ProviderObjectRef points at one provider object (ledger provider_object_ref).
type ProviderObjectRef struct {
	Kind ProviderObjectKind
	ID   string
}

// String renders the ref as stored in the ledger.
func (r ProviderObjectRef) String() string { return r.ID }

// PaymentEvent is a verified, normalised provider callback.
type PaymentEvent struct {
	Provider          string
	ProviderEventID   string
	EventType         string // provider type, e.g. invoice.paid
	Kind              NormalisedKind
	Livemode          bool
	ProviderCreatedAt time.Time
	// CustomerRef is the provider customer the object belongs to; the tenant
	// is resolved from it, never from metadata (BIL-PAY-3).
	CustomerRef string
	Object      ProviderObjectRef
	// Metadata is only a cross-check (BIL-PAY-3).
	Metadata map[string]string
	// Payload holds allow-listed fields only; no card data (BIL-PCI-3).
	Payload []byte
}

// ProviderObjectState is the provider's current object, re-read before any
// state change (BIL-PAY-4). Fields carries the allow-listed values the
// handlers need, by name.
type ProviderObjectState struct {
	Ref      ProviderObjectRef
	Status   string
	Metadata map[string]string
	Fields   map[string]any
}
