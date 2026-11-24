//go:build integration

// Package flows_test runs the §8 business flows on Postgres 17 with the
// production roles, against fake provider and IAM adapters that record
// every call (LLD §14.2). Each test uses its own tenant.
package flows_test

import (
	"context"
	"errors"
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
	if err := sql(ctx, `INSERT INTO token_packs (pack_code, tokens, currency, amount_minor, provider, provider_price_id, monthly_cap_tokens,
		effective_from, created_by, reason) VALUES
		('tokens_1m', 1000000, 'USD', 4900, 'stripe', 'price_tokens_1m', NULL, '2026-01-01', '00000000-0000-0000-0000-0000000000c1', 'test pack'),
		('tokens_5m', 5000000, 'USD', 19900, 'stripe', 'price_tokens_5m', 20000000, '2026-01-01', '00000000-0000-0000-0000-0000000000c1', 'test pack')`); err != nil {
		fmt.Fprintf(os.Stderr, "seed packs: %v\n", err)
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
	name         string
	mu           sync.Mutex
	calls        []string
	fail         error
	confirm      []confirmAnswer
	change       []confirmAnswer
	fallback     bool
	refundStatus string
}

func (p *fakeProvider) record(c string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c)
	return p.fail
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Capabilities() port.ProviderCapabilities {
	mode := port.ProrationNative
	if p.fallback {
		mode = port.ProrationFallback
	}
	return port.ProviderCapabilities{HostedCardEntry: true, RecurringSubscriptions: true, Schedules: true, CollectionPause: true, HostedPaymentMethodUpdate: true, Proration: mode, Currencies: []string{"USD"}}
}

func (p *fakeProvider) EnsureCustomer(_ context.Context, in port.CustomerInput, key string) (port.CustomerRef, error) {
	return port.CustomerRef{ProviderCustomerID: "cus_" + in.TenantID[:8]}, p.record("EnsureCustomer " + key)
}

func (p *fakeProvider) CreateHostedCheckout(_ context.Context, in port.CheckoutInput, key string) (port.HostedSession, error) {
	return port.HostedSession{ProviderSessionID: "cs_" + in.Metadata["billing_subscription_id"], URL: "https://checkout.example/" + in.Metadata["billing_subscription_id"],
		ExpiresAt: in.ExpiresAt}, p.record("CreateHostedCheckout " + key)
}

func (p *fakeProvider) AttachDefaultPaymentMethod(_ context.Context, _ port.CustomerRef, pm, key string) error {
	return p.record("AttachDefaultPaymentMethod " + pm + " " + key)
}

func (p *fakeProvider) CreateSubscription(_ context.Context, in port.SubscriptionInput, key string) (port.SubscriptionResult, error) {
	return port.SubscriptionResult{Ref: port.SubscriptionRef{ProviderSubscriptionID: "sub_" + in.Metadata["billing_subscription_id"]},
		Outcome: port.PaymentOutcome{Status: port.PaymentPending}}, p.record("CreateSubscription " + key)
}

// confirm scripts ConfirmInitialPayment answers in order (default: paid).
func (p *fakeProvider) ConfirmInitialPayment(_ context.Context, _ port.SubscriptionRef, key string) (port.PaymentOutcome, error) {
	_ = p.record("ConfirmInitialPayment " + key)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.confirm) == 0 {
		return port.PaymentOutcome{Status: port.PaymentPaid}, nil
	}
	out := p.confirm[0]
	p.confirm = p.confirm[1:]
	return out.outcome, out.err
}

func (p *fakeProvider) CancelNow(_ context.Context, ref port.SubscriptionRef, key string) error {
	return p.record("CancelNow " + ref.ProviderSubscriptionID + " " + key)
}

func (p *fakeProvider) FetchCurrent(_ context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	start := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	return domain.ProviderObjectState{Ref: obj, Status: "active", Fields: map[string]any{
		"current_period_start": start, "current_period_end": start.AddDate(0, 1, 0)}}, p.record("FetchCurrent " + obj.ID)
}

func (p *fakeProvider) PreviewPriceChange(_ context.Context, in port.PriceChange) (port.ProrationPreview, error) {
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: 28400, Currency: "USD"}, ProrationInstant: in.ProrationInstant,
		RenewalAmount: domain.Money{AmountMinor: 89500, Currency: "USD"}}, p.record("PreviewPriceChange " + in.ToPriceRef)
}

// change scripts ChangePrice answers (default: paid).
func (p *fakeProvider) ChangePrice(_ context.Context, in port.PriceChange, key string) (port.ChangeResult, error) {
	_ = p.record("ChangePrice " + in.ToPriceRef + " " + key)
	p.mu.Lock()
	defer p.mu.Unlock()
	res := port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}
	if len(p.change) > 0 {
		a := p.change[0]
		p.change = p.change[1:]
		res.Outcome = a.outcome
		return res, a.err
	}
	return res, nil
}

func (p *fakeProvider) ScheduleAtPeriodEnd(_ context.Context, in port.PeriodEndChange, key string) (port.ScheduleRef, error) {
	return port.ScheduleRef{ProviderScheduleID: "sched_" + key}, p.record("ScheduleAtPeriodEnd " + in.ToPriceRef + " " + key)
}

func (p *fakeProvider) ReleaseSchedule(_ context.Context, ref port.ScheduleRef, key string) error {
	return p.record("ReleaseSchedule " + ref.ProviderScheduleID + " " + key)
}

func (p *fakeProvider) PreviewQuantityChange(_ context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: int64(in.To-in.From) * 2300, Currency: "USD"}, ProrationInstant: in.ProrationInstant},
		p.record("PreviewQuantityChange")
}

func (p *fakeProvider) ChangeQuantity(_ context.Context, in port.QuantityChange, key string) (port.ChangeResult, error) {
	_ = p.record(fmt.Sprintf("ChangeQuantity %d->%d invoice_now=%v %s", in.From, in.To, in.InvoiceNow, key))
	p.mu.Lock()
	defer p.mu.Unlock()
	res := port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}
	if len(p.change) > 0 {
		a := p.change[0]
		p.change = p.change[1:]
		res.Outcome = a.outcome
		return res, a.err
	}
	return res, nil
}

func (p *fakeProvider) SetCancelAtPeriodEnd(_ context.Context, ref port.SubscriptionRef, cancel bool, key string) error {
	return p.record(fmt.Sprintf("SetCancelAtPeriodEnd %s %v %s", ref.ProviderSubscriptionID, cancel, key))
}

func (p *fakeProvider) PayOpenInvoiceLink(_ context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	return port.URLWithExpiry{URL: "https://invoice.example/" + ref.ProviderInvoiceID, ExpiresAt: time.Now().Add(time.Hour)}, p.record("PayOpenInvoiceLink " + ref.ProviderInvoiceID)
}

func (p *fakeProvider) PauseCollection(_ context.Context, ref port.SubscriptionRef, pause bool, key string) error {
	return p.record(fmt.Sprintf("PauseCollection %s %v %s", ref.ProviderSubscriptionID, pause, key))
}

func (p *fakeProvider) FinalizeInvoice(_ context.Context, ref port.InvoiceRef, key string) error {
	return p.record("FinalizeInvoice " + ref.ProviderInvoiceID + " " + key)
}

func (p *fakeProvider) VoidInvoice(_ context.Context, ref port.InvoiceRef, key string) error {
	return p.record("VoidInvoice " + ref.ProviderInvoiceID + " " + key)
}

func (p *fakeProvider) ChargeOneOff(_ context.Context, in port.OneOffCharge, key string) (port.ChargeResult, error) {
	_ = p.record(fmt.Sprintf("ChargeOneOff %s x%d %d %s", in.PriceRef, in.Quantity, in.Amount.AmountMinor, key))
	p.mu.Lock()
	defer p.mu.Unlock()
	res := port.ChargeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}
	if len(p.change) > 0 {
		a := p.change[0]
		p.change = p.change[1:]
		res.Outcome = a.outcome
		return res, a.err
	}
	return res, nil
}

func (p *fakeProvider) ListPaymentMethods(context.Context, port.CustomerRef) ([]port.PaymentMethodView, error) {
	return []port.PaymentMethodView{{ProviderRef: "pm_1QAbCdEfGh1234", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2028, IsDefault: true}},
		p.record("ListPaymentMethods")
}

func (p *fakeProvider) CreatePaymentMethodSetup(_ context.Context, in port.SetupInput, key string) (port.HostedSession, error) {
	return port.HostedSession{ProviderSessionID: "cs_setup_" + key, URL: "https://checkout.example/setup", ExpiresAt: in.ExpiresAt}, p.record("CreatePaymentMethodSetup " + key)
}

func (p *fakeProvider) InvoicePDFLink(_ context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	return port.URLWithExpiry{URL: "https://pdf.example/" + ref.ProviderInvoiceID}, p.record("InvoicePDFLink " + ref.ProviderInvoiceID)
}

func (p *fakeProvider) UpdateCustomer(_ context.Context, _ port.CustomerRef, in port.CustomerInput, key string) error {
	return p.record("UpdateCustomer " + in.Email + " " + key)
}

func (p *fakeProvider) CreditBalance(_ context.Context, in port.BalanceAdjustment, key string) (string, error) {
	return "cbtxn_" + key, p.record(fmt.Sprintf("CreditBalance %d %s", in.Amount.AmountMinor, key))
}

// refundStatus is what Refund answers (default pending).
func (p *fakeProvider) Refund(_ context.Context, in port.RefundInput, key string) (port.RefundResult, error) {
	_ = p.record(fmt.Sprintf("Refund %s %d %s %s", in.Invoice.ProviderInvoiceID, in.Amount.AmountMinor, in.Reason, key))
	st := p.refundStatus
	if st == "" {
		st = "pending"
	}
	return port.RefundResult{ProviderRefundID: "re_" + key, Status: st}, nil
}

func (p *fakeProvider) called(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			out = append(out, c)
		}
	}
	return out
}

type confirmAnswer struct {
	outcome port.PaymentOutcome
	err     error
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

type fakeEnt struct {
	limit *int64
	elig  string
	err   error
	calls int
}

func (f *fakeEnt) TokenQuota(context.Context, string) (port.TokenQuota, error) {
	f.calls++
	return port.TokenQuota{MonthlyLimit: f.limit, Eligibility: f.elig}, f.err
}

func ptrTo[T any](v T) *T { return &v }

type env struct {
	ent    *fakeEnt
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
	e := &env{stripe: &fakeProvider{name: "stripe"}, core: &fakeCore{}, rp: &fakeRP{}, ent: &fakeEnt{limit: ptrTo(int64(5_000_000))}, now: time.Now().UTC().Truncate(time.Microsecond)}
	manual := manualadapter.New()
	e.svc = &app.Service{
		Tx: pgadapter.NewTxRunner(appPool), Lock: pgadapter.TenantLocker{},
		Accounts: pgadapter.NewAccountRepository(appPool), Subs: pgadapter.NewSubscriptionRepository(appPool),
		Prices: pgadapter.NewPriceRepository(appPool), Checkouts: pgadapter.NewCheckoutRepository(appPool), Invoices: pgadapter.NewInvoiceRepository(appPool),
		Changes: pgadapter.NewPlanChangeRepository(appPool, systemPool), Previews: pgadapter.NewPreviewRepository(appPool),
		SeatPurchases: pgadapter.NewSeatPurchaseRepository(appPool), Schedules: pgadapter.NewScheduleRepository(appPool, systemPool),
		FollowUps: pgadapter.NewFollowUpRepository(systemPool), Addons: pgadapter.NewAddonRepository(appPool), Money: pgadapter.NewMoneyRepository(appPool), Entitlements: e.ent,
		Sagas: pgadapter.NewSagaRepository(appPool, systemPool), Emissions: pgadapter.NewEmissionRepository(appPool, systemPool),
		Providers: registry{"stripe": e.stripe, "manual": manual}, Core: e.core, RP: e.rp,
		Events: pgadapter.NewEventEnqueuer(eventbus.Source, eventbus.SpecVersion, schemas.Validate), Audit: pgadapter.NewAuditWriter(enq),
		Settings: app.Settings{DefaultCurrency: "USD", NewAccountProvider: "stripe", CheckoutTTL: time.Hour, TenantAppBaseURL: "https://app.example",
			SagaMaxBackoff: 5 * time.Minute, SCAAbandonAfter: 30 * time.Minute, SagaClaim: 45 * time.Second, SeatsAfterConversionDelay: 120 * time.Second, MaxSeatsPerPurchase: 1000, SeatPreviewTTL: 10 * time.Minute, MinChargeMinor: 200, BelowMinimumPolicy: domain.FreeUntilRenewal,
			FeatureSeatPurchase: true, AutoPauseOnOperatorSuspension: true, ReviewThresholdMinor: 100000},
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

func asDomain(err error, out **domain.DomainError) bool { return errors.As(err, out) }

// activeSubscription creates a converted, active Stripe subscription for a
// new tenant through the real flow, and returns tenant and subscription.
func (e *env) activeSubscription(t *testing.T, plan string, seats int) (string, string) {
	t.Helper()
	ctx := context.Background()
	tenant := newTenant()
	res, err := e.svc.AuthorizeAndCharge(ctx, app.Caller{Principal: "bff"}, app.AuthorizeChargeRequest{TenantID: tenant, PlanCode: plan,
		LicensedSeats: seats, PaymentMethodRef: "pm_1"})
	if err != nil {
		t.Fatal(err)
	}
	return tenant, res.BillingSubscriptionID
}

func (e *env) version(t *testing.T, sub string) int64 {
	t.Helper()
	return scalar[int64](t, `SELECT record_version FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func itoa(n int64) string { return fmt.Sprint(n) }

// paidChain is the server's invoice.paid handler chain.
func paidChain(svc *app.Service) port.PaymentEventHandler {
	return app.FirstInvoicePaid{S: svc, Next: app.UpgradeInvoicePaid{S: svc, Next: app.SeatInvoicePaid{S: svc,
		Next: app.AddonInvoicePaid{S: svc, Next: app.DowngradeOnCycle{S: svc, Next: app.CycleInvoicePaid{S: svc}}}}}}
}

// invoiceState is a re-read invoice for subscription sub.
func invoiceState(id, sub, status, reason string, start time.Time, qty int64) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: id}, Status: status,
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": reason, "amount_due_minor": int64(69000),
			"amount_paid_minor": int64(0), "currency": "USD", "line_period_start": start, "line_period_end": start.AddDate(0, 1, 0),
			"line_quantity": qty, "next_payment_attempt": start.Add(72 * time.Hour)}}
}

func callback(id, typ string, kind domain.NormalisedKind) domain.PaymentEvent {
	return domain.PaymentEvent{Provider: "stripe", ProviderEventID: id, EventType: typ, Kind: kind, ProviderCreatedAt: time.Now()}
}

func events(t *testing.T, tenant string) []string {
	t.Helper()
	return scalar[[]string](t, `SELECT coalesce(array_agg(event_type ORDER BY ordering_seq), '{}') FROM outbox_events WHERE tenant_id = $1`, tenant)
}

func errActionRequired() error { return port.ErrActionRequired{ClientSecret: "pi_s"} }

var errNoPM = errors.New("stripe: no default payment method")

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
