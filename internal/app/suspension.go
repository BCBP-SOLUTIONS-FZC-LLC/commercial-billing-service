package app

import (
	"context"
	"strconv"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Collection pause origins (subscriptions.collection_paused_by).
const (
	pausedByAuto     = "auto"
	pausedByOperator = "operator"
)

// RunLifecycleFollowUps carries out the provider side of IAM lifecycle
// events the consumer recorded (§8.8.4, §8.10): an operator suspension
// pauses collection (AUTO_PAUSE_ON_OPERATOR_SUSPENSION), a reactivation
// lifts an automatic pause, an offboarding closes the subscription. The
// consumer cannot call the provider inside its transaction (BIL-CONC-2), so
// this loop does: provider call outside, commit under the tenant lock after
// re-checking. Returns the tenants handled.
func (s *Service) RunLifecycleFollowUps(ctx context.Context, limit int) (int, error) {
	due, err := s.FollowUps.Due(ctx, s.Settings.AutoPauseOnOperatorSuspension, limit)
	if err != nil {
		return 0, err
	}
	for _, f := range due {
		var err error
		switch f.Action {
		case "pause":
			err = s.setPause(ctx, Caller{}, f.TenantID, f.BillingSubscriptionID, true, pausedByAuto, "Automatic pause on operator suspension (CD-24)")
		case "resume":
			err = s.setPause(ctx, Caller{}, f.TenantID, f.BillingSubscriptionID, false, "", "Automatic pause lifted on reactivation (CD-24)")
		case "close":
			err = s.closeOffboarded(ctx, f.TenantID, f.BillingSubscriptionID)
		case "default_pm":
			err = s.applyDefaultPM(ctx, f.TenantID, f.SessionID)
		}
		if err != nil && s.Log != nil {
			s.Log.Warn("lifecycle follow-up failed; retried on the next pass", map[string]any{"tenant_id": f.TenantID, "action": f.Action, "error": err.Error()})
		}
	}
	return len(due), nil
}

// setPause pauses or resumes collection on the provider, then records it
// (§8.10, BIL-SUB-7) with an AL-5 billing.collection.paused entry.
func (s *Service) setPause(ctx context.Context, c Caller, tenantID, subID string, pause bool, by, reason string) error {
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		sub, err = s.Subs.Get(ctx, subID)
		return err
	}); err != nil {
		return err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return err
	}
	if prov.Capabilities().CollectionPause && sub.ProviderSubscriptionID != nil {
		key := "pause:" + subID + ":" + strconv.FormatBool(pause) + ":" + strconv.FormatInt(sub.RecordVersion, 10)
		if err := prov.PauseCollection(ctx, port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID}, pause, key); err != nil {
			return providerErr(err)
		}
	}
	return s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, err := s.Subs.Update(ctx, subID, 0, func(x *domain.Subscription) {
			x.CollectionPaused = pause
			if pause {
				x.CollectionPausedBy = &by
			} else {
				x.CollectionPausedBy = nil
			}
		})
		if err != nil {
			return err
		}
		now := s.now()
		dir := "resumed"
		if pause {
			dir = "paused"
		}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.collection.paused", Action: "collection." + dir,
			ActorID: c.UserID, TargetType: "subscription", TargetID: subID, SourceEventType: "CollectionPauseChanged",
			NaturalID: subID + ":" + dir + ":" + strconv.FormatInt(cur.RecordVersion, 10), OccurredAt: now, IPAddress: c.ClientIP,
			UserAgent: c.UserAgent, Metadata: map[string]any{"paused": pause, "paused_by": by, "reason": reason}})
	})
}

// SetCollectionPause is B-O10: an operator pause is never undone
// automatically; "continue billing" clears any pause.
func (s *Service) SetCollectionPause(ctx context.Context, c Caller, tenantID string, r CollectionPauseRequest) (SubscriptionView, error) {
	var sub domain.Subscription
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		sub, found, err = s.Subs.Live(ctx, tenantID)
		if err == nil && (!found || sub.Status == domain.StatusIncomplete) {
			return domain.NewError(domain.ErrSubscriptionNotFound, "no live subscription")
		}
		return err
	}); err != nil {
		return SubscriptionView{}, err
	}
	by := ""
	if *r.Paused {
		by = pausedByOperator
	}
	if err := s.setPause(ctx, c, tenantID, sub.BillingSubscriptionID, *r.Paused, by, r.Reason); err != nil {
		return SubscriptionView{}, err
	}
	return s.operatorView(ctx, c, tenantID)
}

// operatorView renders the subscription for an operator response.
func (s *Service) operatorView(ctx context.Context, c Caller, tenantID string) (SubscriptionView, error) {
	var v SubscriptionView
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		sub, found, err := s.Subs.Latest(ctx, tenantID)
		if err != nil || !found {
			return err
		}
		v, err = s.subscriptionView(ctx, c, sub, nil)
		return err
	})
	return v, err
}

// closeOffboarded is §8.8.4: commit 1 cancels what is open; the provider
// subscription is cancelled now (no proration, no refund); commit 2 ends
// Billing's subscription (offboarded). No bus event: IAM already knows.
func (s *Service) closeOffboarded(ctx context.Context, tenantID, subID string) error {
	var sub domain.Subscription
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		var err error
		if sub, err = s.Subs.Get(ctx, subID); err != nil {
			return err
		}
		if p, ok, err := s.SeatPurchases.Open(ctx, subID); err != nil {
			return err
		} else if ok {
			p.Status = domain.PurchaseCancelled
			if err := s.SeatPurchases.Update(ctx, p); err != nil {
				return err
			}
		}
		if err := s.Checkouts.ExpireAll(ctx, tenantID); err != nil {
			return err
		}
		return s.cancelOpenChanges(ctx, sub)
	}); err != nil {
		return err
	}
	if sub.Status == domain.StatusEnded {
		return nil
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return err
	}
	if sub.Status != domain.StatusCancelled && sub.ProviderSubscriptionID != nil && sub.Provider != manualProvider {
		if err := prov.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID}, "cancel:"+subID+":offboarded"); err != nil {
			return providerErr(err)
		}
	}
	if sub.Provider != manualProvider {
		s.voidOpenInvoices(ctx, prov, tenantID)
	}
	return s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, err := s.Subs.Get(ctx, subID)
		if err != nil || cur.Status == domain.StatusEnded {
			return err
		}
		now := s.now()
		from := cur.Status
		ended, err := s.Subs.Transition(ctx, subID, 0, domain.StatusEnded, func(x *domain.Subscription) {
			x.EndedAt = &now
			if x.CancelReason == nil {
				r := domain.CancelOffboarded
				x.CancelReason = &r
			}
		})
		if err != nil {
			return err
		}
		return s.history(ctx, ended, &from, "iam:TenantOffboarded", domain.CommercialSystemPrincipal, nil, nil, nil)
	})
}

// InvoiceUpdated mirrors an invoice change; a draft created while
// collection is paused is held for an operator decision (BIL-SUB-7).
type InvoiceUpdated struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h InvoiceUpdated) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	id, _, err := s.mirrorFromState(ctx, tenantID, ev.Provider, ev, cur)
	if err != nil {
		return "", err
	}
	if cur.Status != "draft" {
		return "applied:invoice_mirrored", nil
	}
	sub, err := s.subscriptionFor(ctx, tenantID, ev.Provider, cur)
	if err != nil || !sub.CollectionPaused {
		return "applied:invoice_mirrored", nil //nolint:nilerr // an invoice without a known subscription is only mirrored
	}
	if err := s.Invoices.SetHeld(ctx, id); err != nil {
		return "", err
	}
	return "applied:invoice_held", nil
}

// DisposeHeldInvoice is B-O16: finalise (charge) or void a held draft.
func (s *Service) DisposeHeldInvoice(ctx context.Context, c Caller, tenantID, invoiceID string, r DispositionRequest) (DispositionResult, error) {
	var (
		inv domain.Invoice
		sub domain.Subscription
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		inv, found, err = s.Invoices.Get(ctx, invoiceID)
		if err != nil {
			return err
		}
		if !found || inv.TenantID != tenantID {
			return domain.NewError(domain.ErrInvoiceNotFound, "invoice not found")
		}
		if !inv.HeldForReview || inv.Status != "draft" || inv.Disposition != nil {
			return domain.NewError(domain.ErrInvoiceNotHeld, "the invoice is not a held draft awaiting a decision")
		}
		sub, found, err = s.Subs.Latest(ctx, tenantID)
		if err == nil && r.Action == "finalize" && found && sub.CollectionPaused {
			return domain.NewError(domain.ErrCollectionPaused, "billing is still paused; continue billing (B-O10) or reactivate first")
		}
		return err
	}); err != nil {
		return DispositionResult{}, err
	}
	prov, err := s.provider(inv.Provider)
	if err != nil {
		return DispositionResult{}, err
	}
	ref := port.InvoiceRef{ProviderInvoiceID: inv.ProviderInvoiceID}
	status := "void"
	if r.Action == "finalize" {
		err, status = prov.FinalizeInvoice(ctx, ref, "disp:"+invoiceID+":finalize"), "open"
	} else {
		err = prov.VoidInvoice(ctx, ref, "disp:"+invoiceID+":void")
	}
	if err != nil {
		return DispositionResult{}, providerErr(err)
	}
	now := s.now()
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		if err := s.Invoices.Dispose(ctx, invoiceID, r.Action, r.Reason, c.UserID, now, status); err != nil {
			return err
		}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.invoice.suspension_disposition",
			Action: "invoice." + r.Action, ActorID: c.UserID, TargetType: "invoice", TargetID: invoiceID,
			SourceEventType: "HeldInvoiceDisposed", NaturalID: invoiceID, OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"action": r.Action, "amount_minor": inv.AmountDueMinor, "currency": inv.Currency, "reason": r.Reason}})
	})
	if err != nil {
		return DispositionResult{}, err
	}
	return DispositionResult{InvoiceID: invoiceID, Status: status, Disposition: r.Action, DisposedBy: c.UserID, DisposedAt: now}, nil
}
