package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/manual"
	stripeadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/stripe"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/webhook"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// providers builds the compiled adapters and the registry (§3.5). In a
// developer environment a listed Stripe without credentials is left out with
// a warning; elsewhere config validation has already refused to start.
func providers(cfg config.Config, sysPool *pgcommon.Pool, log port.Logger) (*provider.Registry, error) {
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
			})
			if err != nil {
				return nil, fmt.Errorf("stripe adapter: %w", err)
			}
			compiled = append(compiled, s)
		}
	}
	return provider.New(compiled, enabled, pgadapter.NewCertificationRepository(sysPool), cfg.ProviderCertSuiteVersion, log)
}

// workers holds the in-pod background loops (§13.1).
type workers struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startWorkers starts the registry refresh and the webhook processor.
func startWorkers(cfg config.Config, reg *provider.Registry, appPool, sysPool *pgcommon.Pool, log port.Logger) *workers {
	ctx, cancel := context.WithCancel(context.Background())
	w := &workers{cancel: cancel, done: make(chan struct{})}
	proc := &app.WebhookProcessor{
		Ledger:      pgadapter.NewLedgerRepository(sysPool),
		Providers:   reg,
		Tx:          pgadapter.NewTxRunner(appPool),
		Handlers:    map[domain.NormalisedKind]port.PaymentEventHandler{}, // Phase 6 registers the §7.4 handlers
		MaxAttempts: cfg.WebhookMaxAttempts,
		Log:         log,
	}
	go func() {
		defer close(w.done)
		go reg.Run(ctx, cfg.ProviderRegistryRefresh, nil)
		proc.Run(ctx, cfg.WebhookPollInterval)
	}()
	return w
}

// stop cancels the loops and waits up to timeout for the processor to return.
func (w *workers) stop(timeout time.Duration) {
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(timeout):
	}
}

// webhookRoute mounts B-W1 (§5.4.3) with no authentication middleware (BIL-AUTH-5).
func webhookRoute(cfg config.Config, reg *provider.Registry, sysPool *pgcommon.Pool, log port.Logger) func(gin.IRouter) {
	h := &webhook.Handler{
		Providers: reg, Ledger: pgadapter.NewLedgerRepository(sysPool), Customers: pgadapter.NewSystemRepository(sysPool),
		MaxBodyBytes: cfg.WebhookMaxBodyBytes, Log: log,
	}
	return h.Register
}
