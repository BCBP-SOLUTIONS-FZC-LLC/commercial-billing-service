package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// ── B-P1 ───────────────────────────────────────────────────────────────────

// GetSubscription is B-P1.
func (s *Service) GetSubscription(ctx context.Context, c Caller, tenantID string) (SubscriptionView, error) {
	var view SubscriptionView
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var acct *domain.BillingAccount
		if a, err := s.Accounts.Get(ctx, tenantID); err == nil {
			acct = &a
		} else if !domain.HasCode(err, domain.ErrSubscriptionNotFound) {
			return err
		}
		sub, found, err := s.Subs.Latest(ctx, tenantID)
		if err != nil {
			return err
		}
		if !found || sub.Status == domain.StatusIncomplete {
			view = SubscriptionView{Status: "none", Trial: acct == nil || acct.IAMLifecycle == domain.LifecycleTrial,
				OpenPurchases: []PurchaseView{}}
			view.CallerPermissions, view.ActionsAvailable = CallerPermissions(c), s.actionsFor(c, nil, acct)
			return nil
		}
		view, err = s.subscriptionView(ctx, c, sub, acct)
		return err
	})
	return view, err
}

// subscriptionView renders §5.4.1 SubscriptionView for sub.
func (s *Service) subscriptionView(ctx context.Context, c Caller, sub domain.Subscription, acct *domain.BillingAccount) (SubscriptionView, error) {
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return SubscriptionView{}, err
	}
	price, err := s.Prices.Get(ctx, item.PriceID)
	if err != nil {
		return SubscriptionView{}, err
	}
	unit := MoneyFrom(domain.Money{AmountMinor: price.UnitAmountMinor, Currency: price.Currency})
	v := SubscriptionView{
		BillingSubscriptionID: sub.BillingSubscriptionID, TenantID: sub.TenantID, Provider: sub.Provider, Origin: string(sub.Origin),
		Plan: string(sub.Plan), Interval: string(sub.Interval), LicensedSeats: item.Quantity, UnitPrice: &unit,
		Status: string(sub.Status), AutoRenew: sub.AutoRenew, CurrentPeriodStart: sub.CurrentPeriodStart,
		CurrentPeriodEnd: sub.CurrentPeriodEnd, CollectionPaused: sub.CollectionPaused, PastDueSince: sub.PastDueSince,
		CancelledAt: sub.CancelledAt, OpenPurchases: []PurchaseView{}, RecordVersion: sub.RecordVersion,
	}
	renewalUnit := price.UnitAmountMinor
	if item.ScheduledPriceID != nil && item.ScheduledPriceAt != nil {
		if sp, err := s.Prices.Get(ctx, *item.ScheduledPriceID); err == nil {
			v.ScheduledUnitPrice = &ScheduledPrice{Money: MoneyFrom(domain.Money{AmountMinor: sp.UnitAmountMinor, Currency: sp.Currency}),
				EffectiveAt: *item.ScheduledPriceAt}
			renewalUnit = sp.UnitAmountMinor
		}
	}
	if sub.Status == domain.StatusActive || sub.Status == domain.StatusPastDue {
		v.RenewalEstimate = &RenewalEstimate{Seats: item.Quantity,
			Amount: MoneyFrom(domain.Money{AmountMinor: renewalUnit * int64(item.Quantity), Currency: price.Currency})}
	}
	if err := s.inFlight(ctx, sub, &v); err != nil {
		return SubscriptionView{}, err
	}
	v.CallerPermissions, v.ActionsAvailable = CallerPermissions(c), s.actionsFor(c, &sub, acct)
	return v, nil
}

// inFlight fills the view's pending plan change, scheduled seat reduction
// and open seat / add-on purchases (§5.4.1 SubscriptionView; F-11, rev
// 0.71). A purchase's client_action is never included: client_secret goes
// only to the caller that started the payment (BIL-API-4), and this view is
// read by anyone with billing.view.
func (s *Service) inFlight(ctx context.Context, sub domain.Subscription, v *SubscriptionView) error {
	// The repository reads below lock rows (FOR UPDATE). Taking the tenant
	// lock first orders them like every writer's (BIL-CONC-1), so a read
	// never deadlocks a writer.
	if err := s.Lock.LockTenant(ctx, sub.TenantID); err != nil {
		return err
	}
	if ch, ok, err := s.Changes.Open(ctx, sub.TenantID); err != nil {
		return err
	} else if ok && ch.BillingSubscriptionID == sub.BillingSubscriptionID {
		bl := make([]Blocker, 0, len(ch.BlockerCodes))
		for _, code := range ch.BlockerCodes {
			bl = append(bl, Blocker{Code: code})
		}
		v.PendingPlanChange = &PlanChangeView{ChangeID: ch.ChangeID, Direction: string(ch.Direction), FromPlan: string(ch.FromPlan),
			ToPlan: string(ch.ToPlan), Status: string(ch.Status), EffectiveAt: ch.EffectiveAt, Blockers: bl}
	}
	if sch, ok, err := s.Schedules.Open(ctx, sub.BillingSubscriptionID); err != nil {
		return err
	} else if ok {
		v.ScheduledReduction = &ScheduleView{ScheduleID: sch.ScheduleID, TargetSeats: sch.TargetSeats, EffectiveAt: sch.EffectiveAt,
			Status: string(sch.Status), SeatsInUse: sch.SeatsInUseAtApply, RenewalEstimateSeats: sch.RenewalSeats}
	}
	if p, ok, err := s.SeatPurchases.Open(ctx, sub.BillingSubscriptionID); err != nil {
		return err
	} else if ok {
		pv := seatView(p)
		pv.ClientAction = nil
		v.OpenPurchases = append(v.OpenPurchases, pv)
	}
	if s.Addons == nil {
		return nil
	}
	addons, err := s.Addons.List(ctx, sub.TenantID, "", 100)
	if err != nil {
		return err
	}
	for _, p := range addons {
		if p.Status.Open() {
			pv := addonView(p)
			pv.ClientAction = nil
			v.OpenPurchases = append(v.OpenPurchases, pv)
		}
	}
	return nil
}

// ── B-P2 ───────────────────────────────────────────────────────────────────

// StartCheckout is B-P2 (§8.1.1).
func (s *Service) StartCheckout(ctx context.Context, c Caller, tenantID string, r CheckoutRequest) (CheckoutResult, error) {
	ctx, end := StartSpan(ctx, "billing.checkout.start", AttrTenant(tenantID))
	out, err := s.startCheckout(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) startCheckout(ctx context.Context, c Caller, tenantID string, r CheckoutRequest) (CheckoutResult, error) {
	plan, interval := domain.PlanCode(r.Plan), domain.BillingInterval(r.Interval)
	if r.LicensedSeats > s.Settings.MaxSeatsPerPurchase {
		return CheckoutResult{}, domain.NewError(domain.ErrValidation, "licensed_seats above MAX_SEATS_PER_PURCHASE")
	}
	// Fail closed on I-11: a checkout below seats in use would create an
	// over-cap tenant (fresh read, §6).
	usage, err := s.Core.SeatUsage(ctx, tenantID)
	if err != nil {
		return CheckoutResult{}, err
	}
	if r.LicensedSeats < usage.InUse() {
		return CheckoutResult{}, domain.NewError(domain.ErrSeatsBelowInUse, "licensed_seats is below the seats in use").
			With("seats_in_use", usage.InUse())
	}

	// The account's customer, or a new one (outside any transaction,
	// BIL-CONC-2; billing_accounts needs the customer id, chk_ba_customer).
	var acct domain.BillingAccount
	var hasAccount bool
	var prior domain.CheckoutSession
	var hasPrior bool
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		a, err := s.Accounts.Get(ctx, tenantID)
		if domain.HasCode(err, domain.ErrSubscriptionNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		acct, hasAccount = a, true
		prior, hasPrior, err = s.Checkouts.Open(ctx, tenantID, domain.PurposeConversion)
		return err
	}); err != nil {
		return CheckoutResult{}, err
	}
	providerName := s.Settings.NewAccountProvider
	if hasAccount {
		providerName = acct.Provider
	}
	prov, err := s.Providers.ForNewAccount(providerName)
	if err != nil {
		return CheckoutResult{}, err
	}
	currency := s.Settings.DefaultCurrency
	if hasAccount {
		currency = acct.Currency
	}
	if !prov.Capabilities().HostedCardEntry {
		return CheckoutResult{}, domain.NewError(domain.ErrProviderOperationUnsupported, "the provider has no hosted checkout")
	}
	subID := newID()
	if !hasAccount {
		cust, err := prov.EnsureCustomer(ctx, port.CustomerInput{TenantID: tenantID, Currency: currency}, "bsub:"+subID+":customer")
		if err != nil {
			return CheckoutResult{}, providerErr(err)
		}
		acct = domain.BillingAccount{TenantID: tenantID, Provider: providerName, ProviderCustomerID: ptr(cust.ProviderCustomerID),
			Currency: currency, IAMLifecycle: domain.LifecycleTrial}
	}

	// Transaction 1: account, incomplete subscription, seat item, session.
	now := s.now()
	for attempt := 1; ; attempt++ {
		// A replaced session is expired at the provider first, outside any
		// transaction (BIL-CONC-2), so it can no longer be paid (§8.1.1).
		expired, err := s.expireSuperseded(ctx, prov, prior, hasPrior, plan, interval, r.LicensedSeats)
		if err != nil {
			return CheckoutResult{}, err
		}
		out, err := s.checkoutTx1(ctx, c, tenantID, r, now, expired, acct, hasAccount, subID)
		switch {
		case errors.Is(err, errCheckoutRace) && attempt == 1:
			// Another B-P2 opened a session since the first read: expire
			// that one too, then try once more.
			prior, hasPrior = out.blocking, true
			continue
		case errors.Is(err, errCheckoutRace):
			return CheckoutResult{}, domain.NewError(domain.ErrConcurrentUpdate, "another checkout was started concurrently; retry")
		case err != nil:
			return CheckoutResult{}, err
		}
		return s.finishCheckout(ctx, prov, tenantID, out, r.LicensedSeats)
	}
}

// errCheckoutRace is transaction 1 finding an open session, created since
// B-P2's first read, that the provider may still accept.
var errCheckoutRace = errors.New("an open checkout appeared concurrently")

// checkoutTx1Out is what B-P2's transaction 1 decided.
type checkoutTx1Out struct {
	acct     domain.BillingAccount
	subID    string
	session  domain.CheckoutSession
	price    domain.PlanPrice
	blocking domain.CheckoutSession // with errCheckoutRace
}

// checkoutTx1 is B-P2's transaction 1 under the tenant lock: account,
// incomplete subscription, seat item, session; or the reused open session.
// expired names the session expireSuperseded expired at the provider; any
// other open session the provider may still accept is returned as blocking
// with errCheckoutRace, and nothing is written.
func (s *Service) checkoutTx1(ctx context.Context, c Caller, tenantID string, r CheckoutRequest, now time.Time, expired string,
	acct domain.BillingAccount, hasAccount bool, subID string) (checkoutTx1Out, error) {
	plan, interval := domain.PlanCode(r.Plan), domain.BillingInterval(r.Interval)
	var (
		session  domain.CheckoutSession
		price    domain.PlanPrice
		blocking domain.CheckoutSession
	)
	err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		if !hasAccount {
			// Re-read under the tenant lock: a concurrent first checkout may
			// have created the account since the read above. Checking here,
			// rather than recovering from the insert's unique violation, keeps
			// the transaction usable (a failed statement aborts it).
			existing, found, err := s.accountIfAny(ctx, tenantID)
			if err != nil {
				return err
			}
			if found {
				acct = existing
			} else if err := s.Accounts.Create(ctx, acct); err != nil {
				return accountCreateErr(err)
			}
		}
		if acct.IAMLifecycle != domain.LifecycleTrial {
			return domain.NewError(domain.ErrSubscriptionAlreadyExists, "the tenant is not on trial")
		}
		var ok bool
		var err error
		price, ok, err = s.Prices.PriceAt(ctx, plan, interval, acct.Currency, acct.Provider, now)
		if err != nil {
			return err
		}
		if !ok {
			return domain.NewError(domain.ErrPriceNotAvailable, "no price for this plan, interval and currency")
		}
		live, found, err := s.Subs.Live(ctx, tenantID)
		if err != nil {
			return err
		}
		if found && live.Status != domain.StatusIncomplete {
			return domain.NewError(domain.ErrSubscriptionAlreadyExists, "the tenant already has a subscription").
				With("billing_subscription_id", live.BillingSubscriptionID)
		}
		open, hasOpen, err := s.Checkouts.Open(ctx, tenantID, domain.PurposeConversion)
		if err != nil {
			return err
		}
		if hasOpen && open.SameRequest(plan, interval, r.LicensedSeats) && open.ExpiresAt.After(now) && open.BillingSubscriptionID != nil {
			session, subID = open, *open.BillingSubscriptionID
			return nil
		}
		if hasOpen {
			// Re-checked under the lock: a session the provider may still
			// accept (another B-P2 created it since the first read) is not
			// superseded before it is expired at the provider.
			if open.SessionID != expired && open.ExpiresAt.After(now) && !strings.HasPrefix(open.ProviderSessionID, domain.PendingSessionPrefix) {
				blocking = open
				return errCheckoutRace
			}
			if err := s.Checkouts.SetStatus(ctx, open.SessionID, domain.SessionExpired); err != nil {
				return err
			}
		}
		if found { // the earlier incomplete subscription is abandoned (§8.1.3)
			if err := s.endIncomplete(ctx, live, "api:B-P2", actorOf(c)); err != nil {
				return err
			}
		}
		sub := domain.Subscription{BillingSubscriptionID: subID, TenantID: tenantID, Provider: acct.Provider,
			Origin: domain.OriginTrialConversion, Plan: plan, Interval: interval, Status: domain.StatusIncomplete, AutoRenew: true}
		if err := s.Subs.Create(ctx, sub, domain.SubscriptionItem{PriceID: price.PriceID, Quantity: r.LicensedSeats}); err != nil {
			return err
		}
		sessionID := newID()
		session = domain.CheckoutSession{SessionID: sessionID, TenantID: tenantID, Purpose: domain.PurposeConversion,
			ProviderSessionID: domain.PendingSessionPrefix + sessionID, BillingSubscriptionID: &subID, RequestedPlan: &plan,
			RequestedInterval: &interval, RequestedSeats: ptr(r.LicensedSeats), RequestedBy: actorOf(c),
			ExpiresAt: now.Add(s.Settings.CheckoutTTL)}
		return s.Checkouts.Insert(ctx, session)
	})
	return checkoutTx1Out{acct: acct, subID: subID, session: session, price: price, blocking: blocking}, err
}

// finishCheckout creates the hosted session outside any transaction and
// records it in transaction 2.
func (s *Service) finishCheckout(ctx context.Context, prov port.PaymentProvider, tenantID string, out checkoutTx1Out, seats int) (CheckoutResult, error) {
	var result CheckoutResult
	acct, session, subID, price := out.acct, out.session, out.subID, out.price
	total := MoneyFrom(domain.Money{AmountMinor: price.UnitAmountMinor * int64(seats), Currency: price.Currency})
	// Outside: the hosted session (idempotent on the subscription id).
	hosted, err := s.createConversionCheckout(ctx, prov, acct, session, subID, price, seats)
	if err != nil {
		return result, err
	}
	// Transaction 2 records the provider session, re-reading ours under the
	// lock: a concurrent B-P2 may have superseded it while it was created.
	// It is then expired at the provider instead of being handed out.
	status := ""
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, found, err := s.Checkouts.GetSession(ctx, session.SessionID)
		if err != nil || !found {
			return err
		}
		status = cur.Status
		return s.Checkouts.SetProviderSession(ctx, session.SessionID, hosted.ProviderSessionID, hosted.ExpiresAt)
	}); err != nil {
		return result, err
	}
	switch status {
	case domain.SessionOpen:
	case domain.SessionCompleted: // a reused session the customer has just paid
		return result, domain.NewError(domain.ErrSubscriptionAlreadyExists, "the checkout was completed; its payment is being confirmed")
	default: // replaced (or removed) meanwhile: never hand it out
		if err := prov.ExpireCheckout(ctx, hosted.ProviderSessionID, "bsub:"+subID+":expire"); err != nil && !errors.Is(err, port.ErrCheckoutCompleted) {
			return result, providerErr(err)
		}
		return result, domain.NewError(domain.ErrConcurrentUpdate, "the checkout was replaced concurrently; retry")
	}
	return CheckoutResult{CheckoutSessionID: session.SessionID, CheckoutURL: hosted.URL, ExpiresAt: hosted.ExpiresAt,
		BillingSubscriptionID: subID, Price: total}, nil
}

// expireSuperseded expires, at the provider, the open conversion session a
// B-P2 with other content replaces, and returns its session id ("" when
// nothing was expired). A session still being created ('pending:'
// placeholder) has no provider id yet; its own B-P2 expires it when it
// finds it superseded. A session the customer already completed is being
// paid: 409 SUBSCRIPTION_ALREADY_EXISTS, nothing is replaced.
func (s *Service) expireSuperseded(ctx context.Context, prov port.PaymentProvider, open domain.CheckoutSession, hasOpen bool,
	plan domain.PlanCode, interval domain.BillingInterval, seats int) (string, error) {
	switch {
	case !hasOpen, strings.HasPrefix(open.ProviderSessionID, domain.PendingSessionPrefix), !open.ExpiresAt.After(s.now()):
		return "", nil
	case open.SameRequest(plan, interval, seats) && open.BillingSubscriptionID != nil:
		return "", nil // reused, not replaced
	}
	key := "checkout:" + open.SessionID + ":expire"
	if open.BillingSubscriptionID != nil {
		key = "bsub:" + *open.BillingSubscriptionID + ":expire"
	}
	err := prov.ExpireCheckout(ctx, open.ProviderSessionID, key)
	switch {
	case errors.Is(err, port.ErrCheckoutCompleted):
		return "", domain.NewError(domain.ErrSubscriptionAlreadyExists, "the earlier checkout was completed; its payment is being confirmed")
	case err != nil:
		return "", providerErr(err)
	}
	return open.SessionID, nil
}

// createConversionCheckout creates (or, for the same subscription, returns
// again: the call is idempotent on bsub:<id>:checkout) the hosted session.
func (s *Service) createConversionCheckout(ctx context.Context, prov port.PaymentProvider, acct domain.BillingAccount,
	session domain.CheckoutSession, subID string, price domain.PlanPrice, seats int) (port.HostedSession, error) {
	if price.ProviderPriceID == nil {
		return port.HostedSession{}, domain.NewError(domain.ErrPriceNotAvailable, "the price has no provider price")
	}
	base := strings.TrimRight(s.Settings.TenantAppBaseURL, "/") + "/billing"
	hosted, err := prov.CreateHostedCheckout(ctx, port.CheckoutInput{
		Customer: port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)}, Purpose: port.CheckoutConversion,
		PriceRef: *price.ProviderPriceID, Quantity: seats, Currency: acct.Currency,
		Metadata:   map[string]string{"tenant_id": acct.TenantID, "billing_subscription_id": subID, "purpose": domain.PurposeConversion},
		SuccessURL: base + "?checkout=success", CancelURL: base + "?checkout=cancelled", ExpiresAt: session.ExpiresAt,
	}, "bsub:"+subID+":checkout")
	if err != nil {
		return hosted, providerErr(err)
	}
	return hosted, nil
}

// endIncomplete closes an abandoned incomplete subscription (§8.1.3).
func (s *Service) endIncomplete(ctx context.Context, sub domain.Subscription, cause, actor string) error {
	now := s.now()
	from := sub.Status
	ended, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusEnded, func(x *domain.Subscription) { x.EndedAt = &now })
	if err != nil {
		return err
	}
	return s.history(ctx, ended, &from, cause, actor, nil, nil, nil)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ── Callbacks (§7.4, §8.1.1) ───────────────────────────────────────────────

// eventRef is the history payment_event_ref of a callback.
func eventRef(ev domain.PaymentEvent) *string { return ptr(ev.Provider + ":" + ev.ProviderEventID) }

// CheckoutCompleted links the provider subscription to the incomplete one
// (§8.1.1 step 3); a payment-method setup session marks the account.
type CheckoutCompleted struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h CheckoutCompleted) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	session, found, err := s.Checkouts.ByProviderSession(ctx, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found || session.TenantID != tenantID {
		return "", &port.IgnoredEvent{Reason: "unknown_session"}
	}
	if session.Status == domain.SessionCompleted {
		return "", &port.IgnoredEvent{Reason: "already_completed"}
	}
	providerSub, _ := cur.Fields["subscription"].(string)
	if session.Status == domain.SessionExpired && session.BillingSubscriptionID != nil && providerSub != "" {
		// A session Billing superseded or expired was paid anyway. Parked for
		// an operator (§8.1.1) only when its subscription is ended or linked
		// to another provider subscription; a completion of the provider
		// subscription already linked (invoice.paid was handled first and
		// confirmed the payment) is a no-op, never a refund.
		sub, err := s.Subs.Get(ctx, *session.BillingSubscriptionID)
		if err != nil {
			return "", err
		}
		switch {
		case sub.ProviderSubscriptionID != nil && *sub.ProviderSubscriptionID == providerSub && sub.Status != domain.StatusEnded:
			return "", &port.IgnoredEvent{Reason: "already_linked"}
		case sub.Status == domain.StatusEnded, sub.ProviderSubscriptionID != nil:
			return "", s.paymentNeedsDecision(ctx, tenantID, ev, *session.BillingSubscriptionID, "checkout_superseded")
		}
	}
	if err := s.Checkouts.SetStatus(ctx, session.SessionID, domain.SessionCompleted); err != nil {
		return "", err
	}
	if session.BillingSubscriptionID == nil || providerSub == "" {
		return "applied:session_completed", nil
	}
	linked, err := s.Subs.Update(ctx, *session.BillingSubscriptionID, 0, func(x *domain.Subscription) {
		// A reactivation replaces the deleted provider subscription (§8.9).
		if x.ProviderSubscriptionID == nil || session.Purpose == domain.PurposeReactivation {
			x.ProviderSubscriptionID = &providerSub
		}
	})
	if err != nil {
		return "", err
	}
	if session.Purpose == domain.PurposeReactivation {
		if err := s.applyReactivationPrice(ctx, linked, session); err != nil {
			return "", err
		}
	}
	// The re-read session carries its first invoice (BIL-PAY-4). If that is
	// already paid, the payment is confirmed here, whatever the order and
	// fate of the invoice.paid callback (§9.4).
	if paid, _ := cur.Fields["invoice_status"].(string); paid == "paid" && session.Purpose == domain.PurposeConversion &&
		linked.Status == domain.StatusIncomplete && linked.Origin == domain.OriginTrialConversion &&
		linked.ProviderSubscriptionID != nil && *linked.ProviderSubscriptionID == providerSub {
		if err := s.confirmConversionPayment(ctx, linked, ev, cur.Fields["invoice_line_period_start"], cur.Fields["invoice_line_period_end"],
			cur.Fields["invoice_line_quantity"]); err != nil {
			return "", err
		}
		return "applied:conversion_payment", nil
	}
	return "applied:subscription_linked", nil
}

// applyReactivationPrice records what the B-P14 checkout bought (F-4, rev
// 0.71): the new provider subscription runs on the requested interval (the
// previous one by default) at the price row current when the checkout was
// created, so the subscription's interval and the seat item's price_id follow
// it. Otherwise renewals, previews and schedules would price the old
// interval or an expired row (BIL-SUB-8).
func (s *Service) applyReactivationPrice(ctx context.Context, sub domain.Subscription, session domain.CheckoutSession) error {
	interval := sub.Interval
	if session.RequestedInterval != nil {
		interval = *session.RequestedInterval
	}
	acct, err := s.Accounts.Get(ctx, sub.TenantID)
	if err != nil {
		return err
	}
	price, ok, err := s.Prices.PriceAt(ctx, sub.Plan, interval, acct.Currency, sub.Provider, session.CreatedAt)
	if err != nil || !ok {
		return err // B-P14 priced it from this row; none means the price book was edited since: keep the item
	}
	if interval != sub.Interval {
		if _, err := s.Subs.Update(ctx, sub.BillingSubscriptionID, 0, func(x *domain.Subscription) { x.Interval = interval }); err != nil {
			return err
		}
	}
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil || item.PriceID == price.PriceID && item.ScheduledPriceID == nil {
		return err
	}
	// A renewal reprice scheduled on the deleted provider subscription no
	// longer applies (§8.16).
	item.PriceID, item.ScheduledPriceID, item.ScheduledPriceAt = price.PriceID, nil, nil
	return s.Subs.SetItem(ctx, item)
}

// CheckoutExpired abandons the session's incomplete subscription (§8.1.3).
type CheckoutExpired struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h CheckoutExpired) Handle(ctx context.Context, tenantID string, _ domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	session, found, err := s.Checkouts.ByProviderSession(ctx, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found || session.Status != domain.SessionOpen {
		return "", &port.IgnoredEvent{Reason: "session_not_open"}
	}
	if err := s.Checkouts.SetStatus(ctx, session.SessionID, domain.SessionExpired); err != nil {
		return "", err
	}
	if session.BillingSubscriptionID != nil {
		sub, err := s.Subs.Get(ctx, *session.BillingSubscriptionID)
		if err != nil {
			return "", err
		}
		if sub.Status == domain.StatusIncomplete {
			if err := s.endIncomplete(ctx, sub, "webhook:checkout.session.expired", domain.CommercialSystemPrincipal); err != nil {
				return "", err
			}
		}
	}
	return "applied:checkout_expired", nil
}

// FirstInvoicePaid confirms payment of a conversion (§8.1.1 step 4): the
// subscription becomes active and the saga starts in rp_convert_pending
// (BIL-SAGA-1: only on a re-read, verified payment). No billing.events.
// Other invoice.paid cases belong to later flows and are passed to Next.
type FirstInvoicePaid struct {
	S    *Service
	Next port.PaymentEventHandler
}

// Handle implements port.PaymentEventHandler.
func (h FirstInvoicePaid) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if cur.Status != "paid" {
		return "", &port.IgnoredEvent{Reason: "invoice_not_paid"}
	}
	providerSub, _ := cur.Fields["subscription"].(string)
	if providerSub == "" {
		return h.next(ctx, tenantID, ev, cur)
	}
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	sub, found, err := s.Subs.ByProvider(ctx, ev.Provider, providerSub)
	if err != nil {
		return "", err
	}
	if found && sub.TenantID != tenantID {
		return "", &port.IgnoredEvent{Reason: "unknown_subscription"}
	}
	if !found {
		// Out of order (§9.4): invoice.paid processed before
		// checkout.session.completed linked the provider subscription.
		if sub, err = h.resolveUnlinked(ctx, tenantID, ev, cur, providerSub); err != nil {
			return "", err
		}
	}
	if sub.Status != domain.StatusIncomplete || sub.Origin != domain.OriginTrialConversion {
		return h.next(ctx, tenantID, ev, cur)
	}
	if err := s.confirmConversionPayment(ctx, sub, ev, cur.Fields["line_period_start"], cur.Fields["line_period_end"], cur.Fields["line_quantity"]); err != nil {
		return "", err
	}
	return "applied:conversion_payment", nil
}

// confirmConversionPayment is §8.1.1 step 4 for an incomplete trial
// conversion whose first invoice the provider reports paid (re-read,
// BIL-SAGA-1): active, payment_confirmed_at, the provider's period, the
// purchased quantity, history, saga in rp_convert_pending. No billing.events.
func (s *Service) confirmConversionPayment(ctx context.Context, sub domain.Subscription, ev domain.PaymentEvent, periodStart, periodEnd, quantity any) error {
	now := s.now()
	start, _ := periodStart.(time.Time)
	end, _ := periodEnd.(time.Time)
	from := sub.Status
	active, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusActive, func(x *domain.Subscription) {
		x.PaymentConfirmedAt = &now
		if !start.IsZero() {
			x.CurrentPeriodStart, x.CurrentPeriodEnd = &start, &end
		}
	})
	if err != nil {
		return err
	}
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return err
	}
	if q, ok := intField(quantity); ok && q > 0 && q != item.Quantity { // the purchased quantity wins
		item.Quantity = q
		if err := s.Subs.SetItem(ctx, item); err != nil {
			return err
		}
	}
	if err := s.history(ctx, active, &from, "webhook:"+ev.EventType, domain.CommercialSystemPrincipal, eventRef(ev),
		&[2]*int{nil, &item.Quantity}, nil); err != nil {
		return err
	}
	// The conversion's checkout session is completed with its payment, so
	// purchase-expiry never expires a paid session (whichever of
	// invoice.paid and checkout.session.completed comes first).
	if open, found, err := s.Checkouts.Open(ctx, sub.TenantID, domain.PurposeConversion); err != nil {
		return err
	} else if found && open.BillingSubscriptionID != nil && *open.BillingSubscriptionID == sub.BillingSubscriptionID {
		if err := s.Checkouts.SetStatus(ctx, open.SessionID, domain.SessionCompleted); err != nil {
			return err
		}
	}
	return s.startSaga(ctx, active, domain.SagaTrialConversion, now)
}

// resolveUnlinked finds the Billing subscription of a paid invoice whose
// provider subscription is not linked yet, through the
// billing_subscription_id Billing set in the provider subscription's
// metadata (§8.1.1), read from the re-read invoice (BIL-PAY-4). The tenant
// comes from the customer mapping (BIL-PAY-3) and the read runs under the
// tenant's RLS, so the metadata cannot reach another tenant's subscription.
//   - an incomplete trial conversion with no provider subscription is linked
//     here, and its payment is then confirmed;
//   - an ended subscription (a superseded or expired checkout paid anyway)
//     is parked for an operator decision;
//   - a subscription linked to another provider subscription: the first
//     invoice of a cancelled subscription's new provider subscription (a
//     B-P14 reactivation whose checkout completion is not processed yet)
//     waits for its link (retryable); any other invoice (a late payment on
//     a provider subscription a reactivation replaced) is ignored with
//     superseded_provider_subscription, never retried to SEV-3;
//   - any other live subscription without a link (a direct signup not yet
//     committed) waits for its link: retryable;
//   - no usable metadata: every provider subscription Billing creates
//     carries billing_subscription_id (conversion and reactivation
//     checkouts, B-I1, B-O1), so none can ever be linked to this one (a
//     dashboard-created subscription): ignored with unknown_subscription.
func (h FirstInvoicePaid) resolveUnlinked(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState,
	providerSub string) (domain.Subscription, error) {
	s := h.S
	notLinked := domain.NewError(domain.ErrSubscriptionNotFound, "the provider subscription is not linked yet; retried")
	bsID, _ := cur.Fields["billing_subscription_id"].(string)
	if bsID == "" || uuid.Validate(bsID) != nil {
		return domain.Subscription{}, &port.IgnoredEvent{Reason: "unknown_subscription"}
	}
	sub, err := s.Subs.Get(ctx, bsID)
	if domain.HasCode(err, domain.ErrSubscriptionNotFound) {
		return sub, &port.IgnoredEvent{Reason: "unknown_subscription"}
	}
	if err != nil {
		return sub, err
	}
	if sub.TenantID != tenantID {
		return sub, &port.IgnoredEvent{Reason: "unknown_subscription"}
	}
	switch {
	case sub.Status == domain.StatusIncomplete && sub.Origin == domain.OriginTrialConversion && sub.ProviderSubscriptionID == nil:
		return s.Subs.Update(ctx, sub.BillingSubscriptionID, 0, func(x *domain.Subscription) { x.ProviderSubscriptionID = &providerSub })
	case sub.Status == domain.StatusEnded:
		return sub, s.paymentNeedsDecision(ctx, tenantID, ev, sub.BillingSubscriptionID, "paid_after_end")
	case sub.ProviderSubscriptionID != nil && (sub.Status != domain.StatusCancelled || !firstInvoiceOfSubscription(cur)):
		if s.Log != nil {
			s.logger(ctx).Warn("invoice paid on a provider subscription Billing no longer links; not applied", map[string]any{
				"tenant_id": tenantID, "billing_subscription_id": sub.BillingSubscriptionID, "provider": ev.Provider,
				"provider_event_id": ev.ProviderEventID})
		}
		return sub, &port.IgnoredEvent{Reason: "superseded_provider_subscription"}
	}
	return sub, notLinked
}

// paymentNeedsDecision raises the operator alert for a provider payment
// Billing must not apply (a superseded or expired checkout paid anyway) and
// returns the ParkedEvent that leaves the ledger row failed:<reason> with no
// retry (§8.15 step 4, BIL-OPS-6), like failed:unknown_customer. The
// operator cancels the stray provider subscription and refunds its payment
// in the provider dashboard; the row stays as the record of the incident (a
// B-O13 replay parks it again, since Billing's subscription stays ended).
func (s *Service) paymentNeedsDecision(ctx context.Context, tenantID string, ev domain.PaymentEvent, billingSubscriptionID, reason string) error {
	if s.Log != nil {
		s.logger(ctx).Error("provider payment on a checkout Billing replaced or ended; operator decision needed (SEV-2)", map[string]any{
			"tenant_id": tenantID, "billing_subscription_id": billingSubscriptionID, "provider": ev.Provider,
			"provider_event_id": ev.ProviderEventID, "reason": reason, "alert": "BillingPaymentParked", "severity": "SEV-2"})
	}
	return &port.ParkedEvent{Reason: reason}
}

func (h FirstInvoicePaid) next(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	if h.Next == nil {
		return "", &port.IgnoredEvent{Reason: "no_handler"}
	}
	return h.Next.Handle(ctx, tenantID, ev, cur)
}

// startSaga creates the saga directly in rp_convert_pending (§9.3.1) for a
// trial conversion or the operator stub; once per subscription (BIL-SAGA-2).
func (s *Service) startSaga(ctx context.Context, sub domain.Subscription, kind domain.SagaKind, confirmedAt time.Time) error {
	if _, found, err := s.Sagas.BySubscription(ctx, sub.BillingSubscriptionID); err != nil || found {
		return err
	}
	key := domain.RP3IdempotencyKey(sub.TenantID, sub.BillingSubscriptionID)
	return s.Sagas.Insert(ctx, domain.ConversionSaga{SagaID: newID(), TenantID: sub.TenantID, BillingSubscriptionID: sub.BillingSubscriptionID,
		Kind: kind, State: domain.SagaRPConvertPending, PaymentConfirmedAt: confirmedAt, RPIdempotencyKey: &key, NextAttemptAt: &confirmedAt})
}

func intField(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

// ── Saga worker (§8.1.2) ───────────────────────────────────────────────────

// RunSagas calls RP-3 for each due saga: claim under the tenant lock
// (pushing next_attempt_at past the RP timeout so other replicas skip it),
// call RP-3 outside the transaction (BIL-CONC-2), then apply the answer
// under the lock after re-reading the saga. Returns sagas handled.
func (s *Service) RunSagas(ctx context.Context, limit int) (int, error) {
	due, err := s.Sagas.Due(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	for _, ref := range due {
		if err := s.runSaga(ctx, ref); err != nil && s.Log != nil {
			s.logger(ctx).Warn("saga attempt failed; retried on the next pass", map[string]any{"saga_id": ref.SagaID, "tenant_id": ref.TenantID, "error": err.Error()})
		}
	}
	return len(due), nil
}

func (s *Service) runSaga(ctx context.Context, ref port.SagaRef) error {
	ctx, end := StartSpan(ctx, "billing.saga.step", AttrTenant(ref.TenantID), AttrSagaID(ref.SagaID))
	err := s.runSagaStep(ctx, ref)
	end(err)
	return err
}

func (s *Service) runSagaStep(ctx context.Context, ref port.SagaRef) error {
	var saga domain.ConversionSaga
	var sub domain.Subscription
	claimed := false
	if err := s.locked(ctx, ref.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Sagas.Get(ctx, ref.SagaID)
		if err != nil || !found {
			return err
		}
		now := s.now()
		if (cur.State != domain.SagaRPConvertPending && cur.State != domain.SagaReverting) || cur.NextAttemptAt == nil || cur.NextAttemptAt.After(now) {
			return nil
		}
		lease := now.Add(s.Settings.SagaClaim)
		cur.NextAttemptAt = &lease
		if err := s.Sagas.Update(ctx, cur); err != nil {
			return err
		}
		if sub, err = s.Subs.Get(ctx, cur.BillingSubscriptionID); err != nil {
			return err
		}
		saga, claimed = cur, true
		return nil
	}); err != nil || !claimed {
		return err
	}
	if saga.State == domain.SagaReverting {
		return s.runReversalStep(ctx, saga, sub)
	}

	ans := s.RP.Convert(ctx, saga.TenantID, port.ConvertRequest{Plan: sub.Plan, BillingSubscriptionID: sub.BillingSubscriptionID},
		deref(saga.RPIdempotencyKey))
	action := domain.ClassifyRP3(ans)
	s.metrics().RPConvertAttempt(rpOutcome(action, ans.Status))

	return s.locked(ctx, saga.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Sagas.Get(ctx, saga.SagaID)
		if err != nil || !found || cur.State != domain.SagaRPConvertPending { // e.g. TenantConverted arrived meanwhile
			return err
		}
		now := s.now()
		cur.Attempts++
		code := ans.ErrorCode
		if code == "" && ans.Status != 200 {
			code = fmt.Sprintf("http_%d", ans.Status)
		}
		switch action {
		case domain.RP3Advance:
			cur.State, cur.RPAcceptedAt, cur.NextAttemptAt = domain.SagaAwaitingTenantConverted, &now, nil
			if ans.Status != 200 {
				cur.LastErrorCode, cur.LastErrorAt = &code, &now
			}
		case domain.RP3Retry:
			next := now.Add(domain.SagaBackoff(cur.Attempts, s.Settings.SagaMaxBackoff))
			cur.NextAttemptAt, cur.LastErrorCode, cur.LastErrorAt = &next, &code, &now
		default: // RP3Fail: SEV-2, operator decides (B-O9)
			cur.State, cur.NextAttemptAt, cur.LastErrorCode, cur.LastErrorAt = domain.SagaFailed, nil, &code, &now
			if s.Log != nil {
				s.logger(ctx).Error("conversion saga failed (SEV-2, runbook §20.1)", map[string]any{"saga_id": cur.SagaID, "tenant_id": cur.TenantID,
					"rp_status": ans.Status, "rp_error": ans.ErrorCode, "severity": "SEV-2"})
			}
		}
		return s.Sagas.Update(ctx, cur)
	})
}

// runReversalStep continues a B-O8 reversal that RP-4 had not finished
// (§8.2, rev 0.63): RP-4 again with the same key, then the completion once it
// answers 200. A terminal answer drops the request and pages the operator.
func (s *Service) runReversalStep(ctx context.Context, saga domain.ConversionSaga, sub domain.Subscription) error {
	// The request's operator is the actor of record (AL-5, subscription history).
	c := Caller{UserID: deref(saga.ReversalRequestedBy)}
	_, terminal, err := s.reverse(ctx, c, saga, sub)
	if terminal != "" {
		if s.Log != nil {
			s.logger(ctx).Error("conversion reversal refused by Realm Provisioner; operator decision needed (SEV-2)", map[string]any{
				"tenant_id": saga.TenantID, "saga_id": saga.SagaID, "rp": terminal,
				"alert": "BillingReversalDecisionNeeded", "severity": "SEV-2"})
		}
		return nil
	}
	if domain.HasCode(err, domain.ErrIAMUnavailable) {
		return nil // recorded with backoff; the next pass retries
	}
	return err
}

// ── TenantConverted (§7.1) ─────────────────────────────────────────────────

// ConfirmConversion is the saga side of TenantConverted: in the event's
// transaction under the tenant lock; exempt from the recency skip.
func (s *Service) ConfirmConversion(ctx context.Context, ev TenantLifecycleEvent) (bool, error) {
	ctx, end := StartSpan(ctx, "billing.saga.confirm", AttrTenant(ev.TenantID))
	out, err := s.confirmConversion(ctx, ev)
	end(err)
	return out, err
}

func (s *Service) confirmConversion(ctx context.Context, ev TenantLifecycleEvent) (bool, error) {
	if ev.BillingSubscriptionID == "" {
		s.unmatchedConversion(ctx, ev, "no billing_subscription_id")
		return false, nil
	}
	saga, found, err := s.Sagas.BySubscription(ctx, ev.BillingSubscriptionID)
	if err != nil {
		return false, err
	}
	if !found || saga.TenantID != ev.TenantID {
		s.unmatchedConversion(ctx, ev, "no saga")
		return false, nil
	}
	if saga.State == domain.SagaConfirmed || saga.State == domain.SagaReverting {
		// Already confirmed: no-op. Reverting: the conversion was confirmed
		// before B-O8; a late or redelivered TenantConverted must not drop
		// the recorded reversal.
		return true, nil
	}
	if !domain.CanSagaTransition(saga.State, domain.SagaConfirmed) {
		if s.Log != nil {
			s.logger(ctx).Warn("TenantConverted for a saga that cannot be confirmed", map[string]any{"saga_id": saga.SagaID, "state": string(saga.State)})
		}
		return true, nil
	}
	now := s.now()
	saga.State, saga.ConfirmedAt, saga.TenantConvertedEvent, saga.NextAttemptAt = domain.SagaConfirmed, &now, &ev.EventID, nil
	if err := s.Sagas.Update(ctx, saga); err != nil {
		return false, err
	}
	if s.Addons != nil {
		// §7.1, §8.7 (rev 0.75): no trial-era cap exception reaches the
		// subscription (BIL-ADD-5).
		if err := s.Addons.DeleteTrialCap(ctx, ev.TenantID); err != nil {
			return false, err
		}
	}
	s.metrics().SagaConfirmed(string(saga.Kind), now.Sub(saga.PaymentConfirmedAt))
	sub, err := s.Subs.Update(ctx, saga.BillingSubscriptionID, 0, func(x *domain.Subscription) {
		if x.ConvertedAt == nil {
			at := ev.Time
			x.ConvertedAt = &at
		}
	})
	if err != nil {
		return false, err
	}
	if ev.Plan != "" && ev.Plan != sub.Plan && s.Log != nil {
		s.logger(ctx).Error("plan_mismatch: TenantConverted plan differs from Billing's (Billing's plan wins; reconciliation repairs)",
			map[string]any{"tenant_id": ev.TenantID, "event_plan": string(ev.Plan), "billing_plan": string(sub.Plan), "severity": "SEV-3"})
	}
	_, err = s.Emissions.Schedule(ctx, domain.ScheduledEmission{TenantID: sub.TenantID, EventType: domain.EventTenantSeatsChanged,
		PayloadBuilder: domain.BuilderSeatsAfterConversion, RefID: sub.BillingSubscriptionID, DueAt: now.Add(s.Settings.SeatsAfterConversionDelay)})
	return true, err
}

func (s *Service) unmatchedConversion(ctx context.Context, ev TenantLifecycleEvent, why string) {
	s.metrics().UnmatchedConversion()
	if s.Log != nil {
		s.logger(ctx).Error("TenantConverted Billing did not initiate (SEV-3)", map[string]any{"tenant_id": ev.TenantID, "event_id": ev.EventID,
			"reason": why, "severity": "SEV-3", "metric": "commercial_billing_unmatched_conversions_total"})
	}
}

// rpOutcome is the rp_convert_attempts_total outcome (§11.2).
func rpOutcome(a domain.RP3Action, status int) string {
	switch {
	case a == domain.RP3Advance && status == 200:
		return "ok"
	case a == domain.RP3Advance:
		return "in_progress"
	case a == domain.RP3Retry:
		return "retryable"
	default:
		return "terminal"
	}
}

// accountIfAny reads the tenant's billing account; found is false when it
// has none.
func (s *Service) accountIfAny(ctx context.Context, tenantID string) (domain.BillingAccount, bool, error) {
	a, err := s.Accounts.Get(ctx, tenantID)
	switch {
	case err == nil:
		return a, true, nil
	case domain.HasCode(err, domain.ErrSubscriptionNotFound):
		return domain.BillingAccount{}, false, nil
	default:
		return domain.BillingAccount{}, false, err
	}
}

// accountCreateErr maps a failed account insert. The account is re-read
// under the tenant lock first, so ErrAccountExists means a writer that does
// not take the lock won a race; the transaction is aborted, so the caller
// gets a retryable 409 instead of a recovery read that would fail.
func accountCreateErr(err error) error {
	if errors.Is(err, port.ErrAccountExists) {
		return domain.NewError(domain.ErrConcurrentUpdate, "the billing account was created concurrently; retry")
	}
	return err
}
