package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// seatPreview is what B-P6 stores for B-P7 (the amount shown is the
// amount charged).
type seatPreview struct {
	AddSeats         int       `json:"add_seats"`
	SeatsBefore      int       `json:"seats_before"`
	ProrationInstant time.Time `json:"proration_instant"`
	AmountMinor      int64     `json:"amount_minor"`
	Currency         string    `json:"currency"`
	AmountSource     string    `json:"amount_source"`
	UnitAmountMinor  *int64    `json:"unit_amount_minor,omitempty"`
	RemainingSeconds *int64    `json:"remaining_seconds,omitempty"`
	PeriodSeconds    *int64    `json:"period_seconds,omitempty"`
	AmountUnrounded  *string   `json:"amount_unrounded_minor,omitempty"`
	DeferredMinor    *int64    `json:"deferred_amount_minor,omitempty"`
	DeferralReason   *string   `json:"deferral_reason,omitempty"`
}

// seatSubscription reads the subscription for B-P6 / B-P7: 409
// SUBSCRIPTION_REQUIRED on trial, 409 SUBSCRIPTION_NOT_ACTIVE, 409
// COLLECTION_PAUSED.
func (s *Service) seatSubscription(ctx context.Context, tenantID string) (domain.Subscription, domain.SubscriptionItem, domain.PlanPrice, error) {
	return s.liveForChange(ctx, tenantID)
}

// PreviewSeats is B-P6 (§8.5 step 1, §8.5.3).
func (s *Service) PreviewSeats(ctx context.Context, _ Caller, tenantID string, r SeatPreviewRequest) (SeatPreview, error) {
	sub, item, price, err := s.seatSubscription(ctx, tenantID)
	if err != nil {
		return SeatPreview{}, err
	}
	if item.Quantity+r.AddSeats > domain.MaxLicensedSeats {
		return SeatPreview{}, domain.NewError(domain.ErrValidation, fmt.Sprintf("licensed seats would exceed %d", domain.MaxLicensedSeats))
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return SeatPreview{}, err
	}
	if sub.CurrentPeriodStart == nil || sub.CurrentPeriodEnd == nil {
		return SeatPreview{}, domain.NewError(domain.ErrSubscriptionNotActive, "the subscription has no current period")
	}
	now := s.now()
	pv := seatPreview{AddSeats: r.AddSeats, SeatsBefore: item.Quantity, Currency: price.Currency}
	if prov.Capabilities().Proration == port.ProrationFallback {
		res, err := domain.Prorate(domain.ProrationInput{UnitAmountMinor: price.UnitAmountMinor, SeatsAdded: int64(r.AddSeats),
			PeriodStart: *sub.CurrentPeriodStart, PeriodEnd: *sub.CurrentPeriodEnd, ProrationInstant: now,
			MinChargeMinor: s.Settings.MinChargeMinor, Policy: s.Settings.BelowMinimumPolicy})
		if err != nil {
			return SeatPreview{}, domain.NewError(domain.ErrValidation, err.Error())
		}
		pv.ProrationInstant, pv.AmountMinor, pv.AmountSource = now, res.AmountMinor, domain.AmountSourceCalculator
		pv.UnitAmountMinor, pv.RemainingSeconds, pv.PeriodSeconds, pv.AmountUnrounded = &res.UnitAmountMinor, &res.RemainingSeconds, &res.PeriodSeconds, &res.AmountUnrounded
		if res.DeferralReason != "" {
			pv.DeferredMinor, pv.DeferralReason = &res.DeferredAmountMinor, &res.DeferralReason
		}
	} else {
		p, err := prov.PreviewQuantityChange(ctx, port.QuantityChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: deref(sub.ProviderSubscriptionID)},
			From: item.Quantity, To: item.Quantity + r.AddSeats, ProrationInstant: now, InvoiceNow: true})
		if err != nil {
			return SeatPreview{}, providerErr(err)
		}
		pv.ProrationInstant, pv.AmountMinor, pv.AmountSource = p.ProrationInstant, p.Amount.AmountMinor, domain.AmountSourceProvider
	}
	id, exp := newID(), now.Add(s.Settings.SeatPreviewTTL)
	body, _ := json.Marshal(pv)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error { return s.Previews.Save(ctx, tenantID, id, body, exp) }); err != nil {
		return SeatPreview{}, err
	}
	out := SeatPreview{PreviewID: id, SeatsBefore: item.Quantity, SeatsAfter: item.Quantity + r.AddSeats,
		AmountDueNow: MoneyFrom(domain.Money{AmountMinor: pv.AmountMinor, Currency: pv.Currency}), AmountSource: pv.AmountSource,
		ProrationInstant: pv.ProrationInstant, CurrentPeriodEnd: *sub.CurrentPeriodEnd, ExpiresAt: exp,
		NextRenewalAmount: MoneyFrom(domain.Money{AmountMinor: price.UnitAmountMinor * int64(item.Quantity+r.AddSeats), Currency: price.Currency})}
	if pv.AmountUnrounded != nil {
		out.AmountUnroundedMinor = *pv.AmountUnrounded
	}
	if pv.DeferredMinor != nil {
		d := MoneyFrom(domain.Money{AmountMinor: *pv.DeferredMinor, Currency: pv.Currency})
		out.DeferredAmount, out.DeferralReason = &d, *pv.DeferralReason
	}
	return out, nil
}

// PurchaseSeats is B-P7 (§8.5 step 2–4).
func (s *Service) PurchaseSeats(ctx context.Context, c Caller, tenantID string, r SeatPurchaseRequest) (PurchaseView, error) {
	ctx, end := StartSpan(ctx, "billing.seats.purchase", AttrTenant(tenantID))
	out, err := s.purchaseSeats(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) purchaseSeats(ctx context.Context, c Caller, tenantID string, r SeatPurchaseRequest) (PurchaseView, error) {
	sub, item, _, err := s.seatSubscription(ctx, tenantID)
	if err != nil {
		return PurchaseView{}, err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return PurchaseView{}, err
	}
	var pv seatPreview
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		body, exp, found, err := s.Previews.Load(ctx, tenantID, r.PreviewID)
		switch {
		case err != nil:
			return err
		case !found || exp.Before(s.now()):
			return domain.NewError(domain.ErrPreviewExpired, "the preview expired; preview again")
		}
		return json.Unmarshal(body, &pv)
	}); err != nil {
		return PurchaseView{}, err
	}
	if pv.AddSeats != r.AddSeats || pv.SeatsBefore != item.Quantity {
		return PurchaseView{}, domain.NewError(domain.ErrPreviewStale, "the preview no longer matches the subscription; preview again")
	}
	key := c.IdempotencyKey
	p := domain.SeatPurchase{PurchaseID: newID(), TenantID: tenantID, BillingSubscriptionID: sub.BillingSubscriptionID,
		Reason: domain.SeatReasonPurchase, SeatsAdded: r.AddSeats, SeatsBefore: item.Quantity, SeatsAfter: item.Quantity + r.AddSeats,
		ProrationInstant: pv.ProrationInstant, AmountMinor: pv.AmountMinor, Currency: pv.Currency, AmountSource: pv.AmountSource,
		UnitAmountMinor: pv.UnitAmountMinor, RemainingSeconds: pv.RemainingSeconds, PeriodSeconds: pv.PeriodSeconds,
		AmountUnroundedMinor: pv.AmountUnrounded, DeferredAmountMinor: pv.DeferredMinor, DeferralReason: pv.DeferralReason,
		Status: domain.PurchasePendingPayment, RequestedBy: actorOf(c), IdempotencyKey: key}
	if p.IdempotencyKey == "" {
		p.IdempotencyKey = "seatpur:" + p.PurchaseID
	}
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		if open, found, err := s.SeatPurchases.Open(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if found {
			return domain.NewError(domain.ErrSeatPurchaseInProgress, "a seat purchase is already in progress").With("purchase_id", open.PurchaseID)
		}
		return s.SeatPurchases.Insert(ctx, p)
	}); err != nil {
		return PurchaseView{}, err
	}

	// Outside the transaction (BIL-CONC-2). A deferral (below the provider
	// minimum) adds the seats with proration off and no invoice.
	deferred := p.DeferralReason != nil
	res, err := prov.ChangeQuantity(ctx, port.QuantityChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: deref(sub.ProviderSubscriptionID)},
		From: p.SeatsBefore, To: p.SeatsAfter, ProrationInstant: p.ProrationInstant, InvoiceNow: !deferred}, "seatpur:"+p.PurchaseID)
	if deferred && err == nil {
		res.Outcome.Status = port.PaymentPaid
	}
	var action port.ErrActionRequired
	var declined port.ErrDeclined
	switch {
	case errors.As(err, &declined) || err == nil && res.Outcome.Status == port.PaymentDeclined:
		code := declined.Code
		if code == "" {
			code = res.Outcome.DeclineCode
		}
		return s.seatsDeclined(ctx, c, sub, p, res.Invoice.ProviderInvoiceID, code)
	case errors.As(err, &action) || err == nil && res.Outcome.Status == port.PaymentRequiresAction:
		secret := action.ClientSecret
		if secret == "" {
			secret = res.Outcome.ClientSecret
		}
		raw, _ := json.Marshal(ClientAction{Type: "3ds", ClientSecret: secret})
		p.Status, p.ClientAction = domain.PurchaseRequiresAction, raw
	case err != nil:
		return PurchaseView{}, providerErr(err) // stays pending; the callback or purchase-expiry settles it
	}
	if res.Invoice.ProviderInvoiceID != "" {
		p.ProviderInvoiceID = &res.Invoice.ProviderInvoiceID
	}
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, found, err := s.SeatPurchases.Get(ctx, p.PurchaseID)
		if err != nil || !found {
			return err
		}
		if cur.Status == domain.PurchaseConfirmed { // the callback was first
			p = cur
			return nil
		}
		cur.ProviderInvoiceID, cur.ClientAction, cur.Status = p.ProviderInvoiceID, p.ClientAction, p.Status
		if res.Outcome.Status == port.PaymentPaid && p.Status == domain.PurchasePendingPayment {
			if err := s.confirmSeats(ctx, &cur, "api:B-P7", actorOf(c), nil, metaFor(c)); err != nil {
				return err
			}
			s.metrics().PurchaseToEvent(port.PurchaseKindSeats, 0) // paid in the request: the event commits with it
			if cur.DeferralReason != nil && cur.DeferredAmountMinor != nil {
				s.metrics().ProrationDeferred(cur.Currency, *cur.DeferralReason, *cur.DeferredAmountMinor)
			}
		} else if err := s.SeatPurchases.Update(ctx, cur); err != nil {
			return err
		}
		p = cur
		return nil
	})
	if err != nil {
		return PurchaseView{}, err
	}
	return seatView(p), nil
}

// seatsDeclined is §8.5.1 "declined → failed": the open proration invoice
// is voided outside the transaction first (the pending update then lapses),
// so a later collection cannot charge for a failed purchase. If the void
// fails the purchase stays pending and purchase-expiry voids it; if the
// invoice turns out paid the purchase is confirmed.
func (s *Service) seatsDeclined(ctx context.Context, c Caller, sub domain.Subscription, p domain.SeatPurchase, invoiceID, code string) (PurchaseView, error) {
	if p.CreatedAt.IsZero() { // the intent just committed
		p.CreatedAt = s.now()
	}
	inv, verr := s.closeInvoice(ctx, sub.Provider, nonEmpty(invoiceID), seatTarget(deref(sub.ProviderSubscriptionID), p), "seatpur:"+p.PurchaseID+":void")
	confirmed := false
	_ = s.locked(ctx, p.TenantID, func(ctx context.Context) error {
		cur, found, err := s.SeatPurchases.Get(ctx, p.PurchaseID)
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
			if err := s.confirmSeats(ctx, &cur, "api:B-P7", actorOf(c), nil, metaFor(c)); err != nil {
				return err
			}
			p, confirmed = cur, true
			return nil
		case verr == nil:
			cur.Status, cur.ClientAction = domain.PurchaseFailed, nil
		}
		return s.SeatPurchases.Update(ctx, cur)
	})
	if confirmed {
		return seatView(p), nil
	}
	s.metrics().SeatChange("purchase", "declined")
	s.metrics().PaymentFailure(declineReason(code))
	return PurchaseView{}, domain.NewError(domain.ErrPaymentDeclined, "payment declined; no seats added").With("decline_reason", declineReason(code))
}

// confirmSeats is §8.5 step 4: purchase confirmed, seat item quantity,
// history, TenantSeatsChanged {reason: purchase}. Under the lock.
func (s *Service) confirmSeats(ctx context.Context, p *domain.SeatPurchase, cause, actor string, evRef *string, m port.EventMeta) error {
	now := s.now()
	item, err := s.Subs.Item(ctx, p.BillingSubscriptionID)
	if err != nil {
		return err
	}
	before := item.Quantity
	item.Quantity = before + p.SeatsAdded // the current count, so a concurrent change is not lost
	if err := s.Subs.SetItem(ctx, item); err != nil {
		return err
	}
	p.Status, p.ConfirmedAt, p.ClientAction = domain.PurchaseConfirmed, &now, nil
	if err := s.SeatPurchases.Update(ctx, *p); err != nil {
		return err
	}
	sub, err := s.Subs.Get(ctx, p.BillingSubscriptionID)
	if err != nil {
		return err
	}
	from := sub.Status
	if err := s.history(ctx, sub, &from, cause, actor, evRef, &[2]*int{&before, &item.Quantity}, nil); err != nil {
		return err
	}
	ev := domain.TenantSeatsChanged{TenantID: p.TenantID, LicensedSeats: item.Quantity, PreviousLicensedSeats: &before,
		Reason: domain.SeatsPurchase, BillingRef: &p.PurchaseID, BillingSubscriptionID: p.BillingSubscriptionID}
	if p.DeferralReason != nil {
		ev.DeferredAmountMinor, ev.DeferralReason = p.DeferredAmountMinor, *p.DeferralReason
	}
	_, err = s.Events.Enqueue(ctx, ev, m)
	if err == nil {
		s.metrics().SeatChange("purchase", "confirmed")
	}
	return err
}

func seatView(p domain.SeatPurchase) PurchaseView {
	v := PurchaseView{PurchaseID: p.PurchaseID, Kind: "seats", Status: string(p.Status),
		Amount: MoneyFrom(domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}), SeatsAfter: ptr(p.SeatsAfter),
		CreatedAt: p.CreatedAt, ConfirmedAt: p.ConfirmedAt}
	if p.Status == domain.PurchaseConfirmed {
		v.SeatUpdate = "in_progress" // the UI reads Core's seat usage until it rises (§5.4.1 B-P8)
	}
	if len(p.ClientAction) > 0 {
		var ca ClientAction
		if json.Unmarshal(p.ClientAction, &ca) == nil {
			v.ClientAction = &ca
		}
	}
	return v
}

// GetSeatPurchase is B-P8.
func (s *Service) GetSeatPurchase(ctx context.Context, _ Caller, tenantID, purchaseID string) (PurchaseView, error) {
	var out PurchaseView
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		p, found, err := s.SeatPurchases.Get(ctx, purchaseID)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.ErrPurchaseNotFound, "seat purchase not found")
		}
		out = seatView(p)
		return nil
	})
	return out, err
}

// ListSeatPurchases is B-P9.
func (s *Service) ListSeatPurchases(ctx context.Context, _ Caller, tenantID string, pg Page) (List[PurchaseView], error) {
	limit := pg.Limit
	if limit == 0 {
		limit = 20
	}
	out := List[PurchaseView]{Items: []PurchaseView{}}
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		rows, err := s.SeatPurchases.List(ctx, tenantID, pg.Cursor, limit+1)
		if err != nil {
			return err
		}
		for i, p := range rows {
			if i == limit {
				out.NextCursor = ptr(rows[i-1].PurchaseID)
				break
			}
			out.Items = append(out.Items, seatView(p))
		}
		return nil
	})
	return out, err
}

// SeatInvoicePaid confirms a seat purchase when its proration invoice is
// paid (§8.5 step 3); a repeat is a no-op.
type SeatInvoicePaid struct {
	S    *Service
	Next port.PaymentEventHandler
}

// Handle implements port.PaymentEventHandler.
func (h SeatInvoicePaid) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	p, found, err := s.SeatPurchases.ByProviderInvoice(ctx, tenantID, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found {
		// Billing did not record the invoice id (a provider error after the
		// change): match the open purchase on the proration invoice.
		if p, found, err = s.seatByProrationInvoice(ctx, tenantID, cur); err != nil {
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
	if p.DeferralReason == nil && !invoicePaysFor(cur, domain.Money{AmountMinor: p.AmountMinor, Currency: p.Currency}) {
		return "", s.parkMismatchedPayment(ctx, tenantID, port.PurchaseKindSeats, p.PurchaseID, ev, cur) // BIL-PAY-7
	}
	if err := s.confirmSeats(ctx, &p, "webhook:"+ev.EventType, domain.CommercialSystemPrincipal, eventRef(ev),
		webhookMeta()); err != nil {
		return "", err
	}
	s.metrics().PurchaseToEvent(port.PurchaseKindSeats, s.now().Sub(ev.ProviderCreatedAt))
	return "applied:seat_purchase", nil
}

// seatByProrationInvoice is the open seat purchase without an invoice id
// that a proration invoice on the tenant's subscription pays (same amount,
// a charged line for the seats after); the invoice id is then recorded on
// it. An invoice another purchase or plan change recorded is never matched
// (BIL-PAY-6).
func (s *Service) seatByProrationInvoice(ctx context.Context, tenantID string, cur domain.ProviderObjectState) (domain.SeatPurchase, bool, error) {
	sub, found, err := s.Subs.Live(ctx, tenantID)
	if err != nil || !found || !isProrationInvoice(cur, sub.ProviderSubscriptionID) {
		return domain.SeatPurchase{}, false, err
	}
	p, found, err := s.SeatPurchases.Open(ctx, sub.BillingSubscriptionID)
	if err != nil || !found || p.ProviderInvoiceID != nil || p.DeferralReason != nil || !invoiceAmountIs(cur, p.AmountMinor, p.Currency) ||
		!chargedLinesMatch(cur, p.SeatsAfter, "") {
		return domain.SeatPurchase{}, false, err
	}
	if claimed, err := s.invoiceClaimed(ctx, tenantID, cur.Ref.ID, p.PurchaseID); err != nil || claimed {
		return domain.SeatPurchase{}, false, err
	}
	p.ProviderInvoiceID = &cur.Ref.ID
	return p, true, nil
}

// ── Scheduled reduction (§8.6.1) ───────────────────────────────────────────

const reductionNote = "Renewal bills the greater of %d and the seats in use at renewal."

// ScheduleReduction is B-P10: no provider call and no event (BIL-SEAT-4).
func (s *Service) ScheduleReduction(ctx context.Context, c Caller, tenantID string, r ReductionRequest) (ScheduleView, error) {
	ctx, end := StartSpan(ctx, "billing.seats.schedule_reduction", AttrTenant(tenantID))
	out, err := s.scheduleReduction(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) scheduleReduction(ctx context.Context, c Caller, tenantID string, r ReductionRequest) (ScheduleView, error) {
	// Seats in use for display; a failure leaves the fields null (§5.4.1).
	var inUse *int
	if u, err := s.Core.SeatUsage(ctx, tenantID); err == nil {
		inUse = ptr(u.InUse())
	}
	var out ScheduleView
	err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		sub, found, err := s.Subs.Live(ctx, tenantID)
		if err != nil {
			return err
		}
		if !found || (sub.Status != domain.StatusActive && sub.Status != domain.StatusPastDue) {
			return domain.NewError(domain.ErrSubscriptionNotActive, "the subscription is not active")
		}
		if !sub.AutoRenew {
			return domain.NewError(domain.ErrSubscriptionEnding, "the subscription ends at the period end")
		}
		item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		if !domain.ValidReductionTarget(r.TargetSeats, item.Quantity) {
			return domain.NewError(domain.ErrTargetNotBelowCurrent, "target_seats must be below the licensed seats").With("licensed_seats", item.Quantity)
		}
		if old, found, err := s.Schedules.Open(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if found {
			if old.Status == domain.ScheduleApplying {
				return domain.NewError(domain.ErrScheduleBeingApplied, "the current reduction is being applied to the renewal")
			}
			old.Status = domain.ScheduleCancelled
			if err := s.Schedules.Update(ctx, old); err != nil {
				return err
			}
		}
		if sub.CurrentPeriodEnd == nil {
			return domain.NewError(domain.ErrSubscriptionNotActive, "the subscription has no current period")
		}
		sch := domain.SeatChangeSchedule{ScheduleID: newID(), TenantID: tenantID, BillingSubscriptionID: sub.BillingSubscriptionID,
			TargetSeats: r.TargetSeats, EffectiveAt: *sub.CurrentPeriodEnd, Status: domain.ScheduleScheduled, RequestedBy: actorOf(c)}
		if err := s.Schedules.Insert(ctx, sch); err != nil {
			return err
		}
		out = ScheduleView{ScheduleID: sch.ScheduleID, TargetSeats: sch.TargetSeats, EffectiveAt: sch.EffectiveAt,
			Status: string(sch.Status), SeatsInUse: inUse, Note: fmt.Sprintf(reductionNote, r.TargetSeats)}
		if inUse != nil {
			out.RenewalEstimateSeats = ptr(domain.RenewalSeatCount(r.TargetSeats, *inUse))
		}
		return nil
	})
	return out, err
}

// CancelReduction is B-P11.
func (s *Service) CancelReduction(ctx context.Context, _ Caller, tenantID string) error {
	var sch domain.SeatChangeSchedule
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if sub, found, err = s.Subs.Live(ctx, tenantID); err != nil {
			return err
		} else if !found {
			return domain.NewError(domain.ErrScheduleNotFound, "no scheduled reduction")
		}
		sch, found, err = s.Schedules.Latest(ctx, sub.BillingSubscriptionID)
		switch {
		case err != nil:
			return err
		case !found || sch.Status == domain.ScheduleCancelled || sch.Status == domain.ScheduleSkipped:
			return domain.NewError(domain.ErrScheduleNotFound, "no scheduled reduction")
		case sch.Status == domain.ScheduleApplied:
			return domain.NewError(domain.ErrScheduleAlreadyApplied, "the reduction was applied at the renewal")
		}
		return nil
	}); err != nil {
		return err
	}
	if sch.Status == domain.ScheduleApplying && sch.ProviderScheduleID != nil {
		prov, err := s.provider(sub.Provider)
		if err != nil {
			return err
		}
		if err := prov.ReleaseSchedule(ctx, port.ScheduleRef{ProviderScheduleID: *sch.ProviderScheduleID}, "sched:"+sch.ScheduleID+":release"); err != nil {
			return providerErr(err)
		}
	}
	return s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, found, err := s.Schedules.Get(ctx, sch.ScheduleID)
		if err != nil || !found || !cur.Status.Open() {
			return err
		}
		cur.Status = domain.ScheduleCancelled
		return s.Schedules.Update(ctx, cur)
	})
}
