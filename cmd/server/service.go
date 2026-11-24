package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/entitlementsclient"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/iamclient"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// sagaClaimMargin is added to RP_TIMEOUT_MS for a saga's claim lease, so a
// slow RP-3 call is never picked up by a second replica.
const sagaClaimMargin = 15 * time.Second

// buildService builds the use cases (§8) on the adapters.
func buildService(cfg config.Config, reg *provider.Registry, appPool, sysPool *pgcommon.Pool, events port.EventEnqueuer,
	auditW port.AuditWriter, log port.Logger) *app.Service {
	return &app.Service{
		Tx: pgadapter.NewTxRunner(appPool), Lock: pgadapter.TenantLocker{},
		Accounts: pgadapter.NewAccountRepository(appPool), Subs: pgadapter.NewSubscriptionRepository(appPool),
		Prices: pgadapter.NewPriceRepository(appPool), Checkouts: pgadapter.NewCheckoutRepository(appPool), Invoices: pgadapter.NewInvoiceRepository(appPool),
		Changes: pgadapter.NewPlanChangeRepository(appPool, sysPool), Previews: pgadapter.NewPreviewRepository(appPool),
		SeatPurchases: pgadapter.NewSeatPurchaseRepository(appPool), Schedules: pgadapter.NewScheduleRepository(appPool, sysPool),
		FollowUps: pgadapter.NewFollowUpRepository(sysPool), Addons: pgadapter.NewAddonRepository(appPool), Money: pgadapter.NewMoneyRepository(appPool),
		Sagas: pgadapter.NewSagaRepository(appPool, sysPool), Emissions: pgadapter.NewEmissionRepository(appPool, sysPool),
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
			ReviewThresholdMinor: cfg.ReviewThresholdMinor, FeatureTokenAddons: cfg.FeatureTokenAddons, FeatureSeatPurchase: cfg.FeatureSeatPurchase,
		},
	}
}

// paymentHandlers registers the §7.4 callback handlers implemented so far.
func paymentHandlers(svc *app.Service) map[domain.NormalisedKind]port.PaymentEventHandler {
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

// pollLoop runs once every interval until ctx ends; a full batch polls
// again at once.
func pollLoop(ctx context.Context, interval time.Duration, batch int, once func(ctx context.Context, limit int) (int, error), log port.Logger, name string) {
	for {
		n, err := once(ctx, batch)
		if err != nil && ctx.Err() == nil {
			log.Warn(name+" pass failed", map[string]any{"error": err.Error()})
		}
		if err == nil && n >= batch {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
