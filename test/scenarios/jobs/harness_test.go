//go:build integration

// Package jobs_test drives the §13.2 CronJobs, the scheduled-emission
// worker (BIL-EVT-6), the webhook processor (§8.15) and the operator price
// book and certification use cases (B-O11 – B-O17) through their edge and
// failure paths, on Postgres 17 with the production roles (test/testkit).
//
// Failure paths are reached by wrapping the real repositories and the fake
// provider with an injector that fails a named call after a given number of
// successful ones; everything else runs on the real use cases. Each test
// starts from an empty tenant data set (the jobs select across tenants).
package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
)

var db *testkit.DB

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	var stop func()
	var err error
	db, stop, err = testkit.Start(context.Background(), "../../..")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	return m.Run()
}

// ── fault injection ─────────────────────────────────────────────────────

var errInjected = errors.New("injected failure")

type rule struct {
	skip, times int // pass skip calls, then fail times calls (0: every call)
	err         error
}

// faults fails named calls ("Schedules.Get") on demand.
type faults struct {
	mu    sync.Mutex
	rules map[string]*rule
	calls map[string]int
}

func newFaults() *faults { return &faults{rules: map[string]*rule{}, calls: map[string]int{}} }

// fail makes name fail every call after skip successful ones.
func (f *faults) fail(name string, skip int) { f.failN(name, skip, 0, errInjected) }

// failN makes name fail times calls (0: all) with err after skip successful ones.
func (f *faults) failN(name string, skip, times int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[name] = &rule{skip: skip, times: times, err: err}
}

func (f *faults) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = map[string]*rule{}
	f.calls = map[string]int{}
}

func (f *faults) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *faults) hit(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	r, ok := f.rules[name]
	if !ok {
		return nil
	}
	if r.skip > 0 {
		r.skip--
		return nil
	}
	if r.times > 0 {
		r.times--
		if r.times == 0 {
			delete(f.rules, name)
		}
	}
	return r.err
}

// ── repository wrappers ─────────────────────────────────────────────────

type schedules struct {
	port.ScheduleRepo
	f *faults
}

func (r schedules) Get(ctx context.Context, id string) (domain.SeatChangeSchedule, bool, error) {
	if err := r.f.hit("Schedules.Get"); err != nil {
		return domain.SeatChangeSchedule{}, false, err
	}
	return r.ScheduleRepo.Get(ctx, id)
}

func (r schedules) Update(ctx context.Context, s domain.SeatChangeSchedule) error {
	if err := r.f.hit("Schedules.Update"); err != nil {
		return err
	}
	return r.ScheduleRepo.Update(ctx, s)
}

func (r schedules) Due(ctx context.Context, until time.Time, limit int) ([]domain.SeatChangeSchedule, error) {
	if err := r.f.hit("Schedules.Due"); err != nil {
		return nil, err
	}
	return r.ScheduleRepo.Due(ctx, until, limit)
}

func (r schedules) Open(ctx context.Context, id string) (domain.SeatChangeSchedule, bool, error) {
	if err := r.f.hit("Schedules.Open"); err != nil {
		return domain.SeatChangeSchedule{}, false, err
	}
	return r.ScheduleRepo.Open(ctx, id)
}

type subs struct {
	port.SubscriptionRepo
	f *faults
}

func (r subs) Get(ctx context.Context, id string) (domain.Subscription, error) {
	if err := r.f.hit("Subs.Get"); err != nil {
		return domain.Subscription{}, err
	}
	return r.SubscriptionRepo.Get(ctx, id)
}

func (r subs) Item(ctx context.Context, id string) (domain.SubscriptionItem, error) {
	if err := r.f.hit("Subs.Item"); err != nil {
		return domain.SubscriptionItem{}, err
	}
	return r.SubscriptionRepo.Item(ctx, id)
}

func (r subs) SetItem(ctx context.Context, it domain.SubscriptionItem) error {
	if err := r.f.hit("Subs.SetItem"); err != nil {
		return err
	}
	return r.SubscriptionRepo.SetItem(ctx, it)
}

func (r subs) AppendHistory(ctx context.Context, h domain.StatusHistoryEntry) error {
	if err := r.f.hit("Subs.AppendHistory"); err != nil {
		return err
	}
	return r.SubscriptionRepo.AppendHistory(ctx, h)
}

type prices struct {
	port.PriceRepo
	f *faults
}

func (r prices) Get(ctx context.Context, id string) (domain.PlanPrice, error) {
	if err := r.f.hit("Prices.Get"); err != nil {
		return domain.PlanPrice{}, err
	}
	return r.PriceRepo.Get(ctx, id)
}

func (r prices) PriceAt(ctx context.Context, plan domain.PlanCode, interval domain.BillingInterval, currency, provider string, at time.Time) (domain.PlanPrice, bool, error) {
	if err := r.f.hit("Prices.PriceAt"); err != nil {
		return domain.PlanPrice{}, false, err
	}
	return r.PriceRepo.PriceAt(ctx, plan, interval, currency, provider, at)
}

type changes struct {
	port.PlanChangeRepo
	f *faults
}

func (r changes) Open(ctx context.Context, tenantID string) (domain.PlanChangeRequest, bool, error) {
	if err := r.f.hit("Changes.Open"); err != nil {
		return domain.PlanChangeRequest{}, false, err
	}
	return r.PlanChangeRepo.Open(ctx, tenantID)
}

func (r changes) Update(ctx context.Context, c domain.PlanChangeRequest) error {
	if err := r.f.hit("Changes.Update"); err != nil {
		return err
	}
	return r.PlanChangeRepo.Update(ctx, c)
}

func (r changes) DueDowngrades(ctx context.Context, until time.Time, limit int) ([]domain.PlanChangeRequest, error) {
	if err := r.f.hit("Changes.DueDowngrades"); err != nil {
		return nil, err
	}
	return r.PlanChangeRepo.DueDowngrades(ctx, until, limit)
}

type seatPurchases struct {
	port.SeatPurchaseRepo
	f *faults
}

func (r seatPurchases) Get(ctx context.Context, id string) (domain.SeatPurchase, bool, error) {
	if err := r.f.hit("SeatPurchases.Get"); err != nil {
		return domain.SeatPurchase{}, false, err
	}
	return r.SeatPurchaseRepo.Get(ctx, id)
}

func (r seatPurchases) Open(ctx context.Context, id string) (domain.SeatPurchase, bool, error) {
	if err := r.f.hit("SeatPurchases.Open"); err != nil {
		return domain.SeatPurchase{}, false, err
	}
	return r.SeatPurchaseRepo.Open(ctx, id)
}

func (r seatPurchases) Update(ctx context.Context, p domain.SeatPurchase) error {
	if err := r.f.hit("SeatPurchases.Update"); err != nil {
		return err
	}
	return r.SeatPurchaseRepo.Update(ctx, p)
}

func (r seatPurchases) Insert(ctx context.Context, p domain.SeatPurchase) error {
	if err := r.f.hit("SeatPurchases.Insert"); err != nil {
		return err
	}
	return r.SeatPurchaseRepo.Insert(ctx, p)
}

type addons struct {
	port.AddonRepo
	f *faults
}

func (r addons) Get(ctx context.Context, id string) (domain.QuotaAddonPurchase, bool, error) {
	if err := r.f.hit("Addons.Get"); err != nil {
		return domain.QuotaAddonPurchase{}, false, err
	}
	return r.AddonRepo.Get(ctx, id)
}

func (r addons) Update(ctx context.Context, p domain.QuotaAddonPurchase) error {
	if err := r.f.hit("Addons.Update"); err != nil {
		return err
	}
	return r.AddonRepo.Update(ctx, p)
}

type accounts struct {
	port.AccountRepo
	f *faults
}

func (r accounts) Get(ctx context.Context, tenantID string) (domain.BillingAccount, error) {
	if err := r.f.hit("Accounts.Get"); err != nil {
		return domain.BillingAccount{}, err
	}
	return r.AccountRepo.Get(ctx, tenantID)
}

type checkouts struct {
	port.CheckoutRepo
	f *faults
}

func (r checkouts) GetSession(ctx context.Context, id string) (domain.CheckoutSession, bool, error) {
	if err := r.f.hit("Checkouts.GetSession"); err != nil {
		return domain.CheckoutSession{}, false, err
	}
	return r.CheckoutRepo.GetSession(ctx, id)
}

func (r checkouts) SetStatus(ctx context.Context, id, status string) error {
	if err := r.f.hit("Checkouts.SetStatus"); err != nil {
		return err
	}
	return r.CheckoutRepo.SetStatus(ctx, id, status)
}

type sagas struct {
	port.SagaRepo
	f *faults
}

func (r sagas) Due(ctx context.Context, now time.Time, limit int) ([]port.SagaRef, error) {
	if err := r.f.hit("Sagas.Due"); err != nil {
		return nil, err
	}
	return r.SagaRepo.Due(ctx, now, limit)
}

// jobs also lets a test substitute the first LiveSubscriptions page, to
// exercise paging past jobBatch (200) without 200 tenants.
type jobs struct {
	port.JobRepo
	f         *faults
	mu        sync.Mutex
	firstPage []port.LiveSubscription
	afters    []string
}

func (r *jobs) OverdueSagas(ctx context.Context, before time.Time) ([]port.SagaRef, int, error) {
	if err := r.f.hit("Jobs.OverdueSagas"); err != nil {
		return nil, 0, err
	}
	return r.JobRepo.OverdueSagas(ctx, before)
}

func (r *jobs) StaleWork(ctx context.Context, now, abandonBefore, scaBefore time.Time, limit int) ([]port.StaleWork, error) {
	if err := r.f.hit("Jobs.StaleWork"); err != nil {
		return nil, err
	}
	return r.JobRepo.StaleWork(ctx, now, abandonBefore, scaBefore, limit)
}

func (r *jobs) LiveSubscriptions(ctx context.Context, after string, limit int) ([]port.LiveSubscription, error) {
	if err := r.f.hit("Jobs.LiveSubscriptions"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.afters = append(r.afters, after)
	page := r.firstPage
	r.firstPage = nil
	r.mu.Unlock()
	if page != nil {
		return page, nil
	}
	return r.JobRepo.LiveSubscriptions(ctx, after, limit)
}

func (r *jobs) RecentResyncs(ctx context.Context, tenantID string, since time.Time) (int, error) {
	if err := r.f.hit("Jobs.RecentResyncs"); err != nil {
		return 0, err
	}
	return r.JobRepo.RecentResyncs(ctx, tenantID, since)
}

func (r *jobs) PIICandidates(ctx context.Context, off, abandoned time.Time, limit int) ([]port.PIICandidate, error) {
	if err := r.f.hit("Jobs.PIICandidates"); err != nil {
		return nil, err
	}
	return r.JobRepo.PIICandidates(ctx, off, abandoned, limit)
}

func (r *jobs) ExpiredIdempotencyTenants(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if err := r.f.hit("Jobs.ExpiredIdempotencyTenants"); err != nil {
		return nil, err
	}
	return r.JobRepo.ExpiredIdempotencyTenants(ctx, now, limit)
}

func (r *jobs) PruneIdempotency(ctx context.Context, now time.Time) (int64, error) {
	if err := r.f.hit("Jobs.PruneIdempotency"); err != nil {
		return 0, err
	}
	return r.JobRepo.PruneIdempotency(ctx, now)
}

type admin struct {
	port.AdminRepo
	f *faults
}

func (r admin) AddTokenPack(ctx context.Context, p domain.TokenPack, by, reason string) error {
	if err := r.f.hit("Admin.AddTokenPack"); err != nil {
		return err
	}
	return r.AdminRepo.AddTokenPack(ctx, p, by, reason)
}

func (r admin) Certification(ctx context.Context, id string) (domain.Certification, bool, error) {
	if err := r.f.hit("Admin.Certification"); err != nil {
		return domain.Certification{}, false, err
	}
	return r.AdminRepo.Certification(ctx, id)
}

func (r admin) ReviewCertification(ctx context.Context, id string, st domain.CertificationStatus, by, reason string, at time.Time) (domain.Certification, error) {
	if err := r.f.hit("Admin.ReviewCertification"); err != nil {
		return domain.Certification{}, err
	}
	return r.AdminRepo.ReviewCertification(ctx, id, st, by, reason, at)
}

func (r admin) RevokeCertification(ctx context.Context, id, by, reason string, at time.Time) (domain.Certification, error) {
	if err := r.f.hit("Admin.RevokeCertification"); err != nil {
		return domain.Certification{}, err
	}
	return r.AdminRepo.RevokeCertification(ctx, id, by, reason, at)
}

// ── fake provider ───────────────────────────────────────────────────────

// provider is testkit's recording provider plus the schedule, invoice,
// price and one-off calls the jobs make; every call can be failed through
// faults ("prov.<Method>").
type provider struct {
	*testkit.Provider
	f             *faults
	mu            sync.Mutex
	calls         []string
	invoiceStatus string                // FetchCurrent of an invoice (default "open")
	invoicePaid   map[string]int64      // amount paid by invoice id; default: what its purchase charged (in_<provider key>)
	expireErr     error                 // ExpireCheckout's answer (default nil: expired)
	change        []port.PaymentOutcome // ChangePrice / ChangeQuantity / ChargeOneOff outcomes (default paid)
}

func (p *provider) rec(c string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c)
}

// called returns the recorded calls that start with prefix.
func (p *provider) called(prefix string) []string {
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

func (p *provider) next() port.PaymentOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.change) == 0 {
		return port.PaymentOutcome{Status: port.PaymentPaid}
	}
	o := p.change[0]
	p.change = p.change[1:]
	return o
}

func (p *provider) FetchCurrent(ctx context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	p.rec("FetchCurrent " + obj.ID)
	if err := p.f.hit("prov.FetchCurrent"); err != nil {
		return domain.ProviderObjectState{}, err
	}
	if obj.Kind == domain.ObjectInvoice {
		st := p.invoiceStatus
		if st == "" {
			st = "open"
		}
		return domain.ProviderObjectState{Ref: obj, Status: st, Fields: map[string]any{"currency": "USD", "amount_paid_minor": p.paidAmount(ctx, obj.ID)}}, nil
	}
	return p.Provider.FetchCurrent(ctx, obj)
}

// paidAmount is what invoice id paid: invoicePaid[id], else the amount of
// the purchase or plan change its id names ("in_seatpur:<id>",
// "in_addon:<id>", "in_planchg:<id>"), else 0.
func (p *provider) paidAmount(ctx context.Context, id string) int64 {
	p.mu.Lock()
	v, ok := p.invoicePaid[id]
	p.mu.Unlock()
	if ok {
		return v
	}
	for prefix, q := range map[string]string{
		"in_seatpur:": `SELECT amount_minor FROM seat_purchases WHERE purchase_id = $1::uuid`,
		"in_addon:":   `SELECT amount_minor FROM quota_addon_purchases WHERE purchase_id = $1::uuid`,
		"in_planchg:": `SELECT amount_minor FROM plan_change_requests WHERE change_id = $1::uuid`,
	} {
		if rest, found := strings.CutPrefix(id, prefix); found {
			v, _ := testkit.Scalar[int64](ctx, db, q, strings.SplitN(rest, ":", 2)[0])
			return v
		}
	}
	return 0
}

func (p *provider) ScheduleAtPeriodEnd(_ context.Context, in port.PeriodEndChange, key string) (port.ScheduleRef, error) {
	p.rec(fmt.Sprintf("ScheduleAtPeriodEnd %s x%d %s", in.ToPriceRef, in.ToQuantity, key))
	return port.ScheduleRef{ProviderScheduleID: "sched_" + key}, p.f.hit("prov.ScheduleAtPeriodEnd")
}

func (p *provider) ReleaseSchedule(_ context.Context, ref port.ScheduleRef, key string) error {
	p.rec("ReleaseSchedule " + ref.ProviderScheduleID + " " + key)
	return p.f.hit("prov.ReleaseSchedule")
}

func (p *provider) VoidInvoice(_ context.Context, ref port.InvoiceRef, key string) error {
	p.rec("VoidInvoice " + ref.ProviderInvoiceID + " " + key)
	return p.f.hit("prov.VoidInvoice")
}

func (p *provider) CancelNow(ctx context.Context, ref port.SubscriptionRef, key string) error {
	p.rec("CancelNow " + ref.ProviderSubscriptionID + " " + key)
	if err := p.f.hit("prov.CancelNow"); err != nil {
		return err
	}
	return p.Provider.CancelNow(ctx, ref, key)
}

func (p *provider) UpdateCustomer(ctx context.Context, ref port.CustomerRef, in port.CustomerInput, key string) error {
	p.rec("UpdateCustomer " + key)
	if err := p.f.hit("prov.UpdateCustomer"); err != nil {
		return err
	}
	return p.Provider.UpdateCustomer(ctx, ref, in, key)
}

func (p *provider) VerifyPrice(_ context.Context, ref string, _ port.PriceExpectation) error {
	p.rec("VerifyPrice " + ref)
	if ref == "price_bad" {
		return fmt.Errorf("%w: amount differs", port.ErrPriceMismatch)
	}
	return nil
}

func (p *provider) PreviewPriceChange(_ context.Context, in port.PriceChange) (port.ProrationPreview, error) {
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: 28400, Currency: "USD"}, ProrationInstant: in.ProrationInstant,
		RenewalAmount: domain.Money{AmountMinor: 89500, Currency: "USD"}}, nil
}

func (p *provider) ChangePrice(_ context.Context, in port.PriceChange, key string) (port.ChangeResult, error) {
	p.rec("ChangePrice " + in.ToPriceRef + " " + key)
	return port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: p.next()}, nil
}

func (p *provider) ChangeQuantity(_ context.Context, in port.QuantityChange, key string) (port.ChangeResult, error) {
	p.rec(fmt.Sprintf("ChangeQuantity %d->%d %s", in.From, in.To, key))
	res := port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: p.next()}
	return res, p.f.hit("prov.ChangeQuantity")
}

func (p *provider) ChargeOneOff(_ context.Context, in port.OneOffCharge, key string) (port.ChargeResult, error) {
	p.rec(fmt.Sprintf("ChargeOneOff %s %s", in.PriceRef, key))
	return port.ChargeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: p.next()}, nil
}

func (p *provider) CreateHostedCheckout(_ context.Context, in port.CheckoutInput, key string) (port.HostedSession, error) {
	p.rec("CreateHostedCheckout " + key)
	return port.HostedSession{ProviderSessionID: "cs_" + in.Metadata["billing_subscription_id"], URL: "https://checkout.example/x", ExpiresAt: in.ExpiresAt}, nil
}

func (p *provider) ExpireCheckout(_ context.Context, id, key string) error {
	p.rec("ExpireCheckout " + id + " " + key)
	if err := p.f.hit("prov.ExpireCheckout"); err != nil {
		return err
	}
	return p.expireErr
}

func (p *provider) DeleteDraftInvoice(_ context.Context, ref port.InvoiceRef) error {
	p.rec("DeleteDraftInvoice " + ref.ProviderInvoiceID)
	return p.f.hit("prov.DeleteDraftInvoice")
}

func (p *provider) CreatePaymentMethodSetup(_ context.Context, in port.SetupInput, key string) (port.HostedSession, error) {
	p.rec("CreatePaymentMethodSetup " + key)
	return port.HostedSession{ProviderSessionID: "cs_setup_" + key, URL: "https://checkout.example/setup", ExpiresAt: in.ExpiresAt}, nil
}

// ── logger and metrics ──────────────────────────────────────────────────

// logger records every line as "LEVEL msg".
type logger struct {
	mu    sync.Mutex
	lines []string
}

func (l *logger) add(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
}

func (l *logger) Debug(msg string, _ map[string]any) { l.add("DEBUG", msg) }
func (l *logger) Info(msg string, _ map[string]any)  { l.add("INFO", msg) }
func (l *logger) Warn(msg string, _ map[string]any)  { l.add("WARN", msg) }
func (l *logger) Error(msg string, _ map[string]any) { l.add("ERROR", msg) }

// has reports whether a line starts with prefix ("ERROR BillingConversion…").
func (l *logger) has(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

type metrics struct {
	port.NopMetrics
	mu     sync.Mutex
	counts map[string]int
}

func (m *metrics) inc(k string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = map[string]int{}
	}
	m.counts[k]++
}

func (m *metrics) get(k string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[k]
}

func (m *metrics) SeatChange(d, o string)      { m.inc("seat_change:" + d + ":" + o) }
func (m *metrics) DowngradeBlocked(b string)   { m.inc("downgrade_blocked:" + b) }
func (m *metrics) RepricingScheduled(d string) { m.inc("repricing:" + d) }
func (m *metrics) SeatProjectionMismatch()     { m.inc("seat_projection_mismatch") }
func (m *metrics) SeatProjectionRun(found, repeats int) {
	m.inc(fmt.Sprintf("seat_projection_run:%d:%d", found, repeats))
}
func (m *metrics) SagasOverdue(n int)                        { m.inc(fmt.Sprintf("sagas_overdue:%d", n)) }
func (m *metrics) PurchaseToEvent(k string, _ time.Duration) { m.inc("purchase_to_event:" + k) }
func (m *metrics) WebhookProcessed(_, kind, status string, _ time.Duration) {
	m.inc("webhook:" + kind + ":" + status)
}

// ── environment ─────────────────────────────────────────────────────────

type env struct {
	t     *testing.T
	svc   *app.Service
	fakes *testkit.Fakes
	prov  *provider
	f     *faults
	jobs  *jobs
	log   *logger
	met   *metrics
	now   time.Time
}

// newEnv empties the tenant tables and builds the real Service with
// fault-injecting wrappers, a recording logger and metrics, and a clock.
func newEnv(t *testing.T) *env {
	t.Helper()
	reset(t)
	fakes := testkit.NewFakes()
	svc, err := testkit.NewService(db, fakes)
	if err != nil {
		t.Fatal(err)
	}
	f := newFaults()
	e := &env{t: t, svc: svc, fakes: fakes, f: f, log: &logger{}, met: &metrics{}, now: time.Now().UTC().Truncate(time.Microsecond)}
	e.prov = &provider{Provider: fakes.Stripe, f: f}
	e.jobs = &jobs{JobRepo: svc.Jobs, f: f}
	reg := fakes.Registry()
	reg["stripe"] = e.prov
	svc.Providers = reg
	svc.Schedules = schedules{svc.Schedules, f}
	svc.Subs = subs{svc.Subs, f}
	svc.Prices = prices{svc.Prices, f}
	svc.Changes = changes{svc.Changes, f}
	svc.SeatPurchases = seatPurchases{svc.SeatPurchases, f}
	svc.Addons = addons{svc.Addons, f}
	svc.Accounts = accounts{svc.Accounts, f}
	svc.Checkouts = checkouts{svc.Checkouts, f}
	svc.Sagas = sagas{svc.Sagas, f}
	svc.Admin = admin{svc.Admin, f}
	svc.Jobs = e.jobs
	svc.Metrics = e.met
	svc.Log = e.log
	svc.Now = func() time.Time { return e.now }
	return e
}

// unload removes the stripe adapter from the registry (a provider that is
// no longer loaded, e.g. after a config change).
func (e *env) unload() { e.svc.Providers = testkit.Registry{"manual": e.fakes.Registry()["manual"]} }

func reset(t *testing.T) {
	t.Helper()
	if err := db.Exec(context.Background(), `TRUNCATE billing_accounts, subscriptions, subscription_items, subscription_status_history,
		conversion_sagas, checkout_sessions, plan_change_requests, seat_purchases, seat_change_schedules, quota_addon_purchases,
		invoices, invoice_links, adjustments, refunds, disputes, idempotency_records, payment_event_ledger, scheduled_emissions,
		usage_period_records, outbox_events CASCADE`); err != nil {
		t.Fatal(err)
	}
	// Price rows a test added (reason "test …"); testkit's seed rows stay.
	if err := db.Exec(context.Background(), `DELETE FROM plan_prices WHERE reason LIKE 'test %'`); err != nil {
		t.Fatal(err)
	}
}

var (
	owner    = app.Caller{UserID: "4b1e8a52-1111-4222-8333-944455556666", Roles: []string{domain.RoleTenantOwner}}
	operator = app.Caller{UserID: "9a9a9a9a-1111-4222-8333-944455556666", Roles: []string{domain.RolePlatformOperator}}
)

func withKey(c app.Caller, key string) app.Caller { c.IdempotencyKey = key; return c }

func newTenant() string { return uuid.NewString() }

// active creates an active Stripe subscription through B-I1.
func (e *env) active(plan string, seats int) (tenant, sub string) {
	e.t.Helper()
	tenant = newTenant()
	res, err := e.svc.AuthorizeAndCharge(context.Background(), app.Caller{Principal: "bff"}, app.AuthorizeChargeRequest{TenantID: tenant,
		PlanCode: plan, LicensedSeats: seats, PaymentMethodRef: "pm_1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return tenant, res.BillingSubscriptionID
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if err := db.Exec(context.Background(), q, args...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func scalar[T any](t *testing.T, q string, args ...any) T {
	t.Helper()
	v, err := testkit.Scalar[T](context.Background(), db, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func (e *env) periodEnd(sub string) time.Time {
	e.t.Helper()
	return scalar[time.Time](e.t, `SELECT current_period_end FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func (e *env) version(sub string) int64 {
	e.t.Helper()
	return scalar[int64](e.t, `SELECT record_version FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

// run runs a job and fails the test on a job-level error.
func (e *env) run(job func(context.Context) (app.JobResult, error)) app.JobResult {
	e.t.Helper()
	res, err := job(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// runErr runs a job that must fail as a whole with the injected error.
func (e *env) runErr(job func(context.Context) (app.JobResult, error)) {
	e.t.Helper()
	if _, err := job(context.Background()); !errors.Is(err, errInjected) {
		e.t.Fatalf("job error = %v, want the injected failure", err)
	}
}
