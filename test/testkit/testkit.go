//go:build integration

// Package testkit is the shared harness for Billing's end-to-end suites
// (test/e2e): a Postgres 17 testcontainer migrated with the service's own
// migrations and used through the production roles (billing_app with RLS,
// billing_system with BYPASSRLS), a seeded Stripe price book, and recording
// fakes for the payment provider, Core, the Realm Provisioner and
// Entitlements. NewService builds the real app.Service on top, wired as
// cmd/server wires it (internal/bootstrap), so a suite exercises the real use
// cases, repositories, RLS, outbox and audit writes.
//
// It is a non-test package so several suites can share it; the build tag
// keeps it (and testcontainers) out of the production build.
package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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

// DB is one migrated database and the pools the service uses.
type DB struct {
	// Super is the container superuser (assertions and seeding only).
	Super *pgcommon.Pool
	// App is billing_app: RLS-bound, app.tenant_id bound per transaction.
	App *pgcommon.Pool
	// System is billing_system: BYPASSRLS, the cross-tenant paths.
	System *pgcommon.Pool
}

// Start runs a Postgres 17 container with scripts/init-db.sql (the roles),
// applies the library schemas and Billing's migrations as billing_migrator,
// opens the three pools and seeds a Stripe price book and two token packs.
// root is the path from the calling package to the repository root.
func Start(ctx context.Context, root string) (*DB, func(), error) {
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("billing"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		tcpostgres.WithInitScripts(root+"/scripts/init-db.sql"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: %w", err)
	}
	stop := func() { _ = pg.Terminate(context.Background()) }
	superDSN, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		stop()
		return nil, nil, err
	}
	if err := pgadapter.MigrateSchema(ctx, WithUser(superDSN, "billing_migrator"), nil); err != nil {
		stop()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	mk := func(dsn, name string, guc bool) (*pgcommon.Pool, error) {
		cfg := pgcommon.Config{DSN: dsn, PoolName: name, MaxConns: 8, PGBouncerMode: name != "super"}
		if guc {
			cfg.GUCProvider = pgcommon.GUCSetFromContext
		}
		return pgcommon.NewPool(ctx, cfg)
	}
	db := &DB{}
	for _, p := range []struct {
		dst  **pgcommon.Pool
		dsn  string
		name string
		guc  bool
	}{
		{&db.Super, superDSN, "super", false},
		{&db.App, WithUser(superDSN, "billing_app"), "app", true},
		{&db.System, WithUser(superDSN, "billing_system"), "system", false},
	} {
		if *p.dst, err = mk(p.dsn, p.name, p.guc); err != nil {
			db.close()
			stop()
			return nil, nil, fmt.Errorf("%s pool: %w", p.name, err)
		}
	}
	if err := db.seed(ctx); err != nil {
		db.close()
		stop()
		return nil, nil, err
	}
	return db, func() { db.close(); stop() }, nil
}

func (db *DB) close() {
	for _, p := range []*pgcommon.Pool{db.Super, db.App, db.System} {
		if p != nil {
			p.Close()
		}
	}
}

// seed copies the manual price book to a "stripe" one and adds two packs.
func (db *DB) seed(ctx context.Context) error {
	if err := db.Exec(ctx, `INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, created_by, reason)
		SELECT plan, interval, currency, unit_amount_minor, 'stripe', 'price_'||plan||'_'||interval, effective_from, created_by, 'Test Stripe price book row'
		FROM plan_prices WHERE provider = 'manual'`); err != nil {
		return fmt.Errorf("seed prices: %w", err)
	}
	if err := db.Exec(ctx, `INSERT INTO token_packs (pack_code, tokens, currency, amount_minor, provider, provider_price_id, monthly_cap_tokens,
		effective_from, created_by, reason) VALUES
		('tokens_1m', 1000000, 'USD', 4900, 'stripe', 'price_tokens_1m', NULL, '2026-01-01', '00000000-0000-0000-0000-0000000000c1', 'test pack'),
		('tokens_5m', 5000000, 'USD', 19900, 'stripe', 'price_tokens_5m', 20000000, '2026-01-01', '00000000-0000-0000-0000-0000000000c1', 'test pack')`); err != nil {
		return fmt.Errorf("seed packs: %w", err)
	}
	return nil
}

// WithUser rewrites dsn for one of the init-db.sql roles (password devpassword).
func WithUser(dsn, user string) string {
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(user, "devpassword")
	return u.String()
}

// Exec runs a statement as the superuser.
func (db *DB) Exec(ctx context.Context, q string, args ...any) error {
	return pgcommon.RunInTx(ctx, db.Super, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	})
}

// Scalar reads one value as the superuser.
func Scalar[T any](ctx context.Context, db *DB, q string, args ...any) (T, error) {
	var v T
	err := pgcommon.RunInTx(ctx, db.Super, pgcommon.TxOptions{}, func(ctx context.Context, tx pgcommon.Tx) error {
		return tx.QueryRow(ctx, q, args...).Scan(&v)
	})
	return v, err
}

// ── Fakes ───────────────────────────────────────────────────────────────

// SignatureHeader is the header the fake provider's webhook verifier reads;
// ValidSignature is the only value it accepts.
const (
	SignatureHeader = "X-Test-Signature"
	ValidSignature  = "valid"
)

// Provider is a PaymentProvider that records every call with its
// idempotency key and answers like a provider whose payments succeed.
type Provider struct {
	port.PaymentProvider
	ProviderName string
	mu           sync.Mutex
	calls        []string
	// Confirm scripts ConfirmInitialPayment answers in order (default: paid).
	Confirm []port.PaymentOutcome
}

// NewProvider returns a fake named name ("stripe").
func NewProvider(name string) *Provider { return &Provider{ProviderName: name} }

func (p *Provider) record(c string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c)
}

// Calls returns the recorded calls that start with prefix.
func (p *Provider) Calls(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// Name implements port.PaymentProvider.
func (p *Provider) Name() string { return p.ProviderName }

// Capabilities implements port.PaymentProvider.
func (p *Provider) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{HostedCardEntry: true, RecurringSubscriptions: true, OffSessionCharges: true, Schedules: true,
		CollectionPause: true, HostedPaymentMethodUpdate: true, WebhookRedelivery: true, Proration: port.ProrationNative, Currencies: []string{"USD"}}
}

// VerifyAndNormalise accepts a body {"id","type","customer"} signed with
// SignatureHeader: ValidSignature; anything else is ErrInvalidSignature.
func (p *Provider) VerifyAndNormalise(_ context.Context, h http.Header, body []byte) (domain.PaymentEvent, error) {
	if h.Get(SignatureHeader) != ValidSignature {
		return domain.PaymentEvent{}, port.ErrInvalidSignature
	}
	var in struct {
		ID, Type, Customer string
	}
	if err := json.Unmarshal(body, &in); err != nil || in.ID == "" {
		return domain.PaymentEvent{}, port.ErrInvalidSignature
	}
	return domain.PaymentEvent{Provider: p.ProviderName, ProviderEventID: in.ID, EventType: in.Type, Kind: domain.KindInvoiceUpdated,
		ProviderCreatedAt: time.Now().UTC(), CustomerRef: in.Customer,
		Object: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_" + in.ID}, Payload: []byte(`{}`)}, nil
}

// EnsureCustomer implements port.PaymentProvider.
func (p *Provider) EnsureCustomer(_ context.Context, in port.CustomerInput, key string) (port.CustomerRef, error) {
	p.record("EnsureCustomer " + key)
	return port.CustomerRef{ProviderCustomerID: "cus_" + in.TenantID[:8]}, nil
}

// AttachDefaultPaymentMethod implements port.PaymentProvider.
func (p *Provider) AttachDefaultPaymentMethod(_ context.Context, _ port.CustomerRef, pm, key string) error {
	p.record("AttachDefaultPaymentMethod " + pm + " " + key)
	return nil
}

// CreateSubscription implements port.PaymentProvider.
func (p *Provider) CreateSubscription(_ context.Context, in port.SubscriptionInput, key string) (port.SubscriptionResult, error) {
	p.record("CreateSubscription " + key)
	return port.SubscriptionResult{Ref: port.SubscriptionRef{ProviderSubscriptionID: "sub_" + in.Metadata["billing_subscription_id"]},
		Outcome: port.PaymentOutcome{Status: port.PaymentPending}}, nil
}

// ConfirmInitialPayment implements port.PaymentProvider.
func (p *Provider) ConfirmInitialPayment(_ context.Context, _ port.SubscriptionRef, key string) (port.PaymentOutcome, error) {
	p.record("ConfirmInitialPayment " + key)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Confirm) == 0 {
		return port.PaymentOutcome{Status: port.PaymentPaid}, nil
	}
	out := p.Confirm[0]
	p.Confirm = p.Confirm[1:]
	return out, nil
}

// CancelNow implements port.PaymentProvider.
func (p *Provider) CancelNow(_ context.Context, ref port.SubscriptionRef, key string) error {
	p.record("CancelNow " + ref.ProviderSubscriptionID + " " + key)
	return nil
}

// FetchCurrent implements port.PaymentProvider: an active subscription
// with a one-month period from a fixed start.
func (p *Provider) FetchCurrent(_ context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	p.record("FetchCurrent " + obj.ID)
	start := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)
	return domain.ProviderObjectState{Ref: obj, Status: "active", Fields: map[string]any{
		"current_period_start": start, "current_period_end": start.AddDate(0, 1, 0)}}, nil
}

// PreviewQuantityChange implements port.PaymentProvider (2300 minor per seat).
func (p *Provider) PreviewQuantityChange(_ context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	p.record("PreviewQuantityChange")
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: int64(in.To-in.From) * 2300, Currency: "USD"}, ProrationInstant: in.ProrationInstant}, nil
}

// ChangeQuantity implements port.PaymentProvider (paid).
func (p *Provider) ChangeQuantity(_ context.Context, in port.QuantityChange, key string) (port.ChangeResult, error) {
	p.record(fmt.Sprintf("ChangeQuantity %d->%d %s", in.From, in.To, key))
	return port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}, nil
}

// SetCancelAtPeriodEnd implements port.PaymentProvider.
func (p *Provider) SetCancelAtPeriodEnd(_ context.Context, ref port.SubscriptionRef, cancel bool, key string) error {
	p.record(fmt.Sprintf("SetCancelAtPeriodEnd %s %v %s", ref.ProviderSubscriptionID, cancel, key))
	return nil
}

// ListPaymentMethods implements port.PaymentProvider.
func (p *Provider) ListPaymentMethods(context.Context, port.CustomerRef) ([]port.PaymentMethodView, error) {
	p.record("ListPaymentMethods")
	return []port.PaymentMethodView{{ProviderRef: "pm_1QAbCdEfGh1234", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2028, IsDefault: true}}, nil
}

// UpdateCustomer implements port.PaymentProvider.
func (p *Provider) UpdateCustomer(_ context.Context, _ port.CustomerRef, in port.CustomerInput, key string) error {
	p.record("UpdateCustomer " + key)
	return nil
}

// Registry is a ProviderLookup over fixed providers.
type Registry map[string]port.PaymentProvider

// Loaded implements port.ProviderLookup.
func (r Registry) Loaded(n string) (port.PaymentProvider, bool) { p, ok := r[n]; return p, ok }

// ForNewAccount implements port.ProviderLookup.
func (r Registry) ForNewAccount(n string) (port.PaymentProvider, error) {
	if p, ok := r[n]; ok {
		return p, nil
	}
	return nil, port.ProviderNotEnabled(n, "is not enabled")
}

// Core is a CoreClient with fixed answers.
type Core struct {
	Usage port.SeatUsage
	Check port.PlanChangeCheck
	Err   error
}

// SeatUsage implements port.CoreClient.
func (c *Core) SeatUsage(context.Context, string) (port.SeatUsage, error) { return c.Usage, c.Err }

// PlanChangeCheck implements port.CoreClient.
func (c *Core) PlanChangeCheck(context.Context, string, domain.PlanCode) (port.PlanChangeCheck, error) {
	return c.Check, c.Err
}

// RP is a RealmProvisionerClient that accepts every call and records RP-3 keys.
type RP struct {
	mu   sync.Mutex
	Keys []string
}

// Convert implements port.RealmProvisionerClient.
func (r *RP) Convert(_ context.Context, _ string, _ port.ConvertRequest, key string) domain.RP3Answer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Keys = append(r.Keys, key)
	return domain.RP3Answer{Status: 200}
}

// RevertConversion implements port.RealmProvisionerClient.
func (r *RP) RevertConversion(context.Context, string, string, string) domain.RP3Answer {
	return domain.RP3Answer{Status: 200}
}

// Realm implements port.RealmProvisionerClient.
func (r *RP) Realm(context.Context, string) (port.RealmView, error) { return port.RealmView{}, nil }

// Entitlements is an EntitlementsReader with a fixed finite quota.
type Entitlements struct {
	Limit *int64
	Err   error
}

// TokenQuota implements port.EntitlementsReader.
func (e *Entitlements) TokenQuota(context.Context, string) (port.TokenQuota, error) {
	return port.TokenQuota{MonthlyLimit: e.Limit}, e.Err
}

// Fakes are the outbound fakes a Service is built on.
type Fakes struct {
	Stripe       *Provider
	Core         *Core
	RP           *RP
	Entitlements *Entitlements
}

// NewFakes returns fakes whose calls all succeed.
func NewFakes() *Fakes {
	limit := int64(5_000_000)
	return &Fakes{Stripe: NewProvider("stripe"), Core: &Core{Usage: port.SeatUsage{ActiveUsers: 1}, Check: port.PlanChangeCheck{Allowed: true}},
		RP: &RP{}, Entitlements: &Entitlements{Limit: &limit}}
}

// Registry returns the provider lookup: the fake "stripe" and the real manual adapter.
func (f *Fakes) Registry() Registry {
	return Registry{"stripe": f.Stripe, "manual": manualadapter.New()}
}

// NewService builds app.Service as cmd/server does, on db's pools and f.
func NewService(db *DB, f *Fakes) (*app.Service, error) {
	schemas, err := eventbus.LoadSchemas()
	if err != nil {
		return nil, err
	}
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: "commercial-billing", KeyNamespace: audit.KeyNamespace("commercial-billing")})
	if err != nil {
		return nil, err
	}
	appPool, sysPool := db.App, db.System
	return &app.Service{
		Tx: pgadapter.NewTxRunner(appPool), SysTx: pgadapter.NewSystemTxRunner(sysPool), Lock: pgadapter.TenantLocker{},
		Accounts: pgadapter.NewAccountRepository(appPool), Subs: pgadapter.NewSubscriptionRepository(appPool),
		Prices: pgadapter.NewPriceRepository(appPool), Checkouts: pgadapter.NewCheckoutRepository(appPool), Invoices: pgadapter.NewInvoiceRepository(appPool),
		Changes: pgadapter.NewPlanChangeRepository(appPool, sysPool), Previews: pgadapter.NewPreviewRepository(appPool),
		SeatPurchases: pgadapter.NewSeatPurchaseRepository(appPool), Schedules: pgadapter.NewScheduleRepository(appPool, sysPool),
		FollowUps: pgadapter.NewFollowUpRepository(sysPool), Addons: pgadapter.NewAddonRepository(appPool), Money: pgadapter.NewMoneyRepository(appPool),
		Admin: pgadapter.NewAdminRepository(sysPool), Recon: pgadapter.NewReconRepository(sysPool), Ledger: pgadapter.NewLedgerRepository(sysPool),
		Entitlements: f.Entitlements, Sagas: pgadapter.NewSagaRepository(appPool, sysPool), Emissions: pgadapter.NewEmissionRepository(appPool, sysPool),
		Jobs: pgadapter.NewJobRepository(appPool, sysPool), Customers: pgadapter.NewSystemRepository(sysPool),
		LedgerStore: pgadapter.NewLedgerRepository(sysPool), Metrics: port.NopMetrics{},
		Providers: f.Registry(), Core: f.Core, RP: f.RP,
		Events: pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate), Audit: pgadapter.NewAuditWriter(enq),
		Settings: app.Settings{DefaultCurrency: "USD", NewAccountProvider: "stripe", CheckoutTTL: time.Hour, TenantAppBaseURL: "https://app.example",
			SagaMaxBackoff: 5 * time.Minute, SCAAbandonAfter: 30 * time.Minute, SagaClaim: 45 * time.Second, SeatsAfterConversionDelay: 120 * time.Second,
			MaxSeatsPerPurchase: 1000, SeatPreviewTTL: 10 * time.Minute, MinChargeMinor: 200, BelowMinimumPolicy: domain.FreeUntilRenewal,
			FeatureSeatPurchase: true, FeatureTokenAddons: true, FeatureDirectSignup: true, AutoPauseOnOperatorSuspension: true,
			ReviewThresholdMinor: 100000, CertSuiteVersion: "cert-1.0", WorkerName: "e2e",
			SagaAlertAfter: 30 * time.Minute, SeatRenewalLead: 24 * time.Hour, DowngradeRecheckLead: 2 * time.Hour, PurchaseAbandonAfter: 30 * time.Minute,
			SeatResyncAfter: 10 * time.Minute, RepriceCutoff: time.Hour, PIIMinimiseOffboardedDays: 90, PIIMinimiseAbandonedDays: 30},
	}, nil
}
