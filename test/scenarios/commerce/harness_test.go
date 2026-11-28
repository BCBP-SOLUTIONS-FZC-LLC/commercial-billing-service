//go:build integration

// Package commerce_test drives the commerce use cases (money, plan change,
// seats, add-ons, tenant reads, operator views) through their less-travelled
// paths on Postgres 17 with the production roles (test/testkit): scripted
// provider outcomes and errors, IAM / Entitlements answers, and storage
// faults injected at the repository ports, so each refusal, rollback and
// retry path of LLD §8 is asserted against the database, the outbox and the
// provider calls with their idempotency keys.
package commerce_test

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

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/test/testkit"
)

var db *testkit.DB

func TestMain(m *testing.M) {
	d, stop, err := testkit.Start(context.Background(), "../../..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "commerce: %v\n", err)
		os.Exit(1)
	}
	db = d
	code := m.Run()
	stop()
	os.Exit(code)
}

// ── database helpers ────────────────────────────────────────────────────

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := db.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// paidFields is a paid invoice re-read for exactly what the purchase (or
// plan change) id in table charged (BIL-PAY-7), plus extra key/value pairs.
func paidFields(t *testing.T, table, id string, extra ...string) map[string]any {
	t.Helper()
	key := "purchase_id"
	if table == "plan_change_requests" {
		key = "change_id"
	}
	f := map[string]any{"amount_paid_minor": scalar[int64](t, `SELECT amount_minor FROM `+table+` WHERE `+key+` = $1::uuid`, id),
		"currency": scalar[string](t, `SELECT currency FROM `+table+` WHERE `+key+` = $1::uuid`, id)}
	for i := 0; i+1 < len(extra); i += 2 {
		f[extra[i]] = extra[i+1]
	}
	return f
}

func scalar[T any](t *testing.T, q string, args ...any) T {
	t.Helper()
	v, err := testkit.Scalar[T](context.Background(), db, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func count(t *testing.T, q string, args ...any) int { t.Helper(); return scalar[int](t, q, args...) }

// ── injected storage faults ─────────────────────────────────────────────

// errInjected is the storage error a fault returns.
var errInjected = errors.New("injected storage fault")

// errAbsent makes a (value, found, error) method answer "not found".
var errAbsent = errors.New("injected: not found")

type fault struct {
	skip int
	err  error
}

// faults fails a named repository method once, after skip successful calls.
type faults struct {
	mu sync.Mutex
	m  map[string]*fault
}

func (f *faults) set(name string, skip int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]*fault{}
	}
	f.m[name] = &fault{skip: skip, err: err}
}

func (f *faults) hit(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.m[name]
	if !ok {
		return nil
	}
	if x.skip > 0 {
		x.skip--
		return nil
	}
	delete(f.m, name)
	return x.err
}

// pending reports faults that never fired (a test that armed one expects it).
func (f *faults) pending() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.m {
		out = append(out, k)
	}
	return out
}

func f1(fs *faults, name string, call func() error) error {
	if err := fs.hit(name); err != nil {
		return err
	}
	return call()
}

func f2[T any](fs *faults, name string, call func() (T, error)) (T, error) {
	if err := fs.hit(name); err != nil {
		var z T
		return z, err
	}
	return call()
}

func f3[T any](fs *faults, name string, call func() (T, bool, error)) (T, bool, error) {
	if err := fs.hit(name); err != nil {
		var z T
		if errors.Is(err, errAbsent) {
			return z, false, nil
		}
		return z, false, err
	}
	return call()
}

type accounts struct {
	port.AccountRepo
	f *faults
}

func (r accounts) Get(ctx context.Context, t string) (domain.BillingAccount, error) {
	return f2(r.f, "Accounts.Get", func() (domain.BillingAccount, error) { return r.AccountRepo.Get(ctx, t) })
}
func (r accounts) Create(ctx context.Context, a domain.BillingAccount) error {
	return f1(r.f, "Accounts.Create", func() error { return r.AccountRepo.Create(ctx, a) })
}
func (r accounts) ReturnToTrial(ctx context.Context, t string, at time.Time) error {
	return f1(r.f, "Accounts.ReturnToTrial", func() error { return r.AccountRepo.ReturnToTrial(ctx, t, at) })
}

type subs struct {
	port.SubscriptionRepo
	f *faults
}

func (r subs) Get(ctx context.Context, id string) (domain.Subscription, error) {
	return f2(r.f, "Subs.Get", func() (domain.Subscription, error) { return r.SubscriptionRepo.Get(ctx, id) })
}
func (r subs) Live(ctx context.Context, t string) (domain.Subscription, bool, error) {
	return f3(r.f, "Subs.Live", func() (domain.Subscription, bool, error) { return r.SubscriptionRepo.Live(ctx, t) })
}
func (r subs) Latest(ctx context.Context, t string) (domain.Subscription, bool, error) {
	return f3(r.f, "Subs.Latest", func() (domain.Subscription, bool, error) { return r.SubscriptionRepo.Latest(ctx, t) })
}
func (r subs) Item(ctx context.Context, id string) (domain.SubscriptionItem, error) {
	return f2(r.f, "Subs.Item", func() (domain.SubscriptionItem, error) { return r.SubscriptionRepo.Item(ctx, id) })
}
func (r subs) SetItem(ctx context.Context, it domain.SubscriptionItem) error {
	return f1(r.f, "Subs.SetItem", func() error { return r.SubscriptionRepo.SetItem(ctx, it) })
}
func (r subs) Transition(ctx context.Context, id string, v int64, to domain.SubscriptionStatus, m func(*domain.Subscription)) (domain.Subscription, error) {
	return f2(r.f, "Subs.Transition", func() (domain.Subscription, error) { return r.SubscriptionRepo.Transition(ctx, id, v, to, m) })
}
func (r subs) Update(ctx context.Context, id string, v int64, m func(*domain.Subscription)) (domain.Subscription, error) {
	return f2(r.f, "Subs.Update", func() (domain.Subscription, error) { return r.SubscriptionRepo.Update(ctx, id, v, m) })
}
func (r subs) AppendHistory(ctx context.Context, h domain.StatusHistoryEntry) error {
	return f1(r.f, "Subs.AppendHistory", func() error { return r.SubscriptionRepo.AppendHistory(ctx, h) })
}
func (r subs) History(ctx context.Context, id string, limit int) ([]domain.StatusHistoryEntry, error) {
	return f2(r.f, "Subs.History", func() ([]domain.StatusHistoryEntry, error) { return r.SubscriptionRepo.History(ctx, id, limit) })
}

type prices struct {
	port.PriceRepo
	f *faults
}

func (r prices) Get(ctx context.Context, id string) (domain.PlanPrice, error) {
	return f2(r.f, "Prices.Get", func() (domain.PlanPrice, error) { return r.PriceRepo.Get(ctx, id) })
}
func (r prices) PriceAt(ctx context.Context, plan domain.PlanCode, iv domain.BillingInterval, cur, prov string, at time.Time) (domain.PlanPrice, bool, error) {
	return f3(r.f, "Prices.PriceAt", func() (domain.PlanPrice, bool, error) { return r.PriceRepo.PriceAt(ctx, plan, iv, cur, prov, at) })
}

type checkouts struct {
	port.CheckoutRepo
	f *faults
}

func (r checkouts) Insert(ctx context.Context, c domain.CheckoutSession) error {
	return f1(r.f, "Checkouts.Insert", func() error { return r.CheckoutRepo.Insert(ctx, c) })
}
func (r checkouts) SetProviderSession(ctx context.Context, id, ps string, exp time.Time) error {
	return f1(r.f, "Checkouts.SetProviderSession", func() error { return r.CheckoutRepo.SetProviderSession(ctx, id, ps, exp) })
}
func (r checkouts) ByProviderSession(ctx context.Context, ps string) (domain.CheckoutSession, bool, error) {
	return f3(r.f, "Checkouts.ByProviderSession", func() (domain.CheckoutSession, bool, error) { return r.CheckoutRepo.ByProviderSession(ctx, ps) })
}
func (r checkouts) SetStatus(ctx context.Context, id, st string) error {
	return f1(r.f, "Checkouts.SetStatus", func() error { return r.CheckoutRepo.SetStatus(ctx, id, st) })
}
func (r checkouts) GetSession(ctx context.Context, id string) (domain.CheckoutSession, bool, error) {
	return f3(r.f, "Checkouts.GetSession", func() (domain.CheckoutSession, bool, error) { return r.CheckoutRepo.GetSession(ctx, id) })
}
func (r checkouts) MarkDefaultApplied(ctx context.Context, id string) error {
	return f1(r.f, "Checkouts.MarkDefaultApplied", func() error { return r.CheckoutRepo.MarkDefaultApplied(ctx, id) })
}

type invoices struct {
	port.InvoiceRepo
	f *faults
}

func (r invoices) Get(ctx context.Context, id string) (domain.Invoice, bool, error) {
	return f3(r.f, "Invoices.Get", func() (domain.Invoice, bool, error) { return r.InvoiceRepo.Get(ctx, id) })
}
func (r invoices) List(ctx context.Context, t, st, k, after string, drafts bool, limit int) ([]port.InvoiceRow, error) {
	return f2(r.f, "Invoices.List", func() ([]port.InvoiceRow, error) { return r.InvoiceRepo.List(ctx, t, st, k, after, drafts, limit) })
}
func (r invoices) ByProvider(ctx context.Context, p, id string) (domain.Invoice, bool, error) {
	return f3(r.f, "Invoices.ByProvider", func() (domain.Invoice, bool, error) { return r.InvoiceRepo.ByProvider(ctx, p, id) })
}
func (r invoices) ConversionInvoice(ctx context.Context, t string) (domain.Invoice, bool, error) {
	return f3(r.f, "Invoices.ConversionInvoice", func() (domain.Invoice, bool, error) { return r.InvoiceRepo.ConversionInvoice(ctx, t) })
}
func (r invoices) AddRefunded(ctx context.Context, id string, n int64) error {
	return f1(r.f, "Invoices.AddRefunded", func() error { return r.InvoiceRepo.AddRefunded(ctx, id, n) })
}

type changes struct {
	port.PlanChangeRepo
	f *faults
}

func (r changes) Insert(ctx context.Context, c domain.PlanChangeRequest) error {
	return f1(r.f, "Changes.Insert", func() error { return r.PlanChangeRepo.Insert(ctx, c) })
}
func (r changes) Open(ctx context.Context, t string) (domain.PlanChangeRequest, bool, error) {
	return f3(r.f, "Changes.Open", func() (domain.PlanChangeRequest, bool, error) { return r.PlanChangeRepo.Open(ctx, t) })
}
func (r changes) ByProviderInvoice(ctx context.Context, t, id string) (domain.PlanChangeRequest, bool, error) {
	return f3(r.f, "Changes.ByProviderInvoice", func() (domain.PlanChangeRequest, bool, error) { return r.PlanChangeRepo.ByProviderInvoice(ctx, t, id) })
}
func (r changes) Update(ctx context.Context, c domain.PlanChangeRequest) error {
	return f1(r.f, "Changes.Update", func() error { return r.PlanChangeRepo.Update(ctx, c) })
}

type previews struct {
	port.PreviewRepo
	f *faults
}

func (r previews) Save(ctx context.Context, t, id string, body []byte, exp time.Time) error {
	return f1(r.f, "Previews.Save", func() error { return r.PreviewRepo.Save(ctx, t, id, body, exp) })
}
func (r previews) Load(ctx context.Context, t, id string) ([]byte, time.Time, bool, error) {
	if err := r.f.hit("Previews.Load"); err != nil {
		return nil, time.Time{}, false, err
	}
	return r.PreviewRepo.Load(ctx, t, id)
}

type seatPurchases struct {
	port.SeatPurchaseRepo
	f *faults
}

func (r seatPurchases) Insert(ctx context.Context, p domain.SeatPurchase) error {
	return f1(r.f, "SeatPurchases.Insert", func() error { return r.SeatPurchaseRepo.Insert(ctx, p) })
}
func (r seatPurchases) Get(ctx context.Context, id string) (domain.SeatPurchase, bool, error) {
	return f3(r.f, "SeatPurchases.Get", func() (domain.SeatPurchase, bool, error) { return r.SeatPurchaseRepo.Get(ctx, id) })
}
func (r seatPurchases) Open(ctx context.Context, id string) (domain.SeatPurchase, bool, error) {
	return f3(r.f, "SeatPurchases.Open", func() (domain.SeatPurchase, bool, error) { return r.SeatPurchaseRepo.Open(ctx, id) })
}
func (r seatPurchases) ByProviderInvoice(ctx context.Context, t, id string) (domain.SeatPurchase, bool, error) {
	return f3(r.f, "SeatPurchases.ByProviderInvoice", func() (domain.SeatPurchase, bool, error) { return r.SeatPurchaseRepo.ByProviderInvoice(ctx, t, id) })
}
func (r seatPurchases) List(ctx context.Context, t, after string, limit int) ([]domain.SeatPurchase, error) {
	return f2(r.f, "SeatPurchases.List", func() ([]domain.SeatPurchase, error) { return r.SeatPurchaseRepo.List(ctx, t, after, limit) })
}
func (r seatPurchases) Update(ctx context.Context, p domain.SeatPurchase) error {
	return f1(r.f, "SeatPurchases.Update", func() error { return r.SeatPurchaseRepo.Update(ctx, p) })
}

type schedules struct {
	port.ScheduleRepo
	f *faults
}

func (r schedules) Insert(ctx context.Context, s domain.SeatChangeSchedule) error {
	return f1(r.f, "Schedules.Insert", func() error { return r.ScheduleRepo.Insert(ctx, s) })
}
func (r schedules) Open(ctx context.Context, id string) (domain.SeatChangeSchedule, bool, error) {
	return f3(r.f, "Schedules.Open", func() (domain.SeatChangeSchedule, bool, error) { return r.ScheduleRepo.Open(ctx, id) })
}
func (r schedules) Latest(ctx context.Context, id string) (domain.SeatChangeSchedule, bool, error) {
	return f3(r.f, "Schedules.Latest", func() (domain.SeatChangeSchedule, bool, error) { return r.ScheduleRepo.Latest(ctx, id) })
}
func (r schedules) Get(ctx context.Context, id string) (domain.SeatChangeSchedule, bool, error) {
	return f3(r.f, "Schedules.Get", func() (domain.SeatChangeSchedule, bool, error) { return r.ScheduleRepo.Get(ctx, id) })
}
func (r schedules) Update(ctx context.Context, s domain.SeatChangeSchedule) error {
	return f1(r.f, "Schedules.Update", func() error { return r.ScheduleRepo.Update(ctx, s) })
}

type addons struct {
	port.AddonRepo
	f *faults
}

func (r addons) Insert(ctx context.Context, p domain.QuotaAddonPurchase) error {
	return f1(r.f, "Addons.Insert", func() error { return r.AddonRepo.Insert(ctx, p) })
}
func (r addons) Get(ctx context.Context, id string) (domain.QuotaAddonPurchase, bool, error) {
	return f3(r.f, "Addons.Get", func() (domain.QuotaAddonPurchase, bool, error) { return r.AddonRepo.Get(ctx, id) })
}
func (r addons) ByProviderInvoice(ctx context.Context, t, id string) (domain.QuotaAddonPurchase, bool, error) {
	return f3(r.f, "Addons.ByProviderInvoice", func() (domain.QuotaAddonPurchase, bool, error) { return r.AddonRepo.ByProviderInvoice(ctx, t, id) })
}
func (r addons) List(ctx context.Context, t, after string, limit int) ([]domain.QuotaAddonPurchase, error) {
	return f2(r.f, "Addons.List", func() ([]domain.QuotaAddonPurchase, error) { return r.AddonRepo.List(ctx, t, after, limit) })
}
func (r addons) Update(ctx context.Context, p domain.QuotaAddonPurchase) error {
	return f1(r.f, "Addons.Update", func() error { return r.AddonRepo.Update(ctx, p) })
}
func (r addons) ConfirmedTokensSince(ctx context.Context, t, pack string, since time.Time) (int64, error) {
	return f2(r.f, "Addons.ConfirmedTokensSince", func() (int64, error) { return r.AddonRepo.ConfirmedTokensSince(ctx, t, pack, since) })
}
func (r addons) TrialUsage(ctx context.Context, t string) (domain.TrialAddonUsage, error) {
	return f2(r.f, "Addons.TrialUsage", func() (domain.TrialAddonUsage, error) { return r.AddonRepo.TrialUsage(ctx, t) })
}
func (r addons) TrialCap(ctx context.Context, t string) (domain.TrialAddonCap, bool, error) {
	return f3(r.f, "Addons.TrialCap", func() (domain.TrialAddonCap, bool, error) { return r.AddonRepo.TrialCap(ctx, t) })
}
func (r addons) UpsertTrialCap(ctx context.Context, c domain.TrialAddonCap, v int64) (domain.TrialAddonCap, error) {
	return f2(r.f, "Addons.UpsertTrialCap", func() (domain.TrialAddonCap, error) { return r.AddonRepo.UpsertTrialCap(ctx, c, v) })
}
func (r addons) DeleteTrialCap(ctx context.Context, t string) error {
	return f1(r.f, "Addons.DeleteTrialCap", func() error { return r.AddonRepo.DeleteTrialCap(ctx, t) })
}

type money struct {
	port.MoneyRepo
	f *faults
}

func (r money) InsertAdjustment(ctx context.Context, a domain.Adjustment) error {
	return f1(r.f, "Money.InsertAdjustment", func() error { return r.MoneyRepo.InsertAdjustment(ctx, a) })
}
func (r money) SetAdjustmentRef(ctx context.Context, id, ref string) error {
	return f1(r.f, "Money.SetAdjustmentRef", func() error { return r.MoneyRepo.SetAdjustmentRef(ctx, id, ref) })
}
func (r money) RefundByProvider(ctx context.Context, id string) (domain.Refund, bool, error) {
	return f3(r.f, "Money.RefundByProvider", func() (domain.Refund, bool, error) { return r.MoneyRepo.RefundByProvider(ctx, id) })
}
func (r money) RefundsForInvoice(ctx context.Context, id string) ([]domain.Refund, error) {
	return f2(r.f, "Money.RefundsForInvoice", func() ([]domain.Refund, error) { return r.MoneyRepo.RefundsForInvoice(ctx, id) })
}
func (r money) UpdateRefund(ctx context.Context, x domain.Refund) error {
	return f1(r.f, "Money.UpdateRefund", func() error { return r.MoneyRepo.UpdateRefund(ctx, x) })
}
func (r money) UpsertDispute(ctx context.Context, d domain.Dispute) (domain.Dispute, error) {
	return f2(r.f, "Money.UpsertDispute", func() (domain.Dispute, error) { return r.MoneyRepo.UpsertDispute(ctx, d) })
}
func (r money) Dispute(ctx context.Context, id string) (domain.Dispute, bool, error) {
	return f3(r.f, "Money.Dispute", func() (domain.Dispute, bool, error) { return r.MoneyRepo.Dispute(ctx, id) })
}
func (r money) DecideDispute(ctx context.Context, id, d, by string) error {
	return f1(r.f, "Money.DecideDispute", func() error { return r.MoneyRepo.DecideDispute(ctx, id, d, by) })
}
func (r money) Disputes(ctx context.Context, t string) ([]domain.Dispute, error) {
	return f2(r.f, "Money.Disputes", func() ([]domain.Dispute, error) { return r.MoneyRepo.Disputes(ctx, t) })
}

type recon struct {
	port.ReconRepo
	f *faults
}

func (r recon) AddFinding(ctx context.Context, id string, x port.ReconFinding) error {
	return f1(r.f, "Recon.AddFinding", func() error { return r.ReconRepo.AddFinding(ctx, id, x) })
}
func (r recon) Finish(ctx context.Context, id, st string, c map[string]int, e *string) error {
	return f1(r.f, "Recon.Finish", func() error { return r.ReconRepo.Finish(ctx, id, st, c, e) })
}
func (r recon) Get(ctx context.Context, id string) (port.ReconRun, []port.ReconFinding, bool, error) {
	if err := r.f.hit("Recon.Get"); err != nil {
		return port.ReconRun{}, nil, false, err
	}
	return r.ReconRepo.Get(ctx, id)
}
func (r recon) InvoiceMirrors(ctx context.Context, p string, ids []string) (map[string]port.ReconInvoice, error) {
	return f2(r.f, "Recon.InvoiceMirrors", func() (map[string]port.ReconInvoice, error) { return r.ReconRepo.InvoiceMirrors(ctx, p, ids) })
}
func (r recon) BillingSubscriptions(ctx context.Context, p string) (map[string]port.ReconSubscription, error) {
	return f2(r.f, "Recon.BillingSubscriptions", func() (map[string]port.ReconSubscription, error) { return r.ReconRepo.BillingSubscriptions(ctx, p) })
}
func (r recon) KnownIDs(ctx context.Context, p, k string, ids []string) (map[string]bool, error) {
	return f2(r.f, "Recon.KnownIDs", func() (map[string]bool, error) { return r.ReconRepo.KnownIDs(ctx, p, k, ids) })
}
func (r recon) UnknownCustomerEvents(ctx context.Context, p string, from, to time.Time) ([]string, error) {
	return f2(r.f, "Recon.UnknownCustomerEvents", func() ([]string, error) { return r.ReconRepo.UnknownCustomerEvents(ctx, p, from, to) })
}

type ledgerReader struct {
	port.LedgerReader
	f *faults
}

func (r ledgerReader) Excerpt(ctx context.Context, t string, limit int) ([]port.LedgerExcerpt, error) {
	return f2(r.f, "Ledger.Excerpt", func() ([]port.LedgerExcerpt, error) { return r.LedgerReader.Excerpt(ctx, t, limit) })
}

type customers struct {
	port.SystemReader
	f *faults
}

func (r customers) TenantForCustomer(ctx context.Context, p, c string) (string, bool, error) {
	return f3(r.f, "Customers.TenantForCustomer", func() (string, bool, error) { return r.SystemReader.TenantForCustomer(ctx, p, c) })
}

type sagas struct {
	port.SagaRepo
	f *faults
}

func (r sagas) Get(ctx context.Context, id string) (domain.ConversionSaga, bool, error) {
	return f3(r.f, "Sagas.Get", func() (domain.ConversionSaga, bool, error) { return r.SagaRepo.Get(ctx, id) })
}
func (r sagas) BySubscription(ctx context.Context, id string) (domain.ConversionSaga, bool, error) {
	return f3(r.f, "Sagas.BySubscription", func() (domain.ConversionSaga, bool, error) { return r.SagaRepo.BySubscription(ctx, id) })
}
func (r sagas) Update(ctx context.Context, s domain.ConversionSaga) error {
	return f1(r.f, "Sagas.Update", func() error { return r.SagaRepo.Update(ctx, s) })
}

type events struct {
	port.EventEnqueuer
	f *faults
}

func (r events) Enqueue(ctx context.Context, p domain.EventPayload, m port.EventMeta) (string, error) {
	return f2(r.f, "Events.Enqueue", func() (string, error) { return r.EventEnqueuer.Enqueue(ctx, p, m) })
}

type audit struct {
	port.AuditWriter
	f *faults
}

func (r audit) Write(ctx context.Context, e port.AuditEntry) error {
	return f1(r.f, "Audit.Write", func() error { return r.AuditWriter.Write(ctx, e) })
}

type locker struct {
	port.TenantLocker
	f *faults
}

func (r locker) LockTenant(ctx context.Context, t string) error {
	return f1(r.f, "Lock.LockTenant", func() error { return r.TenantLocker.LockTenant(ctx, t) })
}

// ── outbound fakes ──────────────────────────────────────────────────────

// answer scripts one provider call: an outcome, an error, a refund status,
// and a hook run before the answer returns (a callback racing the request).
type answer struct {
	outcome port.PaymentOutcome
	err     error
	status  string
	invoice string
	before  func(key string)
}

// errProvider is a transient provider failure (§17 PROVIDER_UNAVAILABLE).
var errProvider = fmt.Errorf("%w: injected", port.ErrProviderUnavailable)

// provider is the testkit fake extended with the calls the commerce flows
// make, each recorded with its idempotency key and scriptable per method.
type provider struct {
	*testkit.Provider
	mu          sync.Mutex
	log         []string
	script      map[string][]answer
	fallback    bool
	noSchedules bool
	noHostedPM  bool
	setupPM     string
	pms         []port.PaymentMethodView
	recon       map[port.ReconKind][]port.ReconPage
	fetchErr    map[string]error
}

func newProvider() *provider {
	return &provider{Provider: testkit.NewProvider("stripe"), script: map[string][]answer{}, setupPM: "pm_new",
		pms: []port.PaymentMethodView{{ProviderRef: "pm_1QAbCdEfGh1234", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2028, IsDefault: true}}}
}

// on queues answers for method.
func (p *provider) on(method string, a ...answer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script[method] = append(p.script[method], a...)
}

// take records call and pops the next scripted answer for method.
func (p *provider) take(method, call, key string) answer {
	p.mu.Lock()
	p.log = append(p.log, call)
	var a answer
	if q := p.script[method]; len(q) > 0 {
		a, p.script[method] = q[0], q[1:]
	}
	p.mu.Unlock()
	if a.before != nil {
		a.before(key)
	}
	return a
}

// called returns the recorded calls (this fake's and the testkit's) with prefix.
func (p *provider) called(prefix string) []string {
	p.mu.Lock()
	var out []string
	for _, c := range p.log {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	p.mu.Unlock()
	return append(out, p.Calls(prefix)...)
}

func (p *provider) Capabilities() port.ProviderCapabilities {
	c := p.Provider.Capabilities()
	if p.fallback {
		c.Proration = port.ProrationFallback
	}
	c.Schedules, c.HostedPaymentMethodUpdate = !p.noSchedules, !p.noHostedPM
	return c
}

func (p *provider) PreviewPriceChange(_ context.Context, in port.PriceChange) (port.ProrationPreview, error) {
	a := p.take("PreviewPriceChange", "PreviewPriceChange "+in.ToPriceRef, "")
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: 28400, Currency: "USD"}, ProrationInstant: in.ProrationInstant}, a.err
}

func (p *provider) ChangePrice(_ context.Context, in port.PriceChange, key string) (port.ChangeResult, error) {
	a := p.take("ChangePrice", "ChangePrice "+in.ToPriceRef+" "+key, key)
	return p.change(a, key)
}

func (p *provider) change(a answer, key string) (port.ChangeResult, error) {
	res := port.ChangeResult{Invoice: port.InvoiceRef{ProviderInvoiceID: "in_" + key}, Outcome: port.PaymentOutcome{Status: port.PaymentPaid}}
	if a.outcome.Status != "" {
		res.Outcome = a.outcome
	}
	if a.invoice != "" {
		res.Invoice.ProviderInvoiceID = a.invoice
	}
	return res, a.err
}

func (p *provider) ScheduleAtPeriodEnd(_ context.Context, in port.PeriodEndChange, key string) (port.ScheduleRef, error) {
	a := p.take("ScheduleAtPeriodEnd", "ScheduleAtPeriodEnd "+in.ToPriceRef+" "+key, key)
	return port.ScheduleRef{ProviderScheduleID: "sched_" + key}, a.err
}

func (p *provider) ReleaseSchedule(_ context.Context, ref port.ScheduleRef, key string) error {
	return p.take("ReleaseSchedule", "ReleaseSchedule "+ref.ProviderScheduleID+" "+key, key).err
}

func (p *provider) PreviewQuantityChange(_ context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	a := p.take("PreviewQuantityChange", "PreviewQuantityChange", "")
	return port.ProrationPreview{Amount: domain.Money{AmountMinor: int64(in.To-in.From) * 2300, Currency: "USD"}, ProrationInstant: in.ProrationInstant}, a.err
}

func (p *provider) ChangeQuantity(_ context.Context, in port.QuantityChange, key string) (port.ChangeResult, error) {
	a := p.take("ChangeQuantity", fmt.Sprintf("ChangeQuantity %d->%d invoice_now=%v %s", in.From, in.To, in.InvoiceNow, key), key)
	return p.change(a, key)
}

func (p *provider) ChargeOneOff(_ context.Context, in port.OneOffCharge, key string) (port.ChargeResult, error) {
	a := p.take("ChargeOneOff", fmt.Sprintf("ChargeOneOff %s x%d %d %s", in.PriceRef, in.Quantity, in.Amount.AmountMinor, key), key)
	r, err := p.change(a, key)
	return port.ChargeResult{Invoice: r.Invoice, Outcome: r.Outcome}, err
}

// EnsureCustomer is scriptable (a trial tenant's first add-on purchase).
func (p *provider) EnsureCustomer(ctx context.Context, in port.CustomerInput, key string) (port.CustomerRef, error) {
	if a := p.take("EnsureCustomer", "EnsureCustomer! "+in.Currency+" "+key, key); a.err != nil {
		return port.CustomerRef{}, a.err
	}
	return p.Provider.EnsureCustomer(ctx, in, key)
}

func (p *provider) PayOpenInvoiceLink(_ context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	a := p.take("PayOpenInvoiceLink", "PayOpenInvoiceLink "+ref.ProviderInvoiceID, "")
	return port.URLWithExpiry{URL: "https://invoice.example/" + ref.ProviderInvoiceID}, a.err
}

func (p *provider) CreditBalance(_ context.Context, in port.BalanceAdjustment, key string) (string, error) {
	a := p.take("CreditBalance", fmt.Sprintf("CreditBalance %d %s", in.Amount.AmountMinor, key), key)
	return "cbtxn_" + key, a.err
}

func (p *provider) Refund(_ context.Context, in port.RefundInput, key string) (port.RefundResult, error) {
	a := p.take("Refund", fmt.Sprintf("Refund %s %d %s %s", in.Invoice.ProviderInvoiceID, in.Amount.AmountMinor, in.Reason, key), key)
	st := a.status
	if st == "" {
		st = "pending"
	}
	return port.RefundResult{ProviderRefundID: "re_" + key, Status: st}, a.err
}

func (p *provider) CancelNow(ctx context.Context, ref port.SubscriptionRef, key string) error {
	if a := p.take("CancelNow", "CancelNow! "+ref.ProviderSubscriptionID+" "+key, key); a.err != nil {
		return a.err
	}
	return p.Provider.CancelNow(ctx, ref, key)
}

func (p *provider) VoidInvoice(_ context.Context, ref port.InvoiceRef, key string) error {
	return p.take("VoidInvoice", "VoidInvoice "+ref.ProviderInvoiceID+" "+key, key).err
}

func (p *provider) ListSince(_ context.Context, kind port.ReconKind, _ time.Time, cursor string) (port.ReconPage, error) {
	if a := p.take("ListSince", "ListSince "+string(kind)+" "+cursor, ""); a.err != nil {
		return port.ReconPage{}, a.err
	}
	idx := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "p%d", &idx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if pages := p.recon[kind]; idx < len(pages) {
		return pages[idx], nil
	}
	return port.ReconPage{}, nil
}

func (p *provider) FetchCurrent(ctx context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	p.mu.Lock()
	err := p.fetchErr[obj.ID]
	p.mu.Unlock()
	if a := p.take("FetchCurrent", "FetchCurrent "+obj.ID, ""); a.err != nil {
		return domain.ProviderObjectState{}, a.err
	}
	if err != nil {
		return domain.ProviderObjectState{}, err
	}
	if obj.Kind == domain.ObjectCheckoutSession {
		return domain.ProviderObjectState{Ref: obj, Status: "complete", Fields: map[string]any{"mode": "setup", "setup_payment_method": p.setupPM}}, nil
	}
	if obj.Kind == domain.ObjectInvoice {
		return domain.ProviderObjectState{Ref: obj, Status: "open", Metadata: map[string]string{}, Fields: map[string]any{
			"currency": "USD", "amount_due_minor": int64(1000), "kind": "subscription_cycle"}}, nil
	}
	return p.Provider.FetchCurrent(ctx, obj)
}

func (p *provider) AttachDefaultPaymentMethod(ctx context.Context, c port.CustomerRef, pm, key string) error {
	if a := p.take("AttachDefaultPaymentMethod", "AttachDefault! "+pm+" "+key, key); a.err != nil {
		return a.err
	}
	return p.Provider.AttachDefaultPaymentMethod(ctx, c, pm, key)
}

func (p *provider) ListPaymentMethods(context.Context, port.CustomerRef) ([]port.PaymentMethodView, error) {
	a := p.take("ListPaymentMethods", "ListPaymentMethods", "")
	return p.pms, a.err
}

func (p *provider) CreatePaymentMethodSetup(_ context.Context, in port.SetupInput, key string) (port.HostedSession, error) {
	a := p.take("CreatePaymentMethodSetup", "CreatePaymentMethodSetup "+in.SuccessURL+" "+key, key)
	return port.HostedSession{ProviderSessionID: "cs_setup_" + key, URL: "https://checkout.example/setup", ExpiresAt: in.ExpiresAt}, a.err
}

func (p *provider) InvoicePDFLink(_ context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	a := p.take("InvoicePDFLink", "InvoicePDFLink "+ref.ProviderInvoiceID, "")
	return port.URLWithExpiry{URL: "https://pdf.example/" + ref.ProviderInvoiceID}, a.err
}

func (p *provider) UpdateCustomer(_ context.Context, _ port.CustomerRef, in port.CustomerInput, key string) error {
	return p.take("UpdateCustomer", "UpdateCustomer "+in.Email+" "+key, key).err
}

// registry is the provider lookup with providers that can be unloaded
// (a provider disabled after the account was created).
type registry struct {
	m      map[string]port.PaymentProvider
	hidden map[string]bool
}

func (r *registry) Loaded(n string) (port.PaymentProvider, bool) {
	if r.hidden[n] {
		return nil, false
	}
	p, ok := r.m[n]
	return p, ok
}

func (r *registry) ForNewAccount(n string) (port.PaymentProvider, error) {
	if p, ok := r.Loaded(n); ok {
		return p, nil
	}
	return nil, port.ProviderNotEnabled(n, "is not enabled")
}

// core is the Core client (I-11 seat usage, I-18 plan-change check).
type core struct {
	usage    port.SeatUsage
	usageErr error
	check    port.PlanChangeCheck
	checkErr error
}

func (c *core) SeatUsage(context.Context, string) (port.SeatUsage, error) { return c.usage, c.usageErr }
func (c *core) PlanChangeCheck(context.Context, string, domain.PlanCode) (port.PlanChangeCheck, error) {
	return c.check, c.checkErr
}

// rp is the Realm Provisioner: RP-3 accepts, RP-4 answers are scripted.
type rp struct {
	mu      sync.Mutex
	reverts []domain.RP3Answer
	keys    []string
}

func (r *rp) Convert(context.Context, string, port.ConvertRequest, string) domain.RP3Answer {
	return domain.RP3Answer{Status: 200}
}

func (r *rp) RevertConversion(_ context.Context, _, _, key string) domain.RP3Answer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
	if len(r.reverts) == 0 {
		return domain.RP3Answer{Status: 200}
	}
	a := r.reverts[0]
	r.reverts = r.reverts[1:]
	return a
}

func (r *rp) Realm(context.Context, string) (port.RealmView, error) { return port.RealmView{}, nil }

// ent is Entitlements' token quota read.
type ent struct {
	mu    sync.Mutex
	limit *int64
	elig  string
	err   error
	calls int
	hook  func() // runs once, before the next answer (a change racing the read)
}

func (e *ent) TokenQuota(context.Context, string) (port.TokenQuota, error) {
	e.mu.Lock()
	hook := e.hook
	e.hook = nil
	e.calls++
	q, err := port.TokenQuota{MonthlyLimit: e.limit, Eligibility: e.elig}, e.err
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
	return q, err
}

// logger records what the use cases log.
type logger struct {
	mu    sync.Mutex
	lines []string
}

func (l *logger) add(level, msg string, f map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf("%s %s %v", level, msg, f))
}
func (l *logger) Debug(m string, f map[string]any) { l.add("DEBUG", m, f) }
func (l *logger) Info(m string, f map[string]any)  { l.add("INFO", m, f) }
func (l *logger) Warn(m string, f map[string]any)  { l.add("WARN", m, f) }
func (l *logger) Error(m string, f map[string]any) { l.add("ERROR", m, f) }

// has reports a logged line with level and every fragment.
func (l *logger) has(level string, frags ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
next:
	for _, line := range l.lines {
		if !strings.HasPrefix(line, level+" ") {
			continue
		}
		for _, f := range frags {
			if !strings.Contains(line, f) {
				continue next
			}
		}
		return true
	}
	return false
}

// ── environment ─────────────────────────────────────────────────────────

type env struct {
	svc  *app.Service
	p    *provider
	reg  *registry
	core *core
	rp   *rp
	ent  *ent
	log  *logger
	f    *faults
	now  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fakes := testkit.NewFakes()
	svc, err := testkit.NewService(db, fakes)
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(5_000_000)
	e := &env{svc: svc, p: newProvider(), core: &core{usage: port.SeatUsage{ActiveUsers: 1}, check: port.PlanChangeCheck{Allowed: true}},
		rp: &rp{}, ent: &ent{limit: &limit}, log: &logger{}, f: &faults{}, now: time.Now().UTC().Truncate(time.Microsecond)}
	e.reg = &registry{m: map[string]port.PaymentProvider{"stripe": e.p, "manual": fakes.Registry()["manual"]}, hidden: map[string]bool{}}
	f := e.f
	svc.Accounts, svc.Subs, svc.Prices = accounts{svc.Accounts, f}, subs{svc.Subs, f}, prices{svc.Prices, f}
	svc.Checkouts, svc.Invoices, svc.Changes = checkouts{svc.Checkouts, f}, invoices{svc.Invoices, f}, changes{svc.Changes, f}
	svc.Previews, svc.SeatPurchases, svc.Schedules = previews{svc.Previews, f}, seatPurchases{svc.SeatPurchases, f}, schedules{svc.Schedules, f}
	svc.Addons, svc.Money, svc.Recon = addons{svc.Addons, f}, money{svc.Money, f}, recon{svc.Recon, f}
	svc.Ledger, svc.Customers, svc.Sagas = ledgerReader{svc.Ledger, f}, customers{svc.Customers, f}, sagas{svc.Sagas, f}
	svc.Events, svc.Audit, svc.Lock = events{svc.Events, f}, audit{svc.Audit, f}, locker{svc.Lock, f}
	svc.Providers, svc.Core, svc.RP, svc.Entitlements, svc.Log = e.reg, e.core, e.rp, e.ent, e.log
	svc.Now = func() time.Time { return e.now }
	t.Cleanup(func() {
		if p := f.pending(); len(p) > 0 {
			t.Errorf("armed faults never fired: %v", p)
		}
	})
	return e
}

// fail arms a storage fault on the named port method (skip calls pass first).
func (e *env) fail(name string, skip int) { e.f.set(name, skip, errInjected) }

// absent makes the named (value, found, error) port method answer "not found" once.
func (e *env) absent(name string, skip int) { e.f.set(name, skip, errAbsent) }

func newTenant() string { return uuid.NewString() }

var (
	owner    = app.Caller{UserID: "4b1e8a52-1111-4222-8333-944455556666", Roles: []string{domain.RoleTenantOwner}}
	operator = app.Caller{UserID: "9a9a9a9a-1111-4222-8333-944455556666", Roles: []string{domain.RolePlatformOperator}}
)

func withKey(c app.Caller, key string) app.Caller { c.IdempotencyKey = key; return c }

// activeSubscription runs B-I1 (direct signup, paid at once) for a new
// tenant: an active Stripe subscription, saga awaiting_tenant_converted.
func (e *env) activeSubscription(t *testing.T, plan string, seats int) (string, string) {
	t.Helper()
	tenant := newTenant()
	res, err := e.svc.AuthorizeAndCharge(context.Background(), app.Caller{Principal: "bff"}, app.AuthorizeChargeRequest{TenantID: tenant,
		PlanCode: plan, LicensedSeats: seats, PaymentMethodRef: "pm_1"})
	if err != nil {
		t.Fatal(err)
	}
	return tenant, res.BillingSubscriptionID
}

// converted is an active subscription whose IAM lifecycle is active (after TenantConverted).
func (e *env) converted(t *testing.T, plan string, seats int) (string, string) {
	t.Helper()
	tenant, sub := e.activeSubscription(t, plan, seats)
	exec(t, `UPDATE billing_accounts SET iam_lifecycle = 'active' WHERE tenant_id = $1::uuid`, tenant)
	return tenant, sub
}

// handle runs a callback handler as the webhook processor does: in a tenant transaction.
func (e *env) handle(tenant string, h port.PaymentEventHandler, ev domain.PaymentEvent, cur domain.ProviderObjectState) (string, error) {
	var out string
	err := pgadapter.NewTxRunner(db.App).RunInTenantTx(context.Background(), tenant, func(ctx context.Context) error {
		var err error
		out, err = h.Handle(ctx, tenant, ev, cur)
		return err
	})
	return out, err
}

func callback(id, typ string, kind domain.NormalisedKind) domain.PaymentEvent {
	return domain.PaymentEvent{Provider: "stripe", ProviderEventID: id, EventType: typ, Kind: kind, ProviderCreatedAt: time.Now()}
}

// invoiceState is a re-read provider invoice for subscription sub.
func invoiceState(id, sub, status, reason string, start time.Time, qty int64) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: id}, Status: status,
		Fields: map[string]any{"subscription": "sub_" + sub, "billing_reason": reason, "amount_due_minor": int64(69000),
			"amount_paid_minor": int64(0), "currency": "USD", "line_period_start": start, "line_period_end": start.AddDate(0, 1, 0),
			"line_quantity": qty, "next_payment_attempt": start.Add(72 * time.Hour)}}
}

// mirror mirrors a provider invoice of kind and status for the tenant and
// returns Billing's invoice id.
func (e *env) mirror(t *testing.T, tenant, sub, providerID, kind, status string, amount int64) string {
	t.Helper()
	cur := invoiceState(providerID, sub, status, "subscription_create", e.now, 1)
	if sub == "" {
		delete(cur.Fields, "subscription")
	}
	paid := int64(0)
	if status == "paid" {
		paid = amount
	}
	cur.Fields["kind"], cur.Fields["amount_due_minor"], cur.Fields["amount_paid_minor"] = kind, amount, paid
	if _, err := e.handle(tenant, app.InvoiceUpdated{S: e.svc}, callback("evt_m_"+providerID, "invoice."+status, domain.KindInvoiceUpdated), cur); err != nil {
		t.Fatal(err)
	}
	return scalar[string](t, `SELECT invoice_id::text FROM invoices WHERE provider_invoice_id = $1`, providerID)
}

func outbox(t *testing.T, tenant, eventType string) int {
	t.Helper()
	return count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = $2`, tenant, eventType)
}

// isCode asserts err carries the §17 code.
func isCode(t *testing.T, what string, err error, code domain.ErrorCode) {
	t.Helper()
	if !domain.HasCode(err, code) {
		t.Fatalf("%s: want %s, got %v", what, code, err)
	}
}

// isInjected asserts err is the injected storage fault.
func isInjected(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errInjected) {
		t.Fatalf("%s: want the injected fault, got %v", what, err)
	}
}

// ignored asserts the handler ignored the callback with reason.
func ignored(t *testing.T, what string, err error, reason string) {
	t.Helper()
	var ig *port.IgnoredEvent
	if !errors.As(err, &ig) || ig.Reason != reason {
		t.Fatalf("%s: want ignored %q, got %v", what, reason, err)
	}
}

func ptr[T any](v T) *T { return &v }
