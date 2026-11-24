// Package bootstrap builds what cmd/server and cmd/jobs share: the
// provider registry, the AL-5 writer and the use cases on their adapters
// (LLD §3). It is composition only; no behaviour lives here.
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/entitlementsclient"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/iamclient"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/manual"
	stripeadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/stripe"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Providers builds the compiled adapters and the registry (§3.5). In a
// developer environment a listed Stripe without credentials is left out with
// a warning; elsewhere config validation has already refused to start.
func Providers(cfg config.Config, sysPool *pgcommon.Pool, log port.Logger) (*provider.Registry, error) {
	compiled := []port.PaymentProvider{manual.New()}
	enabled := slices.Clone(cfg.EnabledPaymentProviders)
	if slices.Contains(enabled, stripeadapter.Name) {
		if cfg.StripeAPIKey == "" || len(cfg.StripeWebhookSecrets) == 0 {
			log.Warn("stripe listed in ENABLED_PAYMENT_PROVIDERS but STRIPE_API_KEY / STRIPE_WEBHOOK_SECRETS are unset; stripe not loaded (dev only)", nil)
			enabled = slices.DeleteFunc(enabled, func(n string) bool { return n == stripeadapter.Name })
		} else {
			s, err := stripeadapter.New(stripeadapter.Config{
				APIKey: cfg.StripeAPIKey, WebhookSecrets: cfg.StripeWebhookSecrets, Tolerance: cfg.WebhookTolerance,
				Livemode: cfg.AppEnv == "prod", Timeout: cfg.StripeTimeout, MaxRetries: int64(cfg.StripeMaxRetries), MaxRPS: cfg.StripeMaxRPS,
				HTTPClient: httpx.New("stripe", cfg.StripeTimeout), // spans and platform_dependency_request_seconds (§11.2, §11.3)
			})
			if err != nil {
				return nil, fmt.Errorf("stripe adapter: %w", err)
			}
			compiled = append(compiled, s)
		}
	}
	return provider.New(compiled, enabled, pgadapter.NewCertificationRepository(sysPool), cfg.ProviderCertSuiteVersion, log)
}

// sagaClaimMargin is added to RP_TIMEOUT_MS for a saga's claim lease, so a
// slow RP-3 call is never picked up by a second replica.
const sagaClaimMargin = 15 * time.Second

// NewService builds the use cases (§8) on the adapters.
func NewService(cfg config.Config, reg *provider.Registry, appPool, sysPool *pgcommon.Pool, events port.EventEnqueuer,
	auditW port.AuditWriter, metrics port.Metrics, log port.Logger) *app.Service {
	return &app.Service{
		Tx: pgadapter.NewTxRunner(appPool), SysTx: pgadapter.NewSystemTxRunner(sysPool), Lock: pgadapter.TenantLocker{},
		Accounts: pgadapter.NewAccountRepository(appPool), Subs: pgadapter.NewSubscriptionRepository(appPool),
		Prices: pgadapter.NewPriceRepository(appPool), Checkouts: pgadapter.NewCheckoutRepository(appPool), Invoices: pgadapter.NewInvoiceRepository(appPool),
		Changes: pgadapter.NewPlanChangeRepository(appPool, sysPool), Previews: pgadapter.NewPreviewRepository(appPool),
		SeatPurchases: pgadapter.NewSeatPurchaseRepository(appPool), Schedules: pgadapter.NewScheduleRepository(appPool, sysPool),
		FollowUps: pgadapter.NewFollowUpRepository(sysPool), Addons: pgadapter.NewAddonRepository(appPool), Money: pgadapter.NewMoneyRepository(appPool),
		Admin: pgadapter.NewAdminRepository(sysPool),
		Recon: pgadapter.NewReconRepository(sysPool), Ledger: pgadapter.NewLedgerRepository(sysPool),
		Sagas: pgadapter.NewSagaRepository(appPool, sysPool), Emissions: pgadapter.NewEmissionRepository(appPool, sysPool),
		Jobs: pgadapter.NewJobRepository(appPool, sysPool), Customers: pgadapter.NewSystemRepository(sysPool),
		LedgerStore: pgadapter.NewLedgerRepository(sysPool), Metrics: metrics,
		Providers:    reg,
		Core:         iamclient.NewCore(cfg.CoreBaseURL, cfg.CoreTimeout),
		RP:           iamclient.NewRealmProvisioner(cfg.RPBaseURL, cfg.RPTimeout),
		Entitlements: entitlementsclient.New(cfg.EntitlementsBaseURL, cfg.EntitlementsTimeout),
		Events:       events, Audit: auditW, Log: log,
		Settings: app.Settings{
			DefaultCurrency: cfg.DefaultCurrency, NewAccountProvider: "stripe", CheckoutTTL: cfg.CheckoutTTL,
			TenantAppBaseURL: cfg.TenantAppBaseURL, SagaMaxBackoff: cfg.SagaMaxBackoff, SagaClaim: cfg.RPTimeout + sagaClaimMargin,
			SeatsAfterConversionDelay: cfg.SeatsAfterConversionDelay, SCAAbandonAfter: cfg.SCAAbandonAfter, MaxSeatsPerPurchase: cfg.MaxSeatsPerPurchase, SeatPreviewTTL: cfg.SeatPreviewTTL,
			MinChargeMinor: cfg.MinChargeMinor, BelowMinimumPolicy: domain.BelowMinimumPolicy(cfg.ProrationBelowMinimum),
			FeatureDirectSignup: cfg.FeatureDirectSignup, AutoPauseOnOperatorSuspension: cfg.AutoPauseOnOperatorSuspension,
			ReviewThresholdMinor: cfg.ReviewThresholdMinor,
			SagaAlertAfter:       cfg.SagaAlertAfter, SeatRenewalLead: cfg.SeatRenewalLead, DowngradeRecheckLead: cfg.DowngradeRecheckLead,
			PurchaseAbandonAfter: cfg.PurchaseAbandonAfter, SeatResyncAfter: cfg.SeatResyncAfter, RepriceCutoff: cfg.RepriceCutoff,
			PIIMinimiseOffboardedDays: cfg.PIIMinimiseOffboardedDays, PIIMinimiseAbandonedDays: cfg.PIIMinimiseAbandonedDays, CertSuiteVersion: cfg.ProviderCertSuiteVersion, WorkerName: WorkerName(), FeatureTokenAddons: cfg.FeatureTokenAddons, FeatureSeatPurchase: cfg.FeatureSeatPurchase,
		},
	}
}

// PaymentHandlers registers the §7.4 callback handlers implemented so far.
func PaymentHandlers(svc *app.Service) map[domain.NormalisedKind]port.PaymentEventHandler {
	return map[domain.NormalisedKind]port.PaymentEventHandler{
		domain.KindCheckoutCompleted: app.CheckoutCompleted{S: svc},
		domain.KindCheckoutExpired:   app.CheckoutExpired{S: svc},
		// invoice.paid: conversion payment → upgrade proration → seat proration → add-on → downgrade at renewal → cycle (renewal, recovery, reactivation).
		domain.KindInvoicePaid:          app.FirstInvoicePaid{S: svc, Next: app.UpgradeInvoicePaid{S: svc, Next: app.SeatInvoicePaid{S: svc, Next: app.AddonInvoicePaid{S: svc, Next: app.DowngradeOnCycle{S: svc, Next: app.CycleInvoicePaid{S: svc}}}}}},
		domain.KindInvoicePaymentFailed: app.PaymentFailed{S: svc},
		domain.KindSubscriptionEnded:    app.SubscriptionEnded{S: svc},
		domain.KindInvoiceUpdated:       app.InvoiceUpdated{S: svc},
		domain.KindPaymentMethodSet:     app.PaymentMethodSet{S: svc},
		domain.KindRefundUpdated:        app.RefundUpdated{S: svc},
		domain.KindDisputeOpened:        app.DisputeOpened{S: svc},
		domain.KindDisputeUpdated:       app.DisputeOpened{S: svc},
		domain.KindDisputeClosed:        app.DisputeOpened{S: svc},
		domain.KindFraudWarning:         app.DisputeOpened{S: svc},
	}
}

// AuditParts is AL-5 direct-write in outbox mode (§3.3, §10.6).
type AuditParts struct {
	Writer *pgadapter.AuditWriter
	Runner *auditoutbox.Runner // nil when no drainer password is set (dev)
	Pool   *pgcommon.Pool
}

// Audit reads AUDIT_* (failing fast on a malformed value), registers
// the library metrics, checks billing_app can enqueue, and builds the
// runner on the billing_audit_drainer pool.
func Audit(ctx context.Context, cfg config.Config, appCfg pgcommon.Config, appPool *pgcommon.Pool, env string, log port.Logger) (*AuditParts, error) {
	acfg, err := auditconfig.FromEnv("")
	if err != nil {
		return nil, fmt.Errorf("AUDIT_* configuration: %w", err)
	}
	if _, err := audit.InitMetrics(audit.MetricsIdentity{Domain: "commercial", Service: cfg.AppName, Environment: env},
		gincommon.MetricsRegisterer()); err != nil {
		return nil, fmt.Errorf("platform-audit metrics: %w", err)
	}
	if err := auditoutbox.CheckEnqueueAccess(ctx, appPool); err != nil {
		return nil, fmt.Errorf("billing_app cannot enqueue audit entries: %w", err)
	}
	enq, err := auditoutbox.NewEnqueuer(acfg.EnqueueConfig(log))
	if err != nil {
		return nil, err
	}
	out := &AuditParts{Writer: pgadapter.NewAuditWriter(enq)}
	if cfg.PGAuditDrainerPassword == "" {
		log.Warn("PG_AUDIT_DRAINER_PASSWORD unset; audit entries queue in pending_audit_entries and are not delivered (dev only)", nil)
		return out, nil
	}
	dcfg, err := pgadapter.AuditDrainerPoolConfig(appCfg.DSN, cfg.PGAuditDrainerUser, cfg.PGAuditDrainerPassword, int32(cfg.PGAuditDrainerMaxConns), log)
	if err != nil {
		return nil, err
	}
	if out.Pool, err = pgcommon.NewPool(ctx, dcfg); err != nil {
		return nil, fmt.Errorf("audit drainer pool: %w", err)
	}
	client, err := acfg.NewClient(log)
	if err != nil {
		return nil, err
	}
	if out.Runner, err = auditoutbox.NewRunner(acfg.RunnerConfig(out.Pool, client, enq, log)); err != nil {
		return nil, err
	}
	return out, nil
}

// WorkerName identifies this pod in reconciliation_runs.claimed_by.
func WorkerName() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "commercial-billing"
}

// Events builds the billing.events outbox enqueuer with schema validation
// (BIL-EVT-2); publishing is the server's outbox runner.
func Events() (port.EventEnqueuer, error) {
	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		return nil, err
	}
	return pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate), nil
}

// Pools opens the app pool (billing_app, RLS) and the system pool
// (billing_system), traced like the server's (§11.3).
func Pools(ctx context.Context, cfg config.Config, serviceName string, log port.Logger) (appPool, sysPool *pgcommon.Pool, err error) {
	appCfg, warnings := pgadapter.AppPoolConfig(log)
	for _, w := range warnings {
		log.Warn("postgres config warning", map[string]any{"key": w.Key, "reason": w.Reason})
	}
	appCfg.Tracer = gincommon.NewSpanTracer(serviceName)
	if appPool, err = pgcommon.NewPool(ctx, appCfg); err != nil {
		return nil, nil, fmt.Errorf("connect app pool: %w", err)
	}
	sysCfg := pgadapter.SystemPoolConfig(pgadapter.SystemDSNFromEnv(), int32(cfg.PGSystemMaxConns), log)
	sysCfg.Tracer = gincommon.NewSpanTracer(serviceName)
	if sysPool, err = pgcommon.NewPool(ctx, sysCfg); err != nil {
		appPool.Close()
		return nil, nil, fmt.Errorf("connect system pool: %w", err)
	}
	return appPool, sysPool, nil
}
