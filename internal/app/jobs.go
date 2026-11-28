package app

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// The §13.2 CronJobs. Each selects its work across tenants (JobRepo, system
// pool), makes any provider or IAM call outside a transaction (BIL-CONC-2)
// and commits under the tenant lock after re-reading (BIL-CONC-1). A
// failure on one tenant is logged and left for the next run; the job
// returns the number of items it handled.

const (
	jobBatch            = 200
	iamGraceBeforeRenew = time.Hour        // seat-renewal: I-11 unavailable this close → skipped (§8.6.2)
	i18GraceBeforeApply = 15 * time.Minute // downgrade-apply: I-18 unavailable this close → blocked (§8.4.2)
	periodSlack         = time.Minute
)

// JobResult is what a job reports: items handled and items that failed
// (retried next run).
type JobResult struct{ Handled, Failed int }

func (r *JobResult) add(err error) {
	if err != nil {
		r.Failed++
		return
	}
	r.Handled++
}

func (s *Service) metrics() port.Metrics {
	if s.Metrics == nil {
		return port.NopMetrics{}
	}
	return s.Metrics
}

// ── saga-sweeper (§8.2, BIL-OPS-5) ─────────────────────────────────────────

// SweepSagas raises BillingConversionSagaOverdue once per saga (alerted_at)
// and then re-runs due sagas the in-pod worker missed. The overdue check
// runs first, so a hung Realm Provisioner (each attempt waits RP_TIMEOUT)
// cannot push the BIL-OPS-5 page past the job's deadline; the re-runs get
// what is left of it (sagaRunShare).
func (s *Service) SweepSagas(ctx context.Context) (JobResult, error) {
	var res JobResult
	unalerted, overdue, err := s.Jobs.OverdueSagas(ctx, s.now().Add(-s.Settings.SagaAlertAfter))
	if err != nil {
		return res, err
	}
	s.metrics().SagasOverdue(overdue)
	for _, ref := range unalerted {
		var marked bool
		// The saga row is written: under the tenant lock (BIL-CONC-1).
		err := s.locked(ctx, ref.TenantID, func(ctx context.Context) error {
			var err error
			marked, err = s.Jobs.MarkSagaAlerted(ctx, ref.SagaID)
			return err
		})
		if err == nil && marked && s.Log != nil {
			s.logger(ctx).Error("BillingConversionSagaOverdue: conversion not confirmed 30 minutes after payment (SEV-2)",
				map[string]any{"saga_id": ref.SagaID, "tenant_id": ref.TenantID, "severity": "SEV-2"})
		}
		res.add(err)
	}
	runCtx, cancel := sagaRunContext(ctx)
	defer cancel()
	n, err := s.RunSagas(runCtx, jobBatch)
	res.Handled += n
	if err != nil && ctx.Err() == nil && runCtx.Err() == nil {
		return res, err
	}
	return res, nil
}

// sagaRunShare is the part of the job's remaining time the saga re-runs
// may use; the rest lets the job finish and report before its deadline.
const sagaRunShare = 0.8

// sagaRunContext bounds the saga re-runs by the job's deadline, if any.
func sagaRunContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	left := time.Until(deadline)
	return context.WithTimeout(ctx, time.Duration(float64(left)*sagaRunShare))
}

// ── seat-renewal (§8.6.2, §8.6.3) ──────────────────────────────────────────

// RunSeatRenewal applies scheduled reductions within SEAT_RENEWAL_LEAD and
// completes those whose renewal has happened (top-up, final seats, event).
func (s *Service) RunSeatRenewal(ctx context.Context) (JobResult, error) {
	var res JobResult
	now := s.now()
	due, err := s.Schedules.Due(ctx, now.Add(s.Settings.SeatRenewalLead), jobBatch)
	if err != nil {
		return res, err
	}
	for _, sch := range due {
		var err error
		if sch.Status == domain.ScheduleApplying && sch.RenewalSeats != nil {
			err = s.completeRenewal(ctx, sch)
		} else {
			err = s.applyAhead(ctx, sch)
		}
		if err != nil {
			s.logger(ctx).Warn("seat-renewal: schedule not processed; retried next run", map[string]any{"schedule_id": sch.ScheduleID, "tenant_id": sch.TenantID, "error": err.Error()})
		}
		res.add(err)
	}
	return res, nil
}

// applyAhead is §8.6.2: commit applying, fresh I-11, schedule the renewal
// quantity max(target, in use) at the period end without proration.
func (s *Service) applyAhead(ctx context.Context, sch domain.SeatChangeSchedule) error {
	var sub domain.Subscription
	var item domain.SubscriptionItem
	stop := false
	if err := s.locked(ctx, sch.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Schedules.Get(ctx, sch.ScheduleID)
		if err != nil || !found || !cur.Status.Open() || cur.RenewalSeats != nil {
			stop = true
			return err
		}
		if sub, err = s.Subs.Get(ctx, sch.BillingSubscriptionID); err != nil {
			return err
		}
		if (sub.Status != domain.StatusActive && sub.Status != domain.StatusPastDue) || !sub.AutoRenew {
			stop = true
			cur.Status, cur.SkipReason = domain.ScheduleSkipped, ptr("subscription_not_active")
			return s.Schedules.Update(ctx, cur)
		}
		if item, err = s.Subs.Item(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		}
		cur.Status = domain.ScheduleApplying
		sch = cur
		return s.Schedules.Update(ctx, cur)
	}); err != nil || stop {
		return err
	}
	usage, err := s.Core.SeatUsage(ctx, sch.TenantID)
	if err != nil {
		if !s.now().Before(sch.EffectiveAt.Add(-iamGraceBeforeRenew)) {
			// Never reduce blind: the renewal bills the current count.
			if s.Log != nil {
				s.logger(ctx).Error("seat reduction skipped: seat usage unavailable before renewal; the renewal bills the current count",
					map[string]any{"tenant_id": sch.TenantID, "schedule_id": sch.ScheduleID, "severity": "SEV-3"})
			}
			return s.locked(ctx, sch.TenantID, func(ctx context.Context) error {
				cur, found, err := s.Schedules.Get(ctx, sch.ScheduleID)
				if err != nil || !found || !cur.Status.Open() {
					return err
				}
				cur.Status, cur.SkipReason = domain.ScheduleSkipped, ptr("iam_unavailable")
				return s.Schedules.Update(ctx, cur)
			})
		}
		return err
	}
	inUse := usage.InUse()
	renewal := domain.RenewalSeatCount(sch.TargetSeats, inUse)
	var scheduleID *string
	if sub.Provider != manualProvider && sub.ProviderSubscriptionID != nil {
		prov, err := s.provider(sub.Provider)
		if err != nil {
			return err
		}
		priceRef, err := s.renewalPriceRef(ctx, sub, item)
		if err != nil {
			return err
		}
		ref, err := prov.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID},
			ToPriceRef: priceRef, ToQuantity: renewal, At: sch.EffectiveAt}, "sched:"+sch.ScheduleID+":apply")
		if err != nil {
			return providerErr(err)
		}
		scheduleID = &ref.ProviderScheduleID
	}
	s.metrics().SeatChange("reduction", "scheduled")
	return s.locked(ctx, sch.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Schedules.Get(ctx, sch.ScheduleID)
		if err != nil || !found || cur.Status != domain.ScheduleApplying {
			return err
		}
		cur.SeatsInUseAtApply, cur.RenewalSeats, cur.ProviderScheduleID = &inUse, &renewal, scheduleID
		return s.Schedules.Update(ctx, cur)
	})
}

// renewalPriceRef is BIL-SUB-8: the provider price effective at the
// renewal for the plan current then (a scheduled downgrade's target).
func (s *Service) renewalPriceRef(ctx context.Context, sub domain.Subscription, item domain.SubscriptionItem) (string, error) {
	var ref string
	err := s.Tx.RunInTenantTx(ctx, sub.TenantID, func(ctx context.Context) error {
		p, err := s.priceAtRenewal(ctx, sub, item)
		if err == nil {
			ref = deref(p.ProviderPriceID)
		}
		return err
	})
	return ref, err
}

// priceAtRenewal reads the target row at R = current_period_end (§8.16).
func (s *Service) priceAtRenewal(ctx context.Context, sub domain.Subscription, item domain.SubscriptionItem) (domain.PlanPrice, error) {
	cur, err := s.Prices.Get(ctx, item.PriceID)
	if err != nil {
		return domain.PlanPrice{}, err
	}
	if sub.CurrentPeriodEnd == nil {
		return cur, nil
	}
	plan := sub.Plan
	if req, found, err := s.Changes.Open(ctx, sub.TenantID); err != nil {
		return domain.PlanPrice{}, err
	} else if found && req.Direction == domain.DirectionDowngrade && req.Status == domain.ChangeScheduled {
		plan = req.ToPlan
	}
	return s.priceAt(ctx, sub, cur, plan)
}

// planPriceAtRenewal is plan's row effective at R (the item's current row
// when there is no period or no such row).
func (s *Service) planPriceAtRenewal(ctx context.Context, sub domain.Subscription, item domain.SubscriptionItem, plan domain.PlanCode) (domain.PlanPrice, error) {
	cur, err := s.Prices.Get(ctx, item.PriceID)
	if err != nil || sub.CurrentPeriodEnd == nil {
		return cur, err
	}
	return s.priceAt(ctx, sub, cur, plan)
}

func (s *Service) priceAt(ctx context.Context, sub domain.Subscription, cur domain.PlanPrice, plan domain.PlanCode) (domain.PlanPrice, error) {
	p, ok, err := s.Prices.PriceAt(ctx, plan, sub.Interval, cur.Currency, sub.Provider, *sub.CurrentPeriodEnd)
	if err != nil {
		return domain.PlanPrice{}, err
	}
	if !ok {
		return cur, nil
	}
	return p, nil
}

// completeRenewal is §8.6.3 after the cycle invoice.paid rolled the period:
// fresh I-11, a renewal top-up when members were added in the lead window
// (seats kept even if its payment fails, BL-OQ-6), then the final count.
func (s *Service) completeRenewal(ctx context.Context, sch domain.SeatChangeSchedule) error {
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, sch.TenantID, func(ctx context.Context) error {
		var err error
		sub, err = s.Subs.Get(ctx, sch.BillingSubscriptionID)
		return err
	}); err != nil {
		return err
	}
	if sub.CurrentPeriodStart == nil || sub.CurrentPeriodStart.Before(sch.EffectiveAt.Add(-periodSlack)) {
		return nil // the renewal has not happened yet
	}
	usage, err := s.Core.SeatUsage(ctx, sch.TenantID)
	if err != nil {
		return err
	}
	renewal := *sch.RenewalSeats
	final := domain.FinalRenewalSeats(renewal, usage.InUse())
	var topup *domain.SeatPurchase
	if final > renewal {
		if topup, err = s.chargeTopUp(ctx, sub, sch, renewal, final); err != nil {
			return err
		}
	}
	return s.locked(ctx, sch.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Schedules.Get(ctx, sch.ScheduleID)
		if err != nil || !found || cur.Status != domain.ScheduleApplying {
			return err
		}
		item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		before := item.Quantity
		item.Quantity = final
		if err := s.Subs.SetItem(ctx, item); err != nil {
			return err
		}
		cur.Status, cur.FinalSeats = domain.ScheduleApplied, &final
		if err := s.Schedules.Update(ctx, cur); err != nil {
			return err
		}
		reason, ref := domain.SeatsRenewal, &cur.ScheduleID
		if topup != nil {
			now := s.now()
			p, found, err := s.SeatPurchases.Get(ctx, topup.PurchaseID)
			if err != nil {
				return err
			}
			if found && p.Status.Open() {
				p.Status, p.ConfirmedAt, p.ProviderInvoiceID = domain.PurchaseConfirmed, &now, topup.ProviderInvoiceID
				if err := s.SeatPurchases.Update(ctx, p); err != nil {
					return err
				}
			}
			reason, ref = domain.SeatsRenewalTopUp, &topup.PurchaseID
		}
		cur2, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		from := cur2.Status
		if err := s.history(ctx, cur2, &from, "job:seat-renewal", domain.CommercialSystemPrincipal, nil, &[2]*int{&before, &final}, nil); err != nil {
			return err
		}
		if final > cur.TargetSeats && s.Log != nil {
			s.logger(ctx).Info("renewal above the scheduled target (seats in use)", map[string]any{"tenant_id": sch.TenantID, "target_seats": cur.TargetSeats, "final_seats": final})
		}
		if before == final && topup == nil {
			return nil
		}
		err = s.emit(ctx, domain.TenantSeatsChanged{TenantID: sch.TenantID, LicensedSeats: final, PreviousLicensedSeats: &before,
			Reason: reason, BillingRef: ref, BillingSubscriptionID: sub.BillingSubscriptionID}, jobMeta("seat-renewal"))
		return err
	})
}

// chargeTopUp records and charges a renewal top-up (§8.6.3 step 1). The
// quantity is applied at the provider even if the payment fails; the
// invoice then follows dunning. A retry reuses the open top-up row and key.
func (s *Service) chargeTopUp(ctx context.Context, sub domain.Subscription, sch domain.SeatChangeSchedule, renewal, final int) (*domain.SeatPurchase, error) {
	// A customer purchase from the old period is cancelled below: void its
	// invoice first, so it cannot be paid for seats never granted (§8.5.1).
	if err := s.closeSeatPurchaseInvoice(ctx, sub, true); err != nil {
		return nil, err
	}
	var p domain.SeatPurchase
	if err := s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		open, found, err := s.SeatPurchases.Open(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		if found && open.Reason == domain.SeatReasonRenewalTopUp {
			p = open
			return nil
		}
		if found {
			open.Status = domain.PurchaseCancelled // a customer purchase from the old period cannot complete now
			if err := s.SeatPurchases.Update(ctx, open); err != nil {
				return err
			}
		}
		acct, err := s.Accounts.Get(ctx, sub.TenantID)
		if err != nil {
			return err
		}
		// The account's currency, not DEFAULT_CURRENCY (C-3, rev 0.73). The
		// amount is the provider's, on the top-up invoice (amount_minor 0).
		p = domain.SeatPurchase{PurchaseID: newID(), TenantID: sub.TenantID, BillingSubscriptionID: sub.BillingSubscriptionID,
			Reason: domain.SeatReasonRenewalTopUp, SeatsAdded: final - renewal, SeatsBefore: renewal, SeatsAfter: final,
			ProrationInstant: s.now(), Currency: acct.Currency, AmountSource: domain.AmountSourceProvider,
			Status: domain.PurchasePendingPayment, RequestedBy: domain.CommercialSystemPrincipal}
		p.IdempotencyKey = "topup:" + sch.ScheduleID
		return s.SeatPurchases.Insert(ctx, p)
	}); err != nil {
		return nil, err
	}
	if sub.Provider == manualProvider || sub.ProviderSubscriptionID == nil {
		s.metrics().SeatChange("renewal_topup", "manual")
		return &p, nil
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return nil, err
	}
	res, err := prov.ChangeQuantity(ctx, port.QuantityChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID},
		From: p.SeatsBefore, To: p.SeatsAfter, ProrationInstant: p.ProrationInstant, InvoiceNow: true, KeepIfUnpaid: true}, p.IdempotencyKey)
	var declined port.ErrDeclined
	var action port.ErrActionRequired
	switch {
	case err == nil:
		s.metrics().SeatChange("renewal_topup", string(res.Outcome.Status))
	case errors.As(err, &declined), errors.As(err, &action):
		s.metrics().SeatChange("renewal_topup", "unpaid") // seats kept; the invoice follows dunning (BL-OQ-6)
	default:
		return nil, providerErr(err)
	}
	if res.Invoice.ProviderInvoiceID != "" {
		p.ProviderInvoiceID = &res.Invoice.ProviderInvoiceID
	}
	return &p, nil
}

// ── downgrade-apply (§8.4.2 step 2) ────────────────────────────────────────

// RunDowngradeApply re-runs I-18 for scheduled downgrades within
// DOWNGRADE_RECHECK_LEAD. Blocked (or IAM still unavailable 15 min before
// the renewal) → the provider schedule is released and the request blocked.
func (s *Service) RunDowngradeApply(ctx context.Context) (JobResult, error) {
	var res JobResult
	now := s.now()
	due, err := s.Changes.DueDowngrades(ctx, now.Add(s.Settings.DowngradeRecheckLead), jobBatch)
	if err != nil {
		return res, err
	}
	for _, req := range due {
		if req.EffectiveAt == nil || !req.EffectiveAt.After(now) {
			continue // at or past the boundary: the cycle invoice.paid applies it
		}
		err := s.recheckDowngrade(ctx, req)
		if err != nil {
			s.logger(ctx).Warn("downgrade-apply: request not processed; retried next run", map[string]any{"change_id": req.ChangeID, "tenant_id": req.TenantID, "error": err.Error()})
		}
		res.add(err)
	}
	return res, nil
}

func (s *Service) recheckDowngrade(ctx context.Context, req domain.PlanChangeRequest) error {
	check, err := s.Core.PlanChangeCheck(ctx, req.TenantID, req.ToPlan)
	codes := []string(nil)
	switch {
	case err != nil:
		if s.now().Before(req.EffectiveAt.Add(-i18GraceBeforeApply)) {
			return err // retry on the next run
		}
		codes = []string{"iam_unavailable"}
	case check.Allowed:
		return s.locked(ctx, req.TenantID, func(ctx context.Context) error {
			cur, found, err := s.Changes.Open(ctx, req.TenantID)
			if err != nil || !found || cur.ChangeID != req.ChangeID || cur.Status != domain.ChangeScheduled {
				return err
			}
			now := s.now()
			cur.I18CheckedAt = &now
			return s.Changes.Update(ctx, cur)
		})
	default:
		codes = blockerCodes(check)
	}
	for _, c := range codes {
		s.metrics().DowngradeBlocked(c)
	}
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, req.TenantID, func(ctx context.Context) error {
		var err error
		sub, err = s.Subs.Get(ctx, req.BillingSubscriptionID)
		return err
	}); err != nil {
		return err
	}
	if req.ProviderScheduleID != nil && sub.Provider != manualProvider {
		prov, err := s.provider(sub.Provider)
		if err != nil {
			return err
		}
		if err := prov.ReleaseSchedule(ctx, port.ScheduleRef{ProviderScheduleID: *req.ProviderScheduleID}, "planchg:"+req.ChangeID+":release"); err != nil {
			return providerErr(err)
		}
		if err := s.reapplyRenewalQuantity(ctx, prov, sub); err != nil {
			return err
		}
	}
	if s.Log != nil {
		s.logger(ctx).Info("scheduled downgrade blocked at the re-check; the subscription renews on the current plan",
			map[string]any{"tenant_id": req.TenantID, "change_id": req.ChangeID, "blockers": codes})
	}
	return s.locked(ctx, req.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Changes.Open(ctx, req.TenantID)
		if err != nil || !found || cur.ChangeID != req.ChangeID {
			return err
		}
		now := s.now()
		cur.Status, cur.BlockerCodes, cur.I18CheckedAt = domain.ChangeBlocked, codes, &now
		if err := s.Changes.Update(ctx, cur); err != nil {
			return err
		}
		// A renewal price scheduled for the downgrade's plan no longer applies;
		// renewal-repricing re-evaluates on its next run (§8.16 step 2).
		item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
		if err != nil || item.ScheduledPriceID == nil {
			return err
		}
		item.ScheduledPriceID, item.ScheduledPriceAt = nil, nil
		return s.Subs.SetItem(ctx, item)
	})
}

// reapplyRenewalQuantity re-creates a seat reduction's renewal quantity
// after a released provider schedule dropped it.
func (s *Service) reapplyRenewalQuantity(ctx context.Context, prov port.PaymentProvider, sub domain.Subscription) error {
	var sch domain.SeatChangeSchedule
	var item domain.SubscriptionItem
	var price domain.PlanPrice
	found := false
	if err := s.Tx.RunInTenantTx(ctx, sub.TenantID, func(ctx context.Context) error {
		var err error
		if sch, found, err = s.Schedules.Open(ctx, sub.BillingSubscriptionID); err != nil || !found {
			return err
		}
		if item, err = s.Subs.Item(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		}
		// BIL-SUB-8: the current plan's row effective at the renewal (a
		// newer price row), not the item's current one (F-12). Not the
		// downgrade's plan: reapply runs because that downgrade was blocked.
		price, err = s.planPriceAtRenewal(ctx, sub, item, sub.Plan)
		return err
	}); err != nil || !found || sch.RenewalSeats == nil || sub.ProviderSubscriptionID == nil {
		return err
	}
	ref, err := prov.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID},
		ToPriceRef: deref(price.ProviderPriceID), ToQuantity: *sch.RenewalSeats, At: sch.EffectiveAt}, "sched:"+sch.ScheduleID+":reapply")
	if err != nil {
		return providerErr(err)
	}
	return s.locked(ctx, sub.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Schedules.Get(ctx, sch.ScheduleID)
		if err != nil || !found || !cur.Status.Open() {
			return err
		}
		cur.ProviderScheduleID = &ref.ProviderScheduleID
		return s.Schedules.Update(ctx, cur)
	})
}

// ── purchase-expiry (§13.2) ────────────────────────────────────────────────

// ExpireStale expires abandoned seat and add-on purchases and upgrades
// (voiding their open invoice, or settling them when it is paid), checkout sessions past expires_at (ending
// their incomplete subscription), and direct signups whose 3-D Secure was
// abandoned (provider subscription cancelled). Previews expire with the
// idempotency records (idempotency-prune).
func (s *Service) ExpireStale(ctx context.Context) (JobResult, error) {
	var res JobResult
	now := s.now()
	work, err := s.Jobs.StaleWork(ctx, now, now.Add(-s.Settings.PurchaseAbandonAfter), now.Add(-s.Settings.SCAAbandonAfter), jobBatch)
	if err != nil {
		return res, err
	}
	for _, w := range work {
		var err error
		switch w.Kind {
		case port.StaleSeatPurchase:
			err = s.expireSeatPurchase(ctx, w)
		case port.StaleAddonPurchase:
			err = s.expireAddon(ctx, w)
		case port.StaleUpgrade:
			err = s.expireUpgrade(ctx, w)
		case port.StaleCheckout:
			err = s.expireCheckout(ctx, w)
		case port.StaleSignup:
			err = s.expireSignup(ctx, w)
		}
		if err != nil {
			s.logger(ctx).Warn("purchase-expiry: item not expired; retried next run", map[string]any{"kind": w.Kind, "id": w.ID, "tenant_id": w.TenantID, "error": err.Error()})
		}
		res.add(err)
	}
	return res, nil
}

// The purchase-expiry close-out (§8.5.1, §8.7, §8.4.1): closeInvoice looks
// up an invoice Billing has no id for, voids it while open, and reports a
// paid one. A paid purchase is settled as its callback would settle it,
// never expired: the customer paid, so the seats, tokens or plan are
// granted.

func (s *Service) subFor(ctx context.Context, tenantID, subID string) (domain.Subscription, error) {
	var sub domain.Subscription
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		sub, err = s.Subs.Get(ctx, subID)
		return err
	})
	return sub, err
}

// expiryMeta is the envelope metadata of a change purchase-expiry settles.
// jobMeta is the envelope metadata of a job-driven change (§7.3.1): actor
// commercial-system, ip "system", user agent commercial-billing/job:<name>
// (C-2, rev 0.73).
func jobMeta(name string) port.EventMeta {
	return port.EventMeta{Actor: domain.CommercialSystemPrincipal, UserAgent: "commercial-billing/job:" + name}
}

func expiryMeta() port.EventMeta { return jobMeta("purchase-expiry") }

func (s *Service) expireSeatPurchase(ctx context.Context, w port.StaleWork) error {
	var p domain.SeatPurchase
	var found bool
	if err := s.Tx.RunInTenantTx(ctx, w.TenantID, func(ctx context.Context) error {
		var err error
		p, found, err = s.SeatPurchases.Get(ctx, w.ID)
		return err
	}); err != nil || !found || !p.Status.Open() {
		return err
	}
	sub, err := s.subFor(ctx, w.TenantID, p.BillingSubscriptionID)
	if err != nil {
		return err
	}
	inv, err := s.closeInvoice(ctx, sub.Provider, p.ProviderInvoiceID, seatTarget(deref(sub.ProviderSubscriptionID), p), "seatpur:"+p.PurchaseID+":expire")
	if err != nil {
		return err
	}
	outcome := ""
	err = s.locked(ctx, w.TenantID, func(ctx context.Context) error {
		outcome = ""
		cur, found, err := s.SeatPurchases.Get(ctx, w.ID)
		if err != nil || !found || !cur.Status.Open() {
			return err
		}
		if err := s.recheckInvoice(ctx, w.TenantID, w.ID, &inv); err != nil {
			return err
		}
		if inv.ID != "" {
			cur.ProviderInvoiceID = &inv.ID
		}
		if inv.Paid {
			outcome = "settled"
			return s.confirmSeats(ctx, &cur, "job:purchase-expiry", domain.CommercialSystemPrincipal, nil, expiryMeta())
		}
		outcome = "expired"
		cur.Status, cur.ClientAction = domain.PurchaseExpired, nil
		return s.SeatPurchases.Update(ctx, cur)
	})
	if err == nil && outcome == "expired" {
		s.metrics().SeatChange("purchase", "expired")
	}
	return err
}

func (s *Service) expireAddon(ctx context.Context, w port.StaleWork) error {
	var p domain.QuotaAddonPurchase
	var acct domain.BillingAccount
	var found bool
	if err := s.Tx.RunInTenantTx(ctx, w.TenantID, func(ctx context.Context) error {
		var err error
		if p, found, err = s.Addons.Get(ctx, w.ID); err != nil || !found {
			return err
		}
		acct, err = s.Accounts.Get(ctx, w.TenantID)
		return err
	}); err != nil || !found || !p.Status.Open() {
		return err
	}
	inv, err := s.closeInvoice(ctx, acct.Provider, p.ProviderInvoiceID, addonTarget(acct, p), "addon:"+p.PurchaseID+":expire")
	if err != nil {
		return err
	}
	return s.locked(ctx, w.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Addons.Get(ctx, w.ID)
		if err != nil || !found || !cur.Status.Open() {
			return err
		}
		if err := s.recheckInvoice(ctx, w.TenantID, w.ID, &inv); err != nil {
			return err
		}
		if inv.ID != "" {
			cur.ProviderInvoiceID = &inv.ID
		}
		if inv.Paid {
			return s.confirmAddon(ctx, &cur, expiryMeta())
		}
		cur.Status, cur.ClientAction = domain.PurchaseExpired, nil
		return s.Addons.Update(ctx, cur)
	})
}

func (s *Service) expireUpgrade(ctx context.Context, w port.StaleWork) error {
	var req domain.PlanChangeRequest
	var target domain.PlanPrice
	var found bool
	if err := s.Tx.RunInTenantTx(ctx, w.TenantID, func(ctx context.Context) error {
		var err error
		if req, found, err = s.Changes.Open(ctx, w.TenantID); err != nil || !found || req.ProviderInvoiceID != nil {
			return err
		}
		// The target price narrows the lookup of an invoice Billing has no
		// id for; without one the lookup matches on the amount only.
		if tp, err := s.upgradePrice(ctx, req); err == nil {
			target = tp
		}
		return nil
	}); err != nil || !found || req.ChangeID != w.ID || req.Status != domain.ChangePendingPayment {
		return err
	}
	sub, err := s.subFor(ctx, w.TenantID, req.BillingSubscriptionID)
	if err != nil {
		return err
	}
	inv, err := s.closeInvoice(ctx, sub.Provider, req.ProviderInvoiceID, upgradeTarget(deref(sub.ProviderSubscriptionID), req, deref(target.ProviderPriceID)),
		"planchg:"+req.ChangeID+":expire")
	if err != nil {
		return err
	}
	return s.locked(ctx, w.TenantID, func(ctx context.Context) error {
		cur, found, err := s.Changes.Open(ctx, w.TenantID)
		if err != nil || !found || cur.ChangeID != w.ID || cur.Status != domain.ChangePendingPayment {
			return err
		}
		if err := s.recheckInvoice(ctx, w.TenantID, w.ID, &inv); err != nil {
			return err
		}
		if inv.ID != "" {
			cur.ProviderInvoiceID = &inv.ID
		}
		if inv.Paid {
			return s.settleUpgrade(ctx, &cur, domain.CommercialSystemPrincipal, "job:purchase-expiry", nil, expiryMeta())
		}
		cur.Status, cur.ClientAction = domain.ChangeFailed, nil
		return s.Changes.Update(ctx, cur)
	})
}

// expireCheckout is §8.1.3 without a callback: session expired; its
// incomplete subscription ended (cancel_reason stays null). The session is
// first expired at the provider, outside any transaction (BIL-CONC-2), as
// B-P2 does for a superseded one: a session the customer completed just
// before expires_at (its callbacks still queued) is left open for
// checkout.session.completed and invoice.paid, never expired under a paying
// customer. The outcome is then committed under the lock with a re-read.
func (s *Service) expireCheckout(ctx context.Context, w port.StaleWork) error {
	var session domain.CheckoutSession
	var acct domain.BillingAccount
	var found bool
	if err := s.Tx.RunInTenantTx(ctx, w.TenantID, func(ctx context.Context) error {
		var err error
		if session, found, err = s.Checkouts.GetSession(ctx, w.ID); err != nil || !found || session.Status != domain.SessionOpen {
			return err
		}
		if !session.ProviderCreated() {
			return nil
		}
		acct, err = s.Accounts.Get(ctx, w.TenantID)
		return err
	}); err != nil || !found || session.Status != domain.SessionOpen {
		return err
	}
	if session.ProviderCreated() && acct.Provider != manualProvider {
		prov, err := s.provider(acct.Provider)
		if err != nil {
			return err
		}
		key := "checkout:" + session.SessionID + ":expire"
		if session.BillingSubscriptionID != nil {
			key = "bsub:" + *session.BillingSubscriptionID + ":expire"
		}
		switch err := prov.ExpireCheckout(ctx, session.ProviderSessionID, key); {
		case errors.Is(err, port.ErrCheckoutCompleted):
			return nil // paid: its callbacks complete it
		case err != nil:
			return providerErr(err)
		}
	}
	return s.locked(ctx, w.TenantID, func(ctx context.Context) error {
		session, found, err := s.Checkouts.GetSession(ctx, w.ID)
		if err != nil || !found || session.Status != domain.SessionOpen {
			return err
		}
		if err := s.Checkouts.SetStatus(ctx, session.SessionID, domain.SessionExpired); err != nil {
			return err
		}
		if session.BillingSubscriptionID == nil {
			return nil
		}
		sub, err := s.Subs.Get(ctx, *session.BillingSubscriptionID)
		if err != nil || sub.Status != domain.StatusIncomplete {
			return err
		}
		return s.endIncomplete(ctx, sub, "job:purchase-expiry", domain.CommercialSystemPrincipal)
	})
}

// expireSignup is B-I1's 410 CHARGE_EXPIRED path run by the job (§8.3).
func (s *Service) expireSignup(ctx context.Context, w port.StaleWork) error {
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, w.TenantID, func(ctx context.Context) error {
		var err error
		sub, err = s.Subs.Get(ctx, w.ID)
		return err
	}); err != nil || sub.Status != domain.StatusIncomplete {
		return err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return err
	}
	// Paid just before the window closed: activated, not cancelled (rev 0.68).
	if _, err := s.expireCharge(ctx, Caller{}, prov, sub); err != nil && !domain.HasCode(err, domain.ErrChargeExpired) {
		return err
	}
	return nil
}

// ── seat-projection-check (§8.6.4, BIL-EVT-5) ─────────────────────────────

// CheckSeatProjection compares Core's licensed_seats (I-11) with Billing's
// seat item and queues a TenantSeatsChanged {reason: resync} on a mismatch
// older than SEAT_RESYNC_AFTER. A second mismatch in 24 h is SEV-3.
//
// The run's totals go to a per-run gauge (seatProjectionRunMetrics) when
// the metrics adapter has one: each CronJob run pushes a fresh process's
// metrics to the Pushgateway, so a counter restarts at every push and
// increase() never sees persistent drift; the gauge is what
// BillingSeatProjectionDrift reads.
func (s *Service) CheckSeatProjection(ctx context.Context) (res JobResult, err error) {
	var mismatches, repeats int
	defer func() {
		if r, ok := s.metrics().(seatProjectionRunMetrics); ok {
			r.SeatProjectionRun(mismatches, repeats)
		}
	}()
	after := ""
	for {
		page, err := s.Jobs.LiveSubscriptions(ctx, after, jobBatch)
		if err != nil {
			return res, err
		}
		for _, l := range page {
			found, repeat, err := s.checkProjection(ctx, l)
			if found {
				mismatches++
			}
			if repeat {
				repeats++
			}
			res.add(err)
		}
		if len(page) < jobBatch {
			return res, nil
		}
		after = page[len(page)-1].BillingSubscriptionID
	}
}

// seatProjectionRunMetrics is the optional per-run gauge of
// seat-projection-check (metrics.Metrics implements it).
type seatProjectionRunMetrics interface {
	SeatProjectionRun(mismatches, repeats int)
}

// checkProjection reports whether l's seats drifted from Core's and whether
// the tenant was already resynced in the previous 24 h.
func (s *Service) checkProjection(ctx context.Context, l port.LiveSubscription) (found, repeat bool, err error) {
	now := s.now()
	if l.ItemUpdatedAt.After(now.Add(-s.Settings.SeatResyncAfter)) {
		return false, false, nil // a recent change may still be in flight
	}
	usage, err := s.Core.SeatUsage(ctx, l.TenantID)
	if err != nil {
		return false, false, err
	}
	err = s.locked(ctx, l.TenantID, func(ctx context.Context) error {
		found, repeat = false, false // a retried transaction starts over
		item, err := s.Subs.Item(ctx, l.BillingSubscriptionID)
		if err != nil || item.Quantity == usage.LicensedSeats || item.UpdatedAt.After(now.Add(-s.Settings.SeatResyncAfter)) {
			return err
		}
		recent, err := s.Jobs.RecentResyncs(ctx, l.TenantID, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		found, repeat = true, recent > 0
		s.metrics().SeatProjectionMismatch()
		fields := map[string]any{"tenant_id": l.TenantID, "billing_seats": item.Quantity, "core_licensed_seats": usage.LicensedSeats}
		if recent > 0 && s.Log != nil {
			fields["severity"] = "SEV-3"
			s.logger(ctx).Error("seat projection mismatch again within 24 h", fields)
		} else {
			s.logger(ctx).Warn("seat projection mismatch; resync queued", fields)
		}
		_, err = s.Emissions.Schedule(ctx, domain.ScheduledEmission{TenantID: l.TenantID, EventType: domain.EventTenantSeatsChanged,
			PayloadBuilder: domain.BuilderSeatsResync, RefID: l.BillingSubscriptionID, DueAt: now})
		return err
	})
	if err != nil {
		return false, false, err
	}
	return found, repeat, nil
}

// ── pii-minimisation (§15.3, BIL-OPS-9) ───────────────────────────────────

// MinimisePII nulls the billing-contact name and email (and clears the
// provider customer's email) on closed and abandoned accounts. It never
// deletes a row and never touches a financial record.
func (s *Service) MinimisePII(ctx context.Context) (JobResult, error) {
	var res JobResult
	now := s.now()
	day := 24 * time.Hour
	cands, err := s.Jobs.PIICandidates(ctx, now.Add(-time.Duration(s.Settings.PIIMinimiseOffboardedDays)*day),
		now.Add(-time.Duration(s.Settings.PIIMinimiseAbandonedDays)*day), jobBatch)
	if err != nil {
		return res, err
	}
	for _, c := range cands {
		err := s.minimise(ctx, c)
		if err != nil {
			s.logger(ctx).Warn("pii-minimisation: account not minimised; retried next run", map[string]any{"tenant_id": c.TenantID, "error": err.Error()})
		}
		res.add(err)
	}
	return res, nil
}

func (s *Service) minimise(ctx context.Context, c port.PIICandidate) error {
	if c.Provider != manualProvider && c.ProviderCustomerID != nil {
		prov, err := s.provider(c.Provider)
		if err != nil {
			return err
		}
		if err := prov.UpdateCustomer(ctx, port.CustomerRef{ProviderCustomerID: *c.ProviderCustomerID},
			port.CustomerInput{TenantID: c.TenantID}, "pii:"+c.TenantID); err != nil && !errors.Is(err, port.ErrNotFound) {
			return providerErr(err)
		}
	}
	return s.locked(ctx, c.TenantID, func(ctx context.Context) error {
		return s.Accounts.MinimiseContact(ctx, c.TenantID, s.now())
	})
}

// ── idempotency-prune (§9.2) ──────────────────────────────────────────────

// PruneIdempotency deletes idempotency records and stored previews past
// expires_at, tenant by tenant (RLS; billing_system only reads).
func (s *Service) PruneIdempotency(ctx context.Context) (JobResult, error) {
	var res JobResult
	now := s.now()
	for {
		tenants, err := s.Jobs.ExpiredIdempotencyTenants(ctx, now, jobBatch)
		if err != nil {
			return res, err
		}
		for _, t := range tenants {
			var n int64
			err := s.Tx.RunInTenantTx(ctx, t, func(ctx context.Context) error {
				var err error
				n, err = s.Jobs.PruneIdempotency(ctx, now)
				return err
			})
			if err != nil {
				res.Failed++
				continue
			}
			res.Handled += int(n)
		}
		if len(tenants) < jobBatch || res.Failed > 0 {
			return res, nil
		}
	}
}

// ── renewal-repricing (§8.16, BIL-SUB-8) ───────────────────────────────────

// RunRepricing schedules the move to the price row effective at each live
// subscription's next renewal. Manual subscriptions take the scheduled
// price here once R has passed (no provider invoice drives them).
func (s *Service) RunRepricing(ctx context.Context) (JobResult, error) {
	var res JobResult
	after := ""
	for {
		page, err := s.Jobs.LiveSubscriptions(ctx, after, jobBatch)
		if err != nil {
			return res, err
		}
		for _, l := range page {
			err := s.reprice(ctx, l)
			if err != nil {
				s.logger(ctx).Warn("renewal-repricing: subscription not repriced; retried next run", map[string]any{"tenant_id": l.TenantID, "error": err.Error()})
			}
			res.add(err)
		}
		if len(page) < jobBatch {
			return res, nil
		}
		after = page[len(page)-1].BillingSubscriptionID
	}
}

func (s *Service) reprice(ctx context.Context, l port.LiveSubscription) error {
	now := s.now()
	var (
		sub             domain.Subscription
		item            domain.SubscriptionItem
		cur, target     domain.PlanPrice
		qty             int
		skip, manualDue bool
	)
	if err := s.Tx.RunInTenantTx(ctx, l.TenantID, func(ctx context.Context) error {
		var err error
		if sub, err = s.Subs.Get(ctx, l.BillingSubscriptionID); err != nil {
			return err
		}
		if item, err = s.Subs.Item(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		}
		if sub.Provider == manualProvider && item.ScheduledPriceAt != nil && !item.ScheduledPriceAt.After(now) {
			manualDue = true
			return nil
		}
		if !sub.AutoRenew || sub.CurrentPeriodEnd == nil {
			skip = true
			return nil
		}
		if cur, err = s.Prices.Get(ctx, item.PriceID); err != nil {
			return err
		}
		if target, err = s.priceAtRenewal(ctx, sub, item); err != nil {
			return err
		}
		qty = item.Quantity
		if sch, found, err := s.Schedules.Open(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if found && sch.RenewalSeats != nil {
			qty = *sch.RenewalSeats
		}
		return nil
	}); err != nil {
		return err
	}
	if manualDue {
		return s.locked(ctx, l.TenantID, func(ctx context.Context) error { return s.applyScheduledPrice(ctx, sub, now, "job:renewal-repricing") })
	}
	scheduled := deref(item.ScheduledPriceID)
	switch {
	case skip,
		target.PriceID == scheduled, // already scheduled to the target row
		target.PriceID == item.PriceID && scheduled == "",
		sub.CurrentPeriodEnd.Sub(now) < s.Settings.RepriceCutoff: // moved at the following renewal
		return nil
	}
	r := *sub.CurrentPeriodEnd
	if sub.Provider != manualProvider && sub.ProviderSubscriptionID != nil {
		prov, err := s.provider(sub.Provider)
		if err != nil {
			return err
		}
		if _, err := prov.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID},
			ToPriceRef: deref(target.ProviderPriceID), ToQuantity: qty, At: r}, "reprice:"+sub.BillingSubscriptionID+":"+target.PriceID); err != nil {
			return providerErr(err)
		}
	}
	if target.Plan == sub.Plan && target.PriceID != item.PriceID {
		dir := "increase"
		if target.UnitAmountMinor < cur.UnitAmountMinor {
			dir = "decrease"
		}
		s.metrics().RepricingScheduled(dir)
	}
	return s.locked(ctx, l.TenantID, func(ctx context.Context) error {
		it, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		if target.PriceID == it.PriceID {
			it.ScheduledPriceID, it.ScheduledPriceAt = nil, nil
		} else {
			it.ScheduledPriceID, it.ScheduledPriceAt = &target.PriceID, &r
		}
		if err := s.Subs.SetItem(ctx, it); err != nil {
			return err
		}
		cur, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		from := cur.Status
		// The renewal notice is NTF-3 (BL-OQ-12, open): no event defined yet;
		// B-P1 shows scheduled_unit_price meanwhile.
		return s.history(ctx, cur, &from, "job:renewal-repricing", domain.CommercialSystemPrincipal, nil, nil, nil)
	})
}

// applyScheduledPrice is §8.16 step 4: at the renewal the seat item takes
// the scheduled price (same plan only; a plan change sets its own price)
// and the scheduled columns clear. No bus event. Under the lock.
func (s *Service) applyScheduledPrice(ctx context.Context, sub domain.Subscription, periodStart time.Time, cause string) error {
	item, err := s.Subs.Item(ctx, sub.BillingSubscriptionID)
	if err != nil || item.ScheduledPriceID == nil || item.ScheduledPriceAt == nil || item.ScheduledPriceAt.After(periodStart.Add(periodSlack)) {
		return err
	}
	p, err := s.Prices.Get(ctx, *item.ScheduledPriceID)
	if err != nil {
		return err
	}
	cur, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
	if err != nil {
		return err
	}
	changed := p.Plan == cur.Plan && p.PriceID != item.PriceID
	if changed {
		item.PriceID = p.PriceID
	}
	item.ScheduledPriceID, item.ScheduledPriceAt = nil, nil
	if err := s.Subs.SetItem(ctx, item); err != nil || !changed {
		return err
	}
	from := cur.Status
	return s.history(ctx, cur, &from, cause, domain.CommercialSystemPrincipal, nil, nil, nil)
}
