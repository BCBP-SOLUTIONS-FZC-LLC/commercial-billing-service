package app

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// manualProvider is the Phase 1 stub adapter's name (§8.1.4).
const manualProvider = "manual"

// StubConversion is B-O1 (§8.1.4): a paid subscription on the manual
// provider, invoiced outside the product, then the usual saga.
func (s *Service) StubConversion(ctx context.Context, c Caller, tenantID string, r StubConversionRequest) (StubConversionResult, error) {
	plan, interval := domain.PlanCode(r.Plan), domain.BillingInterval(r.Interval)
	prov, err := s.provider(manualProvider)
	if err != nil {
		return StubConversionResult{}, err
	}
	subID := newID()
	// The manual adapter makes no external call; it only stamps the period.
	res, err := prov.CreateSubscription(ctx, port.SubscriptionInput{Quantity: r.LicensedSeats, Currency: s.Settings.DefaultCurrency,
		Metadata: map[string]string{"tenant_id": tenantID, "billing_subscription_id": subID, "interval": r.Interval}}, "bsub:"+subID+":create")
	if err != nil {
		return StubConversionResult{}, providerErr(err)
	}
	var out StubConversionResult
	err = s.locked(ctx, tenantID, func(ctx context.Context) error {
		acct, err := s.Accounts.Get(ctx, tenantID)
		switch {
		case domain.HasCode(err, domain.ErrSubscriptionNotFound):
			acct = domain.BillingAccount{TenantID: tenantID, Provider: manualProvider, Currency: s.Settings.DefaultCurrency, IAMLifecycle: domain.LifecycleTrial}
			if err := s.Accounts.Create(ctx, acct); err != nil {
				return err
			}
		case err != nil:
			return err
		case acct.IAMLifecycle != domain.LifecycleTrial:
			return domain.NewError(domain.ErrSubscriptionAlreadyExists, "the tenant is not on trial")
		}
		if live, found, err := s.Subs.Live(ctx, tenantID); err != nil {
			return err
		} else if found {
			if live.Status != domain.StatusIncomplete {
				return domain.NewError(domain.ErrSubscriptionAlreadyExists, "the tenant already has a subscription").
					With("billing_subscription_id", live.BillingSubscriptionID)
			}
			if err := s.endIncomplete(ctx, live, "operator:B-O1", c.UserID); err != nil {
				return err
			}
		}
		price, ok, err := s.Prices.PriceAt(ctx, plan, interval, acct.Currency, manualProvider, s.now())
		if err != nil {
			return err
		}
		if !ok {
			return domain.NewError(domain.ErrPriceNotAvailable, "no manual price for this plan, interval and currency")
		}
		now := s.now()
		sub := domain.Subscription{BillingSubscriptionID: subID, TenantID: tenantID, Provider: manualProvider,
			ProviderSubscriptionID: ptr("manual_" + subID), Origin: domain.OriginOperatorStub, Plan: plan, Interval: interval,
			Status: domain.StatusActive, AutoRenew: true, CurrentPeriodStart: &res.PeriodStart, CurrentPeriodEnd: &res.PeriodEnd,
			PaymentConfirmedAt: &now}
		if err := s.Subs.Create(ctx, sub, domain.SubscriptionItem{PriceID: price.PriceID, Quantity: r.LicensedSeats}); err != nil {
			return err
		}
		if err := s.history(ctx, sub, nil, "operator:B-O1", c.UserID, nil, &[2]*int{nil, &r.LicensedSeats}, nil); err != nil {
			return err
		}
		if err := s.startSaga(ctx, sub, domain.SagaOperatorStub, now); err != nil {
			return err
		}
		saga, _, err := s.Sagas.BySubscription(ctx, subID)
		if err != nil {
			return err
		}
		out = StubConversionResult{BillingSubscriptionID: subID, SagaID: saga.SagaID, State: string(saga.State), Provider: manualProvider}
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.conversion.stub", Action: "conversion.stub",
			ActorID: c.UserID, TargetType: "subscription", TargetID: subID, SourceEventType: "ConversionStubbed", NaturalID: subID,
			OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent,
			Metadata: map[string]any{"plan": r.Plan, "interval": r.Interval, "licensed_seats": r.LicensedSeats, "reason": r.Reason}})
	})
	return out, err
}

// SagaAction is B-O9: retry, refund or resolved_manually (§9.3.1).
func (s *Service) SagaAction(ctx context.Context, c Caller, tenantID, sagaID string, r SagaActionRequest) (SagaView, error) {
	if r.Action == "refund" {
		return s.sagaRefund(ctx, c, tenantID, sagaID, r)
	}
	var out SagaView
	err := s.locked(ctx, tenantID, func(ctx context.Context) error {
		saga, found, err := s.Sagas.Get(ctx, sagaID)
		if err != nil {
			return err
		}
		if !found || saga.TenantID != tenantID {
			return domain.NewError(domain.ErrSagaNotFound, "saga not found")
		}
		now := s.now()
		switch r.Action {
		case "retry":
			overdue := saga.State.CountsForOverdueAlert() && now.Sub(saga.PaymentConfirmedAt) > 30*time.Minute
			if saga.State != domain.SagaFailed && (!overdue || saga.State != domain.SagaRPConvertPending) {
				return domain.NewError(domain.ErrSagaAlreadyConfirmed, "only a failed or overdue saga can be retried").With("state", string(saga.State))
			}
			saga.State, saga.NextAttemptAt = domain.SagaRPConvertPending, &now
		case "resolved_manually":
			if saga.State == domain.SagaConfirmed {
				return domain.NewError(domain.ErrSagaAlreadyConfirmed, "the saga is already confirmed")
			}
			if saga.State.Terminal() {
				return domain.NewError(domain.ErrSagaAlreadyConfirmed, "the saga is closed").With("state", string(saga.State))
			}
			saga.State, saga.ConfirmedAt, saga.NextAttemptAt = domain.SagaConfirmed, &now, nil
			sub, err := s.Subs.Get(ctx, saga.BillingSubscriptionID)
			if err != nil {
				return err
			}
			if _, err := s.Emissions.Schedule(ctx, domain.ScheduledEmission{TenantID: tenantID, EventType: domain.EventTenantSeatsChanged,
				PayloadBuilder: domain.BuilderSeatsAfterConversion, RefID: sub.BillingSubscriptionID, DueAt: now}); err != nil {
				return err
			}
		}
		saga.Resolution, saga.ResolutionReason, saga.ResolvedBy = &r.Action, &r.Reason, &c.UserID
		if err := s.Sagas.Update(ctx, saga); err != nil {
			return err
		}
		out = sagaView(saga)
		return s.Audit.Write(ctx, port.AuditEntry{TenantID: tenantID, EntryType: "billing.saga.resolved", Action: "saga." + r.Action,
			ActorID: c.UserID, TargetType: "saga", TargetID: sagaID, SourceEventType: "SagaResolved", NaturalID: sagaID + ":" + r.Action + ":" + now.Format(time.RFC3339Nano),
			OccurredAt: now, IPAddress: c.ClientIP, UserAgent: c.UserAgent, Metadata: map[string]any{"action": r.Action, "reason": r.Reason}})
	})
	return out, err
}

// sagaRefund is B-O9 refund; it needs the refund flow (§8.11).
func (s *Service) sagaRefund(context.Context, Caller, string, string, SagaActionRequest) (SagaView, error) {
	return SagaView{}, notImplemented("B-O9 refund", "6g")
}

func sagaView(s domain.ConversionSaga) SagaView {
	return SagaView{SagaID: s.SagaID, Kind: string(s.Kind), State: string(s.State), BillingSubscriptionID: s.BillingSubscriptionID,
		NextAttemptAt: s.NextAttemptAt, ConfirmedAt: s.ConfirmedAt}
}
