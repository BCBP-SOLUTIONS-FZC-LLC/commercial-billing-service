package bootstrap

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

type recLog struct {
	mu    sync.Mutex
	warns []string
}

func (*recLog) Debug(string, map[string]any) {}
func (*recLog) Info(string, map[string]any)  {}
func (*recLog) Error(string, map[string]any) {}
func (l *recLog) Warn(m string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, m)
}

// In dev, a listed Stripe without credentials is left out with a warning;
// the manual adapter is always loaded.
func TestProvidersStripeWithoutCredentials(t *testing.T) {
	log := &recLog{}
	reg, err := Providers(config.Config{EnabledPaymentProviders: []string{"stripe", "manual"}, ProviderCertSuiteVersion: "cert-1.0"}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Loaded("stripe"); ok {
		t.Error("stripe loaded without credentials")
	}
	if _, ok := reg.Loaded("manual"); !ok {
		t.Error("manual not loaded")
	}
	if len(log.warns) != 1 || !strings.Contains(log.warns[0], "stripe not loaded") {
		t.Errorf("warnings %v", log.warns)
	}
}

// With credentials the Stripe adapter is compiled in and loaded.
func TestProvidersStripeLoaded(t *testing.T) {
	cfg := config.Config{AppEnv: "dev", EnabledPaymentProviders: []string{"stripe"}, StripeAPIKey: "rk_test_x", StripeWebhookSecrets: []string{"whsec_x"},
		WebhookTolerance: 5 * time.Minute, StripeTimeout: time.Second, StripeMaxRetries: 1, StripeMaxRPS: 5, ProviderCertSuiteVersion: "cert-1.0"}
	reg, err := Providers(cfg, nil, &recLog{})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Loaded("stripe")
	if !ok || p.Name() != "stripe" {
		t.Fatalf("stripe loaded=%v", ok)
	}
	if _, ok := reg.Loaded("manual"); ok {
		t.Error("manual loaded though not enabled")
	}
}

// A malformed AUDIT_* value fails startup before any database call.
func TestAuditRejectsMalformedConfig(t *testing.T) {
	t.Setenv("AUDIT_SOURCE_SERVICE", "")
	if _, err := Audit(context.Background(), config.Config{AppName: "commercial-billing"}, nil, "dev", &recLog{}); err == nil ||
		!strings.Contains(err.Error(), "AUDIT_* configuration") {
		t.Fatalf("err = %v", err)
	}
}

// An invalid metrics identity fails startup before any database call.
func TestAuditRejectsBadMetricsIdentity(t *testing.T) {
	t.Setenv("AUDIT_SOURCE_SERVICE", "commercial-billing")
	t.Setenv("AUDIT_CALLER_DOMAIN", "commercial")
	if _, err := Audit(context.Background(), config.Config{AppName: "Not A Service!"}, nil, "dev", &recLog{}); err == nil ||
		!strings.Contains(err.Error(), "platform-audit metrics") {
		t.Fatalf("err = %v", err)
	}
}

// An unreachable database fails Pools.
func TestPoolsUnreachable(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://billing_app:x@127.0.0.1:1/billing?sslmode=disable&connect_timeout=2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p, err := Pools(ctx, config.Config{PGSystemUser: "billing_system"}, "commercial-billing", &recLog{}); err == nil {
		p.Close()
		t.Fatal("unreachable database accepted")
	}
}

func TestEventsAndHandlers(t *testing.T) {
	if e, err := Events(); err != nil || e == nil {
		t.Fatalf("Events: %v", err)
	}
	h := PaymentHandlers(nil)
	for _, k := range []domain.NormalisedKind{domain.KindCheckoutCompleted, domain.KindInvoicePaid, domain.KindFraudWarning, domain.KindRefundUpdated} {
		if h[k] == nil {
			t.Errorf("no handler for %s", k)
		}
	}
	if WorkerName() == "" {
		t.Error("empty worker name")
	}
}
