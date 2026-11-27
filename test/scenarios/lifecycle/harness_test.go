//go:build integration

// Package lifecycle_test drives the subscription lifecycle use cases
// (conversion §8.1, direct signup §8.3, cancellation / dunning /
// reactivation §8.8–§8.9, suspension §8.10, offboarding §8.8.4, operator
// stub and saga actions B-O1 / B-O9) through their edge and failure paths
// on the shared harness (test/testkit): the real app.Service on Postgres 17
// with the production roles. Failures are injected at the port boundary by
// thin decorators around the real repositories, so every assertion is on
// what the service committed (or did not), what it asked the provider and
// RP, and what it put in outbox_events and pending_audit_entries.
package lifecycle_test

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

	manualadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/manual"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
)

var db *testkit.DB

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	d, stop, err := testkit.Start(ctx, "../../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testkit: %v\n", err)
		return 1
	}
	defer stop()
	db = d
	return m.Run()
}

// ── fault injection ───────────────────────────────────────────────────────

// errInjected is the failure the decorators return.
var errInjected = errors.New("injected failure")

// faults scripts one failure per named port call: the call after skip
// successful ones fails once with errInjected.
type faults struct {
	mu    sync.Mutex
	rules map[string]int
}

func (f *faults) on(name string, skip int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rules == nil {
		f.rules = map[string]int{}
	}
	f.rules[name] = skip
}

func (f *faults) hit(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	skip, ok := f.rules[name]
	if !ok {
		return nil
	}
	if skip > 0 {
		f.rules[name] = skip - 1
		return nil
	}
	delete(f.rules, name)
	return fmt.Errorf("%s: %w", name, errInjected)
}

type accounts struct {
	port.AccountRepo
	f *faults
}

func (r accounts) Create(ctx context.Context, a domain.BillingAccount) error {
	if err := r.f.hit("Accounts.Create"); err != nil {
		return err
	}
	return r.AccountRepo.Create(ctx, a)
}

func (r accounts) Get(ctx context.Context, tenantID string) (domain.BillingAccount, error) {
	if err := r.f.hit("Accounts.Get"); err != nil {
		return domain.BillingAccount{}, err
	}
	return r.AccountRepo.Get(ctx, tenantID)
}

type subs struct {
	port.SubscriptionRepo
	f *faults
}

func (r subs) Create(ctx context.Context, s domain.Subscription, it domain.SubscriptionItem) error {
	if err := r.f.hit("Subs.Create"); err != nil {
		return err
	}
	return r.SubscriptionRepo.Create(ctx, s, it)
}

func (r subs) Get(ctx context.Context, id string) (domain.Subscription, error) {
	if err := r.f.hit("Subs.Get"); err != nil {
		return domain.Subscription{}, err
	}
	return r.SubscriptionRepo.Get(ctx, id)
}

func (r subs) Live(ctx context.Context, tenantID string) (domain.Subscription, bool, error) {
	if err := r.f.hit("Subs.Live"); err != nil {
		return domain.Subscription{}, false, err
	}
	return r.SubscriptionRepo.Live(ctx, tenantID)
}

func (r subs) Latest(ctx context.Context, tenantID string) (domain.Subscription, bool, error) {
	if err := r.f.hit("Subs.Latest"); err != nil {
		return domain.Subscription{}, false, err
	}
	return r.SubscriptionRepo.Latest(ctx, tenantID)
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

func (r subs) Transition(ctx context.Context, id string, v int64, to domain.SubscriptionStatus, m func(*domain.Subscription)) (domain.Subscription, error) {
	if err := r.f.hit("Subs.Transition"); err != nil {
		return domain.Subscription{}, err
	}
	return r.SubscriptionRepo.Transition(ctx, id, v, to, m)
}

func (r subs) Update(ctx context.Context, id string, v int64, m func(*domain.Subscription)) (domain.Subscription, error) {
	if err := r.f.hit("Subs.Update"); err != nil {
		return domain.Subscription{}, err
	}
	return r.SubscriptionRepo.Update(ctx, id, v, m)
}

func (r subs) ByProvider(ctx context.Context, provider, id string) (domain.Subscription, bool, error) {
	if err := r.f.hit("Subs.ByProvider"); err != nil {
		return domain.Subscription{}, false, err
	}
	return r.SubscriptionRepo.ByProvider(ctx, provider, id)
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

func (r prices) PriceAt(ctx context.Context, plan domain.PlanCode, iv domain.BillingInterval, cur, prov string, at time.Time) (domain.PlanPrice, bool, error) {
	if err := r.f.hit("Prices.PriceAt"); err != nil {
		return domain.PlanPrice{}, false, err
	}
	return r.PriceRepo.PriceAt(ctx, plan, iv, cur, prov, at)
}

func (r prices) Get(ctx context.Context, id string) (domain.PlanPrice, error) {
	if err := r.f.hit("Prices.Get"); err != nil {
		return domain.PlanPrice{}, err
	}
	return r.PriceRepo.Get(ctx, id)
}

type checkouts struct {
	port.CheckoutRepo
	f *faults
}

func (r checkouts) Insert(ctx context.Context, c domain.CheckoutSession) error {
	if err := r.f.hit("Checkouts.Insert"); err != nil {
		return err
	}
	return r.CheckoutRepo.Insert(ctx, c)
}

func (r checkouts) Open(ctx context.Context, tenantID, purpose string) (domain.CheckoutSession, bool, error) {
	if err := r.f.hit("Checkouts.Open"); err != nil {
		return domain.CheckoutSession{}, false, err
	}
	return r.CheckoutRepo.Open(ctx, tenantID, purpose)
}

func (r checkouts) ByProviderSession(ctx context.Context, id string) (domain.CheckoutSession, bool, error) {
	if err := r.f.hit("Checkouts.ByProviderSession"); err != nil {
		return domain.CheckoutSession{}, false, err
	}
	return r.CheckoutRepo.ByProviderSession(ctx, id)
}

func (r checkouts) SetProviderSession(ctx context.Context, id, providerID string, at time.Time) error {
	if err := r.f.hit("Checkouts.SetProviderSession"); err != nil {
		return err
	}
	return r.CheckoutRepo.SetProviderSession(ctx, id, providerID, at)
}

func (r checkouts) SetStatus(ctx context.Context, id, status string) error {
	if err := r.f.hit("Checkouts.SetStatus"); err != nil {
		return err
	}
	return r.CheckoutRepo.SetStatus(ctx, id, status)
}

func (r checkouts) ExpireAll(ctx context.Context, tenantID string) error {
	if err := r.f.hit("Checkouts.ExpireAll"); err != nil {
		return err
	}
	return r.CheckoutRepo.ExpireAll(ctx, tenantID)
}

// sagas also scopes Due to the test's tenants (the repository reads across
// tenants on the system pool) and can replay stale refs, as a second
// replica that read Due before the first one ran the saga would.
type sagas struct {
	port.SagaRepo
	f     *faults
	e     *env
	stale []port.SagaRef
}

func (r *sagas) Insert(ctx context.Context, s domain.ConversionSaga) error {
	if err := r.f.hit("Sagas.Insert"); err != nil {
		return err
	}
	return r.SagaRepo.Insert(ctx, s)
}

func (r *sagas) BySubscription(ctx context.Context, id string) (domain.ConversionSaga, bool, error) {
	if err := r.f.hit("Sagas.BySubscription"); err != nil {
		return domain.ConversionSaga{}, false, err
	}
	return r.SagaRepo.BySubscription(ctx, id)
}

func (r *sagas) Get(ctx context.Context, id string) (domain.ConversionSaga, bool, error) {
	if err := r.f.hit("Sagas.Get"); err != nil {
		return domain.ConversionSaga{}, false, err
	}
	return r.SagaRepo.Get(ctx, id)
}

func (r *sagas) Update(ctx context.Context, s domain.ConversionSaga) error {
	if err := r.f.hit("Sagas.Update"); err != nil {
		return err
	}
	return r.SagaRepo.Update(ctx, s)
}

func (r *sagas) Due(ctx context.Context, now time.Time, limit int) ([]port.SagaRef, error) {
	if err := r.f.hit("Sagas.Due"); err != nil {
		return nil, err
	}
	all, err := r.SagaRepo.Due(ctx, now, limit)
	var out []port.SagaRef
	for _, ref := range all {
		if r.e.owns(ref.TenantID) {
			out = append(out, ref)
		}
	}
	return append(out, r.stale...), err
}

type invoices struct {
	port.InvoiceRepo
	f *faults
}

func (r invoices) Mirror(ctx context.Context, tenantID, provider string, v port.InvoiceView, at time.Time) (string, error) {
	if err := r.f.hit("Invoices.Mirror"); err != nil {
		return "", err
	}
	return r.InvoiceRepo.Mirror(ctx, tenantID, provider, v, at)
}

func (r invoices) Get(ctx context.Context, id string) (domain.Invoice, bool, error) {
	if err := r.f.hit("Invoices.Get"); err != nil {
		return domain.Invoice{}, false, err
	}
	return r.InvoiceRepo.Get(ctx, id)
}

func (r invoices) SetHeld(ctx context.Context, id string) error {
	if err := r.f.hit("Invoices.SetHeld"); err != nil {
		return err
	}
	return r.InvoiceRepo.SetHeld(ctx, id)
}

func (r invoices) Dispose(ctx context.Context, id, action, reason, by string, at time.Time, status string) error {
	if err := r.f.hit("Invoices.Dispose"); err != nil {
		return err
	}
	return r.InvoiceRepo.Dispose(ctx, id, action, reason, by, at, status)
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

type schedules struct {
	port.ScheduleRepo
	f *faults
}

func (r schedules) Open(ctx context.Context, id string) (domain.SeatChangeSchedule, bool, error) {
	if err := r.f.hit("Schedules.Open"); err != nil {
		return domain.SeatChangeSchedule{}, false, err
	}
	return r.ScheduleRepo.Open(ctx, id)
}

func (r schedules) Update(ctx context.Context, s domain.SeatChangeSchedule) error {
	if err := r.f.hit("Schedules.Update"); err != nil {
		return err
	}
	return r.ScheduleRepo.Update(ctx, s)
}

type seatPurchases struct {
	port.SeatPurchaseRepo
	f *faults
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

type emissions struct {
	port.EmissionRepo
	f *faults
}

func (r emissions) Schedule(ctx context.Context, e domain.ScheduledEmission) (bool, error) {
	if err := r.f.hit("Emissions.Schedule"); err != nil {
		return false, err
	}
	return r.EmissionRepo.Schedule(ctx, e)
}

type eventsQ struct {
	port.EventEnqueuer
	f *faults
}

func (r eventsQ) Enqueue(ctx context.Context, p domain.EventPayload, m port.EventMeta) (string, error) {
	if err := r.f.hit("Events.Enqueue"); err != nil {
		return "", err
	}
	return r.EventEnqueuer.Enqueue(ctx, p, m)
}

type auditW struct {
	port.AuditWriter
	f *faults
}

func (r auditW) Write(ctx context.Context, e port.AuditEntry) error {
	if err := r.f.hit("Audit.Write"); err != nil {
		return err
	}
	return r.AuditWriter.Write(ctx, e)
}

type locker struct {
	port.TenantLocker
	f *faults
}

func (r locker) LockTenant(ctx context.Context, tenantID string) error {
	if err := r.f.hit("Lock.LockTenant"); err != nil {
		return err
	}
	return r.TenantLocker.LockTenant(ctx, tenantID)
}

// txRunner can run a hook before the n-th next transaction starts: the
// interleaving point of a concurrent request between two of a use case's
// transactions (no sleeps, no goroutines).
type txRunner struct {
	port.TxRunner
	mu     sync.Mutex
	skip   int
	before func()
}

func (r *txRunner) RunInTenantTx(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error {
	r.mu.Lock()
	var hook func()
	if r.before != nil {
		if r.skip > 0 {
			r.skip--
		} else {
			hook, r.before = r.before, nil
		}
	}
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return r.TxRunner.RunInTenantTx(ctx, tenantID, fn)
}

// beforeTx runs hook before the transaction after skip more start.
func (e *env) beforeTx(skip int, hook func()) {
	e.tx.mu.Lock()
	defer e.tx.mu.Unlock()
	e.tx.skip, e.tx.before = skip, hook
}

// followUps scopes the cross-tenant follow-up query to the test's tenants
// and can replay a stale follow-up (one another replica already handled).
type followUps struct {
	port.FollowUpRepo
	f     *faults
	e     *env
	stale []port.FollowUp
}

func (r *followUps) Due(ctx context.Context, autoPause bool, limit int) ([]port.FollowUp, error) {
	if err := r.f.hit("FollowUps.Due"); err != nil {
		return nil, err
	}
	all, err := r.FollowUpRepo.Due(ctx, autoPause, limit)
	var out []port.FollowUp
	for _, fu := range all {
		if r.e.owns(fu.TenantID) {
			out = append(out, fu)
		}
	}
	return append(out, r.stale...), err
}

// ── outbound fakes ───────────────────────────────────────────────────────

// provider is a Stripe-like PaymentProvider that records every call with its
// idempotency key; f scripts failures by "Provider.<Method>".
type provider struct {
	port.PaymentProvider
	name string
	f    *faults
	mu   sync.Mutex
	log  []string
	caps port.ProviderCapabilities
	// confirm scripts ConfirmInitialPayment answers (default paid).
	confirm []port.PaymentOutcome
	// change scripts ChangeQuantity errors (default paid).
	change []error
	// onEnsureCustomer runs once inside the next EnsureCustomer call.
	onEnsureCustomer func()
	now              func() time.Time
}

func (p *provider) call(method, detail string) error {
	p.mu.Lock()
	p.log = append(p.log, strings.TrimSpace(method+" "+detail))
	p.mu.Unlock()
	return p.f.hit("Provider." + method)
}

// calls returns the recorded calls starting with prefix.
func (p *provider) calls(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.log {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (p *provider) Name() string                            { return p.name }
func (p *provider) Capabilities() port.ProviderCapabilities { return p.caps }

func (p *provider) EnsureCustomer(_ context.Context, in port.CustomerInput, key string) (port.CustomerRef, error) {
	if hook := p.onEnsureCustomer; hook != nil {
		p.onEnsureCustomer = nil
		hook()
	}
	return port.CustomerRef{ProviderCustomerID: "cus_" + in.TenantID[:8]}, p.call("EnsureCustomer", key)
}

func (p *provider) CreateHostedCheckout(_ context.Context, in port.CheckoutInput, key string) (port.HostedSession, error) {
	id := "cs_" + in.Metadata["billing_subscription_id"]
	if in.Purpose == port.CheckoutReactivation {
		id = "cs_react_" + key[strings.LastIndex(key, ":")+1:]
	}
	return port.HostedSession{ProviderSessionID: id, URL: "https://checkout.example/" + id, ExpiresAt: in.ExpiresAt},
		p.call("CreateHostedCheckout", in.PriceRef+" "+key)
}

// ExpireCheckout: f scripts "Provider.ExpireCheckout" (for example
// port.ErrCheckoutCompleted).
func (p *provider) ExpireCheckout(_ context.Context, id, key string) error {
	return p.call("ExpireCheckout", id+" "+key)
}

func (p *provider) AttachDefaultPaymentMethod(_ context.Context, _ port.CustomerRef, pm, key string) error {
	return p.call("AttachDefaultPaymentMethod", pm+" "+key)
}

func (p *provider) CreateSubscription(_ context.Context, in port.SubscriptionInput, key string) (port.SubscriptionResult, error) {
	return port.SubscriptionResult{Ref: port.SubscriptionRef{ProviderSubscriptionID: "sub_" + in.Metadata["billing_subscription_id"]},
		Outcome: port.PaymentOutcome{Status: port.PaymentPending}}, p.call("CreateSubscription", key)
}

func (p *provider) ConfirmInitialPayment(_ context.Context, _ port.SubscriptionRef, key string) (port.PaymentOutcome, error) {
	if err := p.call("ConfirmInitialPayment", key); err != nil {
		return port.PaymentOutcome{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.confirm) == 0 {
		return port.PaymentOutcome{Status: port.PaymentPaid}, nil
	}
	out := p.confirm[0]
	p.confirm = p.confirm[1:]
	return out, nil
}

func (p *provider) CancelNow(_ context.Context, ref port.SubscriptionRef, key string) error {
	return p.call("CancelNow", ref.ProviderSubscriptionID+" "+key)
}

// FetchCurrent answers an active subscription whose period starts at now.
func (p *provider) FetchCurrent(_ context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	start := p.now().Truncate(time.Second)
	return domain.ProviderObjectState{Ref: obj, Status: "active", Fields: map[string]any{
		"current_period_start": start, "current_period_end": start.AddDate(0, 1, 0)}}, p.call("FetchCurrent", obj.ID)
}

func (p *provider) SetCancelAtPeriodEnd(_ context.Context, ref port.SubscriptionRef, cancel bool, key string) error {
	return p.call("SetCancelAtPeriodEnd", fmt.Sprintf("%s %v %s", ref.ProviderSubscriptionID, cancel, key))
}

func (p *provider) ScheduleAtPeriodEnd(_ context.Context, in port.PeriodEndChange, key string) (port.ScheduleRef, error) {
	return port.ScheduleRef{ProviderScheduleID: "sched_" + key}, p.call("ScheduleAtPeriodEnd", in.ToPriceRef+" "+key)
}

func (p *provider) ReleaseSchedule(_ context.Context, ref port.ScheduleRef, key string) error {
	return p.call("ReleaseSchedule", ref.ProviderScheduleID+" "+key)
}

func (p *provider) PayOpenInvoiceLink(_ context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	return port.URLWithExpiry{URL: "https://invoice.example/" + ref.ProviderInvoiceID, ExpiresAt: p.now().Add(time.Hour)},
		p.call("PayOpenInvoiceLink", ref.ProviderInvoiceID)
}

func (p *provider) PauseCollection(_ context.Context, ref port.SubscriptionRef, pause bool, key string) error {
	return p.call("PauseCollection", fmt.Sprintf("%s %v %s", ref.ProviderSubscriptionID, pause, key))
}

func (p *provider) FinalizeInvoice(_ context.Context, ref port.InvoiceRef, key string) error {
	return p.call("FinalizeInvoice", ref.ProviderInvoiceID+" "+key)
}

func (p *provider) VoidInvoice(_ context.Context, ref port.InvoiceRef, key string) error {
	return p.call("VoidInvoice", ref.ProviderInvoiceID+" "+key)
}

func (p *provider) PreviewQuantityChange(_ context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: int64(in.To-in.From) * 2300, Currency: "USD"}, ProrationInstant: in.ProrationInstant},
		p.call("PreviewQuantityChange", "")
}

func (p *provider) ChangeQuantity(_ context.Context, in port.QuantityChange, key string) (port.ChangeResult, error) {
	_ = p.call("ChangeQuantity", fmt.Sprintf("%d->%d %s", in.From, in.To, key))
	p.mu.Lock()
	defer p.mu.Unlock()
	res := port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}
	if len(p.change) > 0 {
		err := p.change[0]
		p.change = p.change[1:]
		return res, err
	}
	return res, nil
}

func (p *provider) ListPaymentMethods(context.Context, port.CustomerRef) ([]port.PaymentMethodView, error) {
	return nil, p.call("ListPaymentMethods", "")
}

// manualProv is the real manual adapter with a scriptable CreateSubscription.
type manualProv struct {
	*manualadapter.Adapter
	f *faults
}

func (m manualProv) CreateSubscription(ctx context.Context, in port.SubscriptionInput, key string) (port.SubscriptionResult, error) {
	if err := m.f.hit("Manual.CreateSubscription"); err != nil {
		return port.SubscriptionResult{}, err
	}
	return m.Adapter.CreateSubscription(ctx, in, key)
}

// rp is the Realm Provisioner: scripted RP-3 answers (default 200), the
// keys it was called with, and an optional hook run during the call.
type rp struct {
	mu      sync.Mutex
	answers []domain.RP3Answer
	keys    []string
	during  func()
}

func (r *rp) Convert(_ context.Context, _ string, _ port.ConvertRequest, key string) domain.RP3Answer {
	if hook := r.during; hook != nil {
		r.during = nil
		hook()
	}
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

func (r *rp) RevertConversion(context.Context, string, string, string) domain.RP3Answer {
	return domain.RP3Answer{Status: 200}
}

func (r *rp) Realm(context.Context, string) (port.RealmView, error) { return port.RealmView{}, nil }

// logRec records what the service logs (SEV markers, follow-up warnings).
type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) add(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
}

func (l *logRec) Debug(msg string, _ map[string]any) { l.add("DEBUG", msg) }
func (l *logRec) Info(msg string, _ map[string]any)  { l.add("INFO", msg) }
func (l *logRec) Warn(msg string, _ map[string]any)  { l.add("WARN", msg) }
func (l *logRec) Error(msg string, _ map[string]any) { l.add("ERROR", msg) }

func (l *logRec) has(level, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, level+" ") && strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// metricsRec counts the domain metrics these flows record (§11.2).
type metricsRec struct {
	port.NopMetrics
	mu sync.Mutex
	n  map[string]int
}

func (m *metricsRec) inc(k string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[k]++
}

func (m *metricsRec) get(k string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[k]
}

func (m *metricsRec) RPConvertAttempt(o string)               { m.inc("rp_attempt:" + o) }
func (m *metricsRec) SagaConfirmed(k string, _ time.Duration) { m.inc("saga_confirmed:" + k) }
func (m *metricsRec) UnmatchedConversion()                    { m.inc("unmatched_conversion") }
func (m *metricsRec) PaymentFailure(r string)                 { m.inc("payment_failure:" + r) }
func (m *metricsRec) DunningTransition(to string)             { m.inc("dunning:" + to) }
func (m *metricsRec) InvoiceDisposition(a string)             { m.inc("disposition:" + a) }

// ── environment ──────────────────────────────────────────────────────────

type env struct {
	svc     *app.Service
	f       *faults
	stripe  *provider
	manual  manualProv
	rp      *rp
	core    *testkit.Core
	log     *logRec
	metrics *metricsRec
	sagas   *sagas
	follow  *followUps
	tx      *txRunner
	now     time.Time
	mu      sync.Mutex
	tenants map[string]bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fk := testkit.NewFakes()
	svc, err := testkit.NewService(db, fk)
	if err != nil {
		t.Fatal(err)
	}
	f := &faults{}
	e := &env{svc: svc, f: f, core: fk.Core, rp: &rp{}, log: &logRec{}, metrics: &metricsRec{}, tenants: map[string]bool{},
		now: time.Now().UTC().Truncate(time.Microsecond)}
	e.stripe = &provider{name: "stripe", f: f, now: func() time.Time { return e.now }, caps: port.ProviderCapabilities{HostedCardEntry: true,
		RecurringSubscriptions: true, OffSessionCharges: true, Schedules: true, CollectionPause: true, HostedPaymentMethodUpdate: true,
		Proration: port.ProrationNative, Currencies: []string{"USD"}}}
	e.manual = manualProv{Adapter: manualadapter.New(), f: f}
	e.sagas = &sagas{SagaRepo: svc.Sagas, f: f, e: e}
	e.follow = &followUps{FollowUpRepo: svc.FollowUps, f: f, e: e}
	svc.Accounts = accounts{svc.Accounts, f}
	svc.Subs = subs{svc.Subs, f}
	svc.Prices = prices{svc.Prices, f}
	svc.Checkouts = checkouts{svc.Checkouts, f}
	svc.Invoices = invoices{svc.Invoices, f}
	svc.Changes = changes{svc.Changes, f}
	svc.Schedules = schedules{svc.Schedules, f}
	svc.SeatPurchases = seatPurchases{svc.SeatPurchases, f}
	svc.Emissions = emissions{svc.Emissions, f}
	svc.Events = eventsQ{svc.Events, f}
	svc.Audit = auditW{svc.Audit, f}
	svc.Lock = locker{svc.Lock, f}
	svc.Sagas = e.sagas
	e.tx = &txRunner{TxRunner: svc.Tx}
	svc.Tx = e.tx
	svc.FollowUps = e.follow
	svc.Providers = testkit.Registry{"stripe": e.stripe, "manual": e.manual}
	svc.RP = e.rp
	svc.Log = e.log
	svc.Metrics = e.metrics
	svc.Now = func() time.Time { return e.now }
	return e
}

// tenant returns a new tenant id owned by this env.
func (e *env) tenant() string {
	id := uuid.NewString()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tenants[id] = true
	return id
}

func (e *env) owns(tenantID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tenants[tenantID]
}

// withoutProvider unloads a provider for one call (a provider disabled
// after the subscription was created).
func (e *env) withoutProvider(name string, fn func()) {
	saved := e.svc.Providers
	reg := testkit.Registry{}
	for _, n := range []string{"stripe", "manual"} {
		if n == name {
			continue
		}
		if p, ok := saved.Loaded(n); ok {
			reg[n] = p
		}
	}
	e.svc.Providers = reg
	defer func() { e.svc.Providers = saved }()
	fn()
}

var (
	owner    = app.Caller{UserID: "4b1e8a52-1111-4222-8333-944455556666", Roles: []string{domain.RoleTenantOwner}}
	operator = app.Caller{UserID: "9a9a9a9a-1111-4222-8333-944455556666", Roles: []string{domain.RolePlatformOperator}}
	bff      = app.Caller{Principal: "cluster.local/ns/bff/sa/bff-signup"}
)

func withKey(c app.Caller, key string) app.Caller { c.IdempotencyKey = key; return c }

func ptr[T any](v T) *T { return &v }

// signup returns an active Stripe subscription (B-I1, §8.3) for a new tenant.
func (e *env) signup(t *testing.T, plan string, seats int) (tenant, sub string) {
	t.Helper()
	tenant = e.tenant()
	res, err := e.svc.AuthorizeAndCharge(context.Background(), bff, app.AuthorizeChargeRequest{TenantID: tenant, PlanCode: plan,
		LicensedSeats: seats, PaymentMethodRef: "pm_1"})
	if err != nil {
		t.Fatal(err)
	}
	return tenant, res.BillingSubscriptionID
}

// stub returns an operator-stub subscription (B-O1) whose saga is due now.
func (e *env) stub(t *testing.T) (tenant string, res app.StubConversionResult) {
	t.Helper()
	tenant = e.tenant()
	res, err := e.svc.StubConversion(context.Background(), operator, tenant, app.StubConversionRequest{Plan: "pro", Interval: "month",
		LicensedSeats: 3, Reason: "Signed order form; invoiced offline"})
	if err != nil {
		t.Fatal(err)
	}
	return tenant, res
}

// handle runs a callback handler as the webhook processor does: in one
// tenant transaction (a handler error rolls everything back).
func (e *env) handle(tenant string, h port.PaymentEventHandler, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	var out string
	err := e.svc.Tx.RunInTenantTx(context.Background(), tenant, func(ctx context.Context) error {
		var err error
		out, err = h.Handle(ctx, tenant, ev, cur)
		return err
	})
	return out, err
}

func (e *env) inTx(tenant string, fn func(ctx context.Context) error) error {
	return e.svc.Tx.RunInTenantTx(context.Background(), tenant, fn)
}

func callback(id, typ string, kind domain.NormalisedKind) domain.PaymentEvent {
	return domain.PaymentEvent{Provider: "stripe", ProviderEventID: id, EventType: typ, Kind: kind, ProviderCreatedAt: time.Now()}
}

// invoiceState is a re-read invoice of provider subscription sub_<sub>.
func invoiceState(id, sub, status, reason string, start time.Time, qty any) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: id}, Status: status,
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": reason, "amount_due_minor": int64(69000),
			"amount_paid_minor": int64(0), "currency": "USD", "line_period_start": start, "line_period_end": start.AddDate(0, 1, 0),
			"line_quantity": qty, "next_payment_attempt": start.Add(72 * time.Hour)}}
}

// ── assertions ───────────────────────────────────────────────────────────

func scalar[T any](t *testing.T, q string, args ...any) T {
	t.Helper()
	v, err := testkit.Scalar[T](context.Background(), db, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := db.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func status(t *testing.T, sub string) string {
	t.Helper()
	return scalar[string](t, `SELECT status::text FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func version(t *testing.T, sub string) int64 {
	t.Helper()
	return scalar[int64](t, `SELECT record_version FROM subscriptions WHERE billing_subscription_id = $1`, sub)
}

func count(t *testing.T, q string, args ...any) int {
	t.Helper()
	return scalar[int](t, q, args...)
}

func events(t *testing.T, tenant string) []string {
	t.Helper()
	return scalar[[]string](t, `SELECT coalesce(array_agg(event_type ORDER BY ordering_seq), '{}') FROM outbox_events WHERE tenant_id = $1`, tenant)
}

// injected fails unless err is the scripted failure of call name.
func injected(t *testing.T, err error, name string) {
	t.Helper()
	if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), name) {
		t.Fatalf("want injected %s failure, got %v", name, err)
	}
}

func wantCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	if !domain.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func ignored(t *testing.T, err error, reason string) {
	t.Helper()
	var ig *port.IgnoredEvent
	if !errors.As(err, &ig) || ig.Reason != reason {
		t.Fatalf("want ignored %q, got %v", reason, err)
	}
}
