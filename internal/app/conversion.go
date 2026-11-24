package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	v.CallerPermissions, v.ActionsAvailable = CallerPermissions(c), s.actionsFor(c, &sub, acct)
	return v, nil
}

// ── B-P2 ───────────────────────────────────────────────────────────────────

// StartCheckout is B-P2 (§8.1.1).
func (s *Service) StartCheckout(ctx context.Context, c Caller, tenantID string, r CheckoutRequest) (CheckoutResult, error) {
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
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		a, err := s.Accounts.Get(ctx, tenantID)
		if err == nil {
			acct, hasAccount = a, true
			return nil
		}
		if domain.HasCode(err, domain.ErrSubscriptionNotFound) {
			return nil
		}
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
	var (
		result  CheckoutResult
		reuse   bool
		session domain.CheckoutSession
		price   domain.PlanPrice
	)
	now := s.now()
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		if !hasAccount {
			if err := s.Accounts.Create(ctx, acct); err != nil {
				if sqlUnique(err) { // a concurrent first checkout created it
					a, gerr := s.Accounts.Get(ctx, tenantID)
					if gerr != nil {
						return gerr
					}
					acct = a
				} else {
					return err
				}
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
			session, reuse, subID = open, true, *open.BillingSubscriptionID
			return nil
		}
		if hasOpen {
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
	if err != nil {
		return result, err
	}
	total := MoneyFrom(domain.Money{AmountMinor: price.UnitAmountMinor * int64(r.LicensedSeats), Currency: price.Currency})
	if reuse {
		subID = *session.BillingSubscriptionID
	}
	// Outside: the hosted session (idempotent on the subscription id).
	hosted, err := s.createConversionCheckout(ctx, prov, acct, session, subID, price, r.LicensedSeats)
	if err != nil {
		return result, err
	}
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.Checkouts.SetProviderSession(ctx, session.SessionID, hosted.ProviderSessionID, hosted.ExpiresAt)
	}); err != nil {
		return result, err
	}
	return CheckoutResult{CheckoutSessionID: session.SessionID, CheckoutURL: hosted.URL, ExpiresAt: hosted.ExpiresAt,
		BillingSubscriptionID: subID, Price: total}, nil
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

// sqlUnique reports a unique violation surfaced by a repository.
func sqlUnique(err error) bool {
	type sqlstate interface{ SQLState() string }
	var s sqlstate
	return errors.As(err, &s) && s.SQLState() == "23505"
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
	if err := s.Checkouts.SetStatus(ctx, session.SessionID, domain.SessionCompleted); err != nil {
		return "", err
	}
	providerSub, _ := cur.Fields["subscription"].(string)
	if session.BillingSubscriptionID == nil || providerSub == "" {
		return "applied:session_completed", nil
	}
	_, err = s.Subs.Update(ctx, *session.BillingSubscriptionID, 0, func(x *domain.Subscription) {
		// A reactivation replaces the deleted provider subscription (§8.9).
		if x.ProviderSubscriptionID == nil || session.Purpose == domain.PurposeReactivation {
			x.ProviderSubscriptionID = &providerSub
		}
	})
	if err != nil {
		return "", err
	}
	return "applied:subscription_linked", nil
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
	if !found || sub.TenantID != tenantID {
		return "", &port.IgnoredEvent{Reason: "unknown_subscription"}
	}
	if sub.Status != domain.StatusIncomplete || sub.Origin != domain.OriginTrialConversion {
		return h.next(ctx, tenantID, ev, cur)
	}
	now := s.now()
	start, _ := cur.Fields["line_period_start"].(time.Time)
	end, _ := cur.Fields["line_period_end"].(time.Time)
	from := sub.Status
	active, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusActive, func(x *domain.Subscription) {
		x.PaymentConfirmedAt = &now
		if !start.IsZero() {
			x.CurrentPeriodStart, x.CurrentPeriodEnd = &start, &end
		}
	})
	if err != nil {
		return "", err
	}
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return "", err
	}
	if q, ok := intField(cur.Fields["line_quantity"]); ok && q > 0 && q != item.Quantity { // the purchased quantity wins
		item.Quantity = q
		if err := s.Subs.SetItem(ctx, item); err != nil {
			return "", err
		}
	}
	if err := s.history(ctx, active, &from, "webhook:"+ev.EventType, domain.CommercialSystemPrincipal, eventRef(ev),
		&[2]*int{nil, &item.Quantity}, nil); err != nil {
		return "", err
	}
	if err := s.startSaga(ctx, active, domain.SagaTrialConversion, now); err != nil {
		return "", err
	}
	return "applied:conversion_payment", nil
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
			s.Log.Warn("saga attempt failed; retried on the next pass", map[string]any{"saga_id": ref.SagaID, "tenant_id": ref.TenantID, "error": err.Error()})
		}
	}
	return len(due), nil
}

func (s *Service) runSaga(ctx context.Context, ref port.SagaRef) error {
	var saga domain.ConversionSaga
	var sub domain.Subscription
	claimed := false
	if err := s.locked(ctx, ref.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Sagas.Get(ctx, ref.SagaID)
		if err != nil || !found {
			return err
		}
		now := s.now()
		if cur.State != domain.SagaRPConvertPending || cur.NextAttemptAt == nil || cur.NextAttemptAt.After(now) {
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

	ans := s.RP.Convert(ctx, saga.TenantID, port.ConvertRequest{Plan: sub.Plan, BillingSubscriptionID: sub.BillingSubscriptionID},
		deref(saga.RPIdempotencyKey))
	action := domain.ClassifyRP3(ans)

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
				s.Log.Error("conversion saga failed (SEV-2, runbook §20.1)", map[string]any{"saga_id": cur.SagaID, "tenant_id": cur.TenantID,
					"rp_status": ans.Status, "rp_error": ans.ErrorCode, "severity": "SEV-2"})
			}
		}
		return s.Sagas.Update(ctx, cur)
	})
}

// ── TenantConverted (§7.1) ─────────────────────────────────────────────────

// ConfirmConversion is the saga side of TenantConverted: in the event's
// transaction under the tenant lock; exempt from the recency skip.
func (s *Service) ConfirmConversion(ctx context.Context, ev TenantLifecycleEvent) (bool, error) {
	if ev.BillingSubscriptionID == "" {
		s.unmatchedConversion(ev, "no billing_subscription_id")
		return false, nil
	}
	saga, found, err := s.Sagas.BySubscription(ctx, ev.BillingSubscriptionID)
	if err != nil {
		return false, err
	}
	if !found || saga.TenantID != ev.TenantID {
		s.unmatchedConversion(ev, "no saga")
		return false, nil
	}
	if saga.State == domain.SagaConfirmed {
		return true, nil // already confirmed: no-op
	}
	if !domain.CanSagaTransition(saga.State, domain.SagaConfirmed) {
		if s.Log != nil {
			s.Log.Warn("TenantConverted for a saga that cannot be confirmed", map[string]any{"saga_id": saga.SagaID, "state": string(saga.State)})
		}
		return true, nil
	}
	now := s.now()
	saga.State, saga.ConfirmedAt, saga.TenantConvertedEvent, saga.NextAttemptAt = domain.SagaConfirmed, &now, &ev.EventID, nil
	if err := s.Sagas.Update(ctx, saga); err != nil {
		return false, err
	}
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
		s.Log.Error("plan_mismatch: TenantConverted plan differs from Billing's (Billing's plan wins; reconciliation repairs)",
			map[string]any{"tenant_id": ev.TenantID, "event_plan": string(ev.Plan), "billing_plan": string(sub.Plan), "severity": "SEV-3"})
	}
	_, err = s.Emissions.Schedule(ctx, domain.ScheduledEmission{TenantID: sub.TenantID, EventType: domain.EventTenantSeatsChanged,
		PayloadBuilder: domain.BuilderSeatsAfterConversion, RefID: sub.BillingSubscriptionID, DueAt: now.Add(s.Settings.SeatsAfterConversionDelay)})
	return true, err
}

func (s *Service) unmatchedConversion(ev TenantLifecycleEvent, why string) {
	if s.Log != nil {
		s.Log.Error("TenantConverted Billing did not initiate (SEV-3)", map[string]any{"tenant_id": ev.TenantID, "event_id": ev.EventID,
			"reason": why, "severity": "SEV-3", "metric": "commercial_billing_unmatched_conversions_total"})
	}
}
