// Package manual is the Phase 1 operator-stub payment adapter (LLD §3.4,
// §8.1.4). It makes no external call: CreateSubscription records a paid
// subscription immediately; every other mutating method returns
// port.ErrUnsupported (→ 409 PROVIDER_OPERATION_UNSUPPORTED). Invoices are
// raised outside the product in Phase 1.
//
// It is exempt from certification (§3.5); migration 000004 inserts its
// exempt record for exactly AdapterVersion.
package manual

import (
	"context"
	"net/http"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Name is the provider name stored in billing_accounts.provider.
const Name = "manual"

// AdapterVersion must equal the version in migration 000004's exempt record.
const AdapterVersion = "1.0.0+manual"

// Adapter implements port.PaymentProvider for the operator stub.
type Adapter struct {
	now func() time.Time
}

var _ port.PaymentProvider = (*Adapter)(nil)

// New returns the adapter.
func New() *Adapter { return &Adapter{now: time.Now} }

// Name returns "manual".
func (a *Adapter) Name() string { return Name }

// AdapterVersion returns the exempt record's version.
func (a *Adapter) AdapterVersion() string { return AdapterVersion }

// Capabilities declares nothing certified: the stub supports no provider
// operation beyond recording a subscription.
func (a *Adapter) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{Currencies: []string{"USD", "AED"}}
}

// EnsureCustomer has no provider customer for manual accounts
// (chk_ba_customer allows NULL for manual).
func (a *Adapter) EnsureCustomer(context.Context, port.CustomerInput, string) (port.CustomerRef, error) {
	return port.CustomerRef{}, nil
}

// CreateSubscription records the stub subscription as paid now, for a
// one-month or one-year period from now (§8.1.4 step 2).
func (a *Adapter) CreateSubscription(_ context.Context, in port.SubscriptionInput, _ string) (port.SubscriptionResult, error) {
	start := a.now().UTC().Truncate(time.Second)
	end := start.AddDate(0, 1, 0)
	if in.Metadata["interval"] == string(domain.IntervalYear) {
		end = start.AddDate(1, 0, 0)
	}
	return port.SubscriptionResult{
		Outcome:     port.PaymentOutcome{Status: port.PaymentPaid},
		PeriodStart: start,
		PeriodEnd:   end,
	}, nil
}

// Everything below is unsupported for manual accounts.

func (a *Adapter) UpdateCustomer(context.Context, port.CustomerRef, port.CustomerInput, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) CreateHostedCheckout(context.Context, port.CheckoutInput, string) (port.HostedSession, error) {
	return port.HostedSession{}, port.ErrUnsupported
}
func (a *Adapter) CreatePaymentMethodSetup(context.Context, port.SetupInput, string) (port.HostedSession, error) {
	return port.HostedSession{}, port.ErrUnsupported
}
func (a *Adapter) ListPaymentMethods(context.Context, port.CustomerRef) ([]port.PaymentMethodView, error) {
	return nil, nil // no payment methods are held for manual accounts
}
func (a *Adapter) AttachDefaultPaymentMethod(context.Context, port.CustomerRef, string, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) ConfirmInitialPayment(context.Context, port.SubscriptionRef, string) (port.PaymentOutcome, error) {
	return port.PaymentOutcome{}, port.ErrUnsupported
}
func (a *Adapter) ChangeQuantity(context.Context, port.QuantityChange, string) (port.ChangeResult, error) {
	return port.ChangeResult{}, port.ErrUnsupported
}
func (a *Adapter) ChangePrice(context.Context, port.PriceChange, string) (port.ChangeResult, error) {
	return port.ChangeResult{}, port.ErrUnsupported
}
func (a *Adapter) ScheduleAtPeriodEnd(context.Context, port.PeriodEndChange, string) (port.ScheduleRef, error) {
	return port.ScheduleRef{}, port.ErrUnsupported
}
func (a *Adapter) ReleaseSchedule(context.Context, port.ScheduleRef, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) SetCancelAtPeriodEnd(context.Context, port.SubscriptionRef, bool, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) CancelNow(context.Context, port.SubscriptionRef, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) PauseCollection(context.Context, port.SubscriptionRef, bool, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) PreviewQuantityChange(context.Context, port.QuantityChange) (port.ProrationPreview, error) {
	return port.ProrationPreview{}, port.ErrUnsupported
}
func (a *Adapter) PreviewPriceChange(context.Context, port.PriceChange) (port.ProrationPreview, error) {
	return port.ProrationPreview{}, port.ErrUnsupported
}
func (a *Adapter) ChargeOneOff(context.Context, port.OneOffCharge, string) (port.ChargeResult, error) {
	return port.ChargeResult{}, port.ErrUnsupported
}
func (a *Adapter) GetInvoice(context.Context, port.InvoiceRef) (port.InvoiceView, error) {
	return port.InvoiceView{}, port.ErrUnsupported
}
func (a *Adapter) InvoicePDFLink(context.Context, port.InvoiceRef) (port.URLWithExpiry, error) {
	return port.URLWithExpiry{}, port.ErrUnsupported
}
func (a *Adapter) PayOpenInvoiceLink(context.Context, port.InvoiceRef) (port.URLWithExpiry, error) {
	return port.URLWithExpiry{}, port.ErrUnsupported
}
func (a *Adapter) VoidInvoice(context.Context, port.InvoiceRef, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) FinalizeInvoice(context.Context, port.InvoiceRef, string) error {
	return port.ErrUnsupported
}
func (a *Adapter) Refund(context.Context, port.RefundInput, string) (port.RefundResult, error) {
	return port.RefundResult{}, port.ErrUnsupported
}
func (a *Adapter) CreditBalance(context.Context, port.BalanceAdjustment, string) (string, error) {
	return "", port.ErrUnsupported
}

// VerifyAndNormalise: the manual provider sends no callbacks; B-W1 answers
// 404 for it, and any call here is refused as unsigned.
func (a *Adapter) VerifyAndNormalise(context.Context, http.Header, []byte) (domain.PaymentEvent, error) {
	return domain.PaymentEvent{}, port.ErrInvalidSignature
}
func (a *Adapter) FetchCurrent(context.Context, domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	return domain.ProviderObjectState{}, port.ErrUnsupported
}
func (a *Adapter) ListSince(context.Context, port.ReconKind, time.Time, string) (port.ReconPage, error) {
	return port.ReconPage{}, nil // nothing to reconcile
}
