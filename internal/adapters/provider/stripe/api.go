package stripe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	stripego "github.com/stripe/stripe-go/v87"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Mapping of LLD §18.8. Every mutating call carries the caller's
// idempotency key.

// ── Customers and payment methods ──────────────────────────────────────

// EnsureCustomer creates the provider customer (metadata.tenant_id).
func (a *Adapter) EnsureCustomer(ctx context.Context, in port.CustomerInput, idemKey string) (port.CustomerRef, error) {
	p := &stripego.CustomerCreateParams{Params: idem(idemKey)}
	p.Metadata = map[string]string{"tenant_id": in.TenantID}
	setIf(&p.Name, in.Name)
	setIf(&p.Email, in.Email)
	setIf(&p.TestClock, a.cfg.TestClock)
	c, err := call(ctx, a, func() (*stripego.Customer, error) { return a.sc.V1Customers.Create(ctx, p) })
	if err != nil {
		return port.CustomerRef{}, err
	}
	return port.CustomerRef{ProviderCustomerID: c.ID}, nil
}

// UpdateCustomer updates the billing contact on the customer (B-P26).
func (a *Adapter) UpdateCustomer(ctx context.Context, ref port.CustomerRef, in port.CustomerInput, idemKey string) error {
	p := &stripego.CustomerUpdateParams{Params: idem(idemKey)}
	p.Name = stripego.String(in.Name)
	p.Email = stripego.String(in.Email)
	_, err := call(ctx, a, func() (*stripego.Customer, error) { return a.sc.V1Customers.Update(ctx, ref.ProviderCustomerID, p) })
	return err
}

// CreateHostedCheckout starts a Checkout Session: mode=subscription for
// conversion and reactivation, mode=payment for an add-on payment page.
func (a *Adapter) CreateHostedCheckout(ctx context.Context, in port.CheckoutInput, idemKey string) (port.HostedSession, error) {
	p := &stripego.CheckoutSessionCreateParams{Params: idem(idemKey)}
	p.Customer = stripego.String(in.Customer.ProviderCustomerID)
	p.SuccessURL = stripego.String(in.SuccessURL)
	p.CancelURL = stripego.String(in.CancelURL)
	p.Metadata = in.Metadata
	if !in.ExpiresAt.IsZero() {
		p.ExpiresAt = stripego.Int64(in.ExpiresAt.Unix())
	}
	p.LineItems = []*stripego.CheckoutSessionCreateLineItemParams{{Price: stripego.String(in.PriceRef), Quantity: stripego.Int64(int64(in.Quantity))}}
	if in.Purpose == port.CheckoutAddonPayment {
		p.Mode = stripego.String(string(stripego.CheckoutSessionModePayment))
	} else {
		p.Mode = stripego.String(string(stripego.CheckoutSessionModeSubscription))
		p.SubscriptionData = &stripego.CheckoutSessionCreateSubscriptionDataParams{Metadata: in.Metadata}
	}
	s, err := call(ctx, a, func() (*stripego.CheckoutSession, error) { return a.sc.V1CheckoutSessions.Create(ctx, p) })
	if err != nil {
		return port.HostedSession{}, err
	}
	return port.HostedSession{ProviderSessionID: s.ID, URL: s.URL, ExpiresAt: time.Unix(s.ExpiresAt, 0).UTC()}, nil
}

// ExpireCheckout expires an open Checkout Session so it can no longer be
// paid (a superseded B-P2, LLD §8.1.1). Stripe refuses to expire a session
// that is not open, so on any refusal the session is re-read: expired is
// success, complete is port.ErrCheckoutCompleted.
func (a *Adapter) ExpireCheckout(ctx context.Context, providerSessionID string, idemKey string) error {
	p := &stripego.CheckoutSessionExpireParams{Params: idem(idemKey)}
	_, err := call(ctx, a, func() (*stripego.CheckoutSession, error) {
		return a.sc.V1CheckoutSessions.Expire(ctx, providerSessionID, p)
	})
	if err == nil || errors.Is(err, port.ErrProviderUnavailable) {
		return err
	}
	s, rerr := call(ctx, a, func() (*stripego.CheckoutSession, error) {
		return a.sc.V1CheckoutSessions.Retrieve(ctx, providerSessionID, nil)
	})
	switch {
	case rerr != nil:
		return rerr
	case s.Status == stripego.CheckoutSessionStatusExpired:
		return nil
	case s.Status == stripego.CheckoutSessionStatusComplete:
		return port.ErrCheckoutCompleted
	}
	return err
}

// CreatePaymentMethodSetup starts a Checkout Session in setup mode (B-P16);
// card entry stays on Stripe (SAQ A).
func (a *Adapter) CreatePaymentMethodSetup(ctx context.Context, in port.SetupInput, idemKey string) (port.HostedSession, error) {
	p := &stripego.CheckoutSessionCreateParams{Params: idem(idemKey)}
	p.Mode = stripego.String(string(stripego.CheckoutSessionModeSetup))
	p.Customer = stripego.String(in.Customer.ProviderCustomerID)
	p.SuccessURL = stripego.String(in.SuccessURL)
	p.CancelURL = stripego.String(in.CancelURL)
	p.Currency = stripego.String("usd")
	if !in.ExpiresAt.IsZero() {
		p.ExpiresAt = stripego.Int64(in.ExpiresAt.Unix())
	}
	s, err := call(ctx, a, func() (*stripego.CheckoutSession, error) { return a.sc.V1CheckoutSessions.Create(ctx, p) })
	if err != nil {
		return port.HostedSession{}, err
	}
	return port.HostedSession{ProviderSessionID: s.ID, URL: s.URL, ExpiresAt: time.Unix(s.ExpiresAt, 0).UTC()}, nil
}

// ListPaymentMethods reads card summaries live from Stripe (B-P15).
func (a *Adapter) ListPaymentMethods(ctx context.Context, ref port.CustomerRef) ([]port.PaymentMethodView, error) {
	if err := a.limiter.wait(ctx); err != nil {
		return nil, err
	}
	cust, err := a.sc.V1Customers.Retrieve(ctx, ref.ProviderCustomerID, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	def := ""
	if cust.InvoiceSettings != nil && cust.InvoiceSettings.DefaultPaymentMethod != nil {
		def = cust.InvoiceSettings.DefaultPaymentMethod.ID
	}
	lp := &stripego.CustomerListPaymentMethodsParams{Customer: stripego.String(ref.ProviderCustomerID)}
	lp.Type = stripego.String(string(stripego.PaymentMethodTypeCard))
	var out []port.PaymentMethodView
	for pm, err := range a.sc.V1Customers.ListPaymentMethods(ctx, lp).All(ctx) {
		if err != nil {
			return nil, mapErr(err)
		}
		v := port.PaymentMethodView{ProviderRef: pm.ID, IsDefault: pm.ID == def}
		if pm.Card != nil {
			v.Brand, v.Last4 = string(pm.Card.Brand), pm.Card.Last4
			v.ExpMonth, v.ExpYear = int(pm.Card.ExpMonth), int(pm.Card.ExpYear)
		}
		out = append(out, v)
	}
	return out, nil
}

// AttachDefaultPaymentMethod attaches a payment method and makes it the
// customer's default for invoices.
func (a *Adapter) AttachDefaultPaymentMethod(ctx context.Context, ref port.CustomerRef, pmRef string, idemKey string) error {
	ap := &stripego.PaymentMethodAttachParams{Params: idem(idemKey + ":attach")}
	ap.Customer = stripego.String(ref.ProviderCustomerID)
	if _, err := call(ctx, a, func() (*stripego.PaymentMethod, error) { return a.sc.V1PaymentMethods.Attach(ctx, pmRef, ap) }); err != nil {
		return err
	}
	up := &stripego.CustomerUpdateParams{Params: idem(idemKey + ":default")}
	up.InvoiceSettings = &stripego.CustomerUpdateInvoiceSettingsParams{DefaultPaymentMethod: stripego.String(pmRef)}
	if _, err := call(ctx, a, func() (*stripego.Customer, error) { return a.sc.V1Customers.Update(ctx, ref.ProviderCustomerID, up) }); err != nil {
		return err
	}
	// A subscription's own default (set by hosted checkout) overrides the
	// customer's, so the live subscriptions get the new method too (B-P16:
	// "the provider's next retry uses it").
	lp := &stripego.SubscriptionListParams{Customer: stripego.String(ref.ProviderCustomerID)}
	for sub, err := range a.sc.V1Subscriptions.List(ctx, lp).All(ctx) {
		if err != nil {
			return mapErr(err)
		}
		if sub.Status != stripego.SubscriptionStatusActive && sub.Status != stripego.SubscriptionStatusPastDue {
			continue
		}
		sp := &stripego.SubscriptionUpdateParams{Params: idem(idemKey + ":" + sub.ID)}
		sp.DefaultPaymentMethod = stripego.String(pmRef)
		if _, err := call(ctx, a, func() (*stripego.Subscription, error) { return a.sc.V1Subscriptions.Update(ctx, sub.ID, sp) }); err != nil {
			return err
		}
	}
	return nil
}

// ── Recurring subscription ─────────────────────────────────────────────

// CreateSubscription creates a subscription with payment_behavior
// default_incomplete (direct signup confirms the first invoice next).
func (a *Adapter) CreateSubscription(ctx context.Context, in port.SubscriptionInput, idemKey string) (port.SubscriptionResult, error) {
	p := &stripego.SubscriptionCreateParams{Params: idem(idemKey)}
	p.Customer = stripego.String(in.Customer.ProviderCustomerID)
	p.Items = []*stripego.SubscriptionCreateItemParams{{Price: stripego.String(in.PriceRef), Quantity: stripego.Int64(int64(in.Quantity))}}
	p.PaymentBehavior = stripego.String("default_incomplete")
	p.Metadata = in.Metadata
	setIf(&p.DefaultPaymentMethod, in.DefaultPaymentMethod)
	p.AddExpand("latest_invoice")
	s, err := call(ctx, a, func() (*stripego.Subscription, error) { return a.sc.V1Subscriptions.Create(ctx, p) })
	if err != nil {
		return port.SubscriptionResult{}, err
	}
	res := port.SubscriptionResult{Ref: port.SubscriptionRef{ProviderSubscriptionID: s.ID}, Outcome: subscriptionOutcome(s)}
	if s.LatestInvoice != nil {
		res.LatestInvoice = port.InvoiceRef{ProviderInvoiceID: s.LatestInvoice.ID}
	}
	res.PeriodStart, res.PeriodEnd = period(s)
	return res, nil
}

// ConfirmInitialPayment pays the subscription's first invoice off-session.
// 3-D Secure comes back as ErrActionRequired with the client secret.
func (a *Adapter) ConfirmInitialPayment(ctx context.Context, ref port.SubscriptionRef, idemKey string) (port.PaymentOutcome, error) {
	if err := a.limiter.wait(ctx); err != nil {
		return port.PaymentOutcome{}, err
	}
	sp := &stripego.SubscriptionRetrieveParams{}
	sp.AddExpand("latest_invoice")
	s, err := a.sc.V1Subscriptions.Retrieve(ctx, ref.ProviderSubscriptionID, sp)
	if err != nil {
		return port.PaymentOutcome{}, mapErr(err)
	}
	if s.LatestInvoice == nil {
		return port.PaymentOutcome{Status: port.PaymentPending}, nil
	}
	if s.LatestInvoice.Status == stripego.InvoiceStatusPaid {
		return port.PaymentOutcome{Status: port.PaymentPaid}, nil
	}
	pp := &stripego.InvoicePayParams{Params: idem(idemKey)}
	pp.OffSession = stripego.Bool(true)
	inv, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Pay(ctx, s.LatestInvoice.ID, pp) })
	if err != nil {
		return port.PaymentOutcome{}, err
	}
	return invoiceOutcome(inv), nil
}

// ChangeQuantity buys seats now: proration invoiced at once, applied only
// when paid (proration_behavior always_invoice, payment_behavior
// pending_if_incomplete, proration_date from the preview).
func (a *Adapter) ChangeQuantity(ctx context.Context, in port.QuantityChange, idemKey string) (port.ChangeResult, error) {
	itemID, _, err := a.seatItem(ctx, in.Sub)
	if err != nil {
		return port.ChangeResult{}, err
	}
	p := &stripego.SubscriptionUpdateParams{Params: idem(idemKey)}
	p.Items = []*stripego.SubscriptionUpdateItemParams{{ID: stripego.String(itemID), Quantity: stripego.Int64(int64(in.To))}}
	if in.KeepIfUnpaid {
		// Renewal top-up: the seats stay even if the charge fails (§8.6.3).
		p.PaymentBehavior = stripego.String("allow_incomplete")
	}
	return a.immediateChange(ctx, in.Sub, p, in.ProrationInstant)
}

// ChangePrice upgrades now, applied when paid.
func (a *Adapter) ChangePrice(ctx context.Context, in port.PriceChange, idemKey string) (port.ChangeResult, error) {
	itemID, _, err := a.seatItem(ctx, in.Sub)
	if err != nil {
		return port.ChangeResult{}, err
	}
	p := &stripego.SubscriptionUpdateParams{Params: idem(idemKey)}
	p.Items = []*stripego.SubscriptionUpdateItemParams{{ID: stripego.String(itemID), Price: stripego.String(in.ToPriceRef)}}
	return a.immediateChange(ctx, in.Sub, p, in.ProrationInstant)
}

func (a *Adapter) immediateChange(ctx context.Context, sub port.SubscriptionRef, p *stripego.SubscriptionUpdateParams, at time.Time) (port.ChangeResult, error) {
	p.ProrationBehavior = stripego.String("always_invoice")
	if p.PaymentBehavior == nil {
		p.PaymentBehavior = stripego.String("pending_if_incomplete")
	}
	if !at.IsZero() {
		p.ProrationDate = stripego.Int64(at.Unix())
	}
	p.AddExpand("latest_invoice")
	s, err := call(ctx, a, func() (*stripego.Subscription, error) {
		return a.sc.V1Subscriptions.Update(ctx, sub.ProviderSubscriptionID, p)
	})
	if err != nil {
		return port.ChangeResult{}, err
	}
	res := port.ChangeResult{Pending: s.PendingUpdate != nil, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}
	if s.LatestInvoice != nil {
		res.Invoice = port.InvoiceRef{ProviderInvoiceID: s.LatestInvoice.ID}
		res.Outcome = invoiceOutcome(s.LatestInvoice)
	}
	if res.Pending && res.Outcome.Status == port.PaymentPaid {
		res.Outcome.Status = port.PaymentPending
	}
	return res, nil
}

// ScheduleAtPeriodEnd moves the subscription to ToPriceRef × ToQuantity at
// At (the renewal), through a Subscription Schedule. An existing schedule
// is updated, so a downgrade, a seat reduction and a repricing combine
// (§8.16, BIL-SUB-8).
func (a *Adapter) ScheduleAtPeriodEnd(ctx context.Context, in port.PeriodEndChange, idemKey string) (port.ScheduleRef, error) {
	if err := a.limiter.wait(ctx); err != nil {
		return port.ScheduleRef{}, err
	}
	s, err := a.sc.V1Subscriptions.Retrieve(ctx, in.Sub.ProviderSubscriptionID, nil)
	if err != nil {
		return port.ScheduleRef{}, mapErr(err)
	}
	scheduleID := ""
	if s.Schedule != nil {
		scheduleID = s.Schedule.ID
	} else {
		cp := &stripego.SubscriptionScheduleCreateParams{Params: idem(idemKey + ":create")}
		cp.FromSubscription = stripego.String(s.ID)
		sch, err := call(ctx, a, func() (*stripego.SubscriptionSchedule, error) { return a.sc.V1SubscriptionSchedules.Create(ctx, cp) })
		if err != nil {
			return port.ScheduleRef{}, err
		}
		scheduleID = sch.ID
	}
	curPrice, curQty := "", int64(0)
	if s.Items != nil && len(s.Items.Data) > 0 {
		it := s.Items.Data[0]
		curQty = it.Quantity
		if it.Price != nil {
			curPrice = it.Price.ID
		}
	}
	start, _ := period(s)
	up := &stripego.SubscriptionScheduleUpdateParams{Params: idem(idemKey + ":phases")}
	up.EndBehavior = stripego.String("release")
	up.Phases = []*stripego.SubscriptionScheduleUpdatePhaseParams{
		{
			Items:     []*stripego.SubscriptionScheduleUpdatePhaseItemParams{{Price: stripego.String(curPrice), Quantity: stripego.Int64(curQty)}},
			StartDate: stripego.Int64(start.Unix()),
			EndDate:   stripego.Int64(in.At.Unix()),
		},
		{
			Items:             []*stripego.SubscriptionScheduleUpdatePhaseItemParams{{Price: stripego.String(in.ToPriceRef), Quantity: stripego.Int64(int64(in.ToQuantity))}},
			StartDate:         stripego.Int64(in.At.Unix()),
			ProrationBehavior: stripego.String("none"),
		},
	}
	if _, err := call(ctx, a, func() (*stripego.SubscriptionSchedule, error) {
		return a.sc.V1SubscriptionSchedules.Update(ctx, scheduleID, up)
	}); err != nil {
		return port.ScheduleRef{}, err
	}
	return port.ScheduleRef{ProviderScheduleID: scheduleID}, nil
}

// ReleaseSchedule releases a schedule: the subscription renews as it is.
func (a *Adapter) ReleaseSchedule(ctx context.Context, ref port.ScheduleRef, idemKey string) error {
	p := &stripego.SubscriptionScheduleReleaseParams{Params: idem(idemKey)}
	_, err := call(ctx, a, func() (*stripego.SubscriptionSchedule, error) {
		return a.sc.V1SubscriptionSchedules.Release(ctx, ref.ProviderScheduleID, p)
	})
	return err
}

// SetCancelAtPeriodEnd turns auto-renew off (true) or back on (false).
func (a *Adapter) SetCancelAtPeriodEnd(ctx context.Context, ref port.SubscriptionRef, cancel bool, idemKey string) error {
	p := &stripego.SubscriptionUpdateParams{Params: idem(idemKey)}
	p.CancelAtPeriodEnd = stripego.Bool(cancel)
	_, err := call(ctx, a, func() (*stripego.Subscription, error) {
		return a.sc.V1Subscriptions.Update(ctx, ref.ProviderSubscriptionID, p)
	})
	return err
}

// CancelNow cancels immediately with no proration credit and no final
// invoice (offboarding, reversal).
func (a *Adapter) CancelNow(ctx context.Context, ref port.SubscriptionRef, idemKey string) error {
	p := &stripego.SubscriptionCancelParams{Params: idem(idemKey)}
	p.Prorate = stripego.Bool(false)
	p.InvoiceNow = stripego.Bool(false)
	_, err := call(ctx, a, func() (*stripego.Subscription, error) {
		return a.sc.V1Subscriptions.Cancel(ctx, ref.ProviderSubscriptionID, p)
	})
	return err
}

// PauseCollection (§8.10, BIL-SUB-7): pause sets pause_collection
// keep_as_draft and auto_advance=false on every already-open invoice, so
// retries stop; resume clears pause_collection and restores auto_advance.
// Held drafts are never finalised here (that is B-O16).
func (a *Adapter) PauseCollection(ctx context.Context, ref port.SubscriptionRef, pause bool, idemKey string) error {
	p := &stripego.SubscriptionUpdateParams{Params: idem(idemKey)}
	if pause {
		p.PauseCollection = &stripego.SubscriptionUpdatePauseCollectionParams{Behavior: stripego.String("keep_as_draft")}
	} else {
		p.AddExtra("pause_collection", "")
	}
	if _, err := call(ctx, a, func() (*stripego.Subscription, error) {
		return a.sc.V1Subscriptions.Update(ctx, ref.ProviderSubscriptionID, p)
	}); err != nil {
		return err
	}
	lp := &stripego.InvoiceListParams{Subscription: stripego.String(ref.ProviderSubscriptionID), Status: stripego.String(string(stripego.InvoiceStatusOpen))}
	for inv, err := range a.sc.V1Invoices.List(ctx, lp).All(ctx) {
		if err != nil {
			return mapErr(err)
		}
		up := &stripego.InvoiceUpdateParams{Params: idem(idemKey + ":" + inv.ID)}
		up.AutoAdvance = stripego.Bool(!pause)
		if _, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Update(ctx, inv.ID, up) }); err != nil {
			return err
		}
	}
	return nil
}

// ── Previews and one-off charges ───────────────────────────────────────

// PreviewQuantityChange previews the proration of a seat change (§8.5).
func (a *Adapter) PreviewQuantityChange(ctx context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	itemID, unit, err := a.seatItem(ctx, in.Sub)
	if err != nil {
		return port.ProrationPreview{}, err
	}
	items := []*stripego.InvoiceCreatePreviewSubscriptionDetailsItemParams{{ID: stripego.String(itemID), Quantity: stripego.Int64(int64(in.To))}}
	pv, err := a.preview(ctx, in.Sub, items, in.ProrationInstant)
	if err != nil {
		return pv, err
	}
	pv.RenewalAmount = domain.Money{AmountMinor: unit.AmountMinor * int64(in.To), Currency: unit.Currency}
	return pv, nil
}

// PreviewPriceChange previews an upgrade's proration (§8.4).
func (a *Adapter) PreviewPriceChange(ctx context.Context, in port.PriceChange) (port.ProrationPreview, error) {
	itemID, _, err := a.seatItem(ctx, in.Sub)
	if err != nil {
		return port.ProrationPreview{}, err
	}
	items := []*stripego.InvoiceCreatePreviewSubscriptionDetailsItemParams{{ID: stripego.String(itemID), Price: stripego.String(in.ToPriceRef)}}
	return a.preview(ctx, in.Sub, items, in.ProrationInstant)
}

func (a *Adapter) preview(ctx context.Context, sub port.SubscriptionRef, items []*stripego.InvoiceCreatePreviewSubscriptionDetailsItemParams, at time.Time) (port.ProrationPreview, error) {
	p := &stripego.InvoiceCreatePreviewParams{Subscription: stripego.String(sub.ProviderSubscriptionID)}
	p.SubscriptionDetails = &stripego.InvoiceCreatePreviewSubscriptionDetailsParams{
		Items: items, ProrationBehavior: stripego.String("always_invoice"),
	}
	if !at.IsZero() {
		p.SubscriptionDetails.ProrationDate = stripego.Int64(at.Unix())
	}
	inv, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.CreatePreview(ctx, p) })
	if err != nil {
		return port.ProrationPreview{}, err
	}
	return port.ProrationPreview{
		Amount:           domain.Money{AmountMinor: inv.AmountDue, Currency: strings.ToUpper(string(inv.Currency))},
		ProrationInstant: at,
		PeriodEnd:        time.Unix(inv.PeriodEnd, 0).UTC(),
	}, nil
}

// ChargeOneOff charges a token add-on: invoice item + invoice
// (charge_automatically), paid off-session (§8.7). The invoice is created
// with auto_advance off, so a draft whose item could not be attached is
// never finalised by the provider on its own (an empty draft would become
// a paid 0 invoice an hour later); paying it finalises it, and an unpaid
// finalised invoice keeps its hosted page (B-P23). A draft left without
// its item is deleted at once (best effort); when that fails its id is
// returned so purchase-expiry deletes it.
func (a *Adapter) ChargeOneOff(ctx context.Context, in port.OneOffCharge, idemKey string) (port.ChargeResult, error) {
	ip := &stripego.InvoiceCreateParams{Params: idem(idemKey + ":invoice")}
	ip.Customer = stripego.String(in.Customer.ProviderCustomerID)
	ip.CollectionMethod = stripego.String("charge_automatically")
	ip.AutoAdvance = stripego.Bool(false)
	ip.Description = stringOrNil(in.Description)
	ip.Metadata = in.Metadata
	inv, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Create(ctx, ip) })
	if err != nil {
		return port.ChargeResult{}, err
	}
	ii := &stripego.InvoiceItemCreateParams{Params: idem(idemKey + ":item")}
	ii.Customer = stripego.String(in.Customer.ProviderCustomerID)
	ii.Invoice = stripego.String(inv.ID)
	if in.PriceRef != "" {
		ii.Pricing = &stripego.InvoiceItemCreatePricingParams{Price: stripego.String(in.PriceRef)}
		ii.Quantity = stripego.Int64(int64(max(in.Quantity, 1)))
	} else {
		ii.Amount = stripego.Int64(in.Amount.AmountMinor)
		ii.Currency = stripego.String(strings.ToLower(in.Amount.Currency))
	}
	if _, err := call(ctx, a, func() (*stripego.InvoiceItem, error) { return a.sc.V1InvoiceItems.Create(ctx, ii) }); err != nil {
		if a.DeleteDraftInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: inv.ID}) == nil {
			return port.ChargeResult{}, err
		}
		// The draft still exists: return it so purchase-expiry deletes it (§8.7).
		return port.ChargeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: inv.ID}}, err
	}
	pp := &stripego.InvoicePayParams{Params: idem(idemKey + ":pay")}
	pp.OffSession = stripego.Bool(true)
	paid, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Pay(ctx, inv.ID, pp) })
	if err != nil {
		return port.ChargeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: inv.ID}}, err
	}
	return port.ChargeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: paid.ID}, Outcome: invoiceOutcome(paid)}, nil
}

// FindInvoice looks up the invoice of a purchase whose id Billing did not
// record (a provider error after the charge, §8.5.1, §8.7): the customer's
// or subscription's invoices created since q.CreatedSince, matched on
// metadata purchase_id (one-off) or on billing_reason subscription_update
// and the amount (the proration invoice of an immediate change).
func (a *Adapter) FindInvoice(ctx context.Context, q port.InvoiceQuery) (port.InvoiceRef, bool, error) {
	lp := &stripego.InvoiceListParams{CreatedRange: &stripego.RangeQueryParams{GreaterThanOrEqual: q.CreatedSince.Unix()}}
	switch {
	case q.Subscription.ProviderSubscriptionID != "":
		lp.Subscription = stripego.String(q.Subscription.ProviderSubscriptionID)
	case q.Customer.ProviderCustomerID != "":
		lp.Customer = stripego.String(q.Customer.ProviderCustomerID)
	default:
		return port.InvoiceRef{}, false, nil
	}
	if err := a.limiter.wait(ctx); err != nil {
		return port.InvoiceRef{}, false, err
	}
	lp.Limit = stripego.Int64(100)
	for inv, err := range a.sc.V1Invoices.List(ctx, lp).All(ctx) {
		if err != nil {
			return port.InvoiceRef{}, false, mapErr(err)
		}
		if invoiceMatches(inv, q) {
			return port.InvoiceRef{ProviderInvoiceID: inv.ID}, true, nil
		}
	}
	return port.InvoiceRef{}, false, nil
}

func invoiceMatches(inv *stripego.Invoice, q port.InvoiceQuery) bool {
	if q.PurchaseID != "" {
		return inv.Metadata["purchase_id"] == q.PurchaseID
	}
	return q.Amount != nil && inv.BillingReason == stripego.InvoiceBillingReasonSubscriptionUpdate &&
		strings.EqualFold(string(inv.Currency), q.Amount.Currency) && (inv.AmountDue == q.Amount.AmountMinor || inv.Total == q.Amount.AmountMinor) &&
		chargedLineMatches(inv, q.Quantity, q.PriceRef)
}

// chargedLineMatches reports whether the invoice charges for the change a
// query names: a positive (charged) line for quantity (a seat purchase's
// seats after) and/or price (an upgrade's target price). Nothing to match
// (0, "") matches any invoice; an invoice without lines matches nothing.
func chargedLineMatches(inv *stripego.Invoice, quantity int, priceRef string) bool {
	if quantity == 0 && priceRef == "" {
		return true
	}
	if inv.Lines == nil {
		return false
	}
	for _, l := range inv.Lines.Data {
		if l.Amount <= 0 {
			continue
		}
		if (quantity == 0 || l.Quantity == int64(quantity)) && (priceRef == "" || linePrice(l) == priceRef) {
			return true
		}
	}
	return false
}

// linePrice is the id of the price a line bills ("" when none).
func linePrice(l *stripego.InvoiceLineItem) string {
	if l.Pricing != nil && l.Pricing.PriceDetails != nil && l.Pricing.PriceDetails.Price != nil {
		return l.Pricing.PriceDetails.Price.ID
	}
	return ""
}

// ── Invoices and refunds ───────────────────────────────────────────────

// GetInvoice returns one invoice.
func (a *Adapter) GetInvoice(ctx context.Context, ref port.InvoiceRef) (port.InvoiceView, error) {
	inv, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Retrieve(ctx, ref.ProviderInvoiceID, nil) })
	if err != nil {
		return port.InvoiceView{}, err
	}
	return invoiceView(inv), nil
}

// invoiceLinkTTL bounds how long Billing hands out a provider invoice link.
const invoiceLinkTTL = time.Hour

// InvoicePDFLink returns Stripe's PDF link (B-P18 302).
func (a *Adapter) InvoicePDFLink(ctx context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	inv, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Retrieve(ctx, ref.ProviderInvoiceID, nil) })
	if err != nil {
		return port.URLWithExpiry{}, err
	}
	if inv.InvoicePDF == "" {
		return port.URLWithExpiry{}, port.ErrNotFound
	}
	return port.URLWithExpiry{URL: inv.InvoicePDF, ExpiresAt: a.now().Add(invoiceLinkTTL)}, nil
}

// PayOpenInvoiceLink returns the hosted invoice page (B-P14).
func (a *Adapter) PayOpenInvoiceLink(ctx context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	inv, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Retrieve(ctx, ref.ProviderInvoiceID, nil) })
	if err != nil {
		return port.URLWithExpiry{}, err
	}
	if inv.HostedInvoiceURL == "" {
		return port.URLWithExpiry{}, port.ErrNotFound
	}
	return port.URLWithExpiry{URL: inv.HostedInvoiceURL, ExpiresAt: a.now().Add(invoiceLinkTTL)}, nil
}

// VoidInvoice voids an invoice (offboarding, B-O16 void).
func (a *Adapter) VoidInvoice(ctx context.Context, ref port.InvoiceRef, idemKey string) error {
	p := &stripego.InvoiceVoidInvoiceParams{Params: idem(idemKey)}
	_, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.VoidInvoice(ctx, ref.ProviderInvoiceID, p) })
	return err
}

// DeleteDraftInvoice deletes a draft invoice (a purchase's invoice that was
// never finalised: Stripe voids only finalised invoices). An invoice that
// no longer exists is already gone: nil.
func (a *Adapter) DeleteDraftInvoice(ctx context.Context, ref port.InvoiceRef) error {
	_, err := call(ctx, a, func() (*stripego.Invoice, error) { return a.sc.V1Invoices.Delete(ctx, ref.ProviderInvoiceID, nil) })
	if errors.Is(err, port.ErrNotFound) {
		return nil
	}
	return err
}

// FinalizeInvoice finalises a held draft and lets Stripe collect it
// automatically (B-O16 finalize: auto_advance true).
func (a *Adapter) FinalizeInvoice(ctx context.Context, ref port.InvoiceRef, idemKey string) error {
	p := &stripego.InvoiceFinalizeInvoiceParams{Params: idem(idemKey)}
	p.AutoAdvance = stripego.Bool(true)
	_, err := call(ctx, a, func() (*stripego.Invoice, error) {
		return a.sc.V1Invoices.FinalizeInvoice(ctx, ref.ProviderInvoiceID, p)
	})
	return err
}

// Refund refunds (part of) an invoice's payment.
func (a *Adapter) Refund(ctx context.Context, in port.RefundInput, idemKey string) (port.RefundResult, error) {
	if err := a.limiter.wait(ctx); err != nil {
		return port.RefundResult{}, err
	}
	gp := &stripego.InvoiceRetrieveParams{}
	gp.AddExpand("payments")
	inv, err := a.sc.V1Invoices.Retrieve(ctx, in.Invoice.ProviderInvoiceID, gp)
	if err != nil {
		return port.RefundResult{}, mapErr(err)
	}
	pi := paymentIntentOf(inv)
	if pi == "" {
		return port.RefundResult{}, port.ErrNotFound
	}
	p := &stripego.RefundCreateParams{Params: idem(idemKey)}
	p.PaymentIntent = stripego.String(pi)
	p.Amount = stripego.Int64(in.Amount.AmountMinor)
	if in.Reason != "" {
		p.Reason = stripego.String(string(in.Reason))
	}
	r, err := call(ctx, a, func() (*stripego.Refund, error) { return a.sc.V1Refunds.Create(ctx, p) })
	if err != nil {
		return port.RefundResult{}, err
	}
	return port.RefundResult{ProviderRefundID: r.ID, Status: string(r.Status)}, nil
}

// CreditBalance posts a customer balance transaction (B-O2). A negative
// amount is a credit.
func (a *Adapter) CreditBalance(ctx context.Context, in port.BalanceAdjustment, idemKey string) (string, error) {
	p := &stripego.CustomerBalanceTransactionCreateParams{Params: idem(idemKey)}
	p.Customer = stripego.String(in.Customer.ProviderCustomerID)
	p.Amount = stripego.Int64(in.Amount.AmountMinor)
	p.Currency = stripego.String(strings.ToLower(in.Amount.Currency))
	p.Description = stringOrNil(in.Description)
	t, err := call(ctx, a, func() (*stripego.CustomerBalanceTransaction, error) {
		return a.sc.V1CustomerBalanceTransactions.Create(ctx, p)
	})
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// ── Re-read and reconciliation ─────────────────────────────────────────

// FetchCurrent re-reads the provider object before any state change (BIL-PAY-4).
func (a *Adapter) FetchCurrent(ctx context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	if err := a.limiter.wait(ctx); err != nil {
		return domain.ProviderObjectState{}, err
	}
	st := domain.ProviderObjectState{Ref: obj, Fields: map[string]any{}}
	switch obj.Kind {
	case domain.ObjectSubscription:
		s, err := a.sc.V1Subscriptions.Retrieve(ctx, obj.ID, nil)
		if err != nil {
			return st, mapErr(err)
		}
		st.Status, st.Metadata = string(s.Status), s.Metadata
		start, end := period(s)
		st.Fields["current_period_start"], st.Fields["current_period_end"] = start, end
		st.Fields["cancel_at_period_end"] = s.CancelAtPeriodEnd
		st.Fields["pending_update"] = s.PendingUpdate != nil
		if s.Items != nil && len(s.Items.Data) > 0 {
			st.Fields["quantity"] = s.Items.Data[0].Quantity
			if s.Items.Data[0].Price != nil {
				st.Fields["price_ref"] = s.Items.Data[0].Price.ID
			}
		}
	case domain.ObjectInvoice:
		inv, err := a.sc.V1Invoices.Retrieve(ctx, obj.ID, nil)
		if err != nil {
			return st, mapErr(err)
		}
		v := invoiceView(inv)
		st.Status, st.Metadata = v.Status, v.Metadata
		st.Fields["amount_due_minor"], st.Fields["amount_paid_minor"] = inv.AmountDue, inv.AmountPaid
		st.Fields["currency"] = strings.ToUpper(string(inv.Currency))
		st.Fields["total_minor"] = inv.Total
		// The charged (positive) lines' quantities and prices, so a callback
		// matching a proration invoice to a purchase checks what it bills.
		qs, prices := chargedLines(inv)
		st.Fields["charged_line_quantities"], st.Fields["charged_line_prices"] = qs, prices
		st.Fields["billing_reason"] = string(inv.BillingReason)
		st.Fields["subscription"] = subscriptionOf(inv)
		// The Billing subscription named in the metadata Billing set on the
		// provider subscription (§8.1.1), so invoice.paid resolves it even
		// before checkout.session.completed links the provider id.
		if id := billingSubscriptionOf(inv); id != "" {
			st.Fields["billing_subscription_id"] = id
		}
		st.Fields["kind"] = string(v.Kind)
		st.Fields["attempt_count"] = inv.AttemptCount
		if inv.NextPaymentAttempt > 0 {
			st.Fields["next_payment_attempt"] = time.Unix(inv.NextPaymentAttempt, 0).UTC()
		}
		// The subscription period this invoice covers (first line), so a
		// handler can apply it without another provider call inside its
		// transaction (BIL-CONC-2).
		lineFields(st.Fields, "", inv)
	case domain.ObjectCheckoutSession:
		sp := &stripego.CheckoutSessionRetrieveParams{}
		sp.AddExpand("setup_intent")
		sp.AddExpand("invoice")
		s, err := a.sc.V1CheckoutSessions.Retrieve(ctx, obj.ID, sp)
		if err != nil {
			return st, mapErr(err)
		}
		st.Status, st.Metadata = string(s.Status), s.Metadata
		st.Fields["mode"] = string(s.Mode)
		// The payment method a setup session saved (an opaque reference; no
		// card data), made the default by the lifecycle follow-up (B-P16).
		if s.SetupIntent != nil && s.SetupIntent.PaymentMethod != nil {
			st.Fields["setup_payment_method"] = s.SetupIntent.PaymentMethod.ID
		}
		// The first invoice of a subscription session, so the completion
		// handler sees a payment that is already settled (§9.4).
		if s.Invoice != nil && s.Invoice.Status != "" {
			st.Fields["invoice"], st.Fields["invoice_status"] = s.Invoice.ID, string(s.Invoice.Status)
			lineFields(st.Fields, "invoice_", s.Invoice)
		}
		if s.Subscription != nil {
			st.Fields["subscription"] = s.Subscription.ID
		}
	case domain.ObjectRefund:
		r, err := a.sc.V1Refunds.Retrieve(ctx, obj.ID, nil)
		if err != nil {
			return st, mapErr(err)
		}
		st.Status, st.Metadata = string(r.Status), r.Metadata
		st.Fields["amount_minor"], st.Fields["currency"] = r.Amount, strings.ToUpper(string(r.Currency))
	case domain.ObjectDispute:
		d, err := a.sc.V1Disputes.Retrieve(ctx, obj.ID, nil)
		if err != nil {
			return st, mapErr(err)
		}
		st.Status = string(d.Status)
		st.Fields["reason"], st.Fields["amount_minor"], st.Fields["created"] = string(d.Reason), d.Amount, time.Unix(d.Created, 0).UTC()
		if err := a.paymentFields(ctx, st.Fields, d.Charge, d.PaymentIntent); err != nil {
			return st, err
		}
	case domain.ObjectEarlyFraudWarning:
		w, err := a.sc.V1RadarEarlyFraudWarnings.Retrieve(ctx, obj.ID, nil)
		if err != nil {
			return st, mapErr(err)
		}
		st.Status, st.Fields["reason"], st.Fields["created"] = "open", string(w.FraudType), time.Unix(w.Created, 0).UTC()
		if err := a.paymentFields(ctx, st.Fields, w.Charge, w.PaymentIntent); err != nil {
			return st, err
		}
	default:
		return st, port.ErrUnsupported
	}
	return st, nil
}

// paymentFields records the disputed payment and the invoice it paid (via
// the invoice payments of its PaymentIntent), so the dispute handler can
// tell a conversion payment without another provider call (CD-10). A
// failed lookup is returned (mapped), so the callback is retried rather than
// applied without its invoice; no invoice payment at all is not an error.
func (a *Adapter) paymentFields(ctx context.Context, f map[string]any, ch *stripego.Charge, pi *stripego.PaymentIntent) error {
	if ch != nil {
		f["payment_ref"] = ch.ID
	}
	if pi == nil {
		return nil
	}
	f["payment_intent"] = pi.ID
	if f["payment_ref"] == nil {
		f["payment_ref"] = pi.ID
	}
	lp := &stripego.InvoicePaymentListParams{Payment: &stripego.InvoicePaymentListPaymentParams{Type: stripego.String("payment_intent"),
		PaymentIntent: stripego.String(pi.ID)}}
	lp.Limit = stripego.Int64(1)
	for ip, err := range a.sc.V1InvoicePayments.List(ctx, lp).All(ctx) {
		if err != nil {
			return mapErr(err)
		}
		if ip.Invoice != nil {
			f["invoice"] = ip.Invoice.ID
		}
		break
	}
	return nil
}

// ListSince lists subscriptions, invoices or events created since a time,
// 100 per page (§8.13).
func (a *Adapter) ListSince(ctx context.Context, kind port.ReconKind, since time.Time, cursor string) (port.ReconPage, error) {
	if err := a.limiter.wait(ctx); err != nil {
		return port.ReconPage{}, err
	}
	created := &stripego.RangeQueryParams{GreaterThanOrEqual: since.Unix()}
	page := port.ReconPage{}
	const limit = 100
	switch kind {
	case port.ReconSubscriptions:
		lp := &stripego.SubscriptionListParams{CreatedRange: created, Status: stripego.String("all")}
		lp.Limit = stripego.Int64(limit)
		if cursor != "" {
			lp.StartingAfter = stripego.String(cursor)
		}
		for s, err := range a.sc.V1Subscriptions.List(ctx, lp).All(ctx) {
			if err != nil {
				return page, mapErr(err)
			}
			page.Items = append(page.Items, domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: s.ID}, Status: string(s.Status), Metadata: s.Metadata})
			if len(page.Items) == limit {
				page.NextCursor = s.ID
				break
			}
		}
	case port.ReconInvoices:
		lp := &stripego.InvoiceListParams{CreatedRange: created}
		lp.Limit = stripego.Int64(limit)
		if cursor != "" {
			lp.StartingAfter = stripego.String(cursor)
		}
		for inv, err := range a.sc.V1Invoices.List(ctx, lp).All(ctx) {
			if err != nil {
				return page, mapErr(err)
			}
			st := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: inv.ID}, Status: string(inv.Status), Metadata: inv.Metadata,
				Fields: map[string]any{}}
			if inv.Customer != nil {
				st.Fields["customer"] = inv.Customer.ID // the tenant comes from Billing's customer mapping (BIL-PAY-3)
			}
			page.Items = append(page.Items, st)
			if len(page.Items) == limit {
				page.NextCursor = inv.ID
				break
			}
		}
	case port.ReconEvents:
		lp := &stripego.EventListParams{CreatedRange: created}
		lp.Limit = stripego.Int64(limit)
		if cursor != "" {
			lp.StartingAfter = stripego.String(cursor)
		}
		for ev, err := range a.sc.V1Events.List(ctx, lp).All(ctx) {
			if err != nil {
				return page, mapErr(err)
			}
			st := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: "event", ID: ev.ID}, Status: string(ev.Type)}
			if ev.Data != nil && ev.Livemode == a.cfg.Livemode {
				// Normalised as a delivery would be, so a repair can ledger it (§8.13).
				if pe, err := normalise(ev.ID, string(ev.Type), ev.Created, ev.Livemode, ev.Data.Raw); err == nil {
					st.Fields = map[string]any{"event": pe}
				}
			}
			page.Items = append(page.Items, st)
			if len(page.Items) == limit {
				page.NextCursor = ev.ID
				break
			}
		}
	default:
		return page, port.ErrUnsupported
	}
	return page, nil
}

// ── helpers ────────────────────────────────────────────────────────────

// seatItem returns the subscription's seat item id and its unit price.
func (a *Adapter) seatItem(ctx context.Context, sub port.SubscriptionRef) (string, domain.Money, error) {
	s, err := call(ctx, a, func() (*stripego.Subscription, error) {
		return a.sc.V1Subscriptions.Retrieve(ctx, sub.ProviderSubscriptionID, nil)
	})
	if err != nil {
		return "", domain.Money{}, err
	}
	if s.Items == nil || len(s.Items.Data) == 0 {
		return "", domain.Money{}, port.ErrNotFound
	}
	it := s.Items.Data[0]
	unit := domain.Money{}
	if it.Price != nil {
		unit = domain.Money{AmountMinor: it.Price.UnitAmount, Currency: strings.ToUpper(string(it.Price.Currency))}
	}
	return it.ID, unit, nil
}

// period returns the seat item's current period (Stripe holds periods on items).
func period(s *stripego.Subscription) (time.Time, time.Time) {
	if s == nil || s.Items == nil || len(s.Items.Data) == 0 {
		return time.Time{}, time.Time{}
	}
	it := s.Items.Data[0]
	return time.Unix(it.CurrentPeriodStart, 0).UTC(), time.Unix(it.CurrentPeriodEnd, 0).UTC()
}

func subscriptionOutcome(s *stripego.Subscription) port.PaymentOutcome {
	switch s.Status {
	case stripego.SubscriptionStatusActive, stripego.SubscriptionStatusTrialing:
		return port.PaymentOutcome{Status: port.PaymentPaid}
	default:
		if s.LatestInvoice != nil {
			return invoiceOutcome(s.LatestInvoice)
		}
		return port.PaymentOutcome{Status: port.PaymentPending}
	}
}

func invoiceOutcome(inv *stripego.Invoice) port.PaymentOutcome {
	switch inv.Status {
	case stripego.InvoiceStatusPaid:
		return port.PaymentOutcome{Status: port.PaymentPaid}
	case stripego.InvoiceStatusUncollectible, stripego.InvoiceStatusVoid:
		return port.PaymentOutcome{Status: port.PaymentDeclined}
	default: // draft, open: not settled yet
		return port.PaymentOutcome{Status: port.PaymentPending}
	}
}

func invoiceView(inv *stripego.Invoice) port.InvoiceView {
	cur := strings.ToUpper(string(inv.Currency))
	v := port.InvoiceView{
		Ref: port.InvoiceRef{ProviderInvoiceID: inv.ID}, Number: inv.Number, Status: string(inv.Status),
		AmountDue: domain.Money{AmountMinor: inv.AmountDue, Currency: cur}, AmountPaid: domain.Money{AmountMinor: inv.AmountPaid, Currency: cur},
		HostedURL: inv.HostedInvoiceURL, Metadata: inv.Metadata, Kind: invoiceKind(inv),
	}
	if inv.PeriodStart > 0 {
		t := time.Unix(inv.PeriodStart, 0).UTC()
		v.PeriodStart = &t
	}
	if inv.PeriodEnd > 0 {
		t := time.Unix(inv.PeriodEnd, 0).UTC()
		v.PeriodEnd = &t
	}
	if inv.Created > 0 {
		t := time.Unix(inv.Created, 0).UTC()
		v.IssuedAt = &t
	}
	if inv.StatusTransitions != nil && inv.StatusTransitions.PaidAt > 0 {
		t := time.Unix(inv.StatusTransitions.PaidAt, 0).UTC()
		v.PaidAt = &t
	}
	return v
}

// invoiceKind classifies an invoice for the mirror (§4.2.13).
func invoiceKind(inv *stripego.Invoice) domain.InvoiceKind {
	if inv.Metadata["purpose"] == "token_addon" {
		return domain.InvoiceTokenAddon
	}
	switch inv.BillingReason {
	case stripego.InvoiceBillingReasonSubscriptionCreate:
		return domain.InvoiceSubscriptionCreate
	case stripego.InvoiceBillingReasonSubscriptionCycle:
		return domain.InvoiceSubscriptionCycle
	case stripego.InvoiceBillingReasonSubscriptionUpdate:
		return domain.InvoiceProration
	default:
		return domain.InvoiceManual
	}
}

func subscriptionOf(inv *stripego.Invoice) string {
	if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil && inv.Parent.SubscriptionDetails.Subscription != nil {
		return inv.Parent.SubscriptionDetails.Subscription.ID
	}
	return ""
}

// lineFields records the subscription period and quantity of the invoice's
// first line under prefix+line_period_start, _end and _quantity, so a
// handler can apply them without another provider call inside its
// transaction (BIL-CONC-2).
func lineFields(f map[string]any, prefix string, inv *stripego.Invoice) {
	if inv.Lines == nil || len(inv.Lines.Data) == 0 {
		return
	}
	l := inv.Lines.Data[0]
	if l.Period != nil {
		f[prefix+"line_period_start"] = time.Unix(l.Period.Start, 0).UTC()
		f[prefix+"line_period_end"] = time.Unix(l.Period.End, 0).UTC()
	}
	if l.Quantity > 0 {
		f[prefix+"line_quantity"] = l.Quantity
	}
}

// chargedLines lists the quantity and price id of each positive line.
func chargedLines(inv *stripego.Invoice) ([]int64, []string) {
	qs, prices := []int64{}, []string{}
	if inv.Lines == nil {
		return qs, prices
	}
	for _, l := range inv.Lines.Data {
		if l.Amount > 0 {
			qs, prices = append(qs, l.Quantity), append(prices, linePrice(l))
		}
	}
	return qs, prices
}

// billingSubscriptionOf reads billing_subscription_id from the snapshot of
// the provider subscription's metadata on the invoice, else the invoice's own.
func billingSubscriptionOf(inv *stripego.Invoice) string {
	if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil {
		if id := inv.Parent.SubscriptionDetails.Metadata["billing_subscription_id"]; id != "" {
			return id
		}
	}
	return inv.Metadata["billing_subscription_id"]
}

func paymentIntentOf(inv *stripego.Invoice) string {
	if inv.Payments == nil {
		return ""
	}
	for _, p := range inv.Payments.Data {
		if p.Payment != nil && p.Payment.PaymentIntent != nil {
			return p.Payment.PaymentIntent.ID
		}
	}
	return ""
}

func setIf(dst **string, v string) {
	if v != "" {
		*dst = stripego.String(v)
	}
}

func stringOrNil(v string) *string {
	if v == "" {
		return nil
	}
	return stripego.String(v)
}

// VerifyPrice checks a Stripe Price for B-O11 / B-O12: active, same unit
// amount and currency, recurring with the same interval and an interval
// count of 1 (Billing's month / year mean one month / one year), or one-off.
func (a *Adapter) VerifyPrice(ctx context.Context, priceRef string, want port.PriceExpectation) error {
	p, err := call(ctx, a, func() (*stripego.Price, error) { return a.sc.V1Prices.Retrieve(ctx, priceRef, nil) })
	if err != nil {
		return err
	}
	var problems []string
	if !p.Active {
		problems = append(problems, "inactive")
	}
	if p.UnitAmount != want.Amount.AmountMinor {
		problems = append(problems, fmt.Sprintf("amount %d ≠ %d", p.UnitAmount, want.Amount.AmountMinor))
	}
	if !strings.EqualFold(string(p.Currency), want.Amount.Currency) {
		problems = append(problems, "currency "+strings.ToUpper(string(p.Currency))+" ≠ "+want.Amount.Currency)
	}
	switch {
	case want.Interval == "" && p.Recurring != nil:
		problems = append(problems, "recurring price for a one-off pack")
	case want.Interval != "" && (p.Recurring == nil || string(p.Recurring.Interval) != want.Interval):
		problems = append(problems, "interval differs")
	case want.Interval != "" && p.Recurring.IntervalCount != 1:
		problems = append(problems, fmt.Sprintf("interval count %d ≠ 1", p.Recurring.IntervalCount))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", port.ErrPriceMismatch, strings.Join(problems, "; "))
	}
	return nil
}
