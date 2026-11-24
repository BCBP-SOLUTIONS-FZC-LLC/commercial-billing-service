package app

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// platformAudit enqueues an AL-5 entry under platform_tenant (cross-tenant
// operator actions, CD-21) in the billing_system transaction in ctx, so the
// global change and its audit entry commit together (BIL-AUD-1).
func (s *Service) platformAudit(ctx context.Context, e port.AuditEntry) error {
	e.TenantID = PlatformTenantID
	return s.Audit.Write(ctx, e)
}

// AddPlanPrice is B-O11: a new price row; the current row for the key
// ends where it starts (409 PRICE_OVERLAP on overlap).
func (s *Service) AddPlanPrice(ctx context.Context, c Caller, r PlanPriceRequest) (domain.PlanPrice, error) {
	if err := s.verifyPrice(ctx, r.Provider, r.ProviderPriceID, domain.Money{AmountMinor: r.UnitAmountMinor, Currency: r.Currency}, r.Interval); err != nil {
		return domain.PlanPrice{}, err
	}
	p := domain.PlanPrice{Plan: domain.PlanCode(r.Plan), Interval: domain.BillingInterval(r.Interval), Currency: r.Currency,
		UnitAmountMinor: r.UnitAmountMinor, Provider: r.Provider, ProviderPriceID: nonEmpty(r.ProviderPriceID), EffectiveFrom: r.EffectiveFrom.UTC(),
		CreatedBy: c.UserID, Reason: r.Reason}
	err := s.SysTx.RunInSystemTx(ctx, func(ctx context.Context) error {
		var err error
		if p, err = s.Admin.AddPlanPrice(ctx, p); err != nil {
			return err
		}
		return s.platformAudit(ctx, port.AuditEntry{EntryType: "billing.price_book.changed", Action: "plan_price.added", ActorID: c.UserID,
			TargetType: "price_book", TargetID: p.PriceID, SourceEventType: "PriceBookChanged", NaturalID: "price:" + p.PriceID,
			OccurredAt: s.now(), IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"plan": r.Plan, "interval": r.Interval, "currency": r.Currency, "unit_amount_minor": r.UnitAmountMinor,
				"provider": r.Provider, "effective_from": p.EffectiveFrom, "reason": r.Reason}})
	})
	return p, err
}

// UpsertTokenPack is B-O12: add, change or retire (active=false) a pack.
func (s *Service) UpsertTokenPack(ctx context.Context, c Caller, r TokenPackRequest) (json.RawMessage, error) {
	if err := s.verifyPrice(ctx, r.Provider, r.ProviderPriceID, domain.Money{AmountMinor: r.AmountMinor, Currency: r.Currency}, ""); err != nil {
		return nil, err
	}
	p := domain.TokenPack{PackCode: r.PackCode, Tokens: r.Tokens, Currency: r.Currency, AmountMinor: r.AmountMinor, Provider: r.Provider,
		ProviderPriceID: nonEmpty(r.ProviderPriceID), MonthlyCapTokens: r.MonthlyCapTokens, Active: *r.Active, EffectiveFrom: r.EffectiveFrom.UTC()}
	if err := s.SysTx.RunInSystemTx(ctx, func(ctx context.Context) error {
		if err := s.Admin.AddTokenPack(ctx, p, c.UserID, r.Reason); err != nil {
			return err
		}
		return s.platformAudit(ctx, port.AuditEntry{EntryType: "billing.price_book.changed", Action: "token_pack.changed", ActorID: c.UserID,
			TargetType: "price_book", TargetID: r.PackCode, SourceEventType: "PriceBookChanged",
			NaturalID: "pack:" + r.PackCode + ":" + r.Currency + ":" + p.EffectiveFrom.Format("20060102T150405Z"), OccurredAt: s.now(),
			IPAddress: c.ClientIP, UserAgent: c.UserAgent, Metadata: map[string]any{"pack_code": r.PackCode, "tokens": r.Tokens,
				"currency": r.Currency, "amount_minor": r.AmountMinor, "active": *r.Active, "reason": r.Reason}})
	}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"pack_code": p.PackCode, "tokens": p.Tokens, "currency": p.Currency, "amount_minor": p.AmountMinor,
		"provider": p.Provider, "monthly_cap_tokens": p.MonthlyCapTokens, "active": p.Active, "effective_from": p.EffectiveFrom})
}

// ReplayWebhookEvent is B-O13: safe because processing re-reads the
// provider object (BIL-PAY-4); the ledger row records the replay.
func (s *Service) ReplayWebhookEvent(ctx context.Context, c Caller, provider, providerEventID string, r OperatorReason) error {
	found, err := s.Admin.ReplayLedger(ctx, provider, providerEventID, "replay:"+c.UserID)
	if err == nil && !found {
		return domain.NewError(domain.ErrLedgerEventNotFound, "no such provider event in the ledger")
	}
	if err == nil && s.Log != nil {
		s.Log.Info("ledger row replayed by an operator", map[string]any{"provider": provider, "provider_event_id": providerEventID,
			"operator": c.UserID, "reason": r.Reason})
	}
	return err
}

func certView(c domain.Certification) CertificationView {
	return CertificationView{ID: c.ID, Provider: c.Provider, AdapterVersion: c.AdapterVersion, SuiteVersion: c.SuiteVersion,
		Status: string(c.Status), CertifiedBy: c.CertifiedBy, ReviewedBy: c.ReviewedBy, ReviewedAt: c.ReviewedAt,
		EnabledForNewAccounts: c.Status.Enables()}
}

// SubmitCertification is B-O14 (§14.7): status derived from the results,
// never sent; a pending_review record enables nothing until B-O17.
func (s *Service) SubmitCertification(ctx context.Context, c Caller, r CertificationRequest) (CertificationView, error) {
	if r.SuiteVersion != s.Settings.CertSuiteVersion {
		return CertificationView{}, domain.NewError(domain.ErrSuiteVersionMismatch, "suite_version must be "+s.Settings.CertSuiteVersion)
	}
	var caps struct {
		Proration string `json:"Proration"`
	}
	if err := json.Unmarshal(r.Capabilities, &caps); err != nil {
		return CertificationView{}, domain.NewError(domain.ErrValidation, "capabilities must be a JSON object")
	}
	results := make([]domain.CertResult, 0, len(r.Results))
	for _, x := range r.Results {
		results = append(results, domain.CertResult{Capability: x.Capability, Case: x.Case, Passed: x.Passed})
	}
	missing, prorationOK, _ := domain.CertOutcome(r.SuiteVersion, r.ProrationMode, caps.Proration, results)
	if !prorationOK {
		missing = append(missing, "proration/"+r.ProrationMode)
	}
	status := domain.CertPendingReview
	if len(missing) > 0 {
		slices.Sort(missing)
		if r.ProrationMode == "fallback" && !prorationOK {
			return CertificationView{}, domain.NewError(domain.ErrCertificationIncomplete,
				"proration_mode fallback needs capabilities.Proration = fallback and a passing calculator case").With("missing_cases", missing)
		}
		status = domain.CertFailed // recorded for history; the provider stays disabled
	}
	resultsJSON, _ := json.Marshal(r.Results)
	run := port.CertificationRun{Provider: r.Provider, AdapterVersion: r.AdapterVersion, SuiteVersion: r.SuiteVersion, ProrationMode: r.ProrationMode,
		CertifiedBy: c.UserID, ProviderAPIVersion: nonEmpty(r.ProviderAPIVersion), ProviderAccountRef: nonEmpty(r.ProviderAccountRef),
		Status: status, Capabilities: r.Capabilities, Results: resultsJSON, RunStartedAt: r.RunStartedAt, RunFinishedAt: r.RunFinishedAt}
	var cert domain.Certification
	if err := s.SysTx.RunInSystemTx(ctx, func(ctx context.Context) error {
		var err error
		if cert, err = s.Admin.InsertCertification(ctx, run); err != nil {
			return err
		}
		return s.platformAudit(ctx, port.AuditEntry{EntryType: "billing.provider.certified", Action: "certification.submitted", ActorID: c.UserID,
			TargetType: "certification", TargetID: cert.ID, SourceEventType: "ProviderCertificationSubmitted", NaturalID: cert.ID,
			OccurredAt: s.now(), IPAddress: c.ClientIP, UserAgent: c.UserAgent, Metadata: map[string]any{"provider": r.Provider,
				"adapter_version": r.AdapterVersion, "suite_version": r.SuiteVersion, "status": string(status), "missing_cases": missing, "reason": r.Reason}})
	}); err != nil {
		return CertificationView{}, err
	}
	if status == domain.CertFailed {
		return certView(cert), domain.NewError(domain.ErrCertificationIncomplete, "required cases did not pass; recorded as failed").
			With("missing_cases", missing).With("id", cert.ID)
	}
	return certView(cert), nil
}

// ReviewCertification is B-O17 (BIL-PROV-4): only a pci_owner_reviewer
// (checked by the route) who did not submit the run.
func (s *Service) ReviewCertification(ctx context.Context, c Caller, id string, r ReviewRequest) (CertificationView, error) {
	cert, found, err := s.Admin.Certification(ctx, id)
	if err != nil {
		return CertificationView{}, err
	}
	switch {
	case !found:
		return CertificationView{}, domain.NewError(domain.ErrCertificationNotFound, "certification not found")
	case cert.Status != domain.CertPendingReview:
		return CertificationView{}, domain.NewError(domain.ErrCertificationNotPending, "the record is not pending review")
	case cert.CertifiedBy == c.UserID:
		return CertificationView{}, domain.NewError(domain.ErrSelfReviewNotAllowed, "the submitter cannot review their own run")
	}
	status := domain.CertRejected
	if r.Decision == "approve" {
		status = domain.CertPassed
	}
	if err := s.SysTx.RunInSystemTx(ctx, func(ctx context.Context) error {
		var err error
		if cert, err = s.Admin.ReviewCertification(ctx, id, status, c.UserID, r.Reason, s.now()); err != nil {
			return err
		}
		return s.platformAudit(ctx, port.AuditEntry{EntryType: "billing.provider.certification_reviewed", Action: "certification." + r.Decision,
			ActorID: c.UserID, TargetType: "certification", TargetID: id, SourceEventType: "ProviderCertificationReviewed", NaturalID: id,
			OccurredAt: s.now(), IPAddress: c.ClientIP, UserAgent: c.UserAgent, Metadata: map[string]any{"decision": r.Decision,
				"provider": cert.Provider, "adapter_version": cert.AdapterVersion, "suite_version": cert.SuiteVersion,
				"certified_by": cert.CertifiedBy, "reviewed_by": c.UserID, "reason": r.Reason}})
	}); err != nil {
		return CertificationView{}, err
	}
	return certView(cert), nil
}

// RevokeCertification is B-O15: the provider stops taking new accounts
// within PROVIDER_REGISTRY_REFRESH; existing accounts keep working.
func (s *Service) RevokeCertification(ctx context.Context, c Caller, id string, r OperatorReason) (CertificationView, error) {
	cert, found, err := s.Admin.Certification(ctx, id)
	if err != nil {
		return CertificationView{}, err
	}
	if !found {
		return CertificationView{}, domain.NewError(domain.ErrCertificationNotFound, "certification not found")
	}
	if cert.Status != domain.CertPassed {
		return CertificationView{}, domain.NewError(domain.ErrCertificationNotActive, "only a passed certification can be revoked").
			With("status", string(cert.Status))
	}
	if err := s.SysTx.RunInSystemTx(ctx, func(ctx context.Context) error {
		var err error
		if cert, err = s.Admin.RevokeCertification(ctx, id, c.UserID, r.Reason, s.now()); err != nil {
			return err
		}
		return s.platformAudit(ctx, port.AuditEntry{EntryType: "billing.provider.certification_revoked", Action: "certification.revoked",
			ActorID: c.UserID, TargetType: "certification", TargetID: id, SourceEventType: "ProviderCertificationRevoked", NaturalID: id,
			OccurredAt: s.now(), IPAddress: c.ClientIP, UserAgent: c.UserAgent, Metadata: map[string]any{"provider": cert.Provider,
				"adapter_version": cert.AdapterVersion, "reason": r.Reason}})
	}); err != nil {
		return CertificationView{}, err
	}
	v := certView(cert)
	now := s.now()
	v.RevokedAt = &now
	return v, nil
}

// verifyPrice is B-O11 / B-O12's provider check: the referenced price
// exists, is active and matches the row (422 PROVIDER_PRICE_MISMATCH).
func (s *Service) verifyPrice(ctx context.Context, provider, priceRef string, amount domain.Money, interval string) error {
	if provider == manualProvider {
		return nil
	}
	prov, err := s.provider(provider)
	if err != nil {
		return err
	}
	return providerErr(prov.VerifyPrice(ctx, priceRef, port.PriceExpectation{Amount: amount, Interval: interval}))
}
