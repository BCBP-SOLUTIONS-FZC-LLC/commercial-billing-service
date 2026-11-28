//go:build integration

package commerce_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// ── BIL-OPS-3: no outbound call inside an open transaction ──────────────

// inTxKey marks a ctx that runs inside a transaction the Service opened.
type inTxKey struct{}

// txWatch marks every ctx handed to work inside a tenant or system
// transaction, and records every outbound call made with a marked ctx.
type txWatch struct {
	mu         sync.Mutex
	calls      int
	violations []string
}

func (w *txWatch) check(ctx context.Context, call string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if ctx.Value(inTxKey{}) != nil {
		w.violations = append(w.violations, call)
	}
}

func mark(ctx context.Context) context.Context { return context.WithValue(ctx, inTxKey{}, true) }

type watchedTx struct{ port.TxRunner }

func (r watchedTx) RunInTenantTx(ctx context.Context, tenant string, fn func(context.Context) error) error {
	return r.TxRunner.RunInTenantTx(ctx, tenant, func(ctx context.Context) error { return fn(mark(ctx)) })
}

type watchedSysTx struct{ port.SystemTxRunner }

func (r watchedSysTx) RunInSystemTx(ctx context.Context, fn func(context.Context) error) error {
	return r.SystemTxRunner.RunInSystemTx(ctx, func(ctx context.Context) error { return fn(mark(ctx)) })
}

// watchedCore is I-11 / I-18 (HTTP to IAM Core) under the watch.
type watchedCore struct {
	port.CoreClient
	w *txWatch
}

func (c watchedCore) SeatUsage(ctx context.Context, tenant string) (port.SeatUsage, error) {
	c.w.check(ctx, "Core.SeatUsage")
	return c.CoreClient.SeatUsage(ctx, tenant)
}

func (c watchedCore) PlanChangeCheck(ctx context.Context, tenant string, target domain.PlanCode) (port.PlanChangeCheck, error) {
	c.w.check(ctx, "Core.PlanChangeCheck")
	return c.CoreClient.PlanChangeCheck(ctx, tenant, target)
}

// watchedProvider is the payment provider (HTTP to Stripe) under the watch:
// every call that takes a ctx is checked, then delegated.
type watchedProvider struct {
	port.PaymentProvider
	w *txWatch
}

func (p watchedProvider) EnsureCustomer(ctx context.Context, in port.CustomerInput, k string) (port.CustomerRef, error) {
	p.w.check(ctx, "EnsureCustomer")
	return p.PaymentProvider.EnsureCustomer(ctx, in, k)
}

func (p watchedProvider) UpdateCustomer(ctx context.Context, ref port.CustomerRef, in port.CustomerInput, k string) error {
	p.w.check(ctx, "UpdateCustomer")
	return p.PaymentProvider.UpdateCustomer(ctx, ref, in, k)
}

func (p watchedProvider) CreateHostedCheckout(ctx context.Context, in port.CheckoutInput, k string) (port.HostedSession, error) {
	p.w.check(ctx, "CreateHostedCheckout")
	return p.PaymentProvider.CreateHostedCheckout(ctx, in, k)
}

func (p watchedProvider) ExpireCheckout(ctx context.Context, id, k string) error {
	p.w.check(ctx, "ExpireCheckout")
	return p.PaymentProvider.ExpireCheckout(ctx, id, k)
}

func (p watchedProvider) CreatePaymentMethodSetup(ctx context.Context, in port.SetupInput, k string) (port.HostedSession, error) {
	p.w.check(ctx, "CreatePaymentMethodSetup")
	return p.PaymentProvider.CreatePaymentMethodSetup(ctx, in, k)
}

func (p watchedProvider) ListPaymentMethods(ctx context.Context, ref port.CustomerRef) ([]port.PaymentMethodView, error) {
	p.w.check(ctx, "ListPaymentMethods")
	return p.PaymentProvider.ListPaymentMethods(ctx, ref)
}

func (p watchedProvider) AttachDefaultPaymentMethod(ctx context.Context, ref port.CustomerRef, pm, k string) error {
	p.w.check(ctx, "AttachDefaultPaymentMethod")
	return p.PaymentProvider.AttachDefaultPaymentMethod(ctx, ref, pm, k)
}

func (p watchedProvider) CreateSubscription(ctx context.Context, in port.SubscriptionInput, k string) (port.SubscriptionResult, error) {
	p.w.check(ctx, "CreateSubscription")
	return p.PaymentProvider.CreateSubscription(ctx, in, k)
}

func (p watchedProvider) ConfirmInitialPayment(ctx context.Context, ref port.SubscriptionRef, k string) (port.PaymentOutcome, error) {
	p.w.check(ctx, "ConfirmInitialPayment")
	return p.PaymentProvider.ConfirmInitialPayment(ctx, ref, k)
}

func (p watchedProvider) ChangeQuantity(ctx context.Context, in port.QuantityChange, k string) (port.ChangeResult, error) {
	p.w.check(ctx, "ChangeQuantity")
	return p.PaymentProvider.ChangeQuantity(ctx, in, k)
}

func (p watchedProvider) ChangePrice(ctx context.Context, in port.PriceChange, k string) (port.ChangeResult, error) {
	p.w.check(ctx, "ChangePrice")
	return p.PaymentProvider.ChangePrice(ctx, in, k)
}

func (p watchedProvider) ScheduleAtPeriodEnd(ctx context.Context, in port.PeriodEndChange, k string) (port.ScheduleRef, error) {
	p.w.check(ctx, "ScheduleAtPeriodEnd")
	return p.PaymentProvider.ScheduleAtPeriodEnd(ctx, in, k)
}

func (p watchedProvider) ReleaseSchedule(ctx context.Context, ref port.ScheduleRef, k string) error {
	p.w.check(ctx, "ReleaseSchedule")
	return p.PaymentProvider.ReleaseSchedule(ctx, ref, k)
}

func (p watchedProvider) SetCancelAtPeriodEnd(ctx context.Context, ref port.SubscriptionRef, cancel bool, k string) error {
	p.w.check(ctx, "SetCancelAtPeriodEnd")
	return p.PaymentProvider.SetCancelAtPeriodEnd(ctx, ref, cancel, k)
}

func (p watchedProvider) CancelNow(ctx context.Context, ref port.SubscriptionRef, k string) error {
	p.w.check(ctx, "CancelNow")
	return p.PaymentProvider.CancelNow(ctx, ref, k)
}

func (p watchedProvider) PauseCollection(ctx context.Context, ref port.SubscriptionRef, pause bool, k string) error {
	p.w.check(ctx, "PauseCollection")
	return p.PaymentProvider.PauseCollection(ctx, ref, pause, k)
}

func (p watchedProvider) PreviewQuantityChange(ctx context.Context, in port.QuantityChange) (port.ProrationPreview, error) {
	p.w.check(ctx, "PreviewQuantityChange")
	return p.PaymentProvider.PreviewQuantityChange(ctx, in)
}

func (p watchedProvider) PreviewPriceChange(ctx context.Context, in port.PriceChange) (port.ProrationPreview, error) {
	p.w.check(ctx, "PreviewPriceChange")
	return p.PaymentProvider.PreviewPriceChange(ctx, in)
}

func (p watchedProvider) ChargeOneOff(ctx context.Context, in port.OneOffCharge, k string) (port.ChargeResult, error) {
	p.w.check(ctx, "ChargeOneOff")
	return p.PaymentProvider.ChargeOneOff(ctx, in, k)
}

func (p watchedProvider) GetInvoice(ctx context.Context, ref port.InvoiceRef) (port.InvoiceView, error) {
	p.w.check(ctx, "GetInvoice")
	return p.PaymentProvider.GetInvoice(ctx, ref)
}

func (p watchedProvider) InvoicePDFLink(ctx context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	p.w.check(ctx, "InvoicePDFLink")
	return p.PaymentProvider.InvoicePDFLink(ctx, ref)
}

func (p watchedProvider) PayOpenInvoiceLink(ctx context.Context, ref port.InvoiceRef) (port.URLWithExpiry, error) {
	p.w.check(ctx, "PayOpenInvoiceLink")
	return p.PaymentProvider.PayOpenInvoiceLink(ctx, ref)
}

func (p watchedProvider) VoidInvoice(ctx context.Context, ref port.InvoiceRef, k string) error {
	p.w.check(ctx, "VoidInvoice")
	return p.PaymentProvider.VoidInvoice(ctx, ref, k)
}

func (p watchedProvider) DeleteDraftInvoice(ctx context.Context, ref port.InvoiceRef) error {
	p.w.check(ctx, "DeleteDraftInvoice")
	return p.PaymentProvider.DeleteDraftInvoice(ctx, ref)
}

func (p watchedProvider) FinalizeInvoice(ctx context.Context, ref port.InvoiceRef, k string) error {
	p.w.check(ctx, "FinalizeInvoice")
	return p.PaymentProvider.FinalizeInvoice(ctx, ref, k)
}

func (p watchedProvider) FindInvoice(ctx context.Context, q port.InvoiceQuery) (port.InvoiceRef, bool, error) {
	p.w.check(ctx, "FindInvoice")
	return p.PaymentProvider.FindInvoice(ctx, q)
}

func (p watchedProvider) Refund(ctx context.Context, in port.RefundInput, k string) (port.RefundResult, error) {
	p.w.check(ctx, "Refund")
	return p.PaymentProvider.Refund(ctx, in, k)
}

func (p watchedProvider) CreditBalance(ctx context.Context, in port.BalanceAdjustment, k string) (string, error) {
	p.w.check(ctx, "CreditBalance")
	return p.PaymentProvider.CreditBalance(ctx, in, k)
}

func (p watchedProvider) VerifyPrice(ctx context.Context, ref string, want port.PriceExpectation) error {
	p.w.check(ctx, "VerifyPrice")
	return p.PaymentProvider.VerifyPrice(ctx, ref, want)
}

func (p watchedProvider) VerifyAndNormalise(ctx context.Context, h http.Header, body []byte) (domain.PaymentEvent, error) {
	p.w.check(ctx, "VerifyAndNormalise")
	return p.PaymentProvider.VerifyAndNormalise(ctx, h, body)
}

func (p watchedProvider) FetchCurrent(ctx context.Context, obj domain.ProviderObjectRef) (domain.ProviderObjectState, error) {
	p.w.check(ctx, "FetchCurrent")
	return p.PaymentProvider.FetchCurrent(ctx, obj)
}

func (p watchedProvider) ListSince(ctx context.Context, kind port.ReconKind, since time.Time, cursor string) (port.ReconPage, error) {
	p.w.check(ctx, "ListSince")
	return p.PaymentProvider.ListSince(ctx, kind, since, cursor)
}

// BIL-OPS-3 / BIL-CONC-2: no provider or IAM Core call is made inside a
// transaction the Service opened. Every transaction runner hands its work a
// marked ctx; the provider and Core clients fail the test on a marked ctx.
// The commerce use cases that call out run end to end under the watch:
// direct signup, seat preview and purchase, a scheduled reduction, an
// upgrade, an adjustment, a refund, payment-method reads and setup.
func TestBIL_OPS_3_NoOutboundCallInsideATransaction(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	w := &txWatch{}
	e.svc.Tx = watchedTx{e.svc.Tx}
	if e.svc.SysTx != nil {
		e.svc.SysTx = watchedSysTx{e.svc.SysTx}
	}
	e.svc.Core = watchedCore{e.svc.Core, w}
	e.reg.m["stripe"] = watchedProvider{e.p, w}

	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-ops3-seat1"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ScheduleReduction(ctx, owner, tenant, app.ReductionRequest{TargetSeats: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ListPaymentMethods(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateSetupSession(ctx, owner, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.IssueAdjustment(ctx, withKey(operator, "adj-ops3-001"), tenant, app.AdjustmentRequest{Kind: "credit",
		Amount: app.Money{AmountMinor: 500, Currency: "USD"}, Description: "Service credit", Reason: "SLA credit approved by Finance"}); err != nil {
		t.Fatal(err)
	}
	paid := e.mirror(t, tenant, sub, "in_paid_"+sub, "subscription_create", "paid", 20700)
	if _, err := e.svc.IssueRefund(ctx, withKey(operator, "ref-ops3-001"), tenant, refundReq(paid, 100, "billing_error")); err != nil {
		t.Fatal(err)
	}

	up, upSub := e.activeSubscription(t, "starter", 3)
	upv, err := e.svc.PreviewPlanChange(ctx, owner, up, app.PlanChangePreviewRequest{TargetPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChangePlan(ctx, withKey(owner, "k-ops3-up01"), up, app.PlanChangeRequest{TargetPlan: "pro",
		RecordVersion: version(t, upSub), PreviewID: upv.PreviewID}); err != nil {
		t.Fatal(err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls < 10 {
		t.Fatalf("only %d outbound calls observed: the watch is not in the path", w.calls)
	}
	if len(w.violations) > 0 {
		t.Fatalf("outbound calls inside an open transaction: %v", w.violations)
	}
}

// ── BIL-OPS-8: a confirmed purchase always commits its event ─────────────

// BIL-OPS-8 (§9.7): a purchase confirmed by payment commits its event in the
// same transaction. When the event cannot be written, nothing of the
// confirmation commits (the callback is retried); when it commits, the
// purchase, the seat item and exactly one TenantSeatsChanged commit together.
func TestBIL_OPS_8_ConfirmedPurchaseCommitsItsEvent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	pv, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.p.on("ChangeQuantity", answer{outcome: port.PaymentOutcome{Status: port.PaymentPending}})
	res, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-ops8-0001"), tenant, app.SeatPurchaseRequest{AddSeats: 2, PreviewID: pv.PreviewID})
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		return scalar[string](t, `SELECT status::text FROM seat_purchases WHERE purchase_id = $1::uuid`, res.PurchaseID)
	}
	seatEvents := func() int {
		return count(t, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND event_type = 'TenantSeatsChanged'
			AND payload->'data'->>'billing_ref' = $2`, tenant, res.PurchaseID)
	}
	if s := state(); s == string(domain.PurchaseConfirmed) || seatEvents() != 0 || quantity(t, sub) != 10 {
		t.Fatalf("before payment: %s, %d events, quantity %d", s, seatEvents(), quantity(t, sub))
	}

	h := app.SeatInvoicePaid{S: e.svc}
	ev := callback("evt_ops8_paid", "invoice.paid", domain.KindInvoicePaid)
	cur := domain.ProviderObjectState{Ref: domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_seatpur:" + res.PurchaseID}, Status: "paid",
		Fields: paidFields(t, "seat_purchases", res.PurchaseID, "subscription", "sub_"+sub, "billing_reason", "subscription_update")}
	before := state()
	e.fail("Events.Enqueue", 0)
	_, err = e.handle(tenant, h, ev, cur)
	isInjected(t, "event write", err)
	if s := state(); s != before || seatEvents() != 0 || quantity(t, sub) != 10 {
		t.Fatalf("event write failed but the confirmation committed: %s, %d events, quantity %d", s, seatEvents(), quantity(t, sub))
	}

	if out, err := e.handle(tenant, h, ev, cur); err != nil || out != "applied:seat_purchase" {
		t.Fatalf("paid: %s %v", out, err)
	}
	if s := state(); s != string(domain.PurchaseConfirmed) || seatEvents() != 1 || quantity(t, sub) != 12 {
		t.Fatalf("after payment: %s, %d events, quantity %d", s, seatEvents(), quantity(t, sub))
	}
}

// ── BIL-PROV-3: the proration mode decides who prices a seat change ──────

// BIL-PROV-3 (§8.5.3): a provider declaring Proration = native prices the
// preview itself; one declaring fallback is never asked, Billing's
// calculator prices the preview and the purchase charges that amount.
func TestBIL_PROV_3_ProrationModeDecidesThePricer(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tenant, sub := e.activeSubscription(t, "pro", 10)
	start := scalar[time.Time](t, `SELECT current_period_start FROM subscriptions WHERE billing_subscription_id = $1`, sub)
	e.now = start.Add(time.Hour)
	asked := func() int { return len(e.p.called("PreviewQuantityChange")) }

	n := asked()
	native, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if err != nil || native.AmountSource != "provider_preview" || asked() != n+1 {
		t.Fatalf("native: %+v %v, provider asked %d times", native, err, asked()-n)
	}

	e.p.fallback = true
	n = asked()
	fb, err := e.svc.PreviewSeats(ctx, owner, tenant, app.SeatPreviewRequest{AddSeats: 1})
	if err != nil || fb.AmountSource != "billing_calculator" || asked() != n {
		t.Fatalf("fallback: %+v %v, provider asked %d times", fb, err, asked()-n)
	}
	res, err := e.svc.PurchaseSeats(ctx, withKey(owner, "k-prov3-001"), tenant, app.SeatPurchaseRequest{AddSeats: 1, PreviewID: fb.PreviewID})
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, `SELECT amount_source::text FROM seat_purchases WHERE purchase_id = $1::uuid`, res.PurchaseID); got != "billing_calculator" || asked() != n {
		t.Fatalf("fallback purchase priced by %s, provider asked %d times", got, asked()-n)
	}
}
