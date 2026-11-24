//go:build integration

// Package flows_test runs the §8 business flows on Postgres 17 with the
// production roles, against fake provider and IAM adapters that record
// every call (LLD §14.2). Each test uses its own tenant.
package flows_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	manualadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/manual"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

var superPool, appPool, systemPool *pgcommon.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("billing"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		tcpostgres.WithInitScripts("../../scripts/init-db.sql"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres: %v\n", err)
		return 1
	}
	defer func() { _ = pg.Terminate(ctx) }()
	superDSN, _ := pg.ConnectionString(ctx, "sslmode=disable")
	if err := pgadapter.MigrateSchema(ctx, withUser(superDSN, "billing_migrator"), nil); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	mk := func(dsn, name string, guc bool) *pgcommon.Pool {
		cfg := pgcommon.Config{DSN: dsn, PoolName: name, MaxConns: 6, PGBouncerMode: name != "super"}
		if guc {
			cfg.GUCProvider = pgcommon.GUCSetFromContext
		}
		p, err := pgcommon.NewPool(ctx, cfg)
		if err != nil {
			panic(err)
		}
		return p
	}
	superPool, appPool, systemPool = mk(superDSN, "super", false), mk(withUser(superDSN, "billing_app"), "app", true), mk(withUser(superDSN, "billing_system"), "system", false)
	defer func() { superPool.Close(); appPool.Close(); systemPool.Close() }()
	// A Stripe price book for the fake "stripe" provider.
	if err := sql(ctx, `INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, created_by, reason)
		SELECT plan, interval, currency, unit_amount_minor, 'stripe', 'price_'||plan||'_'||interval, effective_from, created_by, 'Test Stripe price book row'
		FROM plan_prices WHERE provider = 'manual'`); err != nil {
		fmt.Fprintf(os.Stderr, "seed prices: %v\n", err)
		return 1
	}
	return m.Run()
}

func withUser(dsn, user string) string {
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(user, "devpassword")
	return u.String()
}

func sql(ctx context.Context, q string, args ...any) error {
	return pgcommon.RunInTx(ctx, superPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	})
}

// scalar reads one value as the superuser.
func scalar[T any](t *testing.T, q string, args ...any) T {
	t.Helper()
	var v T
	err := pgcommon.RunInTx(context.Background(), superPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&v)
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// ── fakes ───────────────────────────────────────────────────────────────

// fakeProvider is a PaymentProvider that records calls and their keys.
type fakeProvider struct {
	port.PaymentProvider
	name  string
	mu    sync.Mutex
	calls []string
	fail  error
}

func (p *fakeProvider) record(c string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c)
	return p.fail
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{HostedCardEntry: true, RecurringSubscriptions: true, Currencies: []string{"USD"}}
}

func (p *fakeProvider) EnsureCustomer(_ context.Context, in port.CustomerInput, key string) (port.CustomerRef, error) {
	return port.CustomerRef{ProviderCustomerID: "cus_" + in.TenantID[:8]}, p.record("EnsureCustomer " + key)
}

func (p *fakeProvider) CreateHostedCheckout(_ context.Context, in port.CheckoutInput, key string) (port.HostedSession, error) {
	return port.HostedSession{ProviderSessionID: "cs_" + in.Metadata["billing_subscription_id"], URL: "https://checkout.example/" + in.Metadata["billing_subscription_id"],
		ExpiresAt: in.ExpiresAt}, p.record("CreateHostedCheckout " + key)
}

type registry map[string]port.PaymentProvider

func (r registry) Loaded(n string) (port.PaymentProvider, bool) { p, ok := r[n]; return p, ok }
func (r registry) ForNewAccount(n string) (port.PaymentProvider, error) {
	if p, ok := r[n]; ok {
		return p, nil
	}
	return nil, port.ProviderNotEnabled(n, "is not enabled")
}

type fakeCore struct {
	usage port.SeatUsage
	err   error
	check port.PlanChangeCheck
}

func (c *fakeCore) SeatUsage(context.Context, string) (port.SeatUsage, error) { return c.usage, c.err }
func (c *fakeCore) PlanChangeCheck(context.Context, string, domain.PlanCode) (port.PlanChangeCheck, error) {
	return c.check, c.err
}

type fakeRP struct {
	mu      sync.Mutex
	answers []domain.RP3Answer
	keys    []string
}

func (r *fakeRP) Convert(_ context.Context, _ string, _ port.ConvertRequest, key string) domain.RP3Answer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
	if len(r.answers) == 0 {
		return domain.RP3Answer{Status: 200}
	}
	a := r.answers[0]
	r.answers = r.answers[1:]
	return a
}
func (r *fakeRP) RevertConversion(context.Context, string, string, string) domain.RP3Answer {
	return domain.RP3Answer{Status: 200}
}
func (r *fakeRP) Realm(context.Context, string) (port.RealmView, error) { return port.RealmView{}, nil }

type env struct {
	svc    *app.Service
	stripe *fakeProvider
	core   *fakeCore
	rp     *fakeRP
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: "commercial-billing", KeyNamespace: audit.KeyNamespace("commercial-billing")})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{stripe: &fakeProvider{name: "stripe"}, core: &fakeCore{}, rp: &fakeRP{}, now: time.Now().UTC().Truncate(time.Microsecond)}
	manual := manualadapter.New()
	e.svc = &app.Service{
		Tx: pgadapter.NewTxRunner(appPool), Lock: pgadapter.TenantLocker{},
		Accounts: pgadapter.NewAccountRepository(appPool), Subs: pgadapter.NewSubscriptionRepository(appPool),
		Prices: pgadapter.NewPriceRepository(appPool), Checkouts: pgadapter.NewCheckoutRepository(appPool),
		Sagas: pgadapter.NewSagaRepository(appPool, systemPool), Emissions: pgadapter.NewEmissionRepository(appPool, systemPool),
		Providers: registry{"stripe": e.stripe, "manual": manual}, Core: e.core, RP: e.rp,
		Events: pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate), Audit: pgadapter.NewAuditWriter(enq),
		Settings: app.Settings{DefaultCurrency: "USD", NewAccountProvider: "stripe", CheckoutTTL: time.Hour, TenantAppBaseURL: "https://app.example",
			SagaMaxBackoff: 5 * time.Minute, SagaClaim: 45 * time.Second, SeatsAfterConversionDelay: 120 * time.Second, MaxSeatsPerPurchase: 1000,
			FeatureSeatPurchase: true},
		Now: func() time.Time { return e.now },
	}
	return e
}

func newTenant() string { return uuid.NewString() }

var owner = app.Caller{UserID: "4b1e8a52-1111-4222-8333-944455556666", Roles: []string{domain.RoleTenantOwner}}
var operator = app.Caller{UserID: "9a9a9a9a-1111-4222-8333-944455556666", Roles: []string{domain.RolePlatformOperator}}

// handle runs a callback handler as the webhook processor does: in a
// tenant transaction.
func (e *env) handle(t *testing.T, tenant string, h port.PaymentEventHandler, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	t.Helper()
	var out string
	err := pgadapter.NewTxRunner(appPool).RunInTenantTx(context.Background(), tenant, func(ctx context.Context) error {
		var err error
		out, err = h.Handle(ctx, tenant, ev, cur)
		return err
	})
	return out, err
}

func txRunner() *pgadapter.TxRunner { return pgadapter.NewTxRunner(appPool) }
