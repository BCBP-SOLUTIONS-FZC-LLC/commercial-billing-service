//go:build integration

package certify

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Shared containers: a scratch Postgres 17 server (superuser DSN, the run
// creates its own databases) and stripe-mock.
var (
	serverDSN  string
	stripeMock string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres:", err)
		os.Exit(1)
	}
	serverDSN, _ = pg.ConnectionString(ctx, "sslmode=disable")
	sm, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{Image: "stripe/stripe-mock:latest", ExposedPorts: []string{"12111/tcp"},
			WaitingFor: wait.ForListeningPort("12111/tcp").WithStartupTimeout(60 * time.Second)},
		Started: true,
	})
	if err != nil {
		_ = pg.Terminate(ctx)
		fmt.Fprintln(os.Stderr, "stripe-mock:", err)
		os.Exit(1)
	}
	host, _ := sm.Host(ctx)
	p, _ := sm.MappedPort(ctx, "12111")
	stripeMock = fmt.Sprintf("http://%s:%s", host, p.Port())
	code := m.Run()
	_ = sm.Terminate(ctx)
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

type nopLog struct{}

func (nopLog) Debug(string, map[string]any) {}
func (nopLog) Info(string, map[string]any)  {}
func (nopLog) Warn(string, map[string]any)  {}
func (nopLog) Error(string, map[string]any) {}

var runSeq int

func runID(prefix string) string {
	runSeq++
	return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano()%1_000_000_000, runSeq)
}

// ── fault-injecting provider ─────────────────────────────────────────────

// ctl wraps a provider: a method listed in fail answers its error once it
// was called more than skip times; the override fields replace answers.
type ctl struct {
	port.PaymentProvider

	mu    sync.Mutex
	fail  map[string]failRule
	calls map[string]int

	fetch    func(domain.ProviderObjectRef) (domain.ProviderObjectState, error, bool)
	list     func(call int) (port.ReconPage, bool)
	charge   *port.PaymentOutcome
	change   *port.ChangeResult
	schedule *port.ScheduleRef
	methods  []port.PaymentMethodView
	setup    *port.HostedSession
	pdf      *port.URLWithExpiry
	caps     *port.ProviderCapabilities
}

type failRule struct {
	err  error
	skip int
}

func wrap(p port.PaymentProvider) *ctl {
	return &ctl{PaymentProvider: p, fail: map[string]failRule{}, calls: map[string]int{}}
}

func (c *ctl) failOn(method string, skip int) *ctl {
	c.fail[method] = failRule{err: fmt.Errorf("injected %s failure", method), skip: skip}
	return c
}

func (c *ctl) hit(method string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[method]++
	if r, ok := c.fail[method]; ok && c.calls[method] > r.skip {
		return c.calls[method], r.err
	}
	return c.calls[method], nil
}

func (c *ctl) Capabilities() port.ProviderCapabilities {
	if c.caps != nil {
		return *c.caps
	}
	return c.PaymentProvider.Capabilities()
}

func (c *ctl) CreateSubscription(ctx context.Context, in port.SubscriptionInput, k string) (port.SubscriptionResult, error) {
	if _, err := c.hit("CreateSubscription"); err != nil {
		return port.SubscriptionResult{}, err
	}
	return c.PaymentProvider.CreateSubscription(ctx, in, k)
}

func (c *ctl) FetchCurrent(ctx context.Context, ref domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	if _, err := c.hit("FetchCurrent"); err != nil {
		return domain.ProviderObjectState{}, err
	}
	if c.fetch != nil {
		if st, err, ok := c.fetch(ref); ok {
			return st, err
		}
	}
	return c.PaymentProvider.FetchCurrent(ctx, ref)
}

func (c *ctl) ListSince(ctx context.Context, kind port.ReconKind, since time.Time, cursor string) (port.ReconPage, error) {
	n, err := c.hit("ListSince")
	if err != nil {
		return port.ReconPage{}, err
	}
	if c.list != nil {
		if p, ok := c.list(n); ok {
			return p, nil
		}
	}
	return c.PaymentProvider.ListSince(ctx, kind, since, cursor)
}

func (c *ctl) PreviewQuantityChange(ctx context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	if _, err := c.hit("PreviewQuantityChange"); err != nil {
		return port.ProrationPreview{}, err
	}
	return c.PaymentProvider.PreviewQuantityChange(ctx, in)
}

func (c *ctl) PreviewPriceChange(ctx context.Context, in port.PriceChange) (port.ProrationPreview, error) {
	if _, err := c.hit("PreviewPriceChange"); err != nil {
		return port.ProrationPreview{}, err
	}
	return c.PaymentProvider.PreviewPriceChange(ctx, in)
}

func (c *ctl) ChangeQuantity(ctx context.Context, in port.QuantityChange, k string) (port.ChangeResult, error) {
	if _, err := c.hit("ChangeQuantity"); err != nil {
		return port.ChangeResult{}, err
	}
	if c.change != nil {
		return *c.change, nil
	}
	return c.PaymentProvider.ChangeQuantity(ctx, in, k)
}

func (c *ctl) ChangePrice(ctx context.Context, in port.PriceChange, k string) (port.ChangeResult, error) {
	if _, err := c.hit("ChangePrice"); err != nil {
		return port.ChangeResult{}, err
	}
	return c.PaymentProvider.ChangePrice(ctx, in, k)
}

func (c *ctl) ScheduleAtPeriodEnd(ctx context.Context, in port.PeriodEndChange, k string) (port.ScheduleRef, error) {
	if _, err := c.hit("ScheduleAtPeriodEnd"); err != nil {
		return port.ScheduleRef{}, err
	}
	if c.schedule != nil {
		return *c.schedule, nil
	}
	return c.PaymentProvider.ScheduleAtPeriodEnd(ctx, in, k)
}

func (c *ctl) SetCancelAtPeriodEnd(ctx context.Context, ref port.SubscriptionRef, cancel bool, k string) error {
	if _, err := c.hit("SetCancelAtPeriodEnd"); err != nil {
		return err
	}
	return c.PaymentProvider.SetCancelAtPeriodEnd(ctx, ref, cancel, k)
}

func (c *ctl) CancelNow(ctx context.Context, ref port.SubscriptionRef, k string) error {
	if _, err := c.hit("CancelNow"); err != nil {
		return err
	}
	return c.PaymentProvider.CancelNow(ctx, ref, k)
}

func (c *ctl) PauseCollection(ctx context.Context, ref port.SubscriptionRef, pause bool, k string) error {
	if _, err := c.hit("PauseCollection"); err != nil {
		return err
	}
	return c.PaymentProvider.PauseCollection(ctx, ref, pause, k)
}

func (c *ctl) AttachDefaultPaymentMethod(ctx context.Context, ref port.CustomerRef, pm, k string) error {
	if _, err := c.hit("AttachDefaultPaymentMethod"); err != nil {
		return err
	}
	return c.PaymentProvider.AttachDefaultPaymentMethod(ctx, ref, pm, k)
}

func (c *ctl) ListPaymentMethods(ctx context.Context, ref port.CustomerRef) ([]port.PaymentMethodView, error) {
	if _, err := c.hit("ListPaymentMethods"); err != nil {
		return nil, err
	}
	if c.methods != nil {
		return c.methods, nil
	}
	return c.PaymentProvider.ListPaymentMethods(ctx, ref)
}

func (c *ctl) CreatePaymentMethodSetup(ctx context.Context, in port.SetupInput, k string) (port.HostedSession, error) {
	if _, err := c.hit("CreatePaymentMethodSetup"); err != nil {
		return port.HostedSession{}, err
	}
	if c.setup != nil {
		return *c.setup, nil
	}
	return c.PaymentProvider.CreatePaymentMethodSetup(ctx, in, k)
}

func (c *ctl) ChargeOneOff(ctx context.Context, in port.OneOffCharge, k string) (port.ChargeResult, error) {
	if _, err := c.hit("ChargeOneOff"); err != nil {
		return port.ChargeResult{}, err
	}
	if c.charge != nil {
		r, err := c.PaymentProvider.ChargeOneOff(ctx, in, k)
		r.Outcome = *c.charge
		return r, err
	}
	return c.PaymentProvider.ChargeOneOff(ctx, in, k)
}

func (c *ctl) GetInvoice(ctx context.Context, ref port.InvoiceRef) (port.InvoiceView, error) {
	if _, err := c.hit("GetInvoice"); err != nil {
		return port.InvoiceView{}, err
	}
	return c.PaymentProvider.GetInvoice(ctx, ref)
}

func (c *ctl) InvoicePDFLink(ctx context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	if _, err := c.hit("InvoicePDFLink"); err != nil {
		return port.URLWithExpiry{}, err
	}
	if c.pdf != nil {
		return *c.pdf, nil
	}
	return c.PaymentProvider.InvoicePDFLink(ctx, ref)
}

func (c *ctl) Refund(ctx context.Context, in port.RefundInput, k string) (port.RefundResult, error) {
	if _, err := c.hit("Refund"); err != nil {
		return port.RefundResult{}, err
	}
	return c.PaymentProvider.Refund(ctx, in, k)
}

// ── clock, ledger and event-API faults ───────────────────────────────────

// hookClock is the run's clock with an injected failure (from the
// failAfter-th advance on) and a hook standing in for the provider's
// renewal effects.
type hookClock struct {
	Clock
	failAfter int // 0: never
	advances  int
	onAdvance func(ctx context.Context)
}

func (c *hookClock) Advance(ctx context.Context, to time.Time) error {
	c.advances++
	if c.failAfter > 0 && c.advances >= c.failAfter {
		return fmt.Errorf("injected clock failure")
	}
	if err := c.Clock.Advance(ctx, to); err != nil {
		return err
	}
	if c.onAdvance != nil {
		c.onAdvance(ctx)
	}
	return nil
}

// faultLedger fails Insert, or Claim after claimOK claims.
type faultLedger struct {
	port.LedgerRepo
	insertErr bool
	claimOK   int // -1: never fail
	claims    int
	onFinish  func(ctx context.Context)
}

func (l *faultLedger) Finish(ctx context.Context, provider, id string, st domain.LedgerStatus, outcome string, next *time.Time) error {
	err := l.LedgerRepo.Finish(ctx, provider, id, st, outcome, next)
	if l.onFinish != nil {
		l.onFinish(ctx)
	}
	return err
}

func (l *faultLedger) Insert(ctx context.Context, e domain.LedgerEntry) (bool, error) {
	if l.insertErr {
		return false, fmt.Errorf("injected ledger insert failure")
	}
	return l.LedgerRepo.Insert(ctx, e)
}

func (l *faultLedger) Claim(ctx context.Context, n int, d time.Duration) ([]domain.LedgerEntry, error) {
	l.claims++
	if l.claimOK >= 0 && l.claims > l.claimOK {
		return nil, fmt.Errorf("injected claim failure")
	}
	return l.LedgerRepo.Claim(ctx, n, d)
}

// eventProxy fronts stripe-mock for raw event reads; GET /v1/events/<id>
// fails once id was fetched okFor times.
type eventProxy struct {
	mu      sync.Mutex
	okFor   map[string]int
	fetched map[string]int
	srv     *httptest.Server
}

func newEventProxy(t *testing.T) *eventProxy {
	t.Helper()
	u, _ := url.Parse(stripeMock)
	rp := httputil.NewSingleHostReverseProxy(u)
	p := &eventProxy{okFor: map[string]int{}, fetched: map[string]int{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/events/")
		p.mu.Lock()
		p.fetched[id]++
		n, limited := p.okFor[id]
		over := limited && p.fetched[id] > n
		p.mu.Unlock()
		if over {
			http.Error(w, `{"error":{"type":"api_error"}}`, http.StatusInternalServerError)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// scripted answers B-W1 deliveries with codes in order (then 200).
type scripted struct {
	codes []int
	n     int
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	code := http.StatusOK
	if s.n < len(s.codes) {
		code = s.codes[s.n]
	}
	s.n++
	w.WriteHeader(code)
}

// ── the fixture ──────────────────────────────────────────────────────────

// fixture is one stripe-mock Setup whose provider, clock, ledger and
// delivery can be rewired per scenario.
type fixture struct {
	t         *testing.T
	base      *Env
	raw       port.PaymentProvider // the real Stripe adapter on stripe-mock
	schedules port.ScheduleRepo
}

// faultSchedules fails Insert, or Due once it answered dueOK times.
type faultSchedules struct {
	port.ScheduleRepo
	insertErr bool
	dueOK     int // -1: never fail
	dues      int
}

func (r *faultSchedules) Insert(ctx context.Context, s domain.SeatChangeSchedule) error {
	if r.insertErr {
		return fmt.Errorf("injected schedule insert failure")
	}
	return r.ScheduleRepo.Insert(ctx, s)
}

func (r *faultSchedules) Due(ctx context.Context, until time.Time, limit int) ([]domain.SeatChangeSchedule, error) {
	r.dues++
	if r.dueOK >= 0 && r.dues > r.dueOK {
		return nil, fmt.Errorf("injected schedule read failure")
	}
	return r.ScheduleRepo.Due(ctx, until, limit)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	env, teardown, err := Setup(ctx, Options{Target: TargetStripeMock, ServerDSN: serverDSN, StripeKey: "sk_test_123",
		StripeBaseURL: stripeMock, RunID: runID("fx"), Log: nopLog{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(teardown)
	return &fixture{t: t, base: env, raw: env.Provider.(*mockState).PaymentProvider, schedules: env.Svc.Schedules}
}

// scenario is one rewiring: inner sits below the stripe-mock state layer
// (its failures pass through it), outer above it (its answers are final).
type scenario struct {
	e            *Env
	inner, outer *ctl
	ms           *mockState
	clock        *hookClock
	ledger       *faultLedger
	schedules    *faultSchedules
	proxy        *eventProxy
}

type wiring struct {
	svcOuter, caseOuter, deliverOuter, processorOuter bool
	target                                            Target
}

func (f *fixture) scenario(w wiring) *scenario {
	b := f.base
	s := &scenario{inner: wrap(f.raw)}
	s.ms = newMockState(s.inner, b.Clock.Now)
	s.outer = wrap(s.ms)
	pick := func(outer bool) port.PaymentProvider {
		if outer {
			return s.outer
		}
		return s.ms
	}
	e := *b
	if w.target == "" {
		w.target = TargetTestMode
	}
	e.Target = w.target
	e.Provider = pick(w.caseOuter)
	s.clock = &hookClock{Clock: b.Clock}
	e.Clock = s.clock
	b.Svc.Providers = Registry{P: pick(w.svcOuter)}
	s.schedules = &faultSchedules{ScheduleRepo: f.schedules, dueOK: -1}
	b.Svc.Schedules = s.schedules
	s.ledger = &faultLedger{LedgerRepo: b.Deliver.Processor.Ledger, claimOK: -1}
	proc := *b.Deliver.Processor
	proc.Providers, proc.Ledger = Registry{P: pick(w.processorOuter)}, s.ledger
	d := *b.Deliver
	d.Processor, d.Provider = &proc, pick(w.deliverOuter)
	s.proxy = newEventProxy(f.t)
	d.API.BaseURL = s.proxy.srv.URL
	e.Deliver = &d
	s.e = &e
	return s
}

// brokenSuper makes the suite's superuser statements fail.
func (s *scenario) brokenSuper(t *testing.T) {
	t.Helper()
	p, err := pgcommon.NewPool(context.Background(), pgcommon.Config{DSN: s.e.DB.DSN(), PoolName: "closed", MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	db := *s.e.DB
	db.Super = p
	s.e.DB = &db
}

func (s *scenario) sql(t *testing.T, stmt string, args ...any) {
	t.Helper()
	if err := s.e.DB.Exec(context.Background(), stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// check runs c and compares its error with want ("" = pass).
func check(t *testing.T, name string, c func(context.Context, *Env) (Evidence, error), e *Env, want string) Evidence {
	t.Helper()
	ev, err := c(context.Background(), e)
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: want pass, got %v", name, err)
	case want != "" && err == nil:
		t.Errorf("%s: want error containing %q, passed", name, want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("%s: want error containing %q, got %v", name, want, err)
	}
	return ev
}
