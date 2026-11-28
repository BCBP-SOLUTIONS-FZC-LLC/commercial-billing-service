package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

const webhookUA = "commercial-billing/webhook"

// ── B-P12 / B-P13 ──────────────────────────────────────────────────────────

// CancelSubscription is B-P12: cancel at period end. No event now:
// TenantSubscriptionCancelled is emitted when the provider ends it (§8.8.2).
func (s *Service) CancelSubscription(ctx context.Context, c Caller, tenantID string, r CancelRequest) (SubscriptionView, error) {
	ctx, end := StartSpan(ctx, "billing.subscription.cancel", AttrTenant(tenantID))
	out, err := s.cancelSubscription(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) cancelSubscription(ctx context.Context, c Caller, tenantID string, r CancelRequest) (SubscriptionView, error) {
	return s.setAutoRenew(ctx, c, tenantID, r.RecordVersion, false, "api:B-P12")
}

// ResumeSubscription is B-P13: undo a pending cancellation before the
// period ends.
func (s *Service) ResumeSubscription(ctx context.Context, c Caller, tenantID string, r ResumeRequest) (SubscriptionView, error) {
	return s.setAutoRenew(ctx, c, tenantID, r.RecordVersion, true, "api:B-P13")
}

func (s *Service) setAutoRenew(ctx context.Context, c Caller, tenantID string, version int64, renew bool, cause string) (SubscriptionView, error) {
	var (
		sub      domain.Subscription
		change   *domain.PlanChangeRequest
		schedule *domain.SeatChangeSchedule
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if sub, found, err = s.Subs.Live(ctx, tenantID); err != nil {
			return err
		}
		if !found || (sub.Status != domain.StatusActive && sub.Status != domain.StatusPastDue) {
			return domain.NewError(domain.ErrSubscriptionNotActive, "the subscription is not active")
		}
		if sub.RecordVersion != version {
			return domain.NewError(domain.ErrOptimisticLockConflict, "the subscription changed since it was read").With("record_version", sub.RecordVersion)
		}
		if sub.Provider == manualProvider {
			return domain.NewError(domain.ErrProviderOperationUnsupported, "an operator manages stub subscriptions")
		}
		if renew && sub.CurrentPeriodEnd != nil && !s.now().Before(*sub.CurrentPeriodEnd) {
			return domain.NewError(domain.ErrSubscriptionNotActive, "the period has ended; reactivate instead")
		}
		if !renew { // a cancellation also cancels a scheduled downgrade and reduction
			if ch, ok, err := s.Changes.Open(ctx, tenantID); err != nil {
				return err
			} else if ok && ch.Status == domain.ChangeScheduled {
				change = &ch
			}
			if sc, ok, err := s.Schedules.Open(ctx, sub.BillingSubscriptionID); err != nil {
				return err
			} else if ok {
				schedule = &sc
			}
		}
		return nil
	}); err != nil {
		return SubscriptionView{}, err
	}
	if sub.AutoRenew == renew {
		return s.GetSubscription(ctx, c, tenantID)
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return SubscriptionView{}, err
	}
	// Provider calls outside the transaction (BIL-CONC-2).
	if change != nil && change.ProviderScheduleID != nil {
		if err := prov.ReleaseSchedule(ctx, port.ScheduleRef{ProviderScheduleID: *change.ProviderScheduleID}, "planchg:"+change.ChangeID+":release"); err != nil {
			return SubscriptionView{}, providerErr(err)
		}
	}
	if schedule != nil && schedule.ProviderScheduleID != nil {
		if err := prov.ReleaseSchedule(ctx, port.ScheduleRef{ProviderScheduleID: *schedule.ProviderScheduleID}, "sched:"+schedule.ScheduleID+":release"); err != nil {
			return SubscriptionView{}, providerErr(err)
		}
	}
	ref := port.SubscriptionRef{ProviderSubscriptionID: deref(sub.ProviderSubscriptionID)}
	if err := prov.SetCancelAtPeriodEnd(ctx, ref, !renew, "cancel:"+sub.BillingSubscriptionID+":"+strconv.FormatInt(version, 10)); err != nil {
		return SubscriptionView{}, providerErr(err)
	}
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		updated, err := s.Subs.Update(ctx, sub.BillingSubscriptionID, version, func(x *domain.Subscription) { x.AutoRenew = renew })
		if err != nil {
			return err
		}
		if change != nil {
			change.Status = domain.ChangeCancelled
			if err := s.Changes.Update(ctx, *change); err != nil {
				return err
			}
		}
		if schedule != nil {
			schedule.Status = domain.ScheduleCancelled
			if err := s.Schedules.Update(ctx, *schedule); err != nil {
				return err
			}
		}
		from := updated.Status
		return s.history(ctx, updated, &from, cause, actorOf(c), nil, nil, nil)
	})
	if err != nil {
		return SubscriptionView{}, err
	}
	return s.GetSubscription(ctx, c, tenantID)
}

// ── Callbacks (§8.8, §8.9) ─────────────────────────────────────────────────

// subscriptionFor returns the Billing subscription a callback's provider
// subscription belongs to, or an IgnoredEvent.
func (s *Service) subscriptionFor(ctx context.Context, tenantID, provider string, cur domain.ProviderObjectState) (domain.Subscription, error) {
	providerSub, _ := cur.Fields["subscription"].(string)
	if cur.Ref.Kind == domain.ObjectSubscription {
		providerSub = cur.Ref.ID
	}
	if providerSub == "" {
		return domain.Subscription{}, &port.IgnoredEvent{Reason: "no_subscription"}
	}
	sub, found, err := s.Subs.ByProvider(ctx, provider, providerSub)
	if err != nil {
		return sub, err
	}
	if !found || sub.TenantID != tenantID {
		return sub, &port.IgnoredEvent{Reason: "unknown_subscription"}
	}
	return sub, nil
}

// mirrorFromState writes the re-read invoice into the mirror and returns
// Billing's invoice id (event payloads never carry provider ids).
func (s *Service) mirrorFromState(ctx context.Context, tenantID, provider string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, domain.Money, error) {
	currency, _ := cur.Fields["currency"].(string)
	due, _ := cur.Fields["amount_due_minor"].(int64)
	paid, _ := cur.Fields["amount_paid_minor"].(int64)
	kind, _ := cur.Fields["kind"].(string)
	if kind == "" {
		kind = string(domain.InvoiceSubscriptionCycle)
	}
	v := port.InvoiceView{Ref: port.InvoiceRef{ProviderInvoiceID: cur.Ref.ID}, Status: cur.Status, Kind: domain.InvoiceKind(kind),
		AmountDue: domain.Money{AmountMinor: due, Currency: currency}, AmountPaid: domain.Money{AmountMinor: paid, Currency: currency}}
	if st, ok := cur.Fields["line_period_start"].(time.Time); ok {
		en, _ := cur.Fields["line_period_end"].(time.Time)
		v.PeriodStart, v.PeriodEnd = &st, &en
	}
	at := ev.ProviderCreatedAt
	if at.IsZero() {
		at = s.now()
	}
	id, err := s.Invoices.Mirror(ctx, tenantID, provider, v, at)
	return id, v.AmountDue, err
}

// PaymentFailed is §8.8.1: a cycle invoice payment failed → past_due and
// TenantPaymentPastDue, once (BIL-EVT-7).
type PaymentFailed struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h PaymentFailed) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if reason, _ := cur.Fields["billing_reason"].(string); reason != "subscription_cycle" {
		return "", &port.IgnoredEvent{Reason: "not_a_cycle_invoice"}
	}
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	sub, err := s.subscriptionFor(ctx, tenantID, ev.Provider, cur)
	if err != nil {
		return "", err
	}
	invoiceID, due, err := s.mirrorFromState(ctx, tenantID, ev.Provider, ev, cur)
	if err != nil {
		return "", err
	}
	if sub.Status != domain.StatusActive {
		return "", &port.IgnoredEvent{Reason: "already_" + string(sub.Status)} // later retry failures: no new event
	}
	now := s.now()
	from := sub.Status
	pd, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusPastDue, func(x *domain.Subscription) { x.PastDueSince = &now })
	if err != nil {
		return "", err
	}
	s.metrics().DunningTransition(string(domain.StatusPastDue))
	s.metrics().PaymentFailure(failureReason(cur))
	if err := s.history(ctx, pd, &from, "webhook:"+ev.EventType, domain.CommercialSystemPrincipal, eventRef(ev), nil, nil); err != nil {
		return "", err
	}
	payload := domain.TenantPaymentPastDue{TenantID: tenantID, InvoiceRef: invoiceID, PastDueSince: now}
	if due.Currency != "" {
		payload.AmountDue = &due
	}
	if next, ok := cur.Fields["next_payment_attempt"].(time.Time); ok {
		payload.NextRetryAt = &next
	}
	if err := s.emit(ctx, payload, webhookMeta()); err != nil {
		return "", err
	}
	return "applied:past_due", nil
}

// SubscriptionEnded is §8.8.2: the provider ended the subscription →
// cancelled with its reason; scheduled changes cancelled;
// TenantSubscriptionCancelled.
type SubscriptionEnded struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h SubscriptionEnded) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if cur.Status != "canceled" && cur.Status != "cancelled" && cur.Status != "incomplete_expired" {
		return "", &port.IgnoredEvent{Reason: "subscription_not_ended"}
	}
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	sub, err := s.subscriptionFor(ctx, tenantID, ev.Provider, cur)
	if err != nil {
		return "", err
	}
	if !domain.CanTransition(sub.Status, domain.StatusCancelled) {
		return "", &port.IgnoredEvent{Reason: "transition_not_allowed"}
	}
	reason := domain.CancelAutoRenewOff
	switch {
	case sub.Status == domain.StatusPastDue:
		reason = domain.CancelDunningExhausted
	case !sub.AutoRenew:
		reason = domain.CancelCustomer
	}
	now := s.now()
	from := sub.Status
	cancelled, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusCancelled, func(x *domain.Subscription) {
		x.CancelledAt, x.CancelReason, x.PastDueSince = &now, &reason, nil
	})
	if err != nil {
		return "", err
	}
	s.metrics().DunningTransition(string(domain.StatusCancelled))
	if err := s.cancelOpenChanges(ctx, cancelled); err != nil {
		return "", err
	}
	if err := s.history(ctx, cancelled, &from, "webhook:"+ev.EventType, domain.CommercialSystemPrincipal, eventRef(ev), nil, nil); err != nil {
		return "", err
	}
	if err := s.emit(ctx, domain.TenantSubscriptionCancelled{TenantID: tenantID, CancelledAt: now, Reason: reason},
		webhookMeta()); err != nil {
		return "", err
	}
	return "applied:cancelled", nil
}

// cancelOpenChanges marks a scheduled downgrade and reduction cancelled.
func (s *Service) cancelOpenChanges(ctx context.Context, sub domain.Subscription) error {
	if ch, ok, err := s.Changes.Open(ctx, sub.TenantID); err != nil {
		return err
	} else if ok {
		ch.Status = domain.ChangeCancelled
		if err := s.Changes.Update(ctx, ch); err != nil {
			return err
		}
	}
	if sc, ok, err := s.Schedules.Open(ctx, sub.BillingSubscriptionID); err != nil {
		return err
	} else if ok {
		sc.Status = domain.ScheduleCancelled
		return s.Schedules.Update(ctx, sc)
	}
	return nil
}

// CycleInvoicePaid closes the cycle invoice.paid chain (§8.6.3 step 4,
// §8.9): a past_due subscription recovers (TenantReactivated, previous
// past_due); a cancelled subscription paid on its new provider subscription
// is reactivated (seat event first if seats changed, BIL-EVT-5); otherwise
// the period is updated and nothing is emitted.
type CycleInvoicePaid struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h CycleInvoicePaid) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if cur.Status != "paid" {
		return "", &port.IgnoredEvent{Reason: "invoice_not_paid"}
	}
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	sub, err := s.subscriptionFor(ctx, tenantID, ev.Provider, cur)
	if err != nil {
		return "", err
	}
	if _, _, err := s.mirrorFromState(ctx, tenantID, ev.Provider, ev, cur); err != nil {
		return "", err
	}
	start, hasPeriod := cur.Fields["line_period_start"].(time.Time)
	end, _ := cur.Fields["line_period_end"].(time.Time)
	now := s.now()
	if sub.Status == domain.StatusCancelled && !firstInvoiceOfSubscription(cur) {
		// §8.9: a cancelled subscription is reactivated only by the first
		// invoice of the new provider subscription B-P14 creates. A late
		// payment of an old open invoice of the deleted provider subscription
		// (dunning exhausted) leaves no live provider subscription: the
		// invoice is mirrored above (committed), no transition, no
		// TenantReactivated.
		return "applied:invoice_mirrored", nil
	}
	switch sub.Status {
	case domain.StatusPastDue, domain.StatusCancelled:
		from := sub.Status
		item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return "", err
		}
		active, err := s.Subs.Transition(ctx, sub.BillingSubscriptionID, 0, domain.StatusActive, func(x *domain.Subscription) {
			x.PastDueSince, x.CancelledAt, x.CancelReason, x.AutoRenew = nil, nil, nil, true
			if hasPeriod && x.CurrentPeriodEnd != nil && start.After(x.CurrentPeriodEnd.Add(-time.Minute)) || hasPeriod && from == domain.StatusCancelled {
				x.CurrentPeriodStart, x.CurrentPeriodEnd = &start, &end
			}
		})
		if err != nil {
			return "", err
		}
		before := item.Quantity
		if q, ok := intField(cur.Fields["line_quantity"]); ok && q > 0 && q != item.Quantity && from == domain.StatusCancelled {
			item.Quantity = q
			if err := s.Subs.SetItem(ctx, item); err != nil {
				return "", err
			}
			if err := s.emit(ctx, domain.TenantSeatsChanged{TenantID: tenantID, LicensedSeats: q, PreviousLicensedSeats: &before,
				Reason: domain.SeatsReactivation, BillingSubscriptionID: sub.BillingSubscriptionID}, webhookMeta()); err != nil {
				return "", err
			}
		}
		if err := s.history(ctx, active, &from, "webhook:"+ev.EventType, domain.CommercialSystemPrincipal, eventRef(ev),
			&[2]*int{&before, &item.Quantity}, nil); err != nil {
			return "", err
		}
		if err := s.emit(ctx, domain.TenantReactivated{TenantID: tenantID, Source: domain.ReactivationSource,
			ReactivatedAt: now, PreviousStatus: from}, webhookMeta()); err != nil {
			return "", err
		}
		s.metrics().DunningTransition(string(domain.StatusActive))
		return "applied:reactivated", nil
	case domain.StatusActive:
		if hasPeriod && (sub.CurrentPeriodEnd == nil || start.After(sub.CurrentPeriodEnd.Add(-time.Minute))) {
			if _, err := s.Subs.Update(ctx, sub.BillingSubscriptionID, 0, func(x *domain.Subscription) {
				x.CurrentPeriodStart, x.CurrentPeriodEnd = &start, &end
			}); err != nil {
				return "", err
			}
			// §8.16 step 4: a scheduled renewal price takes effect now.
			if err := s.applyScheduledPrice(ctx, sub, start, "webhook:"+ev.EventType); err != nil {
				return "", err
			}
			return "applied:renewed", nil
		}
		return "applied:invoice_mirrored", nil
	case domain.StatusIncomplete, domain.StatusEnded:
	}
	return "", &port.IgnoredEvent{Reason: "subscription_" + string(sub.Status)}
}

// firstInvoiceOfSubscription reports whether the re-read invoice is a
// provider subscription's first invoice (billing_reason
// subscription_create), the one a B-P14 reactivation checkout produces.
func firstInvoiceOfSubscription(cur domain.ProviderObjectState) bool {
	reason, _ := cur.Fields["billing_reason"].(string)
	kind, _ := cur.Fields["kind"].(string)
	return reason == "subscription_create" || kind == string(domain.InvoiceSubscriptionCreate)
}

// ── B-P14 (§8.9) ───────────────────────────────────────────────────────────

// Reactivate is B-P14: pay the open invoice (past_due) or a hosted checkout
// for a new provider subscription on the same billing_subscription_id
// (cancelled).
func (s *Service) Reactivate(ctx context.Context, c Caller, tenantID string, r ReactivateRequest) (ReactivationResult, error) {
	ctx, end := StartSpan(ctx, "billing.subscription.reactivate", AttrTenant(tenantID))
	out, err := s.reactivate(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) reactivate(ctx context.Context, c Caller, tenantID string, r ReactivateRequest) (ReactivationResult, error) {
	var (
		sub  domain.Subscription
		acct domain.BillingAccount
		item domain.SubscriptionItem
		open string
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		if acct, err = s.Accounts.Get(ctx, tenantID); err != nil {
			return err
		}
		if acct.IAMLifecycle == domain.LifecycleOffboarded {
			return domain.NewError(domain.ErrAccountClosed, "the account is closed; sign up again")
		}
		var found bool
		if sub, found, err = s.Subs.Live(ctx, tenantID); err != nil {
			return err
		}
		switch {
		case !found:
			return domain.NewError(domain.ErrSubscriptionNotFound, "no subscription to reactivate")
		case sub.Status == domain.StatusActive || sub.Status == domain.StatusIncomplete:
			return domain.NewError(domain.ErrSubscriptionNotLapsed, "the subscription is not lapsed")
		case sub.CollectionPaused:
			return domain.NewError(domain.ErrCollectionPaused, "billing is paused; an operator lifts it")
		}
		if item, err = s.Subs.Item(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		}
		if sub.Status == domain.StatusPastDue {
			open, err = s.Invoices.OldestOpen(ctx, tenantID, sub.BillingSubscriptionID)
		}
		return err
	}); err != nil {
		return ReactivationResult{}, err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return ReactivationResult{}, err
	}
	if sub.Status == domain.StatusPastDue {
		if open == "" {
			return ReactivationResult{}, domain.NewError(domain.ErrInvoiceNotFound, "no open invoice to pay")
		}
		link, err := prov.PayOpenInvoiceLink(ctx, port.InvoiceRef{ProviderInvoiceID: open})
		if err != nil {
			return ReactivationResult{}, providerErr(err)
		}
		return ReactivationResult{PaymentURL: link.URL, Kind: "open_invoice", ExpiresAt: link.ExpiresAt}, nil
	}

	// cancelled: seats ≥ in use (fresh I-11, fail closed).
	seats := item.Quantity
	if r.LicensedSeats != nil {
		seats = *r.LicensedSeats
	}
	interval := sub.Interval
	if r.Interval != "" {
		interval = domain.BillingInterval(r.Interval)
	}
	usage, err := s.Core.SeatUsage(ctx, tenantID)
	if err != nil {
		return ReactivationResult{}, err
	}
	if seats < usage.InUse() {
		return ReactivationResult{}, domain.NewError(domain.ErrSeatsBelowInUse, "licensed_seats is below the seats in use").With("seats_in_use", usage.InUse())
	}
	price, err := s.targetPriceFor(ctx, sub, interval, acct.Currency)
	if err != nil {
		return ReactivationResult{}, err
	}
	now := s.now()
	session := domain.CheckoutSession{SessionID: newID(), TenantID: tenantID, Purpose: domain.PurposeReactivation,
		BillingSubscriptionID: &sub.BillingSubscriptionID, RequestedPlan: &sub.Plan, RequestedInterval: &interval, RequestedSeats: &seats,
		RequestedBy: actorOf(c), ExpiresAt: now.Add(s.Settings.CheckoutTTL)}
	session.ProviderSessionID = domain.PendingSessionPrefix + session.SessionID
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		if old, found, err := s.Checkouts.Open(ctx, tenantID, domain.PurposeReactivation); err != nil {
			return err
		} else if found {
			if err := s.Checkouts.SetStatus(ctx, old.SessionID, domain.SessionExpired); err != nil {
				return err
			}
		}
		return s.Checkouts.Insert(ctx, session)
	}); err != nil {
		return ReactivationResult{}, err
	}
	base := strings.TrimRight(s.Settings.TenantAppBaseURL, "/") + "/billing"
	hosted, err := prov.CreateHostedCheckout(ctx, port.CheckoutInput{Customer: port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)},
		Purpose: port.CheckoutReactivation, PriceRef: deref(price.ProviderPriceID), Quantity: seats, Currency: acct.Currency,
		Metadata:   map[string]string{"tenant_id": tenantID, "billing_subscription_id": sub.BillingSubscriptionID, "purpose": domain.PurposeReactivation},
		SuccessURL: base + "?reactivation=success", CancelURL: base + "?reactivation=cancelled", ExpiresAt: session.ExpiresAt},
		"bsub:"+sub.BillingSubscriptionID+":reactivate:"+session.SessionID)
	if err != nil {
		return ReactivationResult{}, providerErr(err)
	}
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		return s.Checkouts.SetProviderSession(ctx, session.SessionID, hosted.ProviderSessionID, hosted.ExpiresAt)
	}); err != nil {
		return ReactivationResult{}, err
	}
	return ReactivationResult{PaymentURL: hosted.URL, Kind: "checkout", CheckoutSessionID: session.SessionID, ExpiresAt: hosted.ExpiresAt}, nil
}

// targetPriceFor is the current price for the subscription's plan.
func (s *Service) targetPriceFor(ctx context.Context, sub domain.Subscription, interval domain.BillingInterval, currency string) (domain.PlanPrice, error) {
	var p domain.PlanPrice
	err := s.Tx.RunInTenantTx(ctx, sub.TenantID, func(ctx context.Context) error {
		var ok bool
		var err error
		p, ok, err = s.Prices.PriceAt(ctx, sub.Plan, interval, currency, sub.Provider, s.now())
		if err == nil && (!ok || p.ProviderPriceID == nil) {
			return domain.NewError(domain.ErrPriceNotAvailable, "no price for this plan and interval")
		}
		return err
	})
	return p, err
}

// failureReason is the payment_failures_total reason: the mapped decline
// code when the provider gave one, else "payment_failed".
func failureReason(cur domain.ProviderObjectState) string {
	if code, _ := cur.Fields["decline_code"].(string); code != "" {
		return declineReason(code)
	}
	return "payment_failed"
}
