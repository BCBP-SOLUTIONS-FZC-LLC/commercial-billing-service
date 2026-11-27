package port

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// PaymentProvider is the CD-56 abstraction (LLD §3.4). Every mutating method
// takes an idempotency key the adapter passes to the provider. Amounts are
// domain.Money. No provider type crosses this port (BIL-PCI-1).
type PaymentProvider interface {
	Name() string           // "stripe" | "manual"
	AdapterVersion() string // "<adapter semver>+<provider api version>"; key of the certification record (§3.5)
	Capabilities() ProviderCapabilities

	// Customers and payment methods
	EnsureCustomer(ctx context.Context, in CustomerInput, idemKey string) (CustomerRef, error)
	UpdateCustomer(ctx context.Context, ref CustomerRef, in CustomerInput, idemKey string) error
	CreateHostedCheckout(ctx context.Context, in CheckoutInput, idemKey string) (HostedSession, error)
	// ExpireCheckout makes a hosted session unpayable (a superseded B-P2,
	// §8.1.1). nil when the session is expired, now or already;
	// ErrCheckoutCompleted when the customer already completed it.
	ExpireCheckout(ctx context.Context, providerSessionID string, idemKey string) error
	CreatePaymentMethodSetup(ctx context.Context, in SetupInput, idemKey string) (HostedSession, error)
	ListPaymentMethods(ctx context.Context, ref CustomerRef) ([]PaymentMethodView, error)
	AttachDefaultPaymentMethod(ctx context.Context, ref CustomerRef, pmRef string, idemKey string) error

	// Recurring subscription
	CreateSubscription(ctx context.Context, in SubscriptionInput, idemKey string) (SubscriptionResult, error)
	ConfirmInitialPayment(ctx context.Context, ref SubscriptionRef, idemKey string) (PaymentOutcome, error)
	ChangeQuantity(ctx context.Context, in QuantityChange, idemKey string) (ChangeResult, error)
	ChangePrice(ctx context.Context, in PriceChange, idemKey string) (ChangeResult, error)
	ScheduleAtPeriodEnd(ctx context.Context, in PeriodEndChange, idemKey string) (ScheduleRef, error)
	ReleaseSchedule(ctx context.Context, ref ScheduleRef, idemKey string) error
	SetCancelAtPeriodEnd(ctx context.Context, ref SubscriptionRef, cancel bool, idemKey string) error
	CancelNow(ctx context.Context, ref SubscriptionRef, idemKey string) error
	// PauseCollection: pause holds new invoices as drafts AND stops automatic
	// retries on already-open invoices; resume restarts retries only (§8.10).
	PauseCollection(ctx context.Context, ref SubscriptionRef, pause bool, idemKey string) error

	// Pricing previews and one-off charges
	// PreviewQuantityChange is never called for Proration = fallback (BIL-PROV-3).
	PreviewQuantityChange(ctx context.Context, in QuantityChange) (ProrationPreview, error)
	PreviewPriceChange(ctx context.Context, in PriceChange) (ProrationPreview, error)
	ChargeOneOff(ctx context.Context, in OneOffCharge, idemKey string) (ChargeResult, error)

	// Invoices, refunds
	GetInvoice(ctx context.Context, ref InvoiceRef) (InvoiceView, error)
	InvoicePDFLink(ctx context.Context, ref InvoiceRef) (URLWithExpiry, error)
	PayOpenInvoiceLink(ctx context.Context, ref InvoiceRef) (URLWithExpiry, error)
	VoidInvoice(ctx context.Context, ref InvoiceRef, idemKey string) error
	// DeleteDraftInvoice deletes a draft (never finalised) invoice; the
	// provider voids only finalised ones. An invoice already gone is nil.
	DeleteDraftInvoice(ctx context.Context, ref InvoiceRef) error
	FinalizeInvoice(ctx context.Context, ref InvoiceRef, idemKey string) error // B-O16
	// FindInvoice looks up an invoice Billing started but whose id it did
	// not record (a provider error after the charge, §8.5.1, §8.7); found is
	// false when the provider has none.
	FindInvoice(ctx context.Context, q InvoiceQuery) (ref InvoiceRef, found bool, err error)
	Refund(ctx context.Context, in RefundInput, idemKey string) (RefundResult, error)
	CreditBalance(ctx context.Context, in BalanceAdjustment, idemKey string) (string, error)

	// Price book (B-O11, B-O12): the provider price exists, is active and
	// matches; a mismatch is a PriceMismatch error.
	VerifyPrice(ctx context.Context, priceRef string, want PriceExpectation) error

	// Callbacks and re-read
	VerifyAndNormalise(ctx context.Context, h http.Header, body []byte) (domain.PaymentEvent, error) // ErrInvalidSignature
	FetchCurrent(ctx context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error)

	// Reconciliation
	ListSince(ctx context.Context, kind ReconKind, since time.Time, cursor string) (ReconPage, error)
}

// ProrationMode is how a provider prices mid-period changes.
type ProrationMode string

// Proration modes.
const (
	ProrationNative   ProrationMode = "native"   // provider returns the preview and charges that amount
	ProrationFallback ProrationMode = "fallback" // Billing's calculator (§8.5.3); named in the certification record
)

// ProviderLookup finds the adapter of an existing account's provider. The
// provider registry implements it (§3.5); loaded providers serve running
// work whether or not they are certified (BIL-PROV-2).
type ProviderLookup interface {
	Loaded(name string) (PaymentProvider, bool)
}

// PaymentEventHandler applies one normalised callback to Billing's state,
// using the provider object re-read just before (BIL-PAY-4). It runs inside
// a tenant transaction and returns the ledger outcome ("applied:<what>").
// Return an *IgnoredEvent to record ignored:<reason> (for example a
// transition §8.14 does not allow from Billing's current state).
type PaymentEventHandler interface {
	Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, current domain.ProviderObjectState) (outcome string, err error)
}

// IgnoredEvent marks a callback that needs no change (ledger ignored:<reason>).
type IgnoredEvent struct{ Reason string }

func (e *IgnoredEvent) Error() string { return "ignored: " + e.Reason }

// ParkedEvent marks a callback Billing must not apply and an operator must
// decide (for example a payment on a checkout Billing superseded): the
// ledger row stays failed:<reason> with no retry (§8.15 step 4, BIL-OPS-6).
// The handler's transaction is rolled back.
type ParkedEvent struct{ Reason string }

func (e *ParkedEvent) Error() string { return "parked: " + e.Reason }

// ProviderCapabilities is declared by each adapter and verified by the
// certification suite (§14.7).
type ProviderCapabilities struct {
	RecurringSubscriptions    bool          `json:"RecurringSubscriptions"`
	WebhookRedelivery         bool          `json:"WebhookRedelivery"`
	StableEventIDs            bool          `json:"StableEventIDs"`
	Proration                 ProrationMode `json:"Proration"`
	InvoiceRetrieval          bool          `json:"InvoiceRetrieval"`
	HostedPaymentMethodUpdate bool          `json:"HostedPaymentMethodUpdate"`
	Refunds                   bool          `json:"Refunds"`
	RecurringRetries          bool          `json:"RecurringRetries"`
	OffSessionCharges         bool          `json:"OffSessionCharges"`
	Disputes                  bool          `json:"Disputes"`
	PendingUpdates            bool          `json:"PendingUpdates"`
	Schedules                 bool          `json:"Schedules"`
	CollectionPause           bool          `json:"CollectionPause"`
	HostedCardEntry           bool          `json:"HostedCardEntry"`
	Currencies                []string      `json:"Currencies"`
}

// MissingRequired lists the required capabilities (HLD §10.7.1) the
// declaration lacks; certification fails while any is missing.
func (c ProviderCapabilities) MissingRequired() []string {
	var out []string
	add := func(ok bool, name string) {
		if !ok {
			out = append(out, name)
		}
	}
	add(c.RecurringSubscriptions, "RecurringSubscriptions")
	add(c.WebhookRedelivery, "WebhookRedelivery")
	add(c.StableEventIDs, "StableEventIDs")
	add(c.Proration == ProrationNative || c.Proration == ProrationFallback, "Proration")
	add(c.InvoiceRetrieval, "InvoiceRetrieval")
	add(c.HostedPaymentMethodUpdate, "HostedPaymentMethodUpdate")
	add(c.Refunds, "Refunds")
	add(c.RecurringRetries, "RecurringRetries")
	add(c.OffSessionCharges, "OffSessionCharges")
	add(c.HostedCardEntry, "HostedCardEntry")
	return out
}

// SupportsCurrency reports whether the provider declares the currency.
func (c ProviderCapabilities) SupportsCurrency(code string) bool {
	return slices.Contains(c.Currencies, code)
}

// CustomerRef is a provider customer id.
type CustomerRef struct{ ProviderCustomerID string }

// CustomerInput creates or updates a provider customer. Contact fields are
// personal data; they are never logged.
type CustomerInput struct {
	TenantID                             string
	Currency                             string
	Name, Email, Company, Country, TaxID string
}

// CheckoutPurpose is the purpose of a hosted checkout session.
type CheckoutPurpose string

// Purposes.
const (
	CheckoutConversion   CheckoutPurpose = "conversion"
	CheckoutReactivation CheckoutPurpose = "reactivation"
	CheckoutAddonPayment CheckoutPurpose = "addon_payment"
)

// CheckoutInput starts a provider-hosted checkout.
type CheckoutInput struct {
	Customer   CustomerRef
	Purpose    CheckoutPurpose
	PriceRef   string
	Quantity   int
	Currency   string
	Metadata   map[string]string // tenant_id, billing_subscription_id, purpose, purchase_id
	SuccessURL string
	CancelURL  string
	ExpiresAt  time.Time
}

// SetupInput starts a provider-hosted payment-method setup.
type SetupInput struct {
	Customer              CustomerRef
	SuccessURL, CancelURL string
	ExpiresAt             time.Time
}

// HostedSession is a provider-hosted page (BIL-API-5: URLs come only from here).
type HostedSession struct {
	ProviderSessionID, URL string
	ExpiresAt              time.Time
}

// PaymentMethodView is a non-sensitive payment-method summary (SAQ A).
type PaymentMethodView struct {
	ProviderRef, Brand, Last4 string
	ExpMonth, ExpYear         int
	IsDefault                 bool
}

// SubscriptionInput creates a provider subscription.
type SubscriptionInput struct {
	Customer             CustomerRef
	PriceRef             string
	Quantity             int
	Currency             string
	DefaultPaymentMethod string
	Metadata             map[string]string
}

// SubscriptionRef is a provider subscription id.
type SubscriptionRef struct{ ProviderSubscriptionID string }

// SubscriptionResult is the outcome of CreateSubscription.
type SubscriptionResult struct {
	Ref                    SubscriptionRef
	LatestInvoice          InvoiceRef
	Outcome                PaymentOutcome
	PeriodStart, PeriodEnd time.Time
}

// PaymentStatus is the normalised payment status.
type PaymentStatus string

// Payment statuses.
const (
	PaymentPaid           PaymentStatus = "paid"
	PaymentRequiresAction PaymentStatus = "requires_action"
	PaymentDeclined       PaymentStatus = "declined"
	PaymentPending        PaymentStatus = "pending"
)

// PaymentOutcome is a payment result. ClientSecret is set only for
// requires_action and is never logged (BIL-API-4).
type PaymentOutcome struct {
	Status       PaymentStatus
	ClientSecret string
	DeclineCode  string // mapped, non-sensitive (§17)
}

// QuantityChange changes the seat quantity.
type QuantityChange struct {
	Sub              SubscriptionRef
	From, To         int
	ProrationInstant time.Time
	InvoiceNow       bool
	// KeepIfUnpaid applies the quantity even when the invoice is not paid;
	// the invoice then follows dunning (renewal top-up, §8.6.3, BL-OQ-6).
	KeepIfUnpaid bool
}

// PriceChange changes the price (upgrade now).
type PriceChange struct {
	Sub              SubscriptionRef
	ToPriceRef       string
	ProrationInstant time.Time
}

// PeriodEndChange schedules a price and/or quantity at period end. ToPriceRef
// is always set by the renewal-price rule (BIL-SUB-8).
type PeriodEndChange struct {
	Sub        SubscriptionRef
	ToPriceRef string
	ToQuantity int
	At         time.Time
}

// ChangeResult is the outcome of an immediate change.
type ChangeResult struct {
	Invoice InvoiceRef
	Outcome PaymentOutcome
	Pending bool
}

// ScheduleRef is a provider schedule id.
type ScheduleRef struct{ ProviderScheduleID string }

// ProrationPreview is a provider proration preview.
type ProrationPreview struct {
	Amount                      domain.Money
	ProrationInstant, PeriodEnd time.Time
	RenewalAmount               domain.Money
}

// OneOffCharge charges outside the subscription cycle (token add-ons).
type OneOffCharge struct {
	Customer    CustomerRef
	PriceRef    string
	Quantity    int
	Amount      domain.Money
	Description string
	Metadata    map[string]string
}

// ChargeResult is the outcome of a one-off charge.
type ChargeResult struct {
	Invoice InvoiceRef
	Outcome PaymentOutcome
}

// RefundReason is the provider-facing refund reason.
type RefundReason string

// Refund reasons.
const (
	RefundReasonDuplicate           RefundReason = "duplicate"
	RefundReasonFraudulent          RefundReason = "fraudulent"
	RefundReasonRequestedByCustomer RefundReason = "requested_by_customer"
)

// RefundInput refunds an invoice's payment.
type RefundInput struct {
	Invoice InvoiceRef
	Amount  domain.Money
	Reason  RefundReason
}

// RefundResult is a provider refund.
type RefundResult struct {
	ProviderRefundID string
	Status           string // pending | succeeded | failed
}

// BalanceAdjustment is an operator credit or debit on the customer balance.
type BalanceAdjustment struct {
	Customer    CustomerRef
	Amount      domain.Money // negative = credit
	Description string
}

// InvoiceRef is a provider invoice id.
type InvoiceRef struct{ ProviderInvoiceID string }

// InvoiceQuery selects the invoice of one purchase. A one-off invoice
// (token add-on) is matched on its metadata purchase_id among the
// customer's invoices; the proration invoice of an immediate change (seats,
// upgrade) on the subscription, billing reason "subscription update" and
// Amount, with a charged line for Quantity (a seat purchase's seats after)
// or PriceRef (an upgrade's target price) when set. Only invoices created
// at or after CreatedSince are considered.
type InvoiceQuery struct {
	Customer     CustomerRef
	Subscription SubscriptionRef
	PurchaseID   string
	Amount       *domain.Money
	Quantity     int
	PriceRef     string
	CreatedSince time.Time
}

// InvoiceView is a provider invoice.
type InvoiceView struct {
	Ref                                      InvoiceRef
	Number, Status                           string
	Kind                                     domain.InvoiceKind
	AmountDue, AmountPaid                    domain.Money
	PeriodStart, PeriodEnd, IssuedAt, PaidAt *time.Time
	HostedURL                                string
	Metadata                                 map[string]string
}

// PriceExpectation is what a price-book row says about a provider price.
// Interval is "" for a one-off price (token packs).
type PriceExpectation struct {
	Amount   domain.Money
	Interval string // month | year | ""
}

// URLWithExpiry is a short-lived provider URL.
type URLWithExpiry struct {
	URL       string
	ExpiresAt time.Time
}

// ReconKind is what reconciliation lists (§8.13).
type ReconKind string

// Reconciliation kinds.
const (
	ReconSubscriptions ReconKind = "subscriptions"
	ReconInvoices      ReconKind = "invoices"
	ReconEvents        ReconKind = "events"
)

// ReconPage is one page of a reconciliation listing.
type ReconPage struct {
	Items      []domain.ProviderObjectState
	NextCursor string // "" at the end
}

// Adapter errors (§3.4). Adapters map provider errors to these; the app
// layer never inspects provider error types.
var (
	ErrProviderUnavailable = errors.New("payment provider unavailable")
	ErrInvalidSignature    = errors.New("invalid webhook signature")
	ErrUnsupported         = errors.New("operation not supported by this provider")
	ErrIdempotencyMismatch = errors.New("provider idempotency key reused with different parameters")
	ErrNotFound            = errors.New("provider object not found")
	ErrPriceMismatch       = errors.New("provider price does not match the price book row")
	ErrCheckoutCompleted   = errors.New("hosted checkout session already completed")
)

// ErrDeclined is a declined payment with its mapped, non-sensitive code.
type ErrDeclined struct{ Code string }

func (e ErrDeclined) Error() string { return "payment declined: " + e.Code }

// ErrActionRequired is a payment needing customer authentication (3-D
// Secure). ClientSecret goes only to the caller that started the payment.
type ErrActionRequired struct{ ClientSecret string }

func (e ErrActionRequired) Error() string { return "payment requires customer action" }

// ProviderNotEnabled is 503 PROVIDER_NOT_ENABLED (BIL-PROV-1). The registry
// builds it here so it depends only on port (LLD §3.2).
func ProviderNotEnabled(provider, why string) error {
	return domain.NewError(domain.ErrProviderNotEnabled, "payment provider "+provider+" "+why)
}

// ProviderErrorToDomain maps an adapter error to the §17 domain error.
func ProviderErrorToDomain(err error) error {
	if err == nil {
		return nil
	}
	var declined ErrDeclined
	var action ErrActionRequired
	switch {
	case errors.As(err, &declined):
		return domain.NewError(domain.ErrPaymentDeclined, "payment declined").With("decline_reason", declined.Code)
	case errors.As(err, &action):
		return domain.NewError(domain.ErrPaymentActionRequired, "customer authentication required").
			With("action", map[string]string{"type": "3ds", "client_secret": action.ClientSecret})
	case errors.Is(err, ErrProviderUnavailable):
		return &domain.DomainError{Code: domain.ErrProviderUnavailable, Message: "payment provider unavailable", Cause: err}
	case errors.Is(err, ErrUnsupported):
		return &domain.DomainError{Code: domain.ErrProviderOperationUnsupported, Message: "not supported by the account's payment provider", Cause: err}
	case errors.Is(err, ErrPriceMismatch):
		return &domain.DomainError{Code: domain.ErrProviderPriceMismatch, Message: err.Error(), Cause: err}
	case errors.Is(err, ErrNotFound):
		return &domain.DomainError{Code: domain.ErrProviderPriceMismatch, Message: "the provider object does not exist", Cause: err}
	case errors.Is(err, ErrInvalidSignature):
		return &domain.DomainError{Code: domain.ErrInvalidSignature, Message: "invalid webhook signature", Cause: err}
	}
	// ErrIdempotencyMismatch and anything unclassified are bugs (paged).
	return &domain.DomainError{Code: domain.ErrInternal, Message: fmt.Sprintf("payment provider error: %v", err), Cause: err}
}
