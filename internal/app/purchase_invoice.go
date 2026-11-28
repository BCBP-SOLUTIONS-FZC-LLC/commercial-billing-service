package app

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// A purchase's invoice outside the happy path (§8.5.1, §8.7): a declined or
// abandoned purchase has its open invoice voided, and a purchase whose
// invoice turns out to be paid is settled, never failed or expired (the
// customer paid, so the seats, plan or tokens are granted).

// providerKeyTTL is how long Billing reuses a provider idempotency key
// (§9.2: Stripe keeps keys 24 h; Billing never retries one after 23 h).
const providerKeyTTL = 23 * time.Hour

// lookupSlack widens a one-off invoice lookup's created-since bound for
// clock skew. A one-off invoice is matched on its exact purchase_id, so the
// slack cannot reach another purchase's invoice. A proration invoice is
// matched on amount and lines only, so its lookup has no backward slack:
// an invoice created before the purchase's intent committed (another
// purchase's) is never a candidate (BIL-PAY-6).
const lookupSlack = time.Minute

// purchaseInvoice is what Billing learnt at the provider about the invoice
// of a purchase: its id ("" when none exists), whether it is paid for the
// purchase's amount and currency (Paid: settle), paid for something else
// (Mismatch: never settled, an operator reviews it), and whether Billing
// found it with FindInvoice rather than from its own record (LookedUp).
type purchaseInvoice struct {
	ID       string
	Paid     bool
	Mismatch bool
	LookedUp bool
}

// invoiceTarget names the purchase whose invoice closeInvoice resolves.
type invoiceTarget struct {
	TenantID   string
	Kind       string // seats | token_addon | plan
	PurchaseID string // purchase or change id
	Query      port.InvoiceQuery
	Amount     *domain.Money // what Billing charged; nil when nothing was invoiced
}

// closeInvoice resolves an open purchase's invoice outside any transaction
// (BIL-CONC-2). When Billing has no invoice id (a provider error after the
// charge) it looks the invoice up with t.Query first; a found invoice that
// another purchase or plan change already recorded is not this purchase's
// (BIL-PAY-6). It re-reads the invoice and voids it while open or deletes
// it while draft (the provider voids only finalised invoices), with key; a
// paid invoice is reported, not voided, so the caller settles the purchase
// instead, but only when it paid the purchase's amount and currency
// (BIL-PAY-7): any other paid invoice is a Mismatch, logged for an
// operator (SEV-2 BillingPaymentParked) and never settled.
func (s *Service) closeInvoice(ctx context.Context, providerName string, invoiceID *string, t invoiceTarget, key string) (purchaseInvoice, error) {
	if providerName == manualProvider {
		return purchaseInvoice{}, nil
	}
	prov, err := s.provider(providerName)
	if err != nil {
		return purchaseInvoice{}, err
	}
	id := deref(invoiceID)
	looked := false
	if id == "" {
		ref, found, err := prov.FindInvoice(ctx, t.Query)
		if err != nil {
			return purchaseInvoice{}, providerErr(err)
		}
		if !found {
			return purchaseInvoice{}, nil
		}
		id, looked = ref.ProviderInvoiceID, true
		var claimed bool
		if err := s.Tx.RunInTenantTx(ctx, t.TenantID, func(ctx context.Context) error {
			var err error
			claimed, err = s.invoiceClaimed(ctx, t.TenantID, id, t.PurchaseID)
			return err
		}); err != nil {
			return purchaseInvoice{}, err
		}
		if claimed { // never voided or settled on behalf of this purchase
			return purchaseInvoice{}, nil
		}
	}
	cur, err := prov.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: id})
	if errors.Is(err, port.ErrNotFound) {
		// Gone at the provider (a draft an earlier pass deleted): nothing to
		// void and nothing can be paid, so the purchase can close (rev 0.71).
		return purchaseInvoice{ID: id, LookedUp: looked}, nil
	}
	if err != nil {
		return purchaseInvoice{ID: id, LookedUp: looked}, providerErr(err)
	}
	out := purchaseInvoice{ID: id, LookedUp: looked}
	switch cur.Status {
	case "paid":
		if t.Amount == nil || invoicePaysFor(cur, *t.Amount) {
			out.Paid = true
		} else {
			out.Mismatch = true
			s.purchasePaymentParked(ctx, t.TenantID, t.Kind, t.PurchaseID, id, "invoice_amount_mismatch", "")
		}
	case "draft":
		if err := prov.DeleteDraftInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: id}); err != nil {
			return out, providerErr(err)
		}
	case "open":
		if err := prov.VoidInvoice(ctx, port.InvoiceRef{ProviderInvoiceID: id}, key); err != nil {
			return out, providerErr(err)
		}
	}
	return out, nil
}

// recheckInvoice runs under the tenant lock before a purchase is settled or
// closed with an invoice closeInvoice looked up: an invoice another
// purchase or plan change recorded meanwhile is dropped (BIL-PAY-6).
func (s *Service) recheckInvoice(ctx context.Context, tenantID, selfID string, inv *purchaseInvoice) error {
	if !inv.LookedUp || inv.ID == "" {
		return nil
	}
	claimed, err := s.invoiceClaimed(ctx, tenantID, inv.ID, selfID)
	if err == nil && claimed {
		*inv = purchaseInvoice{}
	}
	return err
}

// invoiceClaimed reports whether a provider invoice is already recorded on
// a seat purchase, plan change or add-on purchase other than selfID: one
// invoice pays for one purchase only (BIL-PAY-6).
func (s *Service) invoiceClaimed(ctx context.Context, tenantID, invoiceID, selfID string) (bool, error) {
	if p, found, err := s.SeatPurchases.ByProviderInvoice(ctx, tenantID, invoiceID); err != nil || found && p.PurchaseID != selfID {
		return err == nil, err
	}
	if c, found, err := s.Changes.ByProviderInvoice(ctx, tenantID, invoiceID); err != nil || found && c.ChangeID != selfID {
		return err == nil, err
	}
	if a, found, err := s.Addons.ByProviderInvoice(ctx, tenantID, invoiceID); err != nil || found && a.PurchaseID != selfID {
		return err == nil, err
	}
	return false, nil
}

// invoicePaysFor reports whether a re-read invoice is paid for want: the
// amount paid, or its total (a part paid from the customer balance), and
// the currency equal the purchase's (BIL-PAY-7). A 0 invoice never pays for
// a purchase that costs something.
func invoicePaysFor(cur domain.ProviderObjectState, want domain.Money) bool {
	currency, _ := cur.Fields["currency"].(string)
	if cur.Status != "paid" || !strings.EqualFold(currency, want.Currency) {
		return false
	}
	paid, _ := cur.Fields["amount_paid_minor"].(int64)
	total, hasTotal := cur.Fields["total_minor"].(int64)
	return paid == want.AmountMinor || hasTotal && total == want.AmountMinor
}

// purchasePaymentParked raises the operator alert for a provider payment
// Billing must not grant (an invoice paid for another amount or currency
// than the purchase's): SEV-2 BillingPaymentParked, as for a payment on a
// superseded checkout (§8.15 step 4, BIL-OPS-6). The operator refunds or
// re-bills in the provider; the purchase is not granted.
func (s *Service) purchasePaymentParked(ctx context.Context, tenantID, kind, purchaseID, invoiceID, reason, providerEventID string) {
	if s.Log == nil {
		return
	}
	s.logger(ctx).Error("provider payment does not match the purchase; operator decision needed (SEV-2)", map[string]any{
		"tenant_id": tenantID, "purchase_kind": kind, "purchase_id": purchaseID, "provider_invoice_id": invoiceID,
		"provider_event_id": providerEventID, "reason": reason, "alert": "BillingPaymentParked", "severity": "SEV-2"})
}

// parkMismatchedPayment is the callback side of BIL-PAY-7: the paid invoice
// is not for the purchase's amount and currency, so the ledger row is
// parked failed:invoice_amount_mismatch (no retry) and nothing is granted.
func (s *Service) parkMismatchedPayment(ctx context.Context, tenantID, kind, purchaseID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) error {
	s.purchasePaymentParked(ctx, tenantID, kind, purchaseID, cur.Ref.ID, "invoice_amount_mismatch", ev.ProviderEventID)
	return &port.ParkedEvent{Reason: "invoice_amount_mismatch"}
}

// prorationQuery finds the proration invoice of an immediate change (seat
// purchase, upgrade) on the provider subscription: billing reason
// subscription update, the amount Billing showed, a charged line for the
// seats after (quantity) or the target price (priceRef), created since the
// intent committed (no backward slack, BIL-PAY-6).
func prorationQuery(providerSubID string, amount *int64, currency string, quantity int, priceRef string, since time.Time) port.InvoiceQuery {
	if amount == nil {
		return port.InvoiceQuery{} // nothing invoiced (a deferral): nothing to find
	}
	return port.InvoiceQuery{Subscription: port.SubscriptionRef{ProviderSubscriptionID: providerSubID},
		Amount: &domain.Money{AmountMinor: *amount, Currency: currency}, Quantity: quantity, PriceRef: priceRef, CreatedSince: since}
}

// addonTarget finds a token add-on's one-off invoice by its metadata.
func addonTarget(acct domain.BillingAccount, p domain.QuotaAddonPurchase) invoiceTarget {
	return invoiceTarget{TenantID: p.TenantID, Kind: port.PurchaseKindTokenAddon, PurchaseID: p.PurchaseID,
		Query: port.InvoiceQuery{Customer: port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)}, PurchaseID: p.PurchaseID,
			CreatedSince: p.CreatedAt.Add(-lookupSlack)},
		Amount: &domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}}
}

func seatTarget(providerSubID string, p domain.SeatPurchase) invoiceTarget {
	t := invoiceTarget{TenantID: p.TenantID, Kind: port.PurchaseKindSeats, PurchaseID: p.PurchaseID}
	if p.DeferralReason != nil {
		return t // no invoice
	}
	t.Query = prorationQuery(providerSubID, &p.AmountMinor, p.Currency, p.SeatsAfter, "", p.CreatedAt)
	t.Amount = &domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}
	return t
}

// upgradeTarget: targetPriceRef is the provider price of the target plan
// ("" when unknown: the lookup then matches on the amount only).
func upgradeTarget(providerSubID string, req domain.PlanChangeRequest, targetPriceRef string) invoiceTarget {
	t := invoiceTarget{TenantID: req.TenantID, Kind: port.PurchaseKindPlan, PurchaseID: req.ChangeID,
		Query: prorationQuery(providerSubID, req.AmountMinor, deref(req.Currency), 0, targetPriceRef, req.CreatedAt)}
	if req.AmountMinor != nil {
		t.Amount = &domain.Money{AmountMinor: *req.AmountMinor, Currency: deref(req.Currency)}
	}
	return t
}

// chargedLinesMatch reports whether a re-read proration invoice has a
// charged line for quantity (when > 0) and priceRef (when set). A provider
// whose re-read carries no line detail is not checked.
func chargedLinesMatch(cur domain.ProviderObjectState, quantity int, priceRef string) bool {
	qs, hasQ := cur.Fields["charged_line_quantities"].([]int64)
	prices, hasP := cur.Fields["charged_line_prices"].([]string)
	if !hasQ || !hasP || len(qs) != len(prices) {
		return true
	}
	for i := range qs {
		if (quantity == 0 || qs[i] == int64(quantity)) && (priceRef == "" || prices[i] == priceRef) {
			return true
		}
	}
	return false
}

// invoiceAmountIs reports whether a re-read invoice is for amount (the
// proration invoice of an immediate change Billing has no id for).
func invoiceAmountIs(cur domain.ProviderObjectState, amount int64, currency string) bool {
	due, _ := cur.Fields["amount_due_minor"].(int64)
	cur2, _ := cur.Fields["currency"].(string)
	return due == amount && cur2 == currency
}

// isProrationInvoice reports whether a re-read invoice was created by an
// immediate subscription change on providerSubID.
func isProrationInvoice(cur domain.ProviderObjectState, providerSubID *string) bool {
	reason, _ := cur.Fields["billing_reason"].(string)
	sub, _ := cur.Fields["subscription"].(string)
	return reason == "subscription_update" && sub != "" && sub == deref(providerSubID)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// metadataPurchaseID is the purchase id a token add-on invoice carries in
// its metadata ("" when absent or malformed).
func metadataPurchaseID(cur domain.ProviderObjectState) string {
	id := cur.Metadata["purchase_id"]
	if cur.Metadata["purpose"] != "token_addon" || !uuidRE.MatchString(id) {
		return ""
	}
	return id
}

// closeSeatPurchaseInvoice closes the invoice of the subscription's open
// customer seat purchase before Billing cancels the purchase (§8.5.1: at a
// renewal that supersedes it, or at offboarding): the open proration invoice
// is voided outside any transaction so the customer can no longer pay for
// seats that will never be granted. An invoice already paid is parked for an
// operator refund (SEV-2 BillingPaymentParked). A failed void returns the
// error, so the caller retries before cancelling. A renewal top-up is left
// alone (skipTopUp): it follows dunning.
func (s *Service) closeSeatPurchaseInvoice(ctx context.Context, sub domain.Subscription, skipTopUp bool) error {
	var (
		p     domain.SeatPurchase
		found bool
	)
	if err := s.Tx.RunInTenantTx(ctx, sub.TenantID, func(ctx context.Context) error {
		var err error
		p, found, err = s.SeatPurchases.Open(ctx, sub.BillingSubscriptionID)
		return err
	}); err != nil || !found || (skipTopUp && p.Reason == domain.SeatReasonRenewalTopUp) {
		return err
	}
	inv, err := s.closeInvoice(ctx, sub.Provider, p.ProviderInvoiceID, seatTarget(deref(sub.ProviderSubscriptionID), p), "seatpur:"+p.PurchaseID+":void")
	if err != nil {
		return err
	}
	if inv.Paid {
		s.purchasePaymentParked(ctx, sub.TenantID, string(port.PurchaseKindSeats), p.PurchaseID, inv.ID, "paid_after_cancel", "")
	}
	return nil
}
