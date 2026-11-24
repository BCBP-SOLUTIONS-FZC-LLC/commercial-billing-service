package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// ── B-O2 ───────────────────────────────────────────────────────────────────

// IssueAdjustment is B-O2: a customer balance credit (reduces the next
// invoice) or debit (adds to it); AL-5 billing.adjustment.issued.
func (s *Service) IssueAdjustment(ctx context.Context, c Caller, tenantID string, r AdjustmentRequest) (AdjustmentResult, error) {
	ctx, end := StartSpan(ctx, "billing.adjustment.issue", AttrTenant(tenantID))
	out, err := s.issueAdjustment(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) issueAdjustment(ctx context.Context, c Caller, tenantID string, r AdjustmentRequest) (AdjustmentResult, error) {
	acct, err := s.account(ctx, tenantID)
	if err != nil {
		return AdjustmentResult{}, err
	}
	if r.Amount.Currency != acct.Currency {
		return AdjustmentResult{}, domain.NewError(domain.ErrCurrencyMismatch, "the currency differs from the account's").With("currency", acct.Currency)
	}
	if r.Amount.AmountMinor <= 0 {
		return AdjustmentResult{}, domain.NewError(domain.ErrValidation, "amount must be positive")
	}
	adj := domain.Adjustment{AdjustmentID: newID(), TenantID: tenantID, Kind: r.Kind, AmountMinor: r.Amount.AmountMinor, Currency: r.Amount.Currency,
		Description: r.Description, Reason: r.Reason, ActorID: c.UserID, IdempotencyKey: keyOr(c, "adj:")}
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error { return s.Money.InsertAdjustment(ctx, adj) }); err != nil {
		return AdjustmentResult{}, err
	}
	prov, err := s.provider(acct.Provider)
	if err != nil {
		return AdjustmentResult{}, err
	}
	amount := r.Amount.AmountMinor
	if r.Kind == "credit" { // a negative balance transaction is a credit
		amount = -amount
	}
	ref, err := prov.CreditBalance(ctx, port.BalanceAdjustment{Customer: port.CustomerRef{ProviderCustomerID: deref(acct.ProviderCustomerID)},
		Amount: domain.Money{AmountMinor: amount, Currency: r.Amount.Currency}, Description: r.Description}, "adj:"+adj.AdjustmentID)
	if err != nil {
		return AdjustmentResult{}, providerErr(err)
	}
	now := s.now()
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		if err := s.Money.SetAdjustmentRef(ctx, adj.AdjustmentID, ref); err != nil {
			return err
		}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.adjustment.issued", Action: "adjustment." + r.Kind,
			ActorID: c.UserID, TargetType: "subscription", TargetID: tenantID, SourceEventType: "AdjustmentIssued", NaturalID: adj.AdjustmentID,
			OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"kind": r.Kind, "amount_minor": r.Amount.AmountMinor, "currency": r.Amount.Currency, "reason": r.Reason}})
	}); err != nil {
		return AdjustmentResult{}, err
	}
	s.reviewIfLarge(ctx, tenantID, "adjustment", r.Amount.AmountMinor)
	return AdjustmentResult{AdjustmentID: adj.AdjustmentID, Kind: r.Kind, Amount: r.Amount, ProviderRef: ref}, nil
}

// reviewIfLarge is HLD §11.6: an adjustment or refund above
// REVIEW_THRESHOLD_MINOR raises the review alert.
func (s *Service) reviewIfLarge(ctx context.Context, tenantID, what string, amount int64) {
	if s.Settings.ReviewThresholdMinor > 0 && amount > s.Settings.ReviewThresholdMinor && s.Log != nil {
		s.logger(ctx).Warn("operator money action above REVIEW_THRESHOLD_MINOR (review trigger)", map[string]any{
			"tenant_id": tenantID, "kind": what, "amount_minor": amount, "alert": "BillingOperatorReviewNeeded"})
	}
}

func keyOr(c Caller, prefix string) string {
	if c.IdempotencyKey != "" {
		return c.IdempotencyKey
	}
	return prefix + newID()
}

// ── B-O3 and the refund path ───────────────────────────────────────────────

// IssueRefund is B-O3 (§8.11).
func (s *Service) IssueRefund(ctx context.Context, c Caller, tenantID string, r RefundRequest) (RefundResult, error) {
	ctx, end := StartSpan(ctx, "billing.refund.issue", AttrTenant(tenantID))
	out, err := s.issueRefund(ctx, c, tenantID, r)
	end(err)
	return out, err
}

func (s *Service) issueRefund(ctx context.Context, c Caller, tenantID string, r RefundRequest) (RefundResult, error) {
	f, err := s.refund(ctx, c, tenantID, r.InvoiceID, r.Amount.AmountMinor, r.Amount.Currency, r.Category, r.Reason)
	if err != nil {
		return RefundResult{}, err
	}
	s.reviewIfLarge(ctx, tenantID, "refund", f.AmountMinor)
	return RefundResult{RefundID: f.RefundID, Status: f.Status, Amount: MoneyFrom(domain.Money{AmountMinor: f.AmountMinor, Currency: f.Currency})}, nil
}

// refundReason maps a category to the provider's refund reason.
func refundReason(category string) port.RefundReason {
	switch category {
	case "duplicate_charge":
		return port.RefundReasonDuplicate
	case "fraud", domain.RefundCategoryConversionReversal:
		return port.RefundReasonFraudulent
	}
	return port.RefundReasonRequestedByCustomer
}

// refund validates, records (pending), calls the provider outside the
// transaction and, when the provider settles at once, applies success.
func (s *Service) refund(ctx context.Context, c Caller, tenantID, invoiceID string, amount int64, currency, category, reason string) (domain.Refund, error) {
	f := domain.Refund{RefundID: newID(), TenantID: tenantID, InvoiceID: invoiceID, AmountMinor: amount, Currency: currency, Category: category,
		Reason: reason, Status: domain.RefundPending, ActorID: actorOf(c), IdempotencyKey: keyOr(c, "refund:")}
	var inv domain.Invoice
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if inv, found, err = s.Invoices.Get(ctx, invoiceID); err != nil {
			return err
		}
		switch {
		case !found || inv.TenantID != tenantID:
			return domain.NewError(domain.ErrInvoiceNotFound, "invoice not found")
		case inv.Status != "paid":
			return domain.NewError(domain.ErrValidation, "only a paid invoice can be refunded")
		case currency != inv.Currency:
			return domain.NewError(domain.ErrCurrencyMismatch, "the currency differs from the invoice's")
		case amount > inv.AmountPaidMinor-inv.AmountRefundedMinor:
			return domain.NewError(domain.ErrRefundExceedsPaid, "the amount exceeds what is left to refund").
				With("refundable_minor", inv.AmountPaidMinor-inv.AmountRefundedMinor)
		}
		if inv.Kind == domain.InvoiceTokenAddon {
			if !domain.AddonRefundAllowed(domain.AddonRefundCategory(category)) { // BIL-ADD-3
				return domain.NewError(domain.ErrAddonRefundCategory, "add-on invoices are refunded only for billing_error, duplicate_charge or fraud")
			}
			p, ok, err := s.Addons.ByProviderInvoice(ctx, tenantID, inv.ProviderInvoiceID)
			if err != nil {
				return err
			}
			if ok && p.Status == domain.PurchaseConfirmed {
				tokens := p.TokensTotal * amount / p.AmountMinor // proportional, rounded down
				f.AddonPurchaseID = &p.PurchaseID
				if tokens > 0 {
					f.AddonTokens = &tokens
				}
			}
		}
		return s.Money.InsertRefund(ctx, f)
	}); err != nil {
		return f, err
	}
	prov, err := s.provider(inv.Provider)
	if err != nil {
		return f, err
	}
	res, err := prov.Refund(ctx, port.RefundInput{Invoice: port.InvoiceRef{ProviderInvoiceID: inv.ProviderInvoiceID},
		Amount: domain.Money{AmountMinor: amount, Currency: currency}, Reason: refundReason(category)}, "refund:"+f.RefundID)
	if err != nil {
		return f, providerErr(err) // stays pending without a provider id; reconciliation flags it
	}
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		f.ProviderRefundID = &res.ProviderRefundID
		if err := s.Money.UpdateRefund(ctx, f); err != nil {
			return err
		}
		now := s.now()
		if err := s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.refund.issued", Action: "refund.issued",
			ActorID: c.UserID, TargetType: "invoice", TargetID: invoiceID, SourceEventType: "RefundIssued", NaturalID: f.RefundID,
			OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"amount_minor": amount, "currency": currency, "category": category, "reason": reason}}); err != nil {
			return err
		}
		switch res.Status {
		case domain.RefundSucceeded:
			return s.refundSettled(ctx, &f, true, webhookMeta())
		case domain.RefundFailed:
			return s.refundSettled(ctx, &f, false, webhookMeta())
		}
		return nil
	})
	return f, err
}

// refundSettled applies a refund outcome under the lock: the invoice's
// refunded total, the add-on reversal event (BIL-ADD-4) and, for a signup
// refund, the saga (refund_pending → refunded).
func (s *Service) refundSettled(ctx context.Context, f *domain.Refund, ok bool, m port.EventMeta) error {
	if f.Status != domain.RefundPending {
		return nil
	}
	if !ok {
		f.Status = domain.RefundFailed
		return s.Money.UpdateRefund(ctx, *f)
	}
	f.Status = domain.RefundSucceeded
	if err := s.Invoices.AddRefunded(ctx, f.InvoiceID, f.AmountMinor); err != nil {
		return err
	}
	if f.AddonPurchaseID != nil && f.AddonTokens != nil {
		id, err := s.Events.Enqueue(ctx, domain.TenantQuotaAddOnChanged{TenantID: f.TenantID, Resource: domain.ResourceLLMTokens,
			Delta: -*f.AddonTokens, Reason: domain.AddonRefund, BillingRef: f.RefundID, PurchaseRef: *f.AddonPurchaseID}, m)
		if err != nil {
			return err
		}
		f.AddonEventID = &id
	}
	if f.Category == domain.RefundCategorySignupFailed {
		if err := s.sagaRefunded(ctx, f.TenantID); err != nil {
			return err
		}
	}
	return s.Money.UpdateRefund(ctx, *f)
}

// RefundUpdated applies the provider's refund outcome (refund.updated).
type RefundUpdated struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h RefundUpdated) Handle(ctx context.Context, tenantID string, _ domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if cur.Ref.Kind != domain.ObjectRefund {
		return "", &port.IgnoredEvent{Reason: "not_a_refund_object"}
	}
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	f, found, err := s.Money.RefundByProvider(ctx, cur.Ref.ID)
	if err != nil {
		return "", err
	}
	if !found || f.TenantID != tenantID {
		return "", &port.IgnoredEvent{Reason: "unknown_refund"} // a dashboard refund: reconciliation reports it
	}
	if f.Status != domain.RefundPending {
		return "", &port.IgnoredEvent{Reason: "refund_" + f.Status}
	}
	switch cur.Status {
	case domain.RefundSucceeded:
		err = s.refundSettled(ctx, &f, true, webhookMeta())
	case domain.RefundFailed, "canceled":
		err = s.refundSettled(ctx, &f, false, webhookMeta())
	default:
		return "", &port.IgnoredEvent{Reason: "refund_still_" + cur.Status}
	}
	if err != nil {
		return "", err
	}
	return "applied:refund_" + f.Status, nil
}

// ── Disputes (§8.2) ────────────────────────────────────────────────────────

// DisputeOpened records a dispute or early fraud warning; on the
// conversion payment within the revert window (fraudulent dispute or EFW)
// it asks an operator to decide (SEV-2 BillingReversalDecisionNeeded).
type DisputeOpened struct{ S *Service }

// Handle implements port.PaymentEventHandler.
func (h DisputeOpened) Handle(ctx context.Context, tenantID string, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	s := h.S
	if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
		return "", err
	}
	kind := "dispute"
	if cur.Ref.Kind == domain.ObjectEarlyFraudWarning {
		kind = "early_fraud_warning"
	}
	opened, _ := cur.Fields["created"].(time.Time)
	if opened.IsZero() {
		opened = s.now()
	}
	payment, _ := cur.Fields["payment_ref"].(string)
	reason, _ := cur.Fields["reason"].(string)
	d := domain.Dispute{DisputeID: newID(), TenantID: tenantID, Provider: ev.Provider, ProviderDisputeID: cur.Ref.ID, Kind: kind,
		PaymentRef: payment, ProviderStatus: cur.Status, RevertDecision: "none", OpenedAt: opened}
	if reason != "" {
		d.Reason = &reason
	}
	if invRef, _ := cur.Fields["invoice"].(string); invRef != "" {
		if inv, found, err := s.Invoices.ByProvider(ctx, ev.Provider, invRef); err != nil {
			return "", err
		} else if found {
			d.InvoiceID = &inv.InvoiceID
			d.IsConversionPayment = inv.Kind == domain.InvoiceSubscriptionCreate
		}
	}
	if d.IsConversionPayment {
		if sub, found, err := s.Subs.Live(ctx, tenantID); err != nil {
			return "", err
		} else if found && sub.ConvertedAt != nil {
			d.WithinRevertWindow = opened.Sub(*sub.ConvertedAt) <= domain.RevertWindow
		}
	}
	signal := kind == "early_fraud_warning" || reason == "fraudulent"
	if signal && d.IsConversionPayment && d.WithinRevertWindow {
		d.RevertDecision = "pending"
	}
	stored, err := s.Money.UpsertDispute(ctx, d)
	if err != nil {
		return "", err
	}
	if stored.DisputeID == d.DisputeID && d.RevertDecision == "pending" && s.Log != nil {
		s.logger(ctx).Error("conversion reversal decision needed (SEV-2)", map[string]any{"tenant_id": tenantID, "dispute_id": stored.DisputeID,
			"kind": kind, "alert": "BillingReversalDecisionNeeded", "severity": "SEV-2"})
	}
	return "applied:dispute_recorded", nil
}

// ── B-O8 (§8.2) ────────────────────────────────────────────────────────────

// RevertConversion is B-O8: RP-4 first; on success cancel the provider
// subscription now, refund unless the dispute already returned the funds,
// subscription ended (operator_reversal), saga and dispute reverted, AL-5.
// No billing.events: RP-4 is IAM's lifecycle change.
func (s *Service) RevertConversion(ctx context.Context, c Caller, tenantID string, r ReversalRequest) (ReversalResult, error) {
	var (
		sub  domain.Subscription
		saga domain.ConversionSaga
		disp *domain.Dispute
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if sub, found, err = s.Subs.Live(ctx, tenantID); err != nil {
			return err
		}
		if !found || (sub.Status != domain.StatusActive && sub.Status != domain.StatusPastDue) {
			return domain.NewError(domain.ErrSubscriptionNotActive, "no active subscription to revert")
		}
		if sub.ConvertedAt == nil || s.now().Sub(*sub.ConvertedAt) > domain.RevertWindow {
			return domain.NewError(domain.ErrRevertWindowExpired, "the 7-day reversal window has passed")
		}
		if saga, found, err = s.Sagas.BySubscription(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if !found {
			return domain.NewError(domain.ErrSagaNotFound, "no conversion saga for the subscription")
		}
		if r.DisputeID != "" {
			d, ok, err := s.Money.Dispute(ctx, r.DisputeID)
			if err != nil {
				return err
			}
			if !ok || d.TenantID != tenantID {
				return domain.NewError(domain.ErrValidation, "dispute_id does not belong to the tenant")
			}
			disp = &d
		}
		return nil
	}); err != nil {
		return ReversalResult{}, err
	}
	ans := s.RP.RevertConversion(ctx, tenantID, r.Reason, "revert:"+tenantID+":"+sub.BillingSubscriptionID)
	switch {
	case ans.Status == 409 && ans.ErrorCode == "revert_window_expired":
		return ReversalResult{}, domain.NewError(domain.ErrRevertWindowExpired, "Realm Provisioner: the reversal window has passed")
	case ans.Status == 0 || ans.Status >= 500 || ans.Status == 409 && ans.ErrorCode == "operation_in_progress":
		return ReversalResult{}, domain.NewError(domain.ErrIAMUnavailable, "RP-4 unavailable; retry B-O8 with the same key")
	case ans.Status != 200:
		return ReversalResult{}, domain.NewError(domain.ErrInternal, "RP-4 refused the reversal: "+ans.ErrorCode)
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return ReversalResult{}, err
	}
	if sub.ProviderSubscriptionID != nil && sub.Provider != manualProvider {
		if err := prov.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID}, "cancel:"+sub.BillingSubscriptionID+":reversal"); err != nil {
			return ReversalResult{}, providerErr(err)
		}
	}
	out := ReversalResult{ReversalStatus: "reverted", RPResponse: json.RawMessage(`{"status":200}`)}
	fundsReturned := disp != nil && disp.ProviderStatus == "lost"
	if r.Refund && !fundsReturned {
		inv, found, err := s.conversionInvoice(ctx, tenantID)
		if err != nil {
			return ReversalResult{}, err
		}
		if found && inv.AmountPaidMinor-inv.AmountRefundedMinor > 0 {
			f, err := s.refund(ctx, c, tenantID, inv.InvoiceID, inv.AmountPaidMinor-inv.AmountRefundedMinor, inv.Currency,
				domain.RefundCategoryConversionReversal, r.Reason)
			if err != nil {
				return ReversalResult{}, err
			}
			out.RefundID = &f.RefundID
		}
	}
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		now := s.now()
		cur, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		if cur.Status != domain.StatusEnded {
			from := cur.Status
			reason := domain.CancelOperatorReversal
			ended, err := s.Subs.Transition(ctx, cur.BillingSubscriptionID, 0, domain.StatusEnded, func(x *domain.Subscription) {
				x.EndedAt, x.CancelReason = &now, &reason
			})
			if err != nil {
				return err
			}
			if err := s.history(ctx, ended, &from, "operator:B-O8", c.UserID, nil, nil, nil); err != nil {
				return err
			}
		}
		if cur, ok, err := s.Sagas.Get(ctx, saga.SagaID); err != nil {
			return err
		} else if ok && cur.State != domain.SagaReverted {
			res := "reverted"
			cur.State, cur.Resolution, cur.ResolutionReason, cur.ResolvedBy = domain.SagaReverted, &res, &r.Reason, &c.UserID
			if err := s.Sagas.Update(ctx, cur); err != nil {
				return err
			}
		}
		if disp != nil {
			if err := s.Money.DecideDispute(ctx, disp.DisputeID, "reverted", c.UserID); err != nil {
				return err
			}
		}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.conversion.reverted", Action: "conversion.reverted",
			ActorID: c.UserID, TargetType: "subscription", TargetID: sub.BillingSubscriptionID, SourceEventType: "ConversionReverted",
			NaturalID: sub.BillingSubscriptionID, OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"refund": r.Refund, "dispute_id": r.DisputeID, "reason": r.Reason}})
	})
	return out, err
}

func (s *Service) conversionInvoice(ctx context.Context, tenantID string) (domain.Invoice, bool, error) {
	var (
		inv   domain.Invoice
		found bool
	)
	err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		inv, found, err = s.Invoices.ConversionInvoice(ctx, tenantID)
		return err
	})
	return inv, found, err
}

// ── B-O9 refund ────────────────────────────────────────────────────────────

// sagaRefund is B-O9 refund (§9.3.1): allowed before TenantConverted;
// refunds the conversion payment (signup_failed), cancels the provider
// subscription, ends the subscription; the saga is refund_pending until the
// refund succeeds.
func (s *Service) sagaRefund(ctx context.Context, c Caller, tenantID, sagaID string, r SagaActionRequest) (SagaView, error) {
	var (
		saga domain.ConversionSaga
		sub  domain.Subscription
	)
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if saga, found, err = s.Sagas.Get(ctx, sagaID); err != nil {
			return err
		}
		if !found || saga.TenantID != tenantID {
			return domain.NewError(domain.ErrSagaNotFound, "saga not found")
		}
		if saga.State == domain.SagaConfirmed || saga.State.Terminal() {
			return domain.NewError(domain.ErrSagaAlreadyConfirmed, "the saga is confirmed or closed; use B-O8 or B-O3").With("state", string(saga.State))
		}
		if sub, err = s.Subs.Get(ctx, saga.BillingSubscriptionID); err != nil {
			return err
		}
		res := "refund"
		saga.State, saga.NextAttemptAt, saga.Resolution, saga.ResolutionReason, saga.ResolvedBy = domain.SagaRefundPending, nil, &res, &r.Reason, &c.UserID
		return s.Sagas.Update(ctx, saga)
	}); err != nil {
		return SagaView{}, err
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return SagaView{}, err
	}
	if sub.ProviderSubscriptionID != nil && sub.Provider != manualProvider {
		if err := prov.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID}, "cancel:"+sub.BillingSubscriptionID+":signup_refund"); err != nil {
			return SagaView{}, providerErr(err)
		}
	}
	inv, found, err := s.conversionInvoice(ctx, tenantID)
	if err != nil {
		return SagaView{}, err
	}
	if found && inv.AmountPaidMinor-inv.AmountRefundedMinor > 0 {
		if _, err := s.refund(ctx, c, tenantID, inv.InvoiceID, inv.AmountPaidMinor-inv.AmountRefundedMinor, inv.Currency,
			domain.RefundCategorySignupFailed, r.Reason); err != nil {
			return SagaView{}, err
		}
	} else if err := s.locked(ctx, tenantID, func(ctx context.Context) error { return s.sagaRefunded(ctx, tenantID) }); err != nil {
		return SagaView{}, err // nothing to refund (stub, or already refunded): close now
	}
	var out SagaView
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		if cur.Status != domain.StatusEnded {
			if err := s.endSignup(ctx, cur, "operator:B-O9"); err != nil {
				return err
			}
		}
		latest, _, err := s.Sagas.Get(ctx, sagaID)
		out = sagaView(latest)
		return err
	})
	return out, err
}

// sagaRefunded closes the tenant's refund_pending saga (refund succeeded).
func (s *Service) sagaRefunded(ctx context.Context, tenantID string) error {
	sub, found, err := s.Subs.Latest(ctx, tenantID)
	if err != nil || !found {
		return err
	}
	saga, found, err := s.Sagas.BySubscription(ctx, sub.BillingSubscriptionID)
	if err != nil || !found || saga.State != domain.SagaRefundPending {
		return err
	}
	saga.State = domain.SagaRefunded
	return s.Sagas.Update(ctx, saga)
}
