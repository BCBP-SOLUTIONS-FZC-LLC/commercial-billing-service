package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
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

// addonState is what B-P19 and B-P20 read before deciding: the account
// (nil before a trial tenant's first purchase), the live subscription (nil
// for none or incomplete), whether the tenant is on trial (BIL-ADD-1 (b)),
// the packs on sale to it and, for a trial tenant, the BIL-ADD-5 cap and its
// usage.
type addonState struct {
	acct     *domain.BillingAccount
	sub      *domain.Subscription
	trial    bool
	provider string
	currency string
	packs    []domain.TokenPack
	cap      domain.TrialAddonCap
	used     domain.TrialAddonUsage
}

// lifecycle is the IAM lifecycle for eligibility: trial without an account.
func (st addonState) lifecycle() domain.IAMLifecycle {
	if st.acct == nil {
		return domain.LifecycleTrial
	}
	return st.acct.IAMLifecycle
}

// eligibility applies BIL-ADD-1 to the tenant's state, the Entitlements
// answer and the caller. A W-1 reason from Entitlements wins over the
// interim finite-quota rule; a trial tenant needs Entitlements to report
// llm_tokens eligible (its trial active) and an owner caller.
func eligibility(st addonState, q port.TokenQuota, known, owner bool) (bool, domain.IneligibleReason) {
	in := domain.AddonEligibilityInput{EntitlementsKnown: known, TokenQuotaFinite: q.MonthlyLimit != nil,
		EntitlementsReason: q.Eligibility, IAMLifecycle: st.lifecycle(), Owner: owner}
	if st.sub != nil {
		in.SubscriptionStatus, in.CollectionPaused = st.sub.Status, st.sub.CollectionPaused
	}
	return domain.AddonEligibility(in)
}

// isOwner is the trial purchase's caller check (P-5c): the caller holds
// commercial.payment_method.manage.
func isOwner(c Caller) bool { return domain.PermissionsFor(c.Roles)[domain.PermPaymentMethodManage] }

// addonState reads the account, live subscription and packs: a trial tenant
// (with or without an account) sees DEFAULT_CURRENCY packs (BIL-ADD-1 (b)),
// a paid tenant its account currency's; and a trial tenant's cap and usage.
func (s *Service) addonState(ctx context.Context, tenantID string) (addonState, error) {
	var st addonState
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		st, err = s.readAddonState(ctx, tenantID)
		return err
	})
	return st, err
}

// readAddonState is addonState in the caller's transaction.
func (s *Service) readAddonState(ctx context.Context, tenantID string) (addonState, error) {
	st, err := s.trialState(ctx, tenantID)
	if err != nil {
		return st, err
	}
	st.provider, st.currency = s.Settings.NewAccountProvider, s.Settings.DefaultCurrency
	if st.acct != nil {
		st.provider = st.acct.Provider
		if !st.trial {
			st.currency = st.acct.Currency
		}
	}
	if st.trial {
		if st.cap, st.used, err = s.trialCap(ctx, tenantID); err != nil {
			return st, err
		}
	}
	st.packs, err = s.Addons.ActivePacks(ctx, st.currency, st.provider, s.now())
	return st, err
}

func subStatus(sub *domain.Subscription) domain.SubscriptionStatus {
	if sub == nil {
		return ""
	}
	return sub.Status
}

// trialCap is the BIL-ADD-5 cap in force (the B-O18 override, or the
// default) and what the tenant's trial purchases use of it.
func (s *Service) trialCap(ctx context.Context, tenantID string) (domain.TrialAddonCap, domain.TrialAddonUsage, error) {
	c, found, err := s.Addons.TrialCap(ctx, tenantID)
	if err != nil {
		return c, domain.TrialAddonUsage{}, err
	}
	if !found {
		c = domain.DefaultTrialAddonCap(tenantID)
	}
	used, err := s.Addons.TrialUsage(ctx, tenantID)
	return c, used, err
}

// trialCapView is B-P19's trial_cap.
func (s *Service) trialCapView(c domain.TrialAddonCap, used domain.TrialAddonUsage) *TrialCapView {
	return &TrialCapView{MaxPacks: c.MaxPacks, MaxAmountMinor: c.MaxAmountMinor, UsedPacks: used.Packs,
		UsedAmountMinor: used.AmountMinor, Currency: s.Settings.DefaultCurrency}
}

// ListTokenPacks is B-P19.
func (s *Service) ListTokenPacks(ctx context.Context, c Caller, tenantID string) (TokenPackCatalog, error) {
	st, err := s.addonState(ctx, tenantID)
	if err != nil {
		return TokenPackCatalog{}, err
	}
	now := s.now()
	q, known := s.quotas.get(tenantID, now)
	if !known {
		// Read for a tenant without an account too: a trial tenant's
		// eligibility is Entitlements' (BIL-ADD-1 (b)).
		if fresh, err := s.Entitlements.TokenQuota(ctx, tenantID); err == nil {
			q, known = fresh, true
			s.quotas.put(tenantID, fresh, now)
		}
	}
	ok, reason := eligibility(st, q, known, isOwner(c))
	out := TokenPackCatalog{Resource: domain.ResourceLLMTokens, Eligible: ok, Items: []TokenPackView{}, Terms: addonTerms}
	if !ok {
		out.IneligibleReason = ptr(string(reason))
	}
	if st.trial {
		out.TrialCap = s.trialCapView(st.cap, st.used)
	}
	var tightest *int64
	for _, p := range st.packs {
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
	ctx, end := StartSpan(ctx, "billing.addon.purchase", AttrTenant(tenantID))
	out, err := s.purchaseTokenAddon(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) purchaseTokenAddon(ctx context.Context, c Caller, tenantID string, r AddonPurchaseRequest) (PurchaseView, error) {
	if !r.AcceptTerms {
		return PurchaseView{}, domain.NewError(domain.ErrTermsNotAccepted, "accept the add-on terms (non-refundable, 12-month expiry)")
	}
	st, err := s.addonState(ctx, tenantID)
	if err != nil {
		return PurchaseView{}, err
	}
	var pack *domain.TokenPack
	for i := range st.packs {
		if st.packs[i].PackCode == r.PackCode {
			pack = &st.packs[i]
		}
	}
	if pack == nil {
		// A trial tenant is offered DEFAULT_CURRENCY packs only (BIL-ADD-1 (b)).
		return PurchaseView{}, domain.NewError(domain.ErrAddonNotAvailable, "the pack is not available for this account")
	}
	// Status checks first, then the fresh Entitlements read (fail closed).
	if ok, reason := eligibility(st, port.TokenQuota{MonthlyLimit: ptr(int64(1)), Eligibility: "eligible"}, true, true); !ok {
		return PurchaseView{}, notEligible(reason)
	}
	q, err := s.Entitlements.TokenQuota(ctx, tenantID)
	if err != nil {
		return PurchaseView{}, err
	}
	if ok, reason := eligibility(st, q, true, isOwner(c)); !ok {
		return PurchaseView{}, notEligible(reason)
	}
	if st.trial && !domain.WithinTrialCap(st.cap, st.used, r.PackQuantity, pack.AmountMinor*int64(r.PackQuantity)) {
		// Refused before any provider customer is created; the authoritative
		// check runs again under the lock with the insert (BIL-ADD-5).
		return PurchaseView{}, trialCapReached(st.cap, st.used)
	}
	var acct domain.BillingAccount
	if st.acct != nil {
		acct = *st.acct
	} else if acct, err = s.newTrialAccount(ctx, tenantID); err != nil {
		// A trial tenant's first purchase: the provider customer is created
		// outside any transaction (BIL-CONC-2); the account is inserted with
		// the purchase below.
		return PurchaseView{}, err
	}
	tokens := pack.Tokens * int64(r.PackQuantity)
	p := domain.QuotaAddonPurchase{PurchaseID: newID(), TenantID: tenantID, PackCode: pack.PackCode, PackQuantity: r.PackQuantity,
		TokensTotal: tokens, AmountMinor: pack.AmountMinor * int64(r.PackQuantity), Currency: pack.Currency,
		Status: domain.PurchasePendingPayment, RequestedBy: actorOf(c), IdempotencyKey: c.IdempotencyKey, TrialPurchase: st.trial}
	if p.IdempotencyKey == "" {
		p.IdempotencyKey = "addon:" + p.PurchaseID
	}
	resumed := false
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		// §9.2 one row per key: a retry of this key (the first attempt failed
		// with a 5xx, which releases the idempotency record) resumes its row,
		// and so the provider key addon:<purchase_id>. Looked up before the
		// trial re-check, so a trial purchase retried after the conversion
		// still resumes (§8.7: a pending trial purchase completes as usual).
		if prev, found, err := s.Addons.ByIdempotencyKey(ctx, tenantID, p.IdempotencyKey); err != nil {
			return err
		} else if found {
			if prev.PackCode != p.PackCode || prev.PackQuantity != p.PackQuantity {
				return domain.NewError(domain.ErrIdempotencyKeyReused, "Idempotency-Key was used with a different request")
			}
			p, resumed = prev, true
			if st.acct == nil { // the first attempt created the account with the row
				cur, err := s.Accounts.Get(ctx, tenantID)
				if err != nil {
					return err
				}
				acct = cur
			}
			return nil
		}
		if st.trial {
			cur, err := s.trialPurchaseAccount(ctx, tenantID, acct)
			if err != nil {
				return err
			}
			acct = cur
		}
		used, err := s.Addons.ConfirmedTokensSince(ctx, tenantID, pack.PackCode, monthStart(s.now()))
		if err != nil {
			return err
		}
		if !domain.WithinCap(pack.MonthlyCapTokens, used, tokens) {
			rem, _ := domain.CapRemaining(pack.MonthlyCapTokens, used)
			return domain.NewError(domain.ErrAddonCapReached, "the monthly add-on cap is reached").With("cap_remaining_tokens", rem)
		}
		if st.trial {
			// BIL-ADD-5: counted under the tenant lock with the insert
			// (BIL-CONC-1), so two concurrent purchases cannot both fit.
			if err := s.checkTrialCap(ctx, tenantID, p); err != nil {
				return err
			}
		}
		return keyInUse(s.Addons.Insert(ctx, p))
	}); err != nil {
		return PurchaseView{}, err
	}
	if resumed && (p.Status != domain.PurchasePendingPayment || s.now().Sub(p.CreatedAt) > providerKeyTTL) {
		return addonView(p), nil // settled, or too old to replay the provider key: the callback or purchase-expiry settles it
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
		return s.addonDeclined(ctx, c, acct, p, res.Invoice.ProviderInvoiceID, code)
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
		// Stays pending; the callback or purchase-expiry settles it. The
		// invoice may exist (the error came after it was created): record it,
		// so purchase-expiry voids or settles it (§8.7).
		if id := res.Invoice.ProviderInvoiceID; id != "" {
			_ = s.locked(ctx, tenantID, func(ctx context.Context) error {
				cur, found, err := s.Addons.Get(ctx, p.PurchaseID)
				if err != nil || !found || !cur.Status.Open() || cur.ProviderInvoiceID != nil {
					return err
				}
				cur.ProviderInvoiceID = &id
				return s.Addons.Update(ctx, cur)
			})
		}
		return PurchaseView{}, providerErr(err)
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
		if !cur.Status.Open() {
			// Cancelled (offboarding) or expired while ChargeOneOff ran: never
			// reopened or granted (BIL-CONC-2, §8.8.4); a payment taken anyway
			// is parked for a refund.
			if res.Outcome.Status == port.PaymentPaid {
				s.purchasePaymentParked(ctx, tenantID, port.PurchaseKindTokenAddon, cur.PurchaseID, deref(p.ProviderInvoiceID), "paid_after_cancel", "")
			}
			p = cur
			return nil
		}
		cur.ProviderInvoiceID, cur.ClientAction, cur.Status = p.ProviderInvoiceID, p.ClientAction, p.Status
		if err == nil && res.Outcome.Status == port.PaymentPaid && cur.Status == domain.PurchasePendingPayment {
			if err := s.confirmAddon(ctx, &cur, metaFor(c)); err != nil {
				return err
			}
			s.metrics().PurchaseToEvent(port.PurchaseKindTokenAddon, 0)
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

// notEligible is 409 ADDON_NOT_ELIGIBLE with its reason (BIL-ADD-1).
func notEligible(reason domain.IneligibleReason) error {
	return domain.NewError(domain.ErrAddonNotEligible, "the tenant cannot buy add-ons now").With("ineligible_reason", string(reason))
}

// trialCustomerKey is the provider key of a trial tenant's customer, created
// on its first add-on purchase (§5.4.1 B-P20).
func trialCustomerKey(tenantID string) string { return "bsub:trial:" + tenantID + ":customer" }

// newTrialAccount creates the provider customer for a trial tenant without
// a billing account, outside any transaction (BIL-CONC-2), and returns the
// account to insert with the purchase: provider DEFAULT_PAYMENT_PROVIDER
// (certified for new accounts, BIL-PROV-1), currency DEFAULT_CURRENCY,
// iam_lifecycle trial. The currency is not fixed by the purchase (BIL-SUB-6
// fixes it only while a subscription exists).
func (s *Service) newTrialAccount(ctx context.Context, tenantID string) (domain.BillingAccount, error) {
	prov, err := s.Providers.ForNewAccount(s.Settings.NewAccountProvider)
	if err != nil {
		return domain.BillingAccount{}, err
	}
	cust, err := prov.EnsureCustomer(ctx, port.CustomerInput{TenantID: tenantID, Currency: s.Settings.DefaultCurrency}, trialCustomerKey(tenantID))
	if err != nil {
		return domain.BillingAccount{}, providerErr(err)
	}
	return domain.BillingAccount{TenantID: tenantID, Provider: s.Settings.NewAccountProvider, ProviderCustomerID: ptr(cust.ProviderCustomerID),
		Currency: s.Settings.DefaultCurrency, IAMLifecycle: domain.LifecycleTrial}, nil
}

// trialPurchaseAccount is a trial purchase's account under the tenant lock:
// the account read again (a concurrent B-P2 or purchase may have created
// it, or the tenant may have converted since the first read), or the new
// new one inserted when there still is none (accounts are never deleted, so
// that is the tenant's first purchase). The tenant must still be on trial
// (BIL-ADD-1 (b)).
func (s *Service) trialPurchaseAccount(ctx context.Context, tenantID string, acct domain.BillingAccount) (domain.BillingAccount, error) {
	cur, err := s.trialState(ctx, tenantID)
	switch {
	case err != nil:
		return acct, err
	case cur.acct == nil:
		return acct, accountCreateErr(s.Accounts.Create(ctx, acct))
	case !cur.trial:
		return acct, notEligible(domain.IneligibleNotActive)
	}
	return *cur.acct, nil
}

// checkTrialCap is BIL-ADD-5 for a trial purchase, in the purchase's locked
// transaction: what the tenant's trial purchases not failed, expired or
// cancelled use, with this one, stays within the cap in force; whichever
// of packs and amount is reached first refuses it.
func (s *Service) checkTrialCap(ctx context.Context, tenantID string, p domain.QuotaAddonPurchase) error {
	c, used, err := s.trialCap(ctx, tenantID)
	if err != nil {
		return err
	}
	if !domain.WithinTrialCap(c, used, p.PackQuantity, p.AmountMinor) {
		return trialCapReached(c, used)
	}
	return nil
}

// trialCapReached is BIL-ADD-5's 409 ADDON_CAP_REACHED with the cap in force
// and what is used.
func trialCapReached(c domain.TrialAddonCap, used domain.TrialAddonUsage) error {
	return domain.NewError(domain.ErrAddonCapReached, "the trial add-on cap is reached").With("trial_cap", map[string]any{
		"max_packs": c.MaxPacks, "max_amount_minor": c.MaxAmountMinor, "used_packs": used.Packs, "used_amount_minor": used.AmountMinor})
}

// addonDeclined is §8.5.1 "declined → failed" for an add-on: the invoice
// (created with auto_advance, so the provider would retry it) is voided
// outside the transaction first, so a later collection cannot charge for a
// failed purchase. If the void fails the purchase stays pending and
// purchase-expiry voids it; if the invoice turns out paid the purchase is
// confirmed.
func (s *Service) addonDeclined(ctx context.Context, c Caller, acct domain.BillingAccount, p domain.QuotaAddonPurchase, invoiceID, code string) (PurchaseView, error) {
	if p.CreatedAt.IsZero() { // the intent just committed
		p.CreatedAt = s.now()
	}
	inv, verr := s.closeInvoice(ctx, acct.Provider, nonEmpty(invoiceID), addonTarget(acct, p), "addon:"+p.PurchaseID+":void")
	confirmed := false
	_ = s.locked(ctx, p.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Addons.Get(ctx, p.PurchaseID)
		if err != nil || !found || !cur.Status.Open() {
			return err
		}
		if err := s.recheckInvoice(ctx, p.TenantID, p.PurchaseID, &inv); err != nil {
			return err
		}
		if inv.ID != "" {
			cur.ProviderInvoiceID = &inv.ID
		}
		switch {
		case inv.Paid:
			if err := s.confirmAddon(ctx, &cur, metaFor(c)); err != nil {
				return err
			}
			p, confirmed = cur, true
			return nil
		case verr == nil:
			cur.Status, cur.ClientAction = domain.PurchaseFailed, nil
		}
		return s.Addons.Update(ctx, cur)
	})
	if confirmed {
		return addonView(p), nil
	}
	s.metrics().AddonPurchase(p.PackCode, "declined")
	s.metrics().PaymentFailure(declineReason(code))
	return PurchaseView{}, domain.NewError(domain.ErrPaymentDeclined, "payment declined").With("decline_reason", declineReason(code))
}

// keyInUse maps a row whose client key another row already holds (a
// concurrent attempt with the same key) to 409 REQUEST_IN_PROGRESS (§9.2).
func keyInUse(err error) error {
	if errors.Is(err, port.ErrIdempotencyKeyTaken) {
		return domain.NewError(domain.ErrRequestInProgress, "a request with this Idempotency-Key is in progress")
	}
	return err
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
	if err := s.Addons.Update(ctx, *p); err != nil {
		return err
	}
	s.metrics().AddonPurchase(p.PackCode, "confirmed")
	return nil
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
		// Billing did not record the invoice id (a provider error after the
		// charge): match the purchase_id the invoice carries (§8.7).
		if p, found, err = s.addonByMetadata(ctx, tenantID, cur); err != nil {
			return "", err
		}
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
	if !invoicePaysFor(cur, domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}) {
		// A 0 or partial invoice (an empty draft the provider finalised)
		// never grants tokens (BIL-PAY-7).
		return "", s.parkMismatchedPayment(ctx, tenantID, port.PurchaseKindTokenAddon, p.PurchaseID, ev, cur)
	}
	if err := s.confirmAddon(ctx, &p, webhookMeta()); err != nil {
		return "", err
	}
	s.metrics().PurchaseToEvent(port.PurchaseKindTokenAddon, s.now().Sub(ev.ProviderCreatedAt))
	return "applied:addon_purchase", nil
}

// addonByMetadata is the add-on purchase an invoice names in its metadata,
// when Billing has no invoice id for it; the invoice id is then recorded on
// it.
func (s *Service) addonByMetadata(ctx context.Context, tenantID string, cur domain.ProviderObjectState) (domain.QuotaAddonPurchase, bool, error) {
	id := metadataPurchaseID(cur)
	if id == "" {
		return domain.QuotaAddonPurchase{}, false, nil
	}
	p, found, err := s.Addons.Get(ctx, id)
	if err != nil || !found || p.TenantID != tenantID || p.ProviderInvoiceID != nil && *p.ProviderInvoiceID != cur.Ref.ID {
		return domain.QuotaAddonPurchase{}, false, err
	}
	if claimed, err := s.invoiceClaimed(ctx, tenantID, cur.Ref.ID, p.PurchaseID); err != nil || claimed {
		return domain.QuotaAddonPurchase{}, false, err
	}
	p.ProviderInvoiceID = &cur.Ref.ID
	return p, true, nil
}

// SetTrialAddonCap is B-O18 (§5.4.4, BIL-ADD-5): a platform_operator raises
// (or lowers) a trial tenant's add-on cap within the 10-pack / USD 2,000
// ceiling. The override is upserted under the tenant lock with its AL-5
// entry billing.trial_addon_cap.changed in the same transaction. A cap
// below what is used stops further purchases and changes none made.
func (s *Service) SetTrialAddonCap(ctx context.Context, c Caller, tenantID string, r TrialAddonCapRequest) (TrialAddonCapView, error) {
	if !domain.ValidTrialCapLimits(r.MaxPacks, r.MaxAmountMinor) {
		return TrialAddonCapView{}, domain.NewError(domain.ErrValidation, "max_packs must be 1 to 10 and max_amount_minor 1 to 200000")
	}
	if n := len([]rune(r.Reason)); n < 10 || n > 1000 {
		return TrialAddonCapView{}, domain.NewError(domain.ErrValidation, "reason must be 10 to 1000 characters")
	}
	now := s.now()
	var out TrialAddonCapView
	err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		st, err := s.trialState(ctx, tenantID)
		if err != nil {
			return err
		}
		if !st.trial {
			return notEligible(domain.IneligibleNotTrial)
		}
		old, found, err := s.Addons.TrialCap(ctx, tenantID)
		if err != nil {
			return err
		}
		if !found {
			old = domain.DefaultTrialAddonCap(tenantID)
		}
		saved, err := s.Addons.UpsertTrialCap(ctx, domain.TrialAddonCap{TenantID: tenantID, MaxPacks: r.MaxPacks, MaxAmountMinor: r.MaxAmountMinor,
			Reason: r.Reason, SetBy: c.UserID, SetAt: now}, old.RecordVersion)
		if errors.Is(err, port.ErrTrialCapChanged) {
			return domain.NewError(domain.ErrConcurrentUpdate, "the trial add-on cap changed concurrently; retry")
		}
		if err != nil {
			return err
		}
		used, err := s.Addons.TrialUsage(ctx, tenantID)
		if err != nil {
			return err
		}
		out = TrialAddonCapView{TenantID: tenantID, MaxPacks: saved.MaxPacks, MaxAmountMinor: saved.MaxAmountMinor, Currency: s.Settings.DefaultCurrency,
			UsedPacks: used.Packs, UsedAmountMinor: used.AmountMinor, SetBy: saved.SetBy, SetAt: saved.SetAt}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.trial_addon_cap.changed", Action: "trial_addon_cap.changed",
			ActorID: c.UserID, TargetType: "tenant", TargetID: tenantID, SourceEventType: "TrialAddonCapChanged",
			NaturalID:  tenantID + ":" + strconv.FormatInt(saved.RecordVersion, 10) + ":" + strconv.FormatInt(now.UnixNano(), 10),
			OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"tenant_id": tenantID, "old_max_packs": old.MaxPacks, "new_max_packs": saved.MaxPacks,
				"old_max_amount_minor": old.MaxAmountMinor, "new_max_amount_minor": saved.MaxAmountMinor, "reason": r.Reason, "operator_id": c.UserID}})
	})
	return out, err
}

// trialState is whether the tenant is on trial (no live subscription;
// iam_lifecycle trial or no account), in the caller's transaction.
func (s *Service) trialState(ctx context.Context, tenantID string) (addonState, error) {
	var st addonState
	a, found, err := s.accountIfAny(ctx, tenantID)
	if err != nil || !found {
		st.trial = err == nil
		return st, err
	}
	st.acct = &a
	live, ok, err := s.Subs.Live(ctx, tenantID)
	if err != nil {
		return st, err
	}
	if ok && live.Status != domain.StatusIncomplete {
		st.sub = &live
	}
	st.trial = domain.IsTrialTenant(subStatus(st.sub), st.lifecycle())
	return st, nil
}
