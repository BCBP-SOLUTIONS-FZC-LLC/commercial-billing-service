package app

import (
	"context"
	"encoding/json"
	"fmt"
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
	resumed := false
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		// §9.2 one row per key: a retry after a 5xx (the idempotency record
		// was released) resumes the row and its provider key adj:<id>.
		prev, found, err := s.Money.AdjustmentByKey(ctx, tenantID, adj.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			// The whole body: the description goes to the provider under the
			// same key adj:<id>, so another one would be a provider
			// idempotency mismatch (§9.2).
			if prev.Kind != adj.Kind || prev.AmountMinor != adj.AmountMinor || prev.Currency != adj.Currency ||
				prev.Description != adj.Description || prev.Reason != adj.Reason {
				return domain.NewError(domain.ErrIdempotencyKeyReused, "Idempotency-Key was used with a different request")
			}
			adj, resumed = prev, true
			return nil
		}
		return keyInUse(s.Money.InsertAdjustment(ctx, adj))
	}); err != nil {
		return AdjustmentResult{}, err
	}
	if resumed && adj.ProviderRef != nil { // done (the reference and the audit entry commit together)
		return AdjustmentResult{AdjustmentID: adj.AdjustmentID, Kind: adj.Kind, Amount: r.Amount, ProviderRef: *adj.ProviderRef}, nil
	}
	if resumed && s.now().Sub(adj.CreatedAt) > providerKeyTTL {
		// The provider key may have lapsed: replaying it could post twice.
		return AdjustmentResult{}, domain.NewError(domain.ErrRequestInProgress, "the adjustment's outcome is unknown; reconcile it before retrying")
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
			ActorID: c.UserID, TargetType: "adjustment", TargetID: adj.AdjustmentID, SourceEventType: "AdjustmentIssued", NaturalID: adj.AdjustmentID,
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
	resumed := false
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		// §9.2 one row per key: a retry after a 5xx (the idempotency record
		// was released) resumes the row and its provider key refund:<id>.
		prev, found, err := s.Money.RefundByKey(ctx, tenantID, f.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			// The whole body: the category picks the provider's refund reason
			// under the same key refund:<id>, so another one would be a
			// provider idempotency mismatch (§9.2).
			if prev.InvoiceID != invoiceID || prev.AmountMinor != amount || prev.Currency != currency ||
				prev.Category != category || prev.Reason != reason {
				return domain.NewError(domain.ErrIdempotencyKeyReused, "Idempotency-Key was used with a different request")
			}
			f, resumed = prev, true
			inv, _, err = s.Invoices.Get(ctx, invoiceID)
			return err
		}
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
		return keyInUse(s.Money.InsertRefund(ctx, f))
	}); err != nil {
		return f, err
	}
	if resumed && (f.ProviderRefundID != nil || s.now().Sub(f.CreatedAt) > providerKeyTTL) {
		// Done, or too old to replay the provider key: stays pending and
		// reconciliation flags it.
		return f, nil
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
		// Re-read (BIL-CONC-2): a concurrent caller with the same key, or the
		// refund.updated webhook, may already have recorded or settled it.
		cur, ok, err := s.Money.RefundByKey(ctx, tenantID, f.IdempotencyKey)
		if err != nil {
			return err
		}
		if ok && (cur.ProviderRefundID != nil || cur.Status != domain.RefundPending) {
			f = cur
			return nil
		}
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

// RevertConversion is B-O8. It first records the request on the saga
// (confirmed → reverting, rev 0.63), then calls RP-4. On 200 it completes the
// reversal: cancel the provider subscription now, refund unless the dispute
// already returned the funds, subscription ended (operator_reversal), saga
// and dispute reverted, AL-5. Realm Provisioner finishes a revert in the
// background (RP-D30), so when RP-4 answers 503, 409 operation_in_progress or
// times out, B-O8 answers 503 and the saga worker keeps calling RP-4 with the
// same key and completes the reversal itself (runReversalStep). A repeat of
// the same request is allowed after the 7-day window once it is recorded.
// No billing.events: RP-4 is IAM's lifecycle change.
func (s *Service) RevertConversion(ctx context.Context, c Caller, tenantID string, r ReversalRequest) (ReversalResult, error) {
	var (
		sub  domain.Subscription
		saga domain.ConversionSaga
	)
	if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		var found bool
		var err error
		if sub, found, err = s.Subs.Live(ctx, tenantID); err != nil {
			return err
		}
		if !found || (sub.Status != domain.StatusActive && sub.Status != domain.StatusPastDue) {
			return domain.NewError(domain.ErrSubscriptionNotActive, "no active subscription to revert")
		}
		if saga, found, err = s.Sagas.BySubscription(ctx, sub.BillingSubscriptionID); err != nil {
			return err
		} else if !found {
			return domain.NewError(domain.ErrSagaNotFound, "no conversion saga for the subscription")
		}
		if saga.State == domain.SagaReverting {
			// Already recorded: a repeat of the same request continues it, even
			// past the window; a different one waits for it (§8.2).
			if deref(saga.ReversalReason) != r.Reason || saga.ReversalRefund == nil || *saga.ReversalRefund != r.Refund ||
				deref(saga.ReversalDisputeID) != r.DisputeID {
				return domain.NewError(domain.ErrRequestInProgress, "a different conversion reversal is already in progress for this subscription")
			}
			// The saga worker (or an earlier B-O8) holds it until next_attempt_at.
			now := s.now()
			if saga.NextAttemptAt != nil && saga.NextAttemptAt.After(now) {
				return domain.NewError(domain.ErrIAMUnavailable, "the conversion reversal is in progress; Billing completes it (check B-O7)")
			}
			lease := now.Add(s.Settings.SagaClaim)
			saga.NextAttemptAt = &lease
			return s.Sagas.Update(ctx, saga)
		}
		if saga.State != domain.SagaConfirmed {
			// Before TenantConverted the path is B-O9 refund; after a reversal
			// or a refund there is nothing left to revert.
			return domain.NewError(domain.ErrSubscriptionNotActive, "the conversion is not confirmed (saga "+string(saga.State)+")")
		}
		if sub.ConvertedAt == nil || s.now().Sub(*sub.ConvertedAt) > domain.RevertWindow {
			return domain.NewError(domain.ErrRevertWindowExpired, "the 7-day reversal window has passed")
		}
		if r.DisputeID != "" {
			d, ok, err := s.Money.Dispute(ctx, r.DisputeID)
			if err != nil {
				return err
			}
			if !ok || d.TenantID != tenantID {
				return domain.NewError(domain.ErrValidation, "dispute_id does not belong to the tenant")
			}
		}
		now := s.now()
		// Leased past this call, so the saga worker does not race it.
		lease := now.Add(s.Settings.SagaClaim)
		refund, by := r.Refund, actorOf(c)
		saga.State, saga.Attempts, saga.NextAttemptAt = domain.SagaReverting, 0, &lease
		saga.ReversalRequestedAt, saga.ReversalRequestedBy, saga.ReversalReason, saga.ReversalRefund = &now, &by, &r.Reason, &refund
		saga.ReversalDisputeID = nil
		if r.DisputeID != "" {
			saga.ReversalDisputeID = &r.DisputeID
		}
		return s.Sagas.Update(ctx, saga)
	}); err != nil {
		return ReversalResult{}, err
	}
	res, terminal, err := s.reverse(ctx, c, saga, sub)
	if terminal != "" && s.Log != nil {
		s.logger(ctx).Warn("conversion reversal refused by Realm Provisioner; request dropped", map[string]any{
			"tenant_id": tenantID, "saga_id": saga.SagaID, "rp": terminal})
	}
	return res, err
}

// reverse calls RP-4 for a reverting saga and acts on the answer:
//   - 200: completes the reversal (completeReversal);
//   - retryable (transport error or timeout, 5xx, 409 operation_in_progress
//     or optimistic_lock_conflict): records the attempt with backoff for the
//     saga worker and returns 503 IAM_UNAVAILABLE;
//   - terminal (409 revert_window_expired, 400, 403, 404, anything else):
//     drops the request (reverting → confirmed) and returns the error plus a
//     description of the answer, so the caller can alert.
func (s *Service) reverse(ctx context.Context, c Caller, saga domain.ConversionSaga, sub domain.Subscription) (ReversalResult, string, error) {
	tenantID := saga.TenantID
	if saga.ReversalRPDoneAt != nil {
		// RP-4 already answered 200 for this request: only Billing's side is
		// left, and the request is never dropped (RP may now refuse RP-4).
		return s.finishReversal(ctx, c, saga, sub)
	}
	ans := s.RP.RevertConversion(ctx, tenantID, deref(saga.ReversalReason), "revert:"+tenantID+":"+sub.BillingSubscriptionID)
	code := ans.ErrorCode
	if code == "" && ans.Status != 200 {
		code = fmt.Sprintf("http_%d", ans.Status)
	}
	switch {
	case ans.Status == 200:
		if err := s.locked(ctx, tenantID, func(ctx context.Context) error {
			cur, ok, err := s.Sagas.Get(ctx, saga.SagaID)
			if err != nil || !ok || cur.State != domain.SagaReverting || cur.ReversalRPDoneAt != nil {
				return err
			}
			now := s.now()
			cur.ReversalRPDoneAt = &now
			return s.Sagas.Update(ctx, cur)
		}); err != nil {
			return ReversalResult{}, "", err
		}
		now := s.now()
		saga.ReversalRPDoneAt = &now
		return s.finishReversal(ctx, c, saga, sub)
	// RP-4 fails its idempotency key on 409 optimistic_lock_conflict, so a
	// retry with the same key re-runs it, as for RP-3 (domain.ClassifyRP3).
	case ans.Status == 0 || ans.Status >= 500 ||
		ans.Status == 409 && (ans.ErrorCode == "operation_in_progress" || ans.ErrorCode == "optimistic_lock_conflict"):
		if err := s.reversalAttempt(ctx, tenantID, saga.SagaID, code, false); err != nil {
			return ReversalResult{}, "", err
		}
		return ReversalResult{}, "", domain.NewError(domain.ErrIAMUnavailable,
			"RP-4 is still running or unavailable; Billing completes the reversal when Realm Provisioner answers (repeat B-O8 with the same key to check)")
	}
	if err := s.reversalAttempt(ctx, tenantID, saga.SagaID, code, true); err != nil {
		return ReversalResult{}, "", err
	}
	terminal := fmt.Sprintf("%d %s", ans.Status, code)
	if ans.Status == 409 && ans.ErrorCode == "revert_window_expired" {
		return ReversalResult{}, terminal, domain.NewError(domain.ErrRevertWindowExpired, "Realm Provisioner: the reversal window has passed")
	}
	// Terminal: 400 invalid_request (tenant not on a dedicated realm, e.g. a
	// revert that already landed but whose RP ledger write failed), 403
	// (insufficient_role, or an Istio RBAC denial with no problem body), 404.
	return ReversalResult{}, terminal, domain.NewError(domain.ErrInternal,
		"RP-4 refused the reversal ("+terminal+"); check the tenant's realm in B-O7 before repeating")
}

// finishReversal runs completeReversal; a failure is recorded on the saga
// (attempts, backoff, last_error_code completion_failed) for the saga worker
// to retry, and pages once at reversalPageAttempts.
func (s *Service) finishReversal(ctx context.Context, c Caller, saga domain.ConversionSaga, sub domain.Subscription) (ReversalResult, string, error) {
	res, err := s.completeReversal(ctx, c, saga, sub)
	if err == nil {
		return res, "", nil
	}
	attempts := 0
	_ = s.locked(ctx, saga.TenantID, func(ctx context.Context) error {
		cur, ok, gerr := s.Sagas.Get(ctx, saga.SagaID)
		if gerr != nil || !ok || cur.State != domain.SagaReverting {
			return gerr
		}
		now := s.now()
		cur.Attempts++
		attempts = cur.Attempts
		code, next := "completion_failed", now.Add(domain.SagaBackoff(cur.Attempts, s.Settings.SagaMaxBackoff))
		cur.LastErrorCode, cur.LastErrorAt, cur.NextAttemptAt = &code, &now, &next
		return s.Sagas.Update(ctx, cur)
	})
	if attempts == reversalPageAttempts && s.Log != nil {
		s.logger(ctx).Error("conversion reversal cannot complete after RP-4 succeeded; operator decision needed (SEV-2)", map[string]any{
			"tenant_id": saga.TenantID, "saga_id": saga.SagaID, "attempts": attempts, "error": err.Error(),
			"alert": "BillingReversalDecisionNeeded", "severity": "SEV-2"})
	}
	return ReversalResult{}, "", err
}

// reversalPageAttempts is the failed completion attempt that pages: with the
// §8.1.2 backoff (2 s doubling, capped at SAGA_MAX_BACKOFF) about 20 minutes.
const reversalPageAttempts = 8

// reversalAttempt records an RP-4 answer on a still-reverting saga: a retry
// with backoff, or (drop) the end of the request, reverting → confirmed.
func (s *Service) reversalAttempt(ctx context.Context, tenantID, sagaID, code string, drop bool) error {
	return s.locked(ctx, tenantID, func(ctx context.Context) error {
		cur, ok, err := s.Sagas.Get(ctx, sagaID)
		if err != nil || !ok || cur.State != domain.SagaReverting || cur.ReversalRPDoneAt != nil {
			return err // never drop or reschedule once RP-4 answered 200
		}
		now := s.now()
		cur.Attempts++
		cur.LastErrorCode, cur.LastErrorAt = &code, &now
		if drop {
			cur.State, cur.NextAttemptAt = domain.SagaConfirmed, nil
			cur.ReversalRequestedAt, cur.ReversalRequestedBy, cur.ReversalReason, cur.ReversalRefund, cur.ReversalDisputeID = nil, nil, nil, nil, nil
		} else {
			next := now.Add(domain.SagaBackoff(cur.Attempts, s.Settings.SagaMaxBackoff))
			cur.NextAttemptAt = &next
		}
		return s.Sagas.Update(ctx, cur)
	})
}

// completeReversal is §8.2 after RP-4 answered 200, from the request recorded
// on the saga. Every step is safe to repeat: the provider cancel and refund
// use keys derived from the subscription (BIL-IDEM-2), and the final commit
// does nothing once the saga is reverted.
func (s *Service) completeReversal(ctx context.Context, c Caller, saga domain.ConversionSaga, sub domain.Subscription) (ReversalResult, error) {
	tenantID := saga.TenantID
	reason, refund := deref(saga.ReversalReason), saga.ReversalRefund != nil && *saga.ReversalRefund
	var disp *domain.Dispute
	if saga.ReversalDisputeID != nil {
		if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
			d, ok, err := s.Money.Dispute(ctx, *saga.ReversalDisputeID)
			if ok {
				disp = &d
			}
			return err
		}); err != nil {
			return ReversalResult{}, err
		}
	}
	prov, err := s.provider(sub.Provider)
	if err != nil {
		return ReversalResult{}, err
	}
	if sub.ProviderSubscriptionID != nil && sub.Provider != manualProvider && sub.Status != domain.StatusEnded {
		if err := prov.CancelNow(ctx, port.SubscriptionRef{ProviderSubscriptionID: *sub.ProviderSubscriptionID}, "cancel:"+sub.BillingSubscriptionID+":reversal"); err != nil {
			return ReversalResult{}, providerErr(err)
		}
	}
	out := ReversalResult{ReversalStatus: "reverted", RPResponse: json.RawMessage(`{"status":200}`)}
	fundsReturned := disp != nil && disp.ProviderStatus == "lost"
	if refund && !fundsReturned {
		id, err := s.reversalRefund(ctx, c, tenantID, sub.BillingSubscriptionID, reason)
		if err != nil {
			return ReversalResult{}, err
		}
		out.RefundID = id
	}
	actor := deref(saga.ReversalRequestedBy)
	if actor == "" {
		actor = actorOf(c)
	}
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		now := s.now()
		curSaga, ok, err := s.Sagas.Get(ctx, saga.SagaID)
		if err != nil {
			return err
		}
		if !ok || curSaga.State == domain.SagaReverted {
			return nil // completed by the other path (B-O8 or the saga worker)
		}
		cur, err := s.Subs.Get(ctx, sub.BillingSubscriptionID)
		if err != nil {
			return err
		}
		if cur.Status != domain.StatusEnded {
			from := cur.Status
			cancel := domain.CancelOperatorReversal
			ended, err := s.Subs.Transition(ctx, cur.BillingSubscriptionID, 0, domain.StatusEnded, func(x *domain.Subscription) {
				x.EndedAt, x.CancelReason = &now, &cancel
			})
			if err != nil {
				return err
			}
			if err := s.history(ctx, ended, &from, "operator:B-O8", actor, nil, nil, nil); err != nil {
				return err
			}
		}
		res := "reverted"
		curSaga.State, curSaga.Resolution, curSaga.ResolutionReason, curSaga.ResolvedBy = domain.SagaReverted, &res, &reason, &actor
		curSaga.NextAttemptAt = nil
		if err := s.Sagas.Update(ctx, curSaga); err != nil {
			return err
		}
		// RP-4 put the tenant back on a trial (§8.2 step 3); Billing does not
		// consume TrialReactivated, so it records that here. Without it the
		// account stays 'active' and B-P2 / B-O1 refuse to convert it again.
		if err := s.Accounts.ReturnToTrial(ctx, tenantID, now); err != nil {
			return err
		}
		if disp != nil {
			if err := s.Money.DecideDispute(ctx, disp.DisputeID, "reverted", actor); err != nil {
				return err
			}
		}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.conversion.reverted", Action: "conversion.reverted",
			ActorID: actor, TargetType: "subscription", TargetID: sub.BillingSubscriptionID, SourceEventType: "ConversionReverted",
			NaturalID: sub.BillingSubscriptionID, OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"refund": refund, "dispute_id": deref(saga.ReversalDisputeID), "reason": reason}})
	})
	return out, err
}

// reversalRefund refunds what is left of the conversion payment under one
// key per subscription, refund:reversal:<billing_subscription_id>, so B-O8
// and the saga worker never refund twice. A refund already recorded under
// that key is resumed with its own body (§9.2), not recomputed.
func (s *Service) reversalRefund(ctx context.Context, c Caller, tenantID, billingSubscriptionID, reason string) (*string, error) {
	rc := c
	rc.IdempotencyKey = "refund:reversal:" + billingSubscriptionID
	var (
		prev  domain.Refund
		found bool
	)
	if err := s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var err error
		prev, found, err = s.Money.RefundByKey(ctx, tenantID, rc.IdempotencyKey)
		return err
	}); err != nil {
		return nil, err
	}
	if found {
		// The provider refused it, or its key is too old to replay: the
		// reversal completes (the subscription must stop billing) and the
		// operator refunds by hand (B-O3).
		if prev.Status == domain.RefundFailed || (prev.ProviderRefundID == nil && s.now().Sub(prev.CreatedAt) > providerKeyTTL) {
			if s.Log != nil {
				s.logger(ctx).Error("conversion reversal refund was not issued; refund by hand with B-O3 (SEV-2)", map[string]any{
					"tenant_id": tenantID, "refund_id": prev.RefundID, "refund_status": prev.Status,
					"alert": "BillingReversalDecisionNeeded", "severity": "SEV-2"})
			}
			return nil, nil
		}
		f, err := s.refund(ctx, rc, tenantID, prev.InvoiceID, prev.AmountMinor, prev.Currency, prev.Category, prev.Reason)
		return &f.RefundID, err
	}
	inv, ok, err := s.conversionInvoice(ctx, tenantID)
	if err != nil || !ok || inv.AmountPaidMinor-inv.AmountRefundedMinor <= 0 {
		return nil, err
	}
	f, err := s.refund(ctx, rc, tenantID, inv.InvoiceID, inv.AmountPaidMinor-inv.AmountRefundedMinor, inv.Currency,
		domain.RefundCategoryConversionReversal, reason)
	if err != nil {
		return nil, err
	}
	return &f.RefundID, nil
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
		if saga.State == domain.SagaConfirmed || saga.State == domain.SagaReverting || saga.State.Terminal() {
			return domain.NewError(domain.ErrSagaAlreadyConfirmed, "the saga is confirmed, being reverted (B-O8) or closed; use B-O8 or B-O3").With("state", string(saga.State))
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
