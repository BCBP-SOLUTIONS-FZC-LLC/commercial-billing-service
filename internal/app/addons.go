package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Add-on terms shown before purchase (§5.4.1 B-P19, FE-4).
var addonTerms = AddonTerms{NonRefundable: true, ExpiresAfterMonths: 12, UsableAfter: "payment_confirmed"}

// entitlementsDisplayTTL is §6's Entitlements display cache.
const entitlementsDisplayTTL = 60 * time.Second

// quotaCache is the advisory, per-pod Entitlements display cache (§6):
// B-P19 only; a purchase always reads fresh (BIL-ADD-1).
type quotaCache struct {
	mu sync.Mutex
	m  map[string]cachedQuota
}

type cachedQuota struct {
	q  port.TokenQuota
	at time.Time
}

func (c *quotaCache) get(tenantID string, now time.Time) (port.TokenQuota, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[tenantID]
	if !ok || now.Sub(v.at) > entitlementsDisplayTTL {
		return port.TokenQuota{}, false
	}
	return v.q, true
}

func (c *quotaCache) put(tenantID string, q port.TokenQuota, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cachedQuota{}
	}
	c.m[tenantID] = cachedQuota{q: q, at: now}
}

// eligibility applies BIL-ADD-1 to the tenant's state and quota. A W-1
// reason from Entitlements wins over the interim finite-quota rule.
func eligibility(sub *domain.Subscription, acct *domain.BillingAccount, q port.TokenQuota, known bool) (bool, domain.IneligibleReason) {
	in := domain.AddonEligibilityInput{EntitlementsKnown: known, TokenQuotaFinite: q.MonthlyLimit != nil}
	if sub != nil {
		in.SubscriptionStatus, in.CollectionPaused = sub.Status, sub.CollectionPaused
	}
	if acct != nil {
		in.IAMLifecycle = acct.IAMLifecycle
	} else {
		in.IAMLifecycle = domain.LifecycleTrial
	}
	ok, reason := domain.AddonEligibility(in)
	if ok && q.Eligibility != "" && q.Eligibility != "eligible" {
		return false, domain.IneligibleReason(q.Eligibility)
	}
	return ok, reason
}

// addonState reads the account, live subscription and packs.
func (s *Service) addonState(ctx context.Context, tenantID string) (*domain.BillingAccount, *domain.Subscription, []domain.TokenPack, error) {
	var (
		acct  *domain.BillingAccount
		sub   *domain.Subscription
		packs []domain.TokenPack
	)
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		a, err := s.Accounts.Get(ctx, tenantID)
		if domain.HasCode(err, domain.ErrSubscriptionNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		acct = &a
		if live, found, err := s.Subs.Live(ctx, tenantID); err != nil {
			return err
		} else if found && live.Status != domain.StatusIncomplete {
			sub = &live
		}
		packs, err = s.Addons.ActivePacks(ctx, a.Currency, a.Provider, s.now())
		return err
	})
	return acct, sub, packs, err
}

// ListTokenPacks is B-P19.
func (s *Service) ListTokenPacks(ctx context.Context, _ Caller, tenantID string) (TokenPackCatalog, error) {
	acct, sub, packs, err := s.addonState(ctx, tenantID)
	if err != nil {
		return TokenPackCatalog{}, err
	}
	now := s.now()
	q, cached := s.quotas.get(tenantID, now)
	known := cached
	if !cached && acct != nil {
		if fresh, err := s.Entitlements.TokenQuota(ctx, tenantID); err == nil {
			q, known = fresh, true
			s.quotas.put(tenantID, fresh, now)
		}
	}
	ok, reason := eligibility(sub, acct, q, known)
	out := TokenPackCatalog{Resource: domain.ResourceLLMTokens, Eligible: ok, Items: []TokenPackView{}, Terms: addonTerms}
	if !ok {
		out.IneligibleReason = ptr(string(reason))
	}
	var tightest *int64
	for _, p := range packs {
		out.Items = append(out.Items, TokenPackView{PackCode: p.PackCode, Tokens: p.Tokens,
			Price: MoneyFrom(domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency})})
		if p.MonthlyCapTokens != nil {
			used, err := s.confirmedThisMonth(ctx, tenantID, p.PackCode)
			if err != nil {
				return out, err
			}
			if rem, has := domain.CapRemaining(p.MonthlyCapTokens, used); has && (tightest == nil || rem < *tightest) {
				tightest = &rem
			}
		}
	}
	out.MonthlyCapRemainingTokens = tightest
	return out, nil
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (s *Service) confirmedThisMonth(ctx context.Context, tenantID, pack string) (int64, error) {
	var n int64
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		n, err = s.Addons.ConfirmedTokensSince(ctx, tenantID, pack, monthStart(s.now()))
		return err
	})
	return n, err
}

// PurchaseTokenAddon is B-P20 (§8.7).
func (s *Service) PurchaseTokenAddon(ctx context.Context, c Caller, tenantID string, r AddonPurchaseRequest) (PurchaseView, error) {
	if !r.AcceptTerms {
		return PurchaseView{}, domain.NewError(domain.ErrTermsNotAccepted, "accept the add-on terms (non-refundable, 12-month expiry)")
	}
	acct, sub, packs, err := s.addonState(ctx, tenantID)
	if err != nil {
		return PurchaseView{}, err
	}
	var pack *domain.TokenPack
	for i := range packs {
		if packs[i].PackCode == r.PackCode {
			pack = &packs[i]
		}
	}
	if pack == nil {
		return PurchaseView{}, domain.NewError(domain.ErrAddonNotAvailable, "the pack is not available for this account")
	}
	// Status checks first, then the fresh Entitlements read (fail closed).
	if ok, reason := eligibility(sub, acct, port.TokenQuota{MonthlyLimit: ptr(int64(1))}, true); !ok {
		return PurchaseView{}, domain.NewError(domain.ErrAddonNotEligible, "the tenant cannot buy add-ons now").With("ineligible_reason", string(reason))
	}
	q, err := s.Entitlements.TokenQuota(ctx, tenantID)
	if err != nil {
		return PurchaseView{}, err
	}
	if ok, reason := eligibility(sub, acct, q, true); !ok {
		return PurchaseView{}, domain.NewError(domain.ErrAddonNotEligible, "the tenant cannot buy add-ons now").With("ineligible_reason", string(reason))
	}
	tokens := pack.Tokens * int64(r.PackQuantity)
	p := domain.QuotaAddonPurchase{PurchaseID: newID(), TenantID: tenantID, PackCode: pack.PackCode, PackQuantity: r.PackQuantity,
		TokensTotal: tokens, AmountMinor: pack.AmountMinor * int64(r.PackQuantity), Currency: pack.Currency,
		Status: domain.PurchasePendingPayment, RequestedBy: actorOf(c), IdempotencyKey: c.IdempotencyKey}
	if p.IdempotencyKey == "" {
		p.IdempotencyKey = "addon:" + p.PurchaseID
	}
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		used, err := s.Addons.ConfirmedTokensSince(ctx, tenantID, pack.PackCode, monthStart(s.now()))
		if err != nil {
			return err
		}
		if !domain.WithinCap(pack.MonthlyCapTokens, used, tokens) {
			rem, _ := domain.CapRemaining(pack.MonthlyCapTokens, used)
			return domain.NewError(domain.ErrAddonCapReached, "the monthly add-on cap is reached").With("cap_remaining_tokens", rem)
		}
		return s.Addons.Insert(ctx, p)
	}); err != nil {
		return PurchaseView{}, err
	}

	prov, err := s.provider(acct.Provider)
	if err != nil {
		return PurchaseView{}, err
	}
	res, err := prov.ChargeOneOff(ctx, port.OneOffCharge{Customer: port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)},
		PriceRef: deref(pack.ProviderPriceID), Quantity: r.PackQuantity, Amount: domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency},
		Description: "Token add-on " + pack.PackCode,
		Metadata:    map[string]string{"tenant_id": tenantID, "purpose": "token_addon", "purchase_id": p.PurchaseID}}, "addon:"+p.PurchaseID)
	var action port.ErrActionRequired
	var declined port.ErrDeclined
	switch {
	case errors.As(err, &declined) || err == nil && res.Outcome.Status == port.PaymentDeclined:
		code := declined.Code
		if code == "" {
			code = res.Outcome.DeclineCode
		}
		_ = s.locked(ctx, tenantID, func(ctx context.Context) error {
			p.Status = domain.PurchaseFailed
			return s.Addons.Update(ctx, p)
		})
		return PurchaseView{}, domain.NewError(domain.ErrPaymentDeclined, "payment declined").With("decline_reason", declineReason(code))
	case errors.As(err, &action) || err == nil && res.Outcome.Status == port.PaymentRequiresAction:
		secret := action.ClientSecret
		if secret == "" {
			secret = res.Outcome.ClientSecret
		}
		p.Status, p.ClientAction = domain.PurchaseRequiresAction, mustJSON(ClientAction{Type: "3ds", ClientSecret: secret})
	case err != nil && res.Invoice.ProviderInvoiceID != "" && !acct.DefaultPMSet:
		// No default payment method: the customer pays on the provider's
		// hosted invoice page (B-P23).
		p.Status, p.ClientAction = domain.PurchaseRequiresAction, mustJSON(ClientAction{Type: "payment_session"})
	case err != nil:
		return PurchaseView{}, providerErr(err) // stays pending; the callback or purchase-expiry settles it
	}
	if res.Invoice.ProviderInvoiceID != "" {
		p.ProviderInvoiceID = &res.Invoice.ProviderInvoiceID
	}
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, found, err := s.Addons.Get(ctx, p.PurchaseID)
		if err != nil || !found {
			return err
		}
		if cur.Status == domain.PurchaseConfirmed {
			p = cur
			return nil
		}
		cur.ProviderInvoiceID, cur.ClientAction, cur.Status = p.ProviderInvoiceID, p.ClientAction, p.Status
		if err == nil && res.Outcome.Status == port.PaymentPaid && cur.Status == domain.PurchasePendingPayment {
			if err := s.confirmAddon(ctx, &cur, metaFor(c)); err != nil {
				return err
			}
		} else if err := s.Addons.Update(ctx, cur); err != nil {
			return err
		}
		p = cur
		return nil
	})
	if err != nil {
		return PurchaseView{}, err
	}
	return addonView(p), nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// confirmAddon is §8.7 step 3: confirmed, purchased_at = confirmed_at,
// TenantQuotaAddOnChanged {purchase}, event_id stored (BIL-ADD-2).
func (s *Service) confirmAddon(ctx context.Context, p *domain.QuotaAddonPurchase, m port.EventMeta) error {
	now := s.now()
	id, err := s.Events.Enqueue(ctx, domain.TenantQuotaAddOnChanged{TenantID: p.TenantID, Resource: domain.ResourceLLMTokens,
		Delta: p.TokensTotal, Reason: domain.AddonPurchase, BillingRef: p.PurchaseID, PurchasedAt: &now, PackCode: p.PackCode,
		PackQuantity: p.PackQuantity}, m)
	if err != nil {
		return err
	}
	p.Status, p.ConfirmedAt, p.EventID, p.ClientAction = domain.PurchaseConfirmed, &now, &id, nil
	return s.Addons.Update(ctx, *p)
}

func addonView(p domain.QuotaAddonPurchase) PurchaseView {
	v := PurchaseView{PurchaseID: p.PurchaseID, Kind: "token_addon", Status: string(p.Status),
		Amount: MoneyFrom(domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}), PackCode: p.PackCode,
		TokensTotal: &p.TokensTotal, CreatedAt: p.CreatedAt, ConfirmedAt: p.ConfirmedAt}
	if p.Status == domain.PurchaseConfirmed {
		v.Activation = "activating"
		if p.EventID != nil {
			v.Activation = "sent" // Billing reports only that the event was published (§5.4.1 B-P21)
		}
	}
	if len(p.ClientAction) > 0 {
		var ca ClientAction
		if json.Unmarshal(p.ClientAction, &ca) == nil {
			v.ClientAction = &ca
		}
	}
	return v
}

// GetAddonPurchase is B-P21.
func (s *Service) GetAddonPurchase(ctx context.Context, _ Caller, tenantID, purchaseID string) (PurchaseView, error) {
	var out PurchaseView
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		p, found, err := s.Addons.Get(ctx, purchaseID)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.ErrPurchaseNotFound, "add-on purchase not found")
		}
		out = addonView(p)
		return nil
	})
	return out, err
}

// ListAddonPurchases is B-P22.
func (s *Service) ListAddonPurchases(ctx context.Context, _ Caller, tenantID string, pg Page) (List[PurchaseView], error) {
	limit := pg.Limit
	if limit == 0 {
		limit = 20
	}
	out := List[PurchaseView]{Items: []PurchaseView{}}
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		rows, err := s.Addons.List(ctx, tenantID, pg.Cursor, limit+1)
		if err != nil {
			return err
		}
		for i, p := range rows {
			if i == limit {
				out.NextCursor = ptr(rows[i-1].PurchaseID)
				break
			}
			out.Items = append(out.Items, addonView(p))
		}
		return nil
	})
	return out, err
}

// AddonPaymentSession is B-P23: the provider-hosted page for the
// purchase's invoice.
func (s *Service) AddonPaymentSession(ctx context.Context, _ Caller, tenantID, purchaseID string) (HostedSession, error) {
	var (
		p    domain.QuotaAddonPurchase
		acct domain.BillingAccount
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if p, found, err = s.Addons.Get(ctx, purchaseID); err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.ErrPurchaseNotFound, "add-on purchase not found")
		}
		if p.Status != domain.PurchaseRequiresAction || p.ProviderInvoiceID == nil {
			return domain.NewError(domain.ErrPurchaseNotAwaitingPayment, "the purchase is not awaiting payment")
		}
		acct, err = s.Accounts.Get(ctx, tenantID)
		return err
	}); err != nil {
		return HostedSession{}, err
	}
	prov, err := s.provider(acct.Provider)
	if err != nil {
		return HostedSession{}, err
	}
	link, err := prov.PayOpenInvoiceLink(ctx, port.InvoiceRef{ProviderInvoiceID: *p.ProviderInvoiceID})
	if err != nil {
		return HostedSession{}, providerErr(err)
	}
	return HostedSession{URL: link.URL, ExpiresAt: link.ExpiresAt}, nil
}

// AddonInvoicePaid confirms an open add-on purchase when its invoice is
// paid (3-D Secure completed or paid on the hosted page).
type AddonInvoicePaid struct {
	S    *Service
	Next port.PaymentEventHandler
}

// Handle implements port.PaymentEventHandler.
func (h AddonInvoicePaid) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	p, found, err := s.Addons.ByProviderInvoice(ctx, tenantID, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found {
		if h.Next == nil {
			return "", &port.IgnoredEvent{Reason: "no_handler"}
		}
		return h.Next.Handle(ctx, tenantID, ev, cur)
	}
	if !p.Status.Open() {
		return "", &port.IgnoredEvent{Reason: "purchase_" + string(p.Status)}
	}
	if err := s.confirmAddon(ctx, &p, webhookMeta()); err != nil {
		return "", err
	}
	return "applied:addon_purchase", nil
}
