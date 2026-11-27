package certify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// stubProv is a provider whose calls answer what the test configured. An
// unset method panics through the nil embedded interface.
type stubProv struct {
	port.PaymentProvider
	err      error
	status   port.PaymentStatus
	pending  bool
	fetch    domain.ProviderObjectState
	pages    []port.ReconPage
	listErr  error
	listCall int
}

func (s *stubProv) Name() string           { return "stripe" }
func (s *stubProv) AdapterVersion() string { return "1.2.3" }
func (s *stubProv) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{Proration: port.ProrationNative}
}
func (s *stubProv) CreateSubscription(context.Context, port.SubscriptionInput, string) (port.SubscriptionResult, error) {
	r := port.SubscriptionResult{}
	r.Ref.ProviderSubscriptionID = "sub_1"
	return r, s.err
}
func (s *stubProv) ConfirmInitialPayment(context.Context, port.SubscriptionRef, string) (port.PaymentOutcome, error) {
	return port.PaymentOutcome{Status: s.status}, s.err
}
func (s *stubProv) FetchCurrent(context.Context, domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	return s.fetch, s.err
}
func (s *stubProv) ChangeQuantity(context.Context, port.QuantityChange, string) (port.ChangeResult, error) {
	return port.ChangeResult{Pending: s.pending, Outcome: port.PaymentOutcome{Status: s.status}}, s.err
}
func (s *stubProv) ChangePrice(context.Context, port.PriceChange, string) (port.ChangeResult, error) {
	return port.ChangeResult{Pending: s.pending, Outcome: port.PaymentOutcome{Status: s.status}}, s.err
}
func (s *stubProv) ChargeOneOff(context.Context, port.OneOffCharge, string) (port.ChargeResult, error) {
	return port.ChargeResult{Outcome: port.PaymentOutcome{Status: s.status}}, s.err
}
func (s *stubProv) Refund(context.Context, port.RefundInput, string) (port.RefundResult, error) {
	return port.RefundResult{ProviderRefundID: "re_real", Status: "succeeded"}, s.err
}
func (s *stubProv) InvoicePDFLink(context.Context, port.InvoiceRef) (port.URLWithExpiry, error) {
	return port.URLWithExpiry{URL: "https://real/pdf"}, s.err
}
func (s *stubProv) ListSince(context.Context, port.ReconKind, time.Time, string) (port.ReconPage, error) {
	if s.listErr != nil {
		return port.ReconPage{}, s.listErr
	}
	if s.listCall >= len(s.pages) {
		return port.ReconPage{}, nil
	}
	s.listCall++
	return s.pages[s.listCall-1], nil
}

func TestCasesPerProrationMode(t *testing.T) {
	native, fallback := Cases(port.ProrationNative), Cases(port.ProrationFallback)
	if len(native) != len(fallback) || len(native) != 31 {
		t.Fatalf("native %d, fallback %d cases, want 31 each", len(native), len(fallback))
	}
	pc := fallback[len(fallback)-2]
	if pc.Capability != "proration" || pc.ID != "calculator_charged" || !pc.Required {
		t.Fatalf("fallback proration case %+v", pc)
	}
	if _, err := pc.Run(context.Background(), &Env{}); err == nil || !strings.Contains(err.Error(), "Proration = fallback") {
		t.Fatalf("fallback case: %v", err)
	}
	if native[len(native)-2].ID != "preview_equals_charge" || native[len(native)-1].Required {
		t.Fatalf("native tail %+v %+v", native[len(native)-2], native[len(native)-1])
	}
}

func TestOrMissing(t *testing.T) {
	if _, err := orMissing(nil)(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("missing implementation: %v", err)
	}
	ok := func(context.Context, *Env) (Evidence, error) { return Evidence{"x": 1}, nil }
	if ev, err := orMissing(ok)(context.Background(), nil); err != nil || ev["x"] != 1 {
		t.Fatalf("implemented: %v %v", ev, err)
	}
}

func TestHelpers(t *testing.T) {
	if err := expect("n", 1, 2); err == nil || err.Error() != "n: got 1, want 2" {
		t.Errorf("expect mismatch: %v", err)
	}
	if expect("n", "a", "a") != nil {
		t.Error("expect equal")
	}
	if !looksLikePAN("card 4242424242424242 used") || looksLikePAN("1234-5678-9012-3456") || looksLikePAN("") {
		t.Error("looksLikePAN")
	}
	if providerAPIVersion("1.2.3") != "1.2.3" || providerAPIVersion("1.2.3+stripeapi.2024-06-20") != "2024-06-20" {
		t.Error("providerAPIVersion")
	}
	if safe("a/b c") != "a_b_c" {
		t.Error("safe")
	}
	if StripeAdapterVersion() == "" {
		t.Error("no adapter version")
	}
	if prorationCase(string(port.ProrationFallback)) != "calculator_charged" || prorationCase("native") != "preview_equals_charge" {
		t.Error("prorationCase")
	}
	if withDB("postgres://%zz", "x", "", "") != "postgres://%zz" {
		t.Error("unparsable DSN must be returned unchanged")
	}
	if got := withDB("postgres://u:p@h:5432/postgres?sslmode=disable", "certify_x", "billing_app", "pw"); got != "postgres://billing_app:pw@h:5432/certify_x?sslmode=disable" {
		t.Errorf("withDB %s", got)
	}
	if e := (&Env{Target: TargetTestMode}); !e.Stateful() {
		t.Error("test mode is stateful")
	}
}

func TestMissing(t *testing.T) {
	if m := Missing(Record{SuiteVersion: "cert-9.9"}); len(m) != 1 || m[0] != "unknown suite cert-9.9" {
		t.Errorf("unknown suite: %v", m)
	}
	rec := Record{SuiteVersion: SuiteVersion, ProrationMode: string(port.ProrationFallback)}
	for group, cases := range domain.CertSuite[SuiteVersion] {
		for _, c := range cases {
			rec.Results = append(rec.Results, app.CertificationResult{Capability: group, Case: c, Passed: true})
		}
	}
	m := Missing(rec)
	if len(m) != 1 || m[0] != "proration/calculator_charged" {
		t.Errorf("fallback without its proration case: %v", m)
	}
	rec.Results = append(rec.Results, app.CertificationResult{Capability: "proration", Case: "calculator_charged", Passed: true})
	if m := Missing(rec); len(m) != 0 {
		t.Errorf("complete fallback record: %v", m)
	}
}

func TestStubs(t *testing.T) {
	ctx := context.Background()
	c := FakeClock{t: time.Unix(100, 0)}
	_ = c.Advance(ctx, time.Unix(50, 0))
	if !c.Now().Equal(time.Unix(100, 0)) {
		t.Error("fake clock moved backwards")
	}
	_ = c.Advance(ctx, time.Unix(200, 0))
	if !c.Now().Equal(time.Unix(200, 0)) {
		t.Error("fake clock did not advance")
	}
	core := &CoreStub{Check: port.PlanChangeCheck{Allowed: true}}
	core.SetUsage(3, 5)
	if u, _ := core.SeatUsage(ctx, "t"); u.ActiveUsers != 3 || u.LicensedSeats != 5 {
		t.Errorf("usage %+v", u)
	}
	if ch, _ := core.PlanChangeCheck(ctx, "t", domain.PlanPro); !ch.Allowed {
		t.Error("check")
	}
	if a := (RPStub{}).Convert(ctx, "t", port.ConvertRequest{}, "k"); a.Status != 200 {
		t.Error("convert")
	}
	if a := (RPStub{}).RevertConversion(ctx, "t", "c", "k"); a.Status != 200 {
		t.Error("revert")
	}
	if _, err := (RPStub{}).Realm(ctx, "t"); err != nil {
		t.Error(err)
	}
	if q, _ := (EntitlementsStub{}).TokenQuota(ctx, "t"); q.MonthlyLimit == nil || *q.MonthlyLimit != 5_000_000 || q.Eligibility != "eligible" {
		t.Errorf("quota %+v", q)
	}
	reg := Registry{P: &stubProv{}}
	if p, err := reg.ForNewAccount("stripe"); err != nil || p == nil {
		t.Errorf("stripe: %v", err)
	}
	if _, err := reg.ForNewAccount("manual"); err == nil {
		t.Error("a provider not under certification was served")
	}
}

// ── mockState ────────────────────────────────────────────────────────────

func TestMockStatePassesProviderErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	m := newMockState(&stubProv{err: boom}, time.Now)
	if _, err := m.CreateSubscription(ctx, port.SubscriptionInput{}, "k"); !errors.Is(err, boom) {
		t.Errorf("CreateSubscription %v", err)
	}
	if _, err := m.ConfirmInitialPayment(ctx, port.SubscriptionRef{}, "k"); !errors.Is(err, boom) {
		t.Errorf("ConfirmInitialPayment %v", err)
	}
	if _, err := m.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectSubscription}); !errors.Is(err, boom) {
		t.Errorf("FetchCurrent %v", err)
	}
	if _, err := m.ChangeQuantity(ctx, port.QuantityChange{}, "k"); !errors.Is(err, boom) {
		t.Errorf("ChangeQuantity %v", err)
	}
	if _, err := m.ChangePrice(ctx, port.PriceChange{}, "k"); !errors.Is(err, boom) {
		t.Errorf("ChangePrice %v", err)
	}
	if _, err := m.ChargeOneOff(ctx, port.OneOffCharge{}, "k"); !errors.Is(err, boom) {
		t.Errorf("ChargeOneOff %v", err)
	}
	if _, err := m.Refund(ctx, port.RefundInput{}, "k"); !errors.Is(err, boom) {
		t.Errorf("Refund %v", err)
	}
	if _, err := m.InvoicePDFLink(ctx, port.InvoiceRef{}); !errors.Is(err, boom) {
		t.Errorf("InvoicePDFLink %v", err)
	}
	if n := m.TakeNotes(); len(n) != 0 {
		t.Errorf("no substitution expected on errors, got %v", n)
	}
}

func TestMockStateSubstitutions(t *testing.T) {
	ctx := context.Background()
	m := newMockState(&stubProv{status: port.PaymentPaid, fetch: domain.ProviderObjectState{Status: "real"}}, time.Now)
	// A paid answer needs no substitution.
	if o, _ := m.ConfirmInitialPayment(ctx, port.SubscriptionRef{}, "k"); o.Status != port.PaymentPaid {
		t.Error("confirm")
	}
	if r, _ := m.ChargeOneOff(ctx, port.OneOffCharge{}, "k"); r.Outcome.Status != port.PaymentPaid {
		t.Error("charge")
	}
	if r, _ := m.ChangeQuantity(ctx, port.QuantityChange{}, "k"); r.Outcome.Status != port.PaymentPaid || r.Pending {
		t.Error("quantity")
	}
	// A real refund and PDF link pass through unchanged.
	if r, _ := m.Refund(ctx, port.RefundInput{}, "k"); r.ProviderRefundID != "re_real" {
		t.Errorf("refund %+v", r)
	}
	if u, _ := m.InvoicePDFLink(ctx, port.InvoiceRef{}); u.URL != "https://real/pdf" {
		t.Errorf("pdf %+v", u)
	}
	// Objects other than subscriptions and invoices are the provider's.
	if st, err := m.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectRefund, ID: "re_1"}); err != nil || st.Status != "real" {
		t.Errorf("refund object %+v %v", st, err)
	}
	if n := m.TakeNotes(); len(n) != 0 {
		t.Errorf("unexpected substitutions %v", n)
	}
	// Not-found refunds and PDF links are substituted.
	nf := newMockState(&stubProv{err: port.ErrNotFound}, time.Now)
	if r, err := nf.Refund(ctx, port.RefundInput{}, "k1"); err != nil || r.ProviderRefundID != "re_mock_k1" || r.Status != "pending" {
		t.Errorf("refund %+v %v", r, err)
	}
	if u, err := nf.InvoicePDFLink(ctx, port.InvoiceRef{}); err != nil || !strings.HasPrefix(u.URL, "https://") {
		t.Errorf("pdf %+v %v", u, err)
	}
	if n := nf.TakeNotes(); len(n) != 2 {
		t.Errorf("notes %v", n)
	}
}

type fakeLedger struct {
	port.LedgerRepo
	insertErr, claimErr error
	rows                []domain.LedgerEntry
	claims, finishes    int
}

func (f *fakeLedger) Insert(context.Context, domain.LedgerEntry) (bool, error) {
	return f.insertErr == nil, f.insertErr
}
func (f *fakeLedger) Claim(context.Context, int, time.Duration) ([]domain.LedgerEntry, error) {
	f.claims++
	return f.rows, f.claimErr
}
func (f *fakeLedger) Finish(context.Context, string, string, domain.LedgerStatus, string, *time.Time) error {
	f.finishes++
	return nil
}

func TestPaidCallbackFailures(t *testing.T) {
	ctx := context.Background()
	m := newMockState(&stubProv{}, time.Now)
	if err := m.paidCallback(ctx, nil, subscriber{ProviderSub: "sub_unknown"}); err == nil || !strings.Contains(err.Error(), "no first invoice") {
		t.Fatalf("unknown subscription: %v", err)
	}
	if _, err := m.CreateSubscription(ctx, port.SubscriptionInput{}, "k"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("ledger down")
	e := &Env{Deliver: &Deliverer{Processor: &app.WebhookProcessor{Ledger: &fakeLedger{insertErr: boom}}}}
	if err := m.paidCallback(ctx, e, subscriber{Tenant: "t", ProviderSub: "sub_1", Customer: "cus_1"}); !errors.Is(err, boom) {
		t.Fatalf("ledger insert failure: %v", err)
	}
}

// ── Deliverer ────────────────────────────────────────────────────────────

func eventServer(t *testing.T, fail bool) StripeAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, `{"error":{"message":"nope"}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + strings.TrimPrefix(r.URL.Path, "/v1/events/") + `","type":"invoice.paid"}`))
	}))
	t.Cleanup(srv.Close)
	return StripeAPI{Key: "sk_test_x", BaseURL: srv.URL}
}

func item(id string) domain.ProviderObjectState {
	return domain.ProviderObjectState{Ref: domain.ProviderObjectRef{ID: id}}
}

func TestDeliverNew(t *testing.T) {
	ctx := context.Background()
	var posted []string
	handler := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Stripe-Signature") == "" {
				t.Error("unsigned delivery")
			}
			posted = append(posted, r.URL.Path)
			w.WriteHeader(code)
		})
	}
	ledger := &fakeLedger{}
	prov := &stubProv{pages: []port.ReconPage{{Items: []domain.ProviderObjectState{item("evt_1"), item("evt_2")}, NextCursor: "c1"},
		{Items: []domain.ProviderObjectState{item("evt_1"), item("evt_3")}}}}
	d := &Deliverer{API: eventServer(t, false), Secret: "whsec_x", Handler: handler(http.StatusOK), Path: "/hook",
		Processor: &app.WebhookProcessor{Ledger: ledger}, Provider: prov, Now: time.Now}
	ids, err := d.DeliverNew(ctx)
	if err != nil || strings.Join(ids, ",") != "evt_1,evt_2,evt_3" || len(posted) != 3 || ledger.claims != 1 {
		t.Fatalf("ids %v err %v posted %v claims %d", ids, err, posted, ledger.claims)
	}

	boom := errors.New("list down")
	if _, err := (&Deliverer{Provider: &stubProv{listErr: boom}}).DeliverNew(ctx); !errors.Is(err, boom) {
		t.Errorf("list error: %v", err)
	}
	one := func() *stubProv {
		return &stubProv{pages: []port.ReconPage{{Items: []domain.ProviderObjectState{item("evt_9")}}}}
	}
	if _, err := (&Deliverer{API: eventServer(t, true), Provider: one()}).DeliverNew(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("raw event error: %v", err)
	}
	d = &Deliverer{API: eventServer(t, false), Secret: "s", Handler: handler(http.StatusBadRequest), Path: "/hook", Provider: one(), Now: time.Now}
	if _, err := d.DeliverNew(ctx); err == nil || !strings.Contains(err.Error(), "B-W1 answered 400 for evt_9") {
		t.Errorf("refused delivery: %v", err)
	}
}

func TestDrain(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("claim failed")
	if err := (&Deliverer{Processor: &app.WebhookProcessor{Ledger: &fakeLedger{claimErr: boom}}}).Drain(ctx); !errors.Is(err, boom) {
		t.Errorf("claim error: %v", err)
	}
	// A processor that never runs dry stops after 50 batches.
	tenant := "t1"
	l := &fakeLedger{rows: []domain.LedgerEntry{{Provider: "stripe", ProviderEventID: "evt_1", NormalisedKind: string(domain.KindIgnored), TenantID: &tenant}}}
	if err := (&Deliverer{Processor: &app.WebhookProcessor{Ledger: l}}).Drain(ctx); err != nil || l.claims != 50 || l.finishes != 50 {
		t.Errorf("err %v claims %d finishes %d", err, l.claims, l.finishes)
	}
}

// ── Run and WriteEvidence ────────────────────────────────────────────────

func TestRunRecoversPanicsAndNotes(t *testing.T) {
	m := newMockState(&stubProv{}, time.Now)
	e := &Env{Target: TargetStripeMock, Provider: m}
	cases := []Case{
		{Capability: "g", ID: "panics", Run: func(context.Context, *Env) (Evidence, error) { panic("kaboom") }},
		{Capability: "g", ID: "notes", Run: func(context.Context, *Env) (Evidence, error) { m.note("substituted"); return nil, nil }},
	}
	res := Run(context.Background(), e, cases)
	if res[0].Passed || res[0].Error != "panic: kaboom" || res[0].Evidence["target"] != "stripe-mock" {
		t.Errorf("panicking case %+v", res[0])
	}
	if !res[1].Passed || len(res[1].Evidence["mock_substitutions"].([]string)) != 1 {
		t.Errorf("noting case %+v", res[1])
	}
}

func TestWriteEvidenceFailures(t *testing.T) {
	e := &Env{Provider: &stubProv{}}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEvidence(e, nil, file, "s3://b", time.Now(), time.Now(), "acct_1"); err == nil {
		t.Error("evidence dir under a file accepted")
	}
	// A directory where the case file goes makes the write fail.
	if err := os.MkdirAll(filepath.Join(dir, "ev", "stripe", "1.2.3", "g-c.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEvidence(e, []Result{{Capability: "g", Case: "c"}}, filepath.Join(dir, "ev"), "s3://b", time.Now(), time.Now(), "acct_1"); err == nil {
		t.Error("unwritable evidence file accepted")
	}
	rec, err := WriteEvidence(e, []Result{{Capability: "z", Case: "c", Passed: true}, {Capability: "a", Case: "c"}}, filepath.Join(dir, "ok"), "s3://b/", time.Now(), time.Now(), "acct_1")
	if err != nil || rec.ProviderAPIVersion != "1.2.3" || rec.Results[0].Capability != "a" || rec.Results[1].EvidenceRef != "s3://b/stripe/1.2.3/z-c.json" {
		t.Errorf("record %+v %v", rec, err)
	}
}

// ── Setup and the database without a server ──────────────────────────────

func TestSetupRefusals(t *testing.T) {
	ctx := context.Background()
	if _, _, err := Setup(ctx, Options{Target: TargetTestMode, StripeKey: "sk_live_x"}); err == nil || !strings.Contains(err.Error(), "BIL-OPS-7") {
		t.Errorf("live key: %v", err)
	}
	// No base URL and no reachable server: the database step fails first.
	if _, _, err := Setup(ctx, Options{Target: TargetStripeMock, StripeKey: "sk_test_x", ServerDSN: "postgres://u:p@127.0.0.1:1/postgres?connect_timeout=1", RunID: "r1"}); err == nil {
		t.Error("unreachable server accepted")
	}
	if _, err := OpenDB(ctx, "postgres://x", "Bad Name!"); err == nil || !strings.Contains(err.Error(), "unsafe database name") {
		t.Errorf("unsafe run id: %v", err)
	}
	if _, err := Certified(ctx, "::not a dsn", "stripe", "1", SuiteVersion); err == nil {
		t.Error("bad check DSN accepted")
	}
	if err := execOn(ctx, "::not a dsn", "SELECT 1"); err == nil {
		t.Error("bad DSN accepted")
	}
}

// ── StripeAPI and Test Clocks against a local fake ───────────────────────

func TestStripeAPI(t *testing.T) {
	ctx := context.Background()
	var noID atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, _ := r.BasicAuth(); u != "sk_test_k" {
			t.Errorf("auth %q", u)
		}
		switch {
		case r.URL.Path == "/v1/products" && noID.Load():
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/v1/products":
			_, _ = w.Write([]byte(`{"id":"prod_1"}`))
		case r.URL.Path == "/v1/prices":
			if err := r.ParseForm(); err != nil || r.PostForm.Get("product") != "prod_1" || r.PostForm.Get("currency") != "usd" {
				t.Errorf("price form %v", r.PostForm)
			}
			_, _ = w.Write([]byte(`{"id":"price_` + r.PostForm.Get("recurring[interval]") + `"}`))
		case r.URL.Path == "/v1/account":
			_, _ = w.Write([]byte(`{"id":"acct_1"}`))
		default:
			http.Error(w, "nope", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	api := StripeAPI{Key: "sk_test_k", BaseURL: srv.URL + "/", HTTP: srv.Client()}
	if id, err := api.CreatePrice(ctx, "p", 100, "USD", "month"); err != nil || id != "price_month" {
		t.Errorf("recurring price %q %v", id, err)
	}
	if id, err := api.CreatePrice(ctx, "p", 100, "USD", ""); err != nil || id != "price_" {
		t.Errorf("one-off price %q %v", id, err)
	}
	if id, err := api.AccountRef(ctx); err != nil || id != "acct_1" {
		t.Errorf("account %q %v", id, err)
	}
	if _, err := api.RawEvent(ctx, "evt_1"); err == nil || !strings.Contains(err.Error(), "418") {
		t.Errorf("non-2xx: %v", err)
	}
	noID.Store(true)
	if _, err := api.CreatePrice(ctx, "p", 100, "USD", ""); err == nil || !strings.Contains(err.Error(), "no id in answer") {
		t.Errorf("no id: %v", err)
	}
	if _, err := (StripeAPI{BaseURL: "http://\x7f"}).RawEvent(ctx, "x"); err == nil {
		t.Error("bad URL accepted")
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := (StripeAPI{BaseURL: closed.URL}).AccountRef(ctx); err == nil {
		t.Error("closed server answered")
	}
	// A body cut short fails the read.
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"id":`))
	}))
	defer short.Close()
	if _, err := (StripeAPI{BaseURL: short.URL}).AccountRef(ctx); err == nil {
		t.Error("truncated body accepted")
	}
}

func TestTestClock(t *testing.T) {
	ctx := context.Background()
	var status atomic.Value
	status.Store("ready")
	var failCreate, failAdvance, failGet atomic.Bool
	var deleted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/test_helpers/test_clocks":
			if failCreate.Load() {
				http.Error(w, "no", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"id":"clock_1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/test_helpers/test_clocks/clock_1/advance":
			if failAdvance.Load() {
				http.Error(w, "no", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"id":"clock_1"}`))
		case r.Method == http.MethodGet:
			if failGet.Load() {
				http.Error(w, "no", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"id":"clock_1","status":"` + status.Load().(string) + `"}`))
		case r.Method == http.MethodDelete:
			deleted.Store(true)
			_, _ = w.Write([]byte(`{"id":"clock_1","deleted":true}`))
		}
	}))
	defer srv.Close()
	api := StripeAPI{Key: "sk_test_k", BaseURL: srv.URL}
	start := time.Unix(1_700_000_000, 0)
	failCreate.Store(true)
	if _, err := NewTestClock(ctx, api, "c", start); err == nil {
		t.Error("failed create accepted")
	}
	failCreate.Store(false)
	c, err := NewTestClock(ctx, api, "c", start)
	if err != nil || c.ID != "clock_1" || !c.Now().Equal(start) {
		t.Fatalf("clock %+v %v", c, err)
	}
	if err := c.Advance(ctx, start); err != nil {
		t.Error("advance to now must be a no-op")
	}
	if err := c.Advance(ctx, start.Add(time.Hour)); err != nil || !c.Now().Equal(start.Add(time.Hour)) {
		t.Errorf("advance ready: %v %s", err, c.Now())
	}
	status.Store("internal_failure")
	if err := c.Advance(ctx, start.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "failed to advance") {
		t.Errorf("internal failure: %v", err)
	}
	failGet.Store(true)
	if err := c.Advance(ctx, start.Add(3*time.Hour)); err == nil {
		t.Error("status read failure accepted")
	}
	failGet.Store(false)
	failAdvance.Store(true)
	if err := c.Advance(ctx, start.Add(4*time.Hour)); err == nil {
		t.Error("advance failure accepted")
	}
	failAdvance.Store(false)
	status.Store("advancing")
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.Advance(cctx, start.Add(5*time.Hour)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancelled wait: %v", err)
	}
	if !c.Now().Equal(start.Add(time.Hour)) {
		t.Error("a failed advance moved the clock")
	}
	if err := c.Delete(ctx); err != nil || !deleted.Load() {
		t.Errorf("delete %v", err)
	}
}

func TestSign(t *testing.T) {
	sig := Sign("whsec_x", time.Unix(1, 0), []byte("{}"))
	if !strings.HasPrefix(sig, "t=1,v1=") || len(sig) != len("t=1,v1=")+64 {
		t.Errorf("sig %s", sig)
	}
}

func TestStripeAPIGetWithQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("limit") != "3" || r.Header.Get("Content-Type") != "" {
			t.Errorf("%s %s %q", r.Method, r.URL, r.Header.Get("Content-Type"))
		}
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	if _, err := (StripeAPI{BaseURL: srv.URL}).do(context.Background(), http.MethodGet, "/v1/events", url.Values{"limit": {"3"}}); err != nil {
		t.Fatal(err)
	}
}
