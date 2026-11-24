package app

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// ProviderRegistry is what the use cases need from the provider registry
// (§3.5): any loaded provider for running work, and the gated choice for a
// new account.
type ProviderRegistry interface {
	port.ProviderLookup
	ForNewAccount(name string) (port.PaymentProvider, error)
}

// Settings are the §12 values the use cases read.
type Settings struct {
	DefaultCurrency           string
	NewAccountProvider        string // stripe in Phase 1 (manual only for B-O1)
	CheckoutTTL               time.Duration
	TenantAppBaseURL          string
	SagaMaxBackoff            time.Duration
	SagaClaim                 time.Duration // lease while RP-3 runs (RP timeout + margin)
	SeatsAfterConversionDelay time.Duration
	SCAAbandonAfter           time.Duration
	MaxSeatsPerPurchase       int
	SeatPreviewTTL            time.Duration
	FeatureDirectSignup       bool
	FeatureTokenAddons        bool
	FeatureSeatPurchase       bool
}

// Service implements the tenant, internal and operator use cases (§5, §8).
// Routes not implemented yet answer through the embedded Unimplemented.
type Service struct {
	Unimplemented

	Tx        port.TxRunner
	Lock      port.TenantLocker
	Accounts  port.AccountRepo
	Subs      port.SubscriptionRepo
	Prices    port.PriceRepo
	Checkouts port.CheckoutRepo
	Invoices  port.InvoiceRepo
	Changes   port.PlanChangeRepo
	Previews  port.PreviewRepo
	Sagas     port.SagaRepo
	Emissions port.EmissionRepo
	Providers ProviderRegistry
	Core      port.CoreClient
	RP        port.RealmProvisionerClient
	Events    port.EventEnqueuer
	Audit     port.AuditWriter
	Settings  Settings
	Now       func() time.Time
	Log       port.Logger
}

var (
	_ TenantAPI   = (*Service)(nil)
	_ InternalAPI = (*Service)(nil)
	_ OperatorAPI = (*Service)(nil)
)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

// locked runs fn in a tenant transaction holding the tenant lock
// (BIL-CONC-1).
func (s *Service) locked(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error {
	return s.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		if err := s.Lock.LockTenant(ctx, tenantID); err != nil {
			return err
		}
		return fn(ctx)
	})
}

// provider returns the loaded adapter for a subscription or account.
func (s *Service) provider(name string) (port.PaymentProvider, error) {
	p, ok := s.Providers.Loaded(name)
	if !ok {
		return nil, port.ProviderNotEnabled(name, "is not loaded")
	}
	return p, nil
}

// providerErr maps an adapter error onto §17.
func providerErr(err error) error { return port.ProviderErrorToDomain(err) }

// actorOf is the history / audit actor: the caller, or commercial-system.
func actorOf(c Caller) string {
	if c.UserID == "" {
		return domain.CommercialSystemPrincipal
	}
	return c.UserID
}

// history appends a §4.2.6 row.
func (s *Service) history(ctx context.Context, sub domain.Subscription, from *domain.SubscriptionStatus, cause, actor string,
	eventRef *string, seats *[2]*int, plans *[2]*domain.PlanCode) error {
	h := domain.StatusHistoryEntry{TenantID: sub.TenantID, BillingSubscriptionID: sub.BillingSubscriptionID, FromStatus: from,
		ToStatus: sub.Status, Cause: cause, PaymentEventRef: eventRef, ActorID: actor}
	if seats != nil {
		h.FromSeats, h.ToSeats = seats[0], seats[1]
	}
	if plans != nil {
		h.FromPlan, h.ToPlan = plans[0], plans[1]
	}
	return s.Subs.AppendHistory(ctx, h)
}

func ptr[T any](v T) *T { return &v }

// actionsFor is SubscriptionView.actions_available: only what the caller
// may do and the state allows (§5.4.1).
func (s *Service) actionsFor(c Caller, sub *domain.Subscription, acct *domain.BillingAccount) []string {
	can := domain.PermissionsFor(c.Roles)
	var out []string
	add := func(p domain.Permission, action string) {
		if can[p] {
			out = append(out, action)
		}
	}
	if sub == nil || sub.Status == domain.StatusIncomplete {
		if acct == nil || acct.IAMLifecycle == domain.LifecycleTrial {
			add(domain.PermSubscriptionChangePlan, "start_subscription")
		}
		return nonNil(out)
	}
	paused := sub.CollectionPaused
	switch sub.Status {
	case domain.StatusActive:
		if !paused {
			add(domain.PermSubscriptionChangePlan, "change_plan")
			if s.Settings.FeatureSeatPurchase {
				add(domain.PermSubscriptionManage, "buy_seats")
			}
			add(domain.PermSubscriptionManage, "schedule_reduction")
			if s.Settings.FeatureTokenAddons {
				add(domain.PermSubscriptionManage, "buy_token_addon")
			}
		}
		if sub.AutoRenew {
			add(domain.PermSubscriptionCancel, "cancel")
		} else {
			add(domain.PermSubscriptionCancel, "resume")
		}
		add(domain.PermPaymentMethodManage, "update_payment_method")
	case domain.StatusPastDue:
		if !paused {
			add(domain.PermSubscriptionChangePlan, "reactivate")
		}
		add(domain.PermPaymentMethodManage, "update_payment_method")
		add(domain.PermSubscriptionCancel, "cancel")
	case domain.StatusCancelled:
		if acct == nil || acct.IAMLifecycle != domain.LifecycleOffboarded {
			add(domain.PermSubscriptionChangePlan, "reactivate")
		}
	case domain.StatusIncomplete, domain.StatusEnded:
	}
	add(domain.PermBillingView, "view_invoices")
	add(domain.PermPaymentMethodManage, "edit_billing_contact")
	return nonNil(out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clip(s)
}
