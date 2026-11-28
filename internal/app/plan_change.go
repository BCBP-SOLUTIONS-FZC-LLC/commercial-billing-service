package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// planPreview is what a B-P3 upgrade preview stores for B-P4.
type planPreview struct {
	TargetPlan       domain.PlanCode `json:"target_plan"`
	ProrationInstant time.Time       `json:"proration_instant"`
	AmountMinor      int64           `json:"amount_minor"`
	Currency         string          `json:"currency"`
	RecordVersion    int64           `json:"record_version"`
}

// liveForChange reads the tenant's subscription for B-P3 / B-P4: active,
// not paused (§5.4.1 errors).
func (s *Service) liveForChange(ctx context.Context, tenantID string) (domain.Subscription, domain.SubscriptionItem, domain.PlanPrice, error) {
	var (
		sub   domain.Subscription
		item  domain.SubscriptionItem
		price domain.PlanPrice
	)
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		sub, found, err = s.Subs.Live(ctx, tenantID)
		if err != nil {
			return err
		}
		if !found || sub.Status == domain.StatusIncomplete {
			return domain.NewError(domain.ErrSubscriptionRequired, "the tenant has no paid subscription")
		}
		if sub.Status != domain.StatusActive {
			return domain.NewError(domain.ErrSubscriptionNotActive, "the subscription is not active").With("status", string(sub.Status))
		}
		if sub.CollectionPaused {
			return domain.NewError(domain.ErrCollectionPaused, "billing is paused for this subscription")
		}
		if item, err = s.Subs.Item(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		}
		price, err = s.Prices.Get(ctx, item.PriceID)
		return err
	})
	return sub, item, price, err
}

// targetPrice is the price row for the target plan on the subscription's
// interval, currency and provider, effective when the change takes effect:
// now for an upgrade, the renewal R = current_period_end for a downgrade
// (BIL-SUB-8: every ScheduleAtPeriodEnd prices at R; F-12, rev 0.71).
func (s *Service) targetPrice(ctx context.Context, sub domain.Subscription, cur domain.PlanPrice, target domain.PlanCode) (domain.PlanPrice, error) {
	at := s.now()
	if domain.DirectionOf(sub.Plan, target) == domain.DirectionDowngrade && sub.CurrentPeriodEnd != nil {
		at = *sub.CurrentPeriodEnd
	}
	var p domain.PlanPrice
	err := s.Tx.RunInTenantTx(ctx, sub.TenantID, func(ctx context.Context) error {
		var ok bool
		var err error
		p, ok, err = s.Prices.PriceAt(ctx, target, sub.Interval, cur.Currency, sub.Provider, at)
		if err == nil && (!ok || (p.ProviderPriceID == nil && sub.Provider != manualProvider)) {
			return domain.NewError(domain.ErrPriceNotAvailable, "no price for the target plan")
		}
		return err
	})
	return p, err
}

// PreviewPlanChange is B-P3.
func (s *Service) PreviewPlanChange(ctx context.Context, c Caller, tenantID string, r PlanChangePreviewRequest) (PlanChangePreview, error) {
	target := domain.PlanCode(r.TargetPlan)
	sub, item, cur, err := s.liveForChange(ctx, tenantID)
	if err != nil {
		return PlanChangePreview{}, err
	}
	dir := domain.DirectionOf(sub.Plan, target)
	if dir == domain.DirectionSame {
		return PlanChangePreview{}, domain.NewError(domain.ErrSamePlan, "the subscription is already on this plan")
	}
	tp, err := s.targetPrice(ctx, sub, cur, target)
	if err != nil {
		return PlanChangePreview{}, err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return PlanChangePreview{}, err
	}
	renewal := MoneyFrom(domain.Money{AmountMinor: tp.UnitAmountMinor * int64(item.Quantity), Currency: tp.Currency})
	out := PlanChangePreview{Direction: string(dir), TargetPlan: string(target), NextRenewalAmount: renewal}
	if dir == domain.DirectionDowngrade {
		check, err := s.Core.PlanChangeCheck(ctx, tenantID, target)
		if err != nil {
			return PlanChangePreview{}, err
		}
		out.Effective, out.EffectiveAt = "period_end", sub.CurrentPeriodEnd
		out.Check = &PlanChange{Allowed: check.Allowed, Blockers: blockers(check)}
		return out, nil
	}
	now := s.now()
	pv, err := prov.PreviewPriceChange(ctx, port.PriceChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: deref(sub.ProviderSubscriptionID)},
		ToPriceRef: deref(tp.ProviderPriceID), ProrationInstant: now})
	if err != nil {
		return PlanChangePreview{}, providerErr(err)
	}
	id, exp := newID(), now.Add(s.Settings.SeatPreviewTTL)
	body, _ := json.Marshal(planPreview{TargetPlan: target, ProrationInstant: pv.ProrationInstant, AmountMinor: pv.Amount.AmountMinor,
		Currency: pv.Amount.Currency, RecordVersion: sub.RecordVersion})
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error { return s.Previews.Save(ctx, tenantID, id, body, exp) }); err != nil {
		return PlanChangePreview{}, err
	}
	amount := MoneyFrom(pv.Amount)
	out.Effective, out.AmountDueNow, out.ProrationInstant, out.PreviewID, out.ExpiresAt = "on_payment", &amount, &pv.ProrationInstant, id, &exp
	if !pv.RenewalAmount.IsZero() {
		out.NextRenewalAmount = MoneyFrom(pv.RenewalAmount)
	}
	return out, nil
}

func blockers(c port.PlanChangeCheck) []Blocker {
	out := make([]Blocker, 0, len(c.Blockers))
	for _, b := range c.Blockers {
		out = append(out, Blocker{Code: b.Code, Detail: b.Detail})
	}
	return out
}

func blockerCodes(c port.PlanChangeCheck) []string {
	out := make([]string, 0, len(c.Blockers))
	for _, b := range c.Blockers {
		out = append(out, b.Code)
	}
	return out
}

// ChangePlan is B-P4 (§8.4).
func (s *Service) ChangePlan(ctx context.Context, c Caller, tenantID string, r PlanChangeRequest) (PlanChangeResult, error) {
	ctx, end := StartSpan(ctx, "billing.plan.change", AttrTenant(tenantID))
	out, err := s.changePlan(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) changePlan(ctx context.Context, c Caller, tenantID string, r PlanChangeRequest) (PlanChangeResult, error) {
	target := domain.PlanCode(r.TargetPlan)
	sub, item, cur, err := s.liveForChange(ctx, tenantID)
	if err != nil {
		return PlanChangeResult{}, err
	}
	if sub.RecordVersion != r.RecordVersion {
		return PlanChangeResult{}, domain.NewError(domain.ErrOptimisticLockConflict, "the subscription changed since it was read").
			With("record_version", sub.RecordVersion)
	}
	dir := domain.DirectionOf(sub.Plan, target)
	if dir == domain.DirectionSame {
		return PlanChangeResult{}, domain.NewError(domain.ErrSamePlan, "the subscription is already on this plan")
	}
	tp, err := s.targetPrice(ctx, sub, cur, target)
	if err != nil {
		return PlanChangeResult{}, err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return PlanChangeResult{}, err
	}
	if !prov.Capabilities().Schedules && dir == domain.DirectionDowngrade || !prov.Capabilities().RecurringSubscriptions {
		return PlanChangeResult{}, domain.NewError(domain.ErrProviderOperationUnsupported, "plan changes are not supported by this provider")
	}
	if dir == domain.DirectionDowngrade {
		return s.scheduleDowngrade(ctx, c, sub, item, tp, prov, target)
	}
	return s.upgrade(ctx, c, sub, tp, prov, target, r.PreviewID)
}

func (s *Service) newChange(c Caller, sub domain.Subscription, target domain.PlanCode, dir domain.PlanChangeDirection, status domain.ChangeStatus) domain.PlanChangeRequest {
	key := c.IdempotencyKey
	id := newID()
	if key == "" {
		key = "change:" + id
	}
	return domain.PlanChangeRequest{ChangeID: id, TenantID: sub.TenantID, BillingSubscriptionID: sub.BillingSubscriptionID,
		FromPlan: sub.Plan, ToPlan: target, Direction: dir, Status: status, RequestedBy: actorOf(c), IdempotencyKey: key}
}

func (s *Service) upgrade(ctx context.Context, c Caller, sub domain.Subscription, tp domain.PlanPrice, prov port.PaymentProvider,
	target domain.PlanCode, previewID string) (PlanChangeResult, error) {
	if previewID == "" {
		return PlanChangeResult{}, domain.NewError(domain.ErrValidation, "preview_id is required for an upgrade")
	}
	var pv planPreview
	if err := s.Tx.RunInTenantTx(ctx, sub.TenantID, func(ctx context.Context) error {
		body, exp, found, err := s.Previews.Load(ctx, sub.TenantID, previewID)
		switch {
		case err != nil:
			return err
		case !found || exp.Before(s.now()):
			return domain.NewError(domain.ErrPreviewExpired, "the preview expired; preview again")
		}
		return json.Unmarshal(body, &pv)
	}); err != nil {
		return PlanChangeResult{}, err
	}
	if pv.TargetPlan != target || pv.RecordVersion != sub.RecordVersion {
		return PlanChangeResult{}, domain.NewError(domain.ErrPreviewStale, "the preview no longer matches the subscription; preview again")
	}

	// Intent under the lock, then the provider call outside (BIL-CONC-2).
	req := s.newChange(c, sub, target, domain.DirectionUpgrade, domain.ChangePendingPayment)
	req.AmountMinor, req.Currency = &pv.AmountMinor, &pv.Currency
	if err := s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		if _, found, err := s.Changes.Open(ctx, sub.TenantID); err != nil {
			return err
		} else if found {
			return domain.NewError(domain.ErrPlanChangePending, "a plan change is already pending")
		}
		return s.Changes.Insert(ctx, req)
	}); err != nil {
		return PlanChangeResult{}, err
	}
	res, err := prov.ChangePrice(ctx, port.PriceChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: deref(sub.ProviderSubscriptionID)},
		ToPriceRef: deref(tp.ProviderPriceID), ProrationInstant: pv.ProrationInstant}, "planchg:"+req.ChangeID)
	amount := MoneyFrom(domain.Money{AmountMinor: pv.AmountMinor, Currency: pv.Currency})
	out := PlanChangeResult{ChangeID: req.ChangeID, Direction: string(domain.DirectionUpgrade), Amount: &amount}
	var action port.ErrActionRequired
	var declined port.ErrDeclined
	switch {
	case errors.As(err, &declined) || err == nil && res.Outcome.Status == port.PaymentDeclined:
		code := declined.Code
		if code == "" {
			code = res.Outcome.DeclineCode
		}
		return s.upgradeDeclined(ctx, c, sub, req, out, res.Invoice.ProviderInvoiceID, deref(tp.ProviderPriceID), code)
	case errors.As(err, &action) || err == nil && res.Outcome.Status == port.PaymentRequiresAction:
		secret := action.ClientSecret
		if secret == "" {
			secret = res.Outcome.ClientSecret
		}
		ca := &ClientAction{Type: "3ds", ClientSecret: secret}
		raw, _ := json.Marshal(ca)
		var settled *domain.PlanChangeRequest
		if err := s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
			cur, pending, err := s.reloadUpgrade(ctx, req)
			if err != nil || !pending {
				settled = &cur
				return err
			}
			cur.ClientAction = raw
			if res.Invoice.ProviderInvoiceID != "" {
				cur.ProviderInvoiceID = &res.Invoice.ProviderInvoiceID
			}
			return s.Changes.Update(ctx, cur)
		}); err != nil {
			return out, err
		}
		if settled != nil {
			out.Status, out.EffectiveAt = string(settled.Status), settled.EffectiveAt
			return out, nil
		}
		out.Status, out.ClientAction = string(domain.ChangePendingPayment), ca
		return out, nil
	case err != nil:
		// The outcome is unknown (the provider may have invoiced and charged):
		// the request stays pending_payment; the callback or purchase-expiry
		// (which looks the invoice up) settles it.
		return out, providerErr(err)
	}
	// Commit the outcome under the lock with a re-read (BIL-CONC-2, F-5 rev
	// 0.71): the invoice.paid callback or purchase-expiry may have settled the
	// request while ChangePrice ran; it is never overwritten or applied twice.
	err = s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		cur, pending, err := s.reloadUpgrade(ctx, req)
		if err != nil {
			return err
		}
		if !pending {
			req = cur
			return nil
		}
		if res.Invoice.ProviderInvoiceID != "" {
			cur.ProviderInvoiceID = &res.Invoice.ProviderInvoiceID
		}
		if res.Outcome.Status != port.PaymentPaid { // pending: completes on invoice.paid
			req = cur
			return s.Changes.Update(ctx, cur)
		}
		if err := s.applyPlanChange(ctx, &cur, tp, actorOf(c), "api:B-P4", nil, metaFor(c)); err != nil {
			return err
		}
		req = cur
		s.metrics().PurchaseToEvent(port.PurchaseKindPlan, 0)
		return nil
	})
	out.Status, out.EffectiveAt = string(req.Status), req.EffectiveAt
	return out, err
}

// reloadUpgrade re-reads an upgrade request under the tenant lock after the
// provider call and reports whether it is still pending_payment (F-5).
func (s *Service) reloadUpgrade(ctx context.Context, req domain.PlanChangeRequest) (domain.PlanChangeRequest, bool, error) {
	cur, found, err := s.Changes.Get(ctx, req.TenantID, req.ChangeID)
	if err != nil {
		return req, false, err
	}
	if !found {
		return req, false, domain.NewError(domain.ErrInternal, "plan change request not found after the provider call")
	}
	return cur, cur.Status == domain.ChangePendingPayment, nil
}

// upgradeDeclined is the declined upgrade (§8.4.1: request failed, plan
// unchanged): the open proration invoice is voided outside the transaction
// first, so a later collection cannot charge for a failed upgrade. If the
// void fails the request stays pending_payment and purchase-expiry voids
// it; if the invoice turns out paid the upgrade is applied.
func (s *Service) upgradeDeclined(ctx context.Context, c Caller, sub domain.Subscription, req domain.PlanChangeRequest, out PlanChangeResult,
	invoiceID, targetPriceRef, code string) (PlanChangeResult, error) {
	if req.CreatedAt.IsZero() { // the intent just committed
		req.CreatedAt = s.now()
	}
	inv, verr := s.closeInvoice(ctx, sub.Provider, nonEmpty(invoiceID), upgradeTarget(deref(sub.ProviderSubscriptionID), req, targetPriceRef),
		"planchg:"+req.ChangeID+":void")
	applied := false
	_ = s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Changes.Open(ctx, sub.TenantID)
		if err != nil || !found || cur.ChangeID != req.ChangeID || cur.Status != domain.ChangePendingPayment {
			return err
		}
		if err := s.recheckInvoice(ctx, sub.TenantID, req.ChangeID, &inv); err != nil {
			return err
		}
		if inv.ID != "" {
			cur.ProviderInvoiceID = &inv.ID
		}
		switch {
		case inv.Paid:
			if err := s.settleUpgrade(ctx, &cur, actorOf(c), "api:B-P4", nil, metaFor(c)); err != nil {
				return err
			}
			req, applied = cur, true
			return nil
		case verr == nil:
			cur.Status, cur.ClientAction = domain.ChangeFailed, nil
		}
		return s.Changes.Update(ctx, cur)
	})
	if applied {
		out.Status, out.EffectiveAt = string(req.Status), req.EffectiveAt
		return out, nil
	}
	return out, domain.NewError(domain.ErrPaymentDeclined, "payment declined; the plan is unchanged").With("decline_reason", declineReason(code))
}

// settleUpgrade applies a pending upgrade whose proration invoice is paid,
// at the target plan's price now. Under the lock.
func (s *Service) settleUpgrade(ctx context.Context, req *domain.PlanChangeRequest, actor, cause string, evRef *string, m port.EventMeta) error {
	tp, err := s.upgradePrice(ctx, *req)
	if err != nil {
		return err
	}
	return s.applyPlanChange(ctx, req, tp, actor, cause, evRef, m)
}

// upgradePrice is the target plan's price an upgrade moves to now (the
// subscription's interval and the seat item's currency). In a transaction.
func (s *Service) upgradePrice(ctx context.Context, req domain.PlanChangeRequest) (domain.PlanPrice, error) {
	sub, err := s.Subs.Get(ctx, req.BillingSubscriptionID)
	if err != nil {
		return domain.PlanPrice{}, err
	}
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return domain.PlanPrice{}, err
	}
	cp, err := s.Prices.Get(ctx, item.PriceID)
	if err != nil {
		return domain.PlanPrice{}, err
	}
	tp, ok, err := s.Prices.PriceAt(ctx, req.ToPlan, sub.Interval, cp.Currency, sub.Provider, s.now())
	if err != nil || !ok {
		return domain.PlanPrice{}, errors.Join(err, domain.NewError(domain.ErrPriceNotAvailable, "target price missing"))
	}
	return tp, nil
}

// upgradeByProrationInvoice is the tenant's pending upgrade without an
// invoice id that a proration invoice on its subscription pays (same
// amount, a charged line at the target price); the invoice id is then
// recorded on it. An invoice another purchase or plan change recorded is
// never matched (BIL-PAY-6).
func (s *Service) upgradeByProrationInvoice(ctx context.Context, tenantID string, cur domain.ProviderObjectState) (domain.PlanChangeRequest, bool, error) {
	req, found, err := s.Changes.Open(ctx, tenantID)
	if err != nil || !found || req.Status != domain.ChangePendingPayment || req.ProviderInvoiceID != nil || req.AmountMinor == nil {
		return domain.PlanChangeRequest{}, false, err
	}
	sub, err := s.Subs.Get(ctx, req.BillingSubscriptionID)
	if err != nil || !isProrationInvoice(cur, sub.ProviderSubscriptionID) || !invoiceAmountIs(cur, *req.AmountMinor, deref(req.Currency)) {
		return domain.PlanChangeRequest{}, false, err
	}
	tp, err := s.upgradePrice(ctx, req)
	if err != nil || !chargedLinesMatch(cur, 0, deref(tp.ProviderPriceID)) {
		return domain.PlanChangeRequest{}, false, err
	}
	if claimed, err := s.invoiceClaimed(ctx, tenantID, cur.Ref.ID, req.ChangeID); err != nil || claimed {
		return domain.PlanChangeRequest{}, false, err
	}
	req.ProviderInvoiceID = &cur.Ref.ID
	return req, true, nil
}

// applyPlanChange moves the subscription to the target plan and price,
// closes the request and emits TenantPlanChanged (§8.4). Under the lock.
func (s *Service) applyPlanChange(ctx context.Context, req *domain.PlanChangeRequest, tp domain.PlanPrice, actor, cause string,
	evRef *string, m port.EventMeta) error {
	now := s.now()
	sub, err := s.Subs.Update(ctx, req.BillingSubscriptionID, 0, func(x *domain.Subscription) { x.Plan = req.ToPlan })
	if err != nil {
		return err
	}
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return err
	}
	item.PriceID = tp.PriceID
	if err := s.Subs.SetItem(ctx, item); err != nil {
		return err
	}
	req.Status, req.EffectiveAt, req.ClientAction = domain.ChangeApplied, &now, nil
	if err := s.Changes.Update(ctx, *req); err != nil {
		return err
	}
	from := sub.Status
	if err := s.history(ctx, sub, &from, cause, actor, evRef, nil, &[2]*domain.PlanCode{&req.FromPlan, &req.ToPlan}); err != nil {
		return err
	}
	err = s.emit(ctx, domain.TenantPlanChanged{TenantID: sub.TenantID, Plan: req.ToPlan, PreviousPlan: req.FromPlan,
		Direction: req.Direction, EffectiveAt: now, BillingRef: req.ChangeID, BillingSubscriptionID: sub.BillingSubscriptionID}, m)
	return err
}

// metaFor is the envelope metadata of a caller-driven change (§7.3.1).
func metaFor(c Caller) port.EventMeta {
	return port.EventMeta{Actor: c.UserID, IPAddress: c.ClientIP, UserAgent: c.UserAgent}
}

// webhookMeta is the envelope metadata of a callback-driven change
// (actor commercial-system, ip "system", §7.3.1).
func webhookMeta() port.EventMeta { return port.EventMeta{UserAgent: webhookUA} }

func (s *Service) scheduleDowngrade(ctx context.Context, c Caller, sub domain.Subscription, item domain.SubscriptionItem, tp domain.PlanPrice,
	prov port.PaymentProvider, target domain.PlanCode) (PlanChangeResult, error) {
	check, err := s.Core.PlanChangeCheck(ctx, sub.TenantID, target)
	if err != nil {
		return PlanChangeResult{}, err
	}
	now := s.now()
	req := s.newChange(c, sub, target, domain.DirectionDowngrade, domain.ChangeScheduled)
	req.I18CheckedAt, req.EffectiveAt = &now, sub.CurrentPeriodEnd
	if !check.Allowed {
		req.Status, req.BlockerCodes = domain.ChangeBlocked, blockerCodes(check)
		for _, b := range req.BlockerCodes {
			s.metrics().DowngradeBlocked(b)
		}
		if err := s.locked(ctx, sub.TenantID, func(ctx context.Context) error { return s.Changes.Insert(ctx, req) }); err != nil {
			return PlanChangeResult{}, err
		}
		return PlanChangeResult{}, domain.NewError(domain.ErrPlanChangeBlocked, "Remove SSO or move users to local accounts before downgrading.").
			With("blockers", blockers(check))
	}
	if err := s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		if _, found, err := s.Changes.Open(ctx, sub.TenantID); err != nil {
			return err
		} else if found {
			return domain.NewError(domain.ErrPlanChangePending, "a plan change is already pending")
		}
		return s.Changes.Insert(ctx, req)
	}); err != nil {
		return PlanChangeResult{}, err
	}
	at := time.Time{}
	if sub.CurrentPeriodEnd != nil {
		at = *sub.CurrentPeriodEnd
	}
	ref, err := prov.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: deref(sub.ProviderSubscriptionID)},
		ToPriceRef: deref(tp.ProviderPriceID), ToQuantity: item.Quantity, At: at}, "planchg:"+req.ChangeID)
	err2 := s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		if err != nil {
			req.Status = domain.ChangeFailed
		} else {
			req.ProviderScheduleID = &ref.ProviderScheduleID
		}
		return s.Changes.Update(ctx, req)
	})
	if err != nil {
		return PlanChangeResult{}, providerErr(err)
	}
	return PlanChangeResult{ChangeID: req.ChangeID, Direction: string(domain.DirectionDowngrade), Status: string(domain.ChangeScheduled),
		EffectiveAt: req.EffectiveAt}, err2
}

// CancelPlanChange is B-P5: release a scheduled downgrade.
func (s *Service) CancelPlanChange(ctx context.Context, _ Caller, tenantID string) error {
	var req domain.PlanChangeRequest
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		req, found, err = s.Changes.Open(ctx, tenantID)
		if err != nil {
			return err
		}
		if !found || req.Status != domain.ChangeScheduled {
			return domain.NewError(domain.ErrPlanChangeNotFound, "no scheduled plan change")
		}
		sub, err = s.Subs.Get(ctx, req.BillingSubscriptionID)
		return err
	}); err != nil {
		return err
	}
	if req.ProviderScheduleID != nil {
		prov, err := s.provider(sub.Provider)
		if err != nil {
			return err
		}
		if err := prov.ReleaseSchedule(ctx, port.ScheduleRef{ProviderScheduleID: *req.ProviderScheduleID}, "planchg:"+req.ChangeID+":release"); err != nil {
			return providerErr(err)
		}
	}
	return s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, found, err := s.Changes.Open(ctx, tenantID)
		if err != nil || !found || cur.ChangeID != req.ChangeID {
			return err
		}
		cur.Status = domain.ChangeCancelled
		return s.Changes.Update(ctx, cur)
	})
}

// UpgradeInvoicePaid applies a pending upgrade when its proration invoice
// is paid (3-D Secure completed, or a pending update paid later).
type UpgradeInvoicePaid struct {
	S    *Service
	Next port.PaymentEventHandler
}

// Handle implements port.PaymentEventHandler.
func (h UpgradeInvoicePaid) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	req, found, err := s.Changes.ByProviderInvoice(ctx, tenantID, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found {
		// Billing did not record the invoice id (a provider error after the
		// change): match the pending upgrade on the proration invoice.
		if req, found, err = s.upgradeByProrationInvoice(ctx, tenantID, cur); err != nil {
			return "", err
		}
	}
	if !found {
		if h.Next == nil {
			return "", &port.IgnoredEvent{Reason: "no_handler"}
		}
		return h.Next.Handle(ctx, tenantID, ev, cur)
	}
	if req.Status != domain.ChangePendingPayment {
		return "", &port.IgnoredEvent{Reason: "change_not_pending"}
	}
	if req.AmountMinor != nil && !invoicePaysFor(cur, domain.Money{AmountMinor: *req.AmountMinor, Currency: deref(req.Currency)}) {
		return "", s.parkMismatchedPayment(ctx, tenantID, port.PurchaseKindPlan, req.ChangeID, ev, cur) // BIL-PAY-7
	}
	if err := s.settleUpgrade(ctx, &req, domain.CommercialSystemPrincipal, "webhook:"+ev.EventType, eventRef(ev), webhookMeta()); err != nil {
		return "", err
	}
	s.metrics().PurchaseToEvent(port.PurchaseKindPlan, s.now().Sub(ev.ProviderCreatedAt))
	return "applied:plan_upgrade", nil
}

// DowngradeOnCycle applies a scheduled downgrade on the first cycle
// invoice.paid of the period it takes effect in (§8.4.2 step 3).
type DowngradeOnCycle struct {
	S    *Service
	Next port.PaymentEventHandler
}

// Handle implements port.PaymentEventHandler.
func (h DowngradeOnCycle) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	next := func() (string, error) {
		if h.Next == nil {
			return "", &port.IgnoredEvent{Reason: "no_handler"}
		}
		return h.Next.Handle(ctx, tenantID, ev, cur)
	}
	if reason, _ := cur.Fields["billing_reason"].(string); reason != "subscription_cycle" {
		return next()
	}
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	req, found, err := s.Changes.Open(ctx, tenantID)
	if err != nil {
		return "", err
	}
	start, _ := cur.Fields["line_period_start"].(time.Time)
	if !found || req.Direction != domain.DirectionDowngrade || req.Status != domain.ChangeScheduled || req.EffectiveAt == nil ||
		start.IsZero() || start.Before(req.EffectiveAt.Add(-time.Minute)) {
		return next()
	}
	sub, err := s.Subs.Get(ctx, req.BillingSubscriptionID)
	if err != nil {
		return "", err
	}
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return "", err
	}
	cp, err := s.Prices.Get(ctx, item.PriceID)
	if err != nil {
		return "", err
	}
	tp, ok, err := s.Prices.PriceAt(ctx, req.ToPlan, sub.Interval, cp.Currency, sub.Provider, start)
	if err != nil || !ok {
		return "", errors.Join(err, domain.NewError(domain.ErrPriceNotAvailable, "target price missing"))
	}
	if err := s.applyPlanChange(ctx, &req, tp, domain.CommercialSystemPrincipal, "webhook:"+ev.EventType, eventRef(ev),
		webhookMeta()); err != nil {
		return "", err
	}
	if _, err := next(); err != nil {
		var ig *port.IgnoredEvent
		if !errors.As(err, &ig) {
			return "", err
		}
	}
	return "applied:plan_downgrade", nil
}
