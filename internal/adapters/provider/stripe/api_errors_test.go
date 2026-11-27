package stripe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

var (
	cus    = port.CustomerRef{ProviderCustomerID: "cus_1"}
	subRef = port.SubscriptionRef{ProviderSubscriptionID: "sub_1"}
	invRef = port.InvoiceRef{ProviderInvoiceID: "in_1"}
	usd5   = domain.Money{AmountMinor: 500, Currency: "USD"}
)

// Every provider call that fails surfaces as a port error, whichever step of
// a multi-call operation fails, and later steps are not attempted (§18.8).
func TestStepFailuresMapToPortErrors(t *testing.T) {
	ctx := context.Background()
	activeSub := sub("sub_1", "active", nil)
	openInv := list("/v1/invoices", invoice("in_1", "open", nil))
	cases := []struct {
		name  string
		setup func(f *fakeStripe)
		run   func(a *Adapter) error
		want  error
		never []string // requests that must not be made after the failure
	}{
		{"EnsureCustomer", func(f *fakeStripe) { f.on("POST", "/v1/customers", 503, unavailable) },
			func(a *Adapter) error {
				_, err := a.EnsureCustomer(ctx, port.CustomerInput{TenantID: "t1"}, "k")
				return err
			}, port.ErrProviderUnavailable, nil},
		{"CreateHostedCheckout", func(f *fakeStripe) { f.on("POST", "/v1/checkout/sessions", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.CreateHostedCheckout(ctx, port.CheckoutInput{Customer: cus, PriceRef: "price_A", Quantity: 1}, "k")
				return err
			}, port.ErrProviderUnavailable, nil},
		{"CreatePaymentMethodSetup", func(f *fakeStripe) { f.on("POST", "/v1/checkout/sessions", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.CreatePaymentMethodSetup(ctx, port.SetupInput{Customer: cus}, "k")
				return err
			}, port.ErrProviderUnavailable, nil},
		{"ListPaymentMethods/customer missing", func(f *fakeStripe) {
			f.on("GET", "/v1/customers/cus_1", 404, apiErr("invalid_request_error", "resource_missing"))
		}, func(a *Adapter) error { _, err := a.ListPaymentMethods(ctx, cus); return err }, port.ErrNotFound,
			[]string{"GET /v1/customers/cus_1/payment_methods"}},
		{"ListPaymentMethods/list", func(f *fakeStripe) {
			f.on("GET", "/v1/customers/cus_1", 200, map[string]any{"id": "cus_1", "object": "customer"})
			f.on("GET", "/v1/customers/cus_1/payment_methods", 503, unavailable)
		}, func(a *Adapter) error { _, err := a.ListPaymentMethods(ctx, cus); return err }, port.ErrProviderUnavailable, nil},
		{"AttachDefaultPaymentMethod/attach declined", func(f *fakeStripe) {
			f.on("POST", "/v1/payment_methods/pm_1/attach", 402, map[string]any{"error": map[string]any{
				"type": "card_error", "code": "card_declined", "decline_code": "insufficient_funds"}})
		}, func(a *Adapter) error { return a.AttachDefaultPaymentMethod(ctx, cus, "pm_1", "k") },
			port.ErrDeclined{Code: "insufficient_funds"}, []string{"POST /v1/customers/cus_1", "GET /v1/subscriptions"}},
		{"AttachDefaultPaymentMethod/default", func(f *fakeStripe) {
			f.on("POST", "/v1/payment_methods/pm_1/attach", 200, map[string]any{"id": "pm_1", "object": "payment_method"})
			f.on("POST", "/v1/customers/cus_1", 500, unavailable)
		}, func(a *Adapter) error { return a.AttachDefaultPaymentMethod(ctx, cus, "pm_1", "k") },
			port.ErrProviderUnavailable, []string{"GET /v1/subscriptions"}},
		{"AttachDefaultPaymentMethod/list subscriptions", func(f *fakeStripe) {
			f.on("POST", "/v1/payment_methods/pm_1/attach", 200, map[string]any{"id": "pm_1", "object": "payment_method"})
			f.on("POST", "/v1/customers/cus_1", 200, map[string]any{"id": "cus_1", "object": "customer"})
			f.on("GET", "/v1/subscriptions", 429, apiErr("invalid_request_error", "rate_limit"))
		}, func(a *Adapter) error { return a.AttachDefaultPaymentMethod(ctx, cus, "pm_1", "k") }, port.ErrProviderUnavailable, nil},
		{"AttachDefaultPaymentMethod/update subscription", func(f *fakeStripe) {
			f.on("POST", "/v1/payment_methods/pm_1/attach", 200, map[string]any{"id": "pm_1", "object": "payment_method"})
			f.on("POST", "/v1/customers/cus_1", 200, map[string]any{"id": "cus_1", "object": "customer"})
			f.on("GET", "/v1/subscriptions", 200, list("/v1/subscriptions", activeSub))
			f.on("POST", "/v1/subscriptions/sub_1", 500, unavailable)
		}, func(a *Adapter) error { return a.AttachDefaultPaymentMethod(ctx, cus, "pm_1", "k") }, port.ErrProviderUnavailable, nil},
		{"CreateSubscription/idempotency reuse", func(f *fakeStripe) {
			f.on("POST", "/v1/subscriptions", 400, apiErr("idempotency_error", ""))
		}, func(a *Adapter) error {
			_, err := a.CreateSubscription(ctx, port.SubscriptionInput{Customer: cus, PriceRef: "price_A", Quantity: 1}, "k")
			return err
		}, port.ErrIdempotencyMismatch, nil},
		{"ConfirmInitialPayment/retrieve", func(f *fakeStripe) { f.on("GET", "/v1/subscriptions/sub_1", 500, unavailable) },
			func(a *Adapter) error { _, err := a.ConfirmInitialPayment(ctx, subRef, "k"); return err }, port.ErrProviderUnavailable, nil},
		{"ChangeQuantity/retrieve", func(f *fakeStripe) { f.on("GET", "/v1/subscriptions/sub_1", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.ChangeQuantity(ctx, port.QuantityChange{Sub: subRef, To: 4}, "k")
				return err
			},
			port.ErrProviderUnavailable, []string{"POST /v1/subscriptions/sub_1"}},
		{"ChangeQuantity/no seat item", func(f *fakeStripe) {
			f.on("GET", "/v1/subscriptions/sub_1", 200, map[string]any{"id": "sub_1", "object": "subscription", "status": "active",
				"items": list("/v1/subscription_items")})
		}, func(a *Adapter) error {
			_, err := a.ChangeQuantity(ctx, port.QuantityChange{Sub: subRef, To: 4}, "k")
			return err
		},
			port.ErrNotFound, []string{"POST /v1/subscriptions/sub_1"}},
		{"ChangePrice/retrieve", func(f *fakeStripe) {
			f.on("GET", "/v1/subscriptions/sub_1", 404, apiErr("invalid_request_error", "resource_missing"))
		},
			func(a *Adapter) error {
				_, err := a.ChangePrice(ctx, port.PriceChange{Sub: subRef, ToPriceRef: "price_B"}, "k")
				return err
			}, port.ErrNotFound, []string{"POST /v1/subscriptions/sub_1"}},
		{"ChangePrice/update", func(f *fakeStripe) {
			f.on("GET", "/v1/subscriptions/sub_1", 200, activeSub)
			f.on("POST", "/v1/subscriptions/sub_1", 503, unavailable)
		}, func(a *Adapter) error {
			_, err := a.ChangePrice(ctx, port.PriceChange{Sub: subRef, ToPriceRef: "price_B"}, "k")
			return err
		}, port.ErrProviderUnavailable, nil},
		{"ScheduleAtPeriodEnd/retrieve", func(f *fakeStripe) { f.on("GET", "/v1/subscriptions/sub_1", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: subRef, ToPriceRef: "price_B", ToQuantity: 2, At: fixedNow}, "k")
				return err
			}, port.ErrProviderUnavailable, []string{"POST /v1/subscription_schedules"}},
		{"ScheduleAtPeriodEnd/create", func(f *fakeStripe) {
			f.on("GET", "/v1/subscriptions/sub_1", 200, activeSub)
			f.on("POST", "/v1/subscription_schedules", 500, unavailable)
		}, func(a *Adapter) error {
			_, err := a.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: subRef, ToPriceRef: "price_B", ToQuantity: 2, At: fixedNow}, "k")
			return err
		}, port.ErrProviderUnavailable, nil},
		{"ScheduleAtPeriodEnd/phases", func(f *fakeStripe) {
			f.on("GET", "/v1/subscriptions/sub_1", 200, activeSub)
			f.on("POST", "/v1/subscription_schedules", 200, map[string]any{"id": "sub_sched_1", "object": "subscription_schedule"})
			f.on("POST", "/v1/subscription_schedules/sub_sched_1", 500, unavailable)
		}, func(a *Adapter) error {
			_, err := a.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: subRef, ToPriceRef: "price_B", ToQuantity: 2, At: fixedNow}, "k")
			return err
		}, port.ErrProviderUnavailable, nil},
		{"PauseCollection/update", func(f *fakeStripe) { f.on("POST", "/v1/subscriptions/sub_1", 500, unavailable) },
			func(a *Adapter) error { return a.PauseCollection(ctx, subRef, true, "k") }, port.ErrProviderUnavailable, []string{"GET /v1/invoices"}},
		{"PauseCollection/list invoices", func(f *fakeStripe) {
			f.on("POST", "/v1/subscriptions/sub_1", 200, activeSub)
			f.on("GET", "/v1/invoices", 500, unavailable)
		}, func(a *Adapter) error { return a.PauseCollection(ctx, subRef, true, "k") }, port.ErrProviderUnavailable, nil},
		{"PauseCollection/update invoice", func(f *fakeStripe) {
			f.on("POST", "/v1/subscriptions/sub_1", 200, activeSub)
			f.on("GET", "/v1/invoices", 200, openInv)
			f.on("POST", "/v1/invoices/in_1", 500, unavailable)
		}, func(a *Adapter) error { return a.PauseCollection(ctx, subRef, true, "k") }, port.ErrProviderUnavailable, nil},
		{"PreviewQuantityChange/retrieve", func(f *fakeStripe) { f.on("GET", "/v1/subscriptions/sub_1", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.PreviewQuantityChange(ctx, port.QuantityChange{Sub: subRef, To: 4})
				return err
			}, port.ErrProviderUnavailable, []string{"POST /v1/invoices/create_preview"}},
		{"PreviewQuantityChange/preview", func(f *fakeStripe) {
			f.on("GET", "/v1/subscriptions/sub_1", 200, activeSub)
			f.on("POST", "/v1/invoices/create_preview", 500, unavailable)
		}, func(a *Adapter) error {
			_, err := a.PreviewQuantityChange(ctx, port.QuantityChange{Sub: subRef, To: 4})
			return err
		}, port.ErrProviderUnavailable, nil},
		{"PreviewPriceChange/retrieve", func(f *fakeStripe) { f.on("GET", "/v1/subscriptions/sub_1", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.PreviewPriceChange(ctx, port.PriceChange{Sub: subRef, ToPriceRef: "price_B"})
				return err
			}, port.ErrProviderUnavailable, []string{"POST /v1/invoices/create_preview"}},
		{"ChargeOneOff/invoice", func(f *fakeStripe) { f.on("POST", "/v1/invoices", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.ChargeOneOff(ctx, port.OneOffCharge{Customer: cus, Amount: usd5}, "k")
				return err
			},
			port.ErrProviderUnavailable, []string{"POST /v1/invoiceitems"}},
		{"ChargeOneOff/item", func(f *fakeStripe) {
			f.on("POST", "/v1/invoices", 200, invoice("in_1", "draft", nil))
			f.on("POST", "/v1/invoiceitems", 500, unavailable)
			f.on("DELETE", "/v1/invoices/in_1", 200, map[string]any{"id": "in_1", "object": "invoice", "deleted": true})
		}, func(a *Adapter) error {
			_, err := a.ChargeOneOff(ctx, port.OneOffCharge{Customer: cus, Amount: usd5}, "k")
			return err
		},
			port.ErrProviderUnavailable, []string{"POST /v1/invoices/in_1/pay"}},
		{"GetInvoice", func(f *fakeStripe) { f.on("GET", "/v1/invoices/in_1", 500, unavailable) },
			func(a *Adapter) error { _, err := a.GetInvoice(ctx, invRef); return err }, port.ErrProviderUnavailable, nil},
		{"InvoicePDFLink", func(f *fakeStripe) {
			f.on("GET", "/v1/invoices/in_1", 404, apiErr("invalid_request_error", "resource_missing"))
		},
			func(a *Adapter) error { _, err := a.InvoicePDFLink(ctx, invRef); return err }, port.ErrNotFound, nil},
		{"PayOpenInvoiceLink", func(f *fakeStripe) { f.on("GET", "/v1/invoices/in_1", 500, unavailable) },
			func(a *Adapter) error { _, err := a.PayOpenInvoiceLink(ctx, invRef); return err }, port.ErrProviderUnavailable, nil},
		{"Refund/retrieve", func(f *fakeStripe) { f.on("GET", "/v1/invoices/in_1", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.Refund(ctx, port.RefundInput{Invoice: invRef, Amount: usd5}, "k")
				return err
			},
			port.ErrProviderUnavailable, []string{"POST /v1/refunds"}},
		{"Refund/no payment intent", func(f *fakeStripe) { f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "paid", nil)) },
			func(a *Adapter) error {
				_, err := a.Refund(ctx, port.RefundInput{Invoice: invRef, Amount: usd5}, "k")
				return err
			},
			port.ErrNotFound, []string{"POST /v1/refunds"}},
		{"Refund/create", func(f *fakeStripe) {
			f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "paid", map[string]any{"payments": list("/v1/invoice_payments",
				map[string]any{"id": "inpay_1", "object": "invoice_payment", "payment": map[string]any{"type": "payment_intent", "payment_intent": "pi_1"}})}))
			f.on("POST", "/v1/refunds", 500, unavailable)
		}, func(a *Adapter) error {
			_, err := a.Refund(ctx, port.RefundInput{Invoice: invRef, Amount: usd5}, "k")
			return err
		},
			port.ErrProviderUnavailable, nil},
		{"CreditBalance", func(f *fakeStripe) { f.on("POST", "/v1/customers/cus_1/balance_transactions", 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.CreditBalance(ctx, port.BalanceAdjustment{Customer: cus, Amount: usd5}, "k")
				return err
			}, port.ErrProviderUnavailable, nil},
		{"VerifyPrice", func(f *fakeStripe) {
			f.on("GET", "/v1/prices/price_A", 404, apiErr("invalid_request_error", "resource_missing"))
		},
			func(a *Adapter) error { return a.VerifyPrice(ctx, "price_A", port.PriceExpectation{Amount: usd5}) }, port.ErrNotFound, nil},
	}
	for _, kind := range []struct {
		kind domain.ProviderObjectKind
		path string
	}{
		{domain.ObjectSubscription, "/v1/subscriptions/x_1"},
		{domain.ObjectInvoice, "/v1/invoices/x_1"},
		{domain.ObjectCheckoutSession, "/v1/checkout/sessions/x_1"},
		{domain.ObjectRefund, "/v1/refunds/x_1"},
		{domain.ObjectDispute, "/v1/disputes/x_1"},
		{domain.ObjectEarlyFraudWarning, "/v1/radar/early_fraud_warnings/x_1"},
	} {
		cases = append(cases, struct {
			name  string
			setup func(f *fakeStripe)
			run   func(a *Adapter) error
			want  error
			never []string
		}{"FetchCurrent/" + string(kind.kind), func(f *fakeStripe) { f.on("GET", kind.path, 500, unavailable) },
			func(a *Adapter) error {
				_, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: kind.kind, ID: "x_1"})
				return err
			}, port.ErrProviderUnavailable, nil})
	}
	for _, kind := range []struct {
		kind port.ReconKind
		path string
	}{
		{port.ReconSubscriptions, "/v1/subscriptions"}, {port.ReconInvoices, "/v1/invoices"}, {port.ReconEvents, "/v1/events"},
	} {
		cases = append(cases, struct {
			name  string
			setup func(f *fakeStripe)
			run   func(a *Adapter) error
			want  error
			never []string
		}{"ListSince/" + string(kind.kind), func(f *fakeStripe) { f.on("GET", kind.path, 503, unavailable) },
			func(a *Adapter) error { _, err := a.ListSince(ctx, kind.kind, fixedNow, ""); return err },
			port.ErrProviderUnavailable, nil})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a := newFakeStripe(t)
			c.setup(f)
			err := c.run(a)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			for _, n := range c.never {
				method, path, _ := strings.Cut(n, " ")
				if got := f.sent(method, path); len(got) != 0 {
					t.Errorf("%s must not be sent after the failure", n)
				}
			}
		})
	}
}

// 3-D Secure on the first invoice comes back as ErrActionRequired carrying the
// PaymentIntent client secret for the caller (§18.8).
func TestConfirmInitialPaymentActionRequired(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "incomplete", map[string]any{"latest_invoice": invoice("in_1", "open", nil)}))
	f.on("POST", "/v1/invoices/in_1/pay", 402, map[string]any{"error": map[string]any{
		"type": "card_error", "code": "authentication_required",
		"payment_intent": map[string]any{"id": "pi_1", "object": "payment_intent", "client_secret": "pi_1_secret_abc"}}})
	_, err := a.ConfirmInitialPayment(context.Background(), subRef, "idem-confirm")
	var ar port.ErrActionRequired
	if !errors.As(err, &ar) || ar.ClientSecret != "pi_1_secret_abc" {
		t.Fatalf("got %v, want ErrActionRequired with the client secret", err)
	}
	wantForm(t, f.only("POST", "/v1/invoices/in_1/pay"), "off_session", "true")
	if r := f.only("POST", "/v1/invoices/in_1/pay"); r.Idem != "idem-confirm" {
		t.Errorf("idempotency key %q", r.Idem)
	}
}

// A one-off charge whose payment fails still returns the invoice it created,
// so the caller can record and later void it (§8.7).
func TestChargeOneOffPayFailureKeepsInvoice(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("POST", "/v1/invoices", 200, invoice("in_7", "draft", nil))
	f.on("POST", "/v1/invoiceitems", 200, map[string]any{"id": "ii_1", "object": "invoiceitem"})
	f.on("POST", "/v1/invoices/in_7/pay", 402, map[string]any{"error": map[string]any{"type": "card_error", "code": "expired_card"}})
	res, err := a.ChargeOneOff(context.Background(), port.OneOffCharge{Customer: cus, Amount: usd5}, "k")
	var d port.ErrDeclined
	if !errors.As(err, &d) || d.Code != "expired_card" {
		t.Fatalf("got %v, want ErrDeclined{expired_card} (no decline_code → code)", err)
	}
	if res.Invoice.ProviderInvoiceID != "in_7" {
		t.Errorf("failed charge must still return the invoice: %+v", res)
	}
}

// A connection failure (Stripe unreachable) is ErrProviderUnavailable.
func TestUnreachableStripeIsUnavailable(t *testing.T) {
	a2, err := New(Config{APIKey: "rk_test_x", WebhookSecrets: []string{"s"}, BaseURL: "http://127.0.0.1:1", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.GetInvoice(context.Background(), invRef); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Fatalf("connection refused: %v", err)
	}
}
