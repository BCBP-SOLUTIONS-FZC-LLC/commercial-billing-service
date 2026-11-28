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
	DefaultCurrency               string
	NewAccountProvider            string // stripe in Phase 1 (manual only for B-O1)
	CheckoutTTL                   time.Duration
	TenantAppBaseURL              string
	SagaMaxBackoff                time.Duration
	SagaClaim                     time.Duration // lease while RP-3 runs (RP timeout + margin)
	SeatsAfterConversionDelay     time.Duration
	SCAAbandonAfter               time.Duration
	MaxSeatsPerPurchase           int
	SeatPreviewTTL                time.Duration
	MinChargeMinor                int64
	BelowMinimumPolicy            domain.BelowMinimumPolicy
	FeatureDirectSignup           bool
	ReviewThresholdMinor          int64
	CertSuiteVersion              string
	WorkerName                    string // pod name, reconciliation_runs.claimed_by
	AutoPauseOnOperatorSuspension bool
	FeatureTokenAddons            bool
	FeatureSeatPurchase           bool
	// CronJobs (§13.2).
	SagaAlertAfter            time.Duration
	SeatRenewalLead           time.Duration
	DowngradeRecheckLead      time.Duration
	PurchaseAbandonAfter      time.Duration
	SeatResyncAfter           time.Duration
	RepriceCutoff             time.Duration
	PIIMinimiseOffboardedDays int
	PIIMinimiseAbandonedDays  int
}

// Service implements the tenant, internal and operator use cases (§5, §8).
// Routes not implemented yet answer through the embedded Unimplemented.
type Service struct {
	Unimplemented

	Tx            port.TxRunner
	SysTx         port.SystemTxRunner
	Lock          port.TenantLocker
	Accounts      port.AccountRepo
	Subs          port.SubscriptionRepo
	Prices        port.PriceRepo
	Checkouts     port.CheckoutRepo
	Invoices      port.InvoiceRepo
	Changes       port.PlanChangeRepo
	Previews      port.PreviewRepo
	SeatPurchases port.SeatPurchaseRepo
	Schedules     port.ScheduleRepo
	FollowUps     port.FollowUpRepo
	Addons        port.AddonRepo
	Money         port.MoneyRepo
	Admin         port.AdminRepo
	Recon         port.ReconRepo
	Ledger        port.LedgerReader
	LedgerStore   port.LedgerRepo // reconciliation repairs (§8.13)
	Entitlements  port.EntitlementsReader
	Sagas         port.SagaRepo
	Emissions     port.EmissionRepo
	Jobs          port.JobRepo
	Customers     port.SystemReader
	Metrics       port.Metrics
	Providers     ProviderRegistry
	Core          port.CoreClient
	RP            port.RealmProvisionerClient
	Events        port.EventEnqueuer
	Audit         port.AuditWriter
	Settings      Settings
	Now           func() time.Time
	Log           port.Logger

	quotas quotaCache
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
	return s.historyEntry(ctx, sub, domain.StatusHistoryEntry{TenantID: sub.TenantID, BillingSubscriptionID: sub.BillingSubscriptionID,
		FromStatus: from, ToStatus: sub.Status, Cause: cause, PaymentEventRef: eventRef, ActorID: actor}, seats, plans)
}

// historyEntry appends h (with seats and plans when given) and logs a
// status transition.
func (s *Service) historyEntry(ctx context.Context, sub domain.Subscription, h domain.StatusHistoryEntry, seats *[2]*int, plans *[2]*domain.PlanCode) error {
	from, cause, actor := h.FromStatus, h.Cause, h.ActorID
	if seats != nil {
		h.FromSeats, h.ToSeats = seats[0], seats[1]
	}
	if plans != nil {
		h.FromPlan, h.ToPlan = plans[0], plans[1]
	}
	if err := s.Subs.AppendHistory(ctx, h); err != nil {
		return err
	}
	if from == nil || *from != sub.Status {
		// §11.4: one INFO line per state transition, with from/to.
		fromStatus := ""
		if from != nil {
			fromStatus = string(*from)
		}
		s.logger(ctx).Info("subscription status changed", map[string]any{"tenant_id": sub.TenantID,
			"billing_subscription_id": sub.BillingSubscriptionID, "from": fromStatus, "to": string(sub.Status), "cause": cause, "actor_id": actor})
	}
	return nil
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
	add(domain.PermBillingDownloadInvoice, "view_invoices") // B-P17 needs it (C-7, rev 0.73)
	add(domain.PermPaymentMethodManage, "edit_billing_contact")
	return nonNil(out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clip(s)
}
