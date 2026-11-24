package certify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	stripeadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/stripe"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/webhook"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/bootstrap"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
)

// SuiteVersion is the version this binary implements (§14.7).
const SuiteVersion = "cert-1.0"

// Options configure one run.
type Options struct {
	Target        Target
	ServerDSN     string // superuser DSN of a scratch Postgres server
	StripeKey     string // test-mode key (sk_test_ / rk_test_), any for stripe-mock
	StripeBaseURL string // empty → https://api.stripe.com
	RunID         string
	Log           port.Logger
}

// Setup builds the run's environment: throwaway database, price book on the
// provider, the adapter (on a Test Clock in test mode), Billing's service,
// B-W1 and the processor. The returned func tears everything down.
func Setup(ctx context.Context, o Options) (*Env, func(), error) {
	if o.Target == TargetTestMode && !strings.HasPrefix(o.StripeKey, "sk_test_") && !strings.HasPrefix(o.StripeKey, "rk_test_") {
		return nil, nil, errors.New("certification runs only in the provider's test mode: the key must be sk_test_ or rk_test_ (BIL-OPS-7)")
	}
	base := o.StripeBaseURL
	if base == "" {
		base = "https://api.stripe.com"
	}
	api := StripeAPI{Key: o.StripeKey, BaseURL: base}
	db, err := OpenDB(ctx, o.ServerDSN, o.RunID)
	if err != nil {
		return nil, nil, err
	}
	cleanups := []func(){func() { _ = db.Drop(context.WithoutCancel(ctx)) }}
	teardown := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*Env, func(), error) { teardown(); return nil, nil, err }

	start := time.Now().UTC().Truncate(time.Second)
	var clock Clock = NewFakeClock(start)
	clockID := ""
	if o.Target == TargetTestMode {
		tc, err := NewTestClock(ctx, api, "certify-"+o.RunID, start)
		if err != nil {
			return fail(err)
		}
		clock, clockID = tc, tc.ID
		cleanups = append(cleanups, func() { _ = tc.Delete(context.WithoutCancel(ctx)) })
	}
	secret := "whsec_certify_" + randomHex(16)
	cfg := stripeadapter.Config{APIKey: o.StripeKey, WebhookSecrets: []string{secret}, Livemode: false,
		Timeout: 30 * time.Second, MaxRPS: 20, TestClock: clockID}
	if o.Target == TargetStripeMock {
		cfg.BaseURL = base
	}
	prov, err := stripeadapter.New(cfg)
	if err != nil {
		return fail(err)
	}

	var under port.PaymentProvider = prov
	if o.Target == TargetStripeMock {
		under = newMockState(prov, clock.Now)
	}
	env := &Env{Target: o.Target, Provider: under, DB: db, Core: &CoreStub{}, Clock: clock, RunStart: start,
		Prices: map[domain.PlanCode]string{}, Log: o.Log}
	if err := seedPriceBook(ctx, env, api); err != nil {
		return fail(fmt.Errorf("price book: %w", err))
	}
	svc, err := service(db, under, env)
	if err != nil {
		return fail(err)
	}
	env.Svc = svc
	reg := Registry{P: under}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	(&webhook.Handler{Providers: reg, Ledger: pgadapter.NewLedgerRepository(db.System), Customers: pgadapter.NewSystemRepository(db.System),
		MaxBodyBytes: 1 << 20, Log: o.Log}).Register(router)
	env.Deliver = &Deliverer{API: api, Secret: secret, Handler: router, Path: "/api/v1/webhooks/" + prov.Name(),
		Processor: &app.WebhookProcessor{Ledger: pgadapter.NewLedgerRepository(db.System), Providers: reg,
			Tx: pgadapter.NewTxRunner(db.App), Handlers: bootstrap.PaymentHandlers(svc), MaxAttempts: 10, Log: o.Log},
		Provider: under, Now: time.Now, Since: start.Add(-time.Minute)}
	return env, teardown, nil
}

// seedPriceBook creates the run's prices on the provider (one product per
// plan, monthly, at the seed amounts) and a one-off token pack, and records
// them as stripe rows in the throwaway price book.
func seedPriceBook(ctx context.Context, e *Env, api StripeAPI) error {
	type row struct {
		Plan   string
		Amount int64
	}
	var rows []row
	for _, plan := range []domain.PlanCode{domain.PlanStarter, domain.PlanPro, domain.PlanEnterprise} {
		amt, err := Scalar[int64](ctx, e.DB, `SELECT unit_amount_minor FROM plan_prices WHERE provider = 'manual' AND plan = $1
			AND interval = 'month' AND currency = 'USD' AND effective_to IS NULL`, string(plan))
		if err != nil {
			return fmt.Errorf("seed price %s: %w", plan, err)
		}
		rows = append(rows, row{string(plan), amt})
	}
	for _, r := range rows {
		id, err := api.CreatePrice(ctx, "certify "+r.Plan, r.Amount, "USD", "month")
		if err != nil {
			return err
		}
		e.Prices[domain.PlanCode(r.Plan)] = id
		if err := e.DB.Exec(ctx, `INSERT INTO plan_prices (plan, interval, currency, unit_amount_minor, provider, provider_price_id, effective_from, created_by, reason)
			VALUES ($1, 'month', 'USD', $2, 'stripe', $3, now() - interval '1 day', $4, 'certification run price book')`,
			r.Plan, r.Amount, id, domain.CommercialSystemPrincipal); err != nil {
			return err
		}
	}
	pack, err := api.CreatePrice(ctx, "certify tokens", 4900, "USD", "")
	if err != nil {
		return err
	}
	e.TokenPack = "certify_1m"
	return e.DB.Exec(ctx, `INSERT INTO token_packs (pack_code, tokens, currency, amount_minor, provider, provider_price_id, effective_from, created_by, reason)
		VALUES ($1, 1000000, 'USD', 4900, 'stripe', $2, now() - interval '1 day', $3, 'certification run token pack')`,
		e.TokenPack, pack, domain.CommercialSystemPrincipal)
}

// service builds Billing's use cases on the throwaway database with the
// adapter under certification and the IAM / Entitlements stubs.
func service(db *DB, prov port.PaymentProvider, e *Env) (*app.Service, error) {
	events, err := bootstrap.Events()
	if err != nil {
		return nil, err
	}
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: "commercial-billing", KeyNamespace: audit.KeyNamespace("commercial-billing")})
	if err != nil {
		return nil, err
	}
	cfg := config.Config{DefaultCurrency: "USD", CheckoutTTL: time.Hour, TenantAppBaseURL: "https://app.certify.invalid",
		SagaMaxBackoff: 5 * time.Minute, RPTimeout: 30 * time.Second, SeatsAfterConversionDelay: 120 * time.Second, SCAAbandonAfter: 30 * time.Minute,
		MaxSeatsPerPurchase: 1000, SeatPreviewTTL: 10 * time.Minute, MinChargeMinor: 50, ProrationBelowMinimum: string(domain.FreeUntilRenewal),
		FeatureDirectSignup: true, FeatureTokenAddons: true, FeatureSeatPurchase: true, AutoPauseOnOperatorSuspension: true,
		ReviewThresholdMinor: 100000, SagaAlertAfter: 30 * time.Minute, SeatRenewalLead: 24 * time.Hour, DowngradeRecheckLead: 2 * time.Hour,
		PurchaseAbandonAfter: 30 * time.Minute, SeatResyncAfter: 10 * time.Minute, RepriceCutoff: time.Hour,
		PIIMinimiseOffboardedDays: 90, PIIMinimiseAbandonedDays: 30, ProviderCertSuiteVersion: SuiteVersion}
	svc := bootstrap.NewService(cfg, nil, db.App, db.System, events, pgadapter.NewAuditWriter(enq), nil, e.Log)
	svc.Providers = Registry{P: prov}
	svc.Core, svc.RP, svc.Entitlements = e.Core, RPStub{}, EntitlementsStub{}
	svc.Now = e.Clock.Now
	return svc, nil
}

// ── Running ──────────────────────────────────────────────────────────────

// Case is one suite case.
type Case struct {
	Capability string
	ID         string
	Required   bool
	Run        func(ctx context.Context, e *Env) (Evidence, error)
}

// Evidence is what a case captured: provider ids, event ids, amounts. Test
// mode only; never secrets or card data.
type Evidence map[string]any

// Result is one case's outcome.
type Result struct {
	Capability string
	Case       string
	Passed     bool
	Error      string
	Evidence   Evidence
}

// Run runs the cases in order; a failing case never stops the run.
func Run(ctx context.Context, e *Env, cases []Case) []Result {
	out := make([]Result, 0, len(cases))
	for _, c := range cases {
		r := Result{Capability: c.Capability, Case: c.ID}
		ev, err := runCase(ctx, e, c)
		r.Evidence, r.Passed = ev, err == nil
		if err != nil {
			r.Error = err.Error()
		}
		if r.Evidence == nil {
			r.Evidence = Evidence{}
		}
		r.Evidence["target"] = string(e.Target)
		if m, ok := e.Provider.(*mockState); ok {
			if notes := m.TakeNotes(); len(notes) > 0 {
				r.Evidence["mock_substitutions"] = notes
			}
		}
		if e.Log != nil {
			e.Log.Info("certification case", map[string]any{"capability": c.Capability, "case": c.ID, "passed": r.Passed, "error": r.Error})
		}
		out = append(out, r)
	}
	return out
}

func runCase(ctx context.Context, e *Env, c Case) (ev Evidence, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return c.Run(ctx, e)
}

// Record is the §19.6 file (B-O14's body without the reason).
type Record = app.CertificationRequest

// WriteEvidence writes each result's evidence to dir/<provider>/<adapter
// version>/<capability>-<case>.json and returns the record with
// evidence_ref = baseURI + the same relative path.
func WriteEvidence(e *Env, results []Result, dir, baseURI string, started, finished time.Time, accountRef string) (Record, error) {
	caps := e.Provider.Capabilities()
	capJSON, err := json.Marshal(caps)
	if err != nil {
		return Record{}, err
	}
	rec := Record{Provider: e.Provider.Name(), AdapterVersion: e.Provider.AdapterVersion(), ProviderAPIVersion: providerAPIVersion(e.Provider.AdapterVersion()),
		SuiteVersion: SuiteVersion, ProrationMode: string(caps.Proration), Capabilities: capJSON, ProviderAccountRef: accountRef,
		RunStartedAt: started, RunFinishedAt: finished}
	rel := filepath.Join(rec.Provider, safe(rec.AdapterVersion))
	if err := os.MkdirAll(filepath.Join(dir, rel), 0o750); err != nil {
		return Record{}, err
	}
	for _, r := range results {
		name := r.Capability + "-" + r.Case + ".json"
		body, _ := json.MarshalIndent(map[string]any{"capability": r.Capability, "case": r.Case, "passed": r.Passed,
			"error": r.Error, "evidence": r.Evidence}, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, rel, name), body, 0o600); err != nil {
			return Record{}, err
		}
		rec.Results = append(rec.Results, app.CertificationResult{Capability: r.Capability, Case: r.Case, Passed: r.Passed,
			EvidenceRef: strings.TrimRight(baseURI, "/") + "/" + filepath.ToSlash(filepath.Join(rel, name))})
	}
	sort.SliceStable(rec.Results, func(i, j int) bool { return rec.Results[i].Capability < rec.Results[j].Capability })
	return rec, nil
}

// Missing lists required cases not passed (CERTIFICATION_INCOMPLETE), with
// the proration rule of BIL-PROV-3.
func Missing(rec Record) []string {
	results := make([]domain.CertResult, 0, len(rec.Results))
	for _, r := range rec.Results {
		results = append(results, domain.CertResult{Capability: r.Capability, Case: r.Case, Passed: r.Passed})
	}
	missing, prorationOK, known := domain.CertOutcome(rec.SuiteVersion, rec.ProrationMode, rec.ProrationMode, results)
	if !known {
		return []string{"unknown suite " + rec.SuiteVersion}
	}
	if !prorationOK {
		missing = append(missing, "proration/"+prorationCase(rec.ProrationMode))
	}
	return missing
}

func prorationCase(mode string) string {
	if mode == string(port.ProrationFallback) {
		return "calculator_charged"
	}
	return "preview_equals_charge"
}

func providerAPIVersion(adapterVersion string) string {
	if i := strings.Index(adapterVersion, "+"); i >= 0 {
		return strings.TrimPrefix(adapterVersion[i+1:], "stripeapi.")
	}
	return adapterVersion
}

func safe(s string) string { return strings.NewReplacer("/", "_", " ", "_").Replace(s) }

// StripeAdapterVersion is the Stripe adapter version compiled into this
// build (AdapterVersion(), the key of the certification record).
func StripeAdapterVersion() string { return stripeadapter.AdapterVersion }
