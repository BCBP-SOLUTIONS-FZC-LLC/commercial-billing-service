package stripe

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// The certification suite's test clock is attached to every new customer
// (§14.7); the tenant id rides in metadata (§18.8).
func TestEnsureCustomerRequest(t *testing.T) {
	f, a := newFakeStripe(t)
	a.cfg.TestClock = "clock_1"
	f.on("POST", "/v1/customers", 200, map[string]any{"id": "cus_9", "object": "customer"})
	ref, err := a.EnsureCustomer(context.Background(), port.CustomerInput{TenantID: "t1", Name: "Acme", Email: "billing@example.com"}, "idem-c")
	if err != nil || ref.ProviderCustomerID != "cus_9" {
		t.Fatalf("got %+v %v", ref, err)
	}
	r := f.only("POST", "/v1/customers")
	wantForm(t, r, "metadata[tenant_id]", "t1", "name", "Acme", "email", "billing@example.com", "test_clock", "clock_1")
	if r.Idem != "idem-c" {
		t.Errorf("idempotency key %q", r.Idem)
	}
}

func TestCreateHostedCheckoutModes(t *testing.T) {
	ctx := context.Background()
	exp := fixedNow.Add(30 * time.Minute)
	resp := map[string]any{"id": "cs_1", "object": "checkout.session", "url": "https://checkout.stripe.com/c/cs_1", "expires_at": exp.Unix()}

	f, a := newFakeStripe(t)
	f.on("POST", "/v1/checkout/sessions", 200, resp)
	hs, err := a.CreateHostedCheckout(ctx, port.CheckoutInput{Customer: cus, Purpose: port.CheckoutAddonPayment, PriceRef: "price_T", Quantity: 2,
		Metadata: map[string]string{"purpose": "token_addon"}, SuccessURL: "https://app/ok", CancelURL: "https://app/no", ExpiresAt: exp}, "idem-co")
	if err != nil || hs.ProviderSessionID != "cs_1" || hs.URL != "https://checkout.stripe.com/c/cs_1" || !hs.ExpiresAt.Equal(exp) {
		t.Fatalf("got %+v %v", hs, err)
	}
	r := f.only("POST", "/v1/checkout/sessions")
	wantForm(t, r, "mode", "payment", "customer", "cus_1", "line_items[0][price]", "price_T", "line_items[0][quantity]", "2",
		"metadata[purpose]", "token_addon", "expires_at", unix(exp), "success_url", "https://app/ok", "cancel_url", "https://app/no")
	wantNoForm(t, r, "subscription_data[metadata][purpose]")
	if r.Idem != "idem-co" {
		t.Errorf("idempotency key %q", r.Idem)
	}

	f, a = newFakeStripe(t)
	f.on("POST", "/v1/checkout/sessions", 200, resp)
	if _, err := a.CreateHostedCheckout(ctx, port.CheckoutInput{Customer: cus, Purpose: port.CheckoutConversion, PriceRef: "price_A", Quantity: 5,
		Metadata: map[string]string{"tenant_id": "t1"}}, "k"); err != nil {
		t.Fatal(err)
	}
	r = f.only("POST", "/v1/checkout/sessions")
	wantForm(t, r, "mode", "subscription", "subscription_data[metadata][tenant_id]", "t1")
	wantNoForm(t, r, "expires_at")
}

// A superseded B-P2 session is expired at Stripe (§8.1.1); a session that
// is no longer open is re-read: expired is success, complete is
// ErrCheckoutCompleted.
func TestExpireCheckout(t *testing.T) {
	ctx := context.Background()
	f, a := newFakeStripe(t)
	f.on("POST", "/v1/checkout/sessions/cs_1/expire", 200, map[string]any{"id": "cs_1", "object": "checkout.session", "status": "expired"})
	if err := a.ExpireCheckout(ctx, "cs_1", "bsub:s1:expire"); err != nil {
		t.Fatal(err)
	}
	if r := f.only("POST", "/v1/checkout/sessions/cs_1/expire"); r.Idem != "bsub:s1:expire" {
		t.Errorf("idempotency key %q", r.Idem)
	}
	for status, want := range map[string]error{"expired": nil, "complete": port.ErrCheckoutCompleted} {
		f, a := newFakeStripe(t)
		f.on("POST", "/v1/checkout/sessions/cs_2/expire", 400, apiErr("invalid_request_error", "checkout_session_not_open"))
		f.on("GET", "/v1/checkout/sessions/cs_2", 200, map[string]any{"id": "cs_2", "object": "checkout.session", "status": status})
		if err := a.ExpireCheckout(ctx, "cs_2", "k"); !errors.Is(err, want) || (want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", status, err, want)
		}
	}
	f, a = newFakeStripe(t)
	f.on("POST", "/v1/checkout/sessions/cs_3/expire", 500, unavailable)
	if err := a.ExpireCheckout(ctx, "cs_3", "k"); !errors.Is(err, port.ErrProviderUnavailable) {
		t.Errorf("outage: %v", err)
	}
}

func TestCreatePaymentMethodSetupRequest(t *testing.T) {
	f, a := newFakeStripe(t)
	exp := fixedNow.Add(time.Hour)
	f.on("POST", "/v1/checkout/sessions", 200, map[string]any{"id": "cs_2", "object": "checkout.session", "url": "https://checkout.stripe.com/c/cs_2", "expires_at": exp.Unix()})
	hs, err := a.CreatePaymentMethodSetup(context.Background(), port.SetupInput{Customer: cus, SuccessURL: "https://app/ok", CancelURL: "https://app/no", ExpiresAt: exp}, "idem-s")
	if err != nil || hs.ProviderSessionID != "cs_2" || !hs.ExpiresAt.Equal(exp) {
		t.Fatalf("got %+v %v", hs, err)
	}
	r := f.only("POST", "/v1/checkout/sessions")
	wantForm(t, r, "mode", "setup", "customer", "cus_1", "currency", "usd", "expires_at", unix(exp))
	wantNoForm(t, r, "line_items[0][price]")
}

// Card summaries only (brand, last4, expiry); the invoice default is marked.
func TestListPaymentMethodsMarksDefault(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/customers/cus_1", 200, map[string]any{"id": "cus_1", "object": "customer",
		"invoice_settings": map[string]any{"default_payment_method": "pm_2"}})
	f.on("GET", "/v1/customers/cus_1/payment_methods", 200, list("/v1/customers/cus_1/payment_methods",
		map[string]any{"id": "pm_1", "object": "payment_method", "type": "card", "card": map[string]any{"brand": "visa", "last4": "4242", "exp_month": 1, "exp_year": 2030}},
		map[string]any{"id": "pm_2", "object": "payment_method", "type": "card", "card": map[string]any{"brand": "mastercard", "last4": "4444", "exp_month": 12, "exp_year": 2031}},
		map[string]any{"id": "pm_3", "object": "payment_method", "type": "card"},
	))
	pms, err := a.ListPaymentMethods(context.Background(), cus)
	if err != nil || len(pms) != 3 {
		t.Fatalf("got %+v %v", pms, err)
	}
	if pms[0].IsDefault || pms[0].Brand != "visa" || pms[0].Last4 != "4242" || pms[0].ExpMonth != 1 || pms[0].ExpYear != 2030 {
		t.Errorf("pm_1 %+v", pms[0])
	}
	if !pms[1].IsDefault || pms[1].ProviderRef != "pm_2" || pms[1].Brand != "mastercard" {
		t.Errorf("pm_2 %+v", pms[1])
	}
	if pms[2].ProviderRef != "pm_3" || pms[2].Brand != "" || pms[2].IsDefault {
		t.Errorf("pm_3 %+v", pms[2])
	}
	wantForm(t, f.only("GET", "/v1/customers/cus_1/payment_methods"), "type", "card")
}

// B-P16: the new method becomes the customer default and the default of every
// live (active / past_due) subscription; ended ones are left alone.
func TestAttachDefaultPaymentMethodUpdatesLiveSubscriptions(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("POST", "/v1/payment_methods/pm_9/attach", 200, map[string]any{"id": "pm_9", "object": "payment_method"})
	f.on("POST", "/v1/customers/cus_1", 200, map[string]any{"id": "cus_1", "object": "customer"})
	f.on("GET", "/v1/subscriptions", 200, list("/v1/subscriptions",
		sub("sub_a", "active", nil), sub("sub_c", "canceled", nil), sub("sub_p", "past_due", nil), sub("sub_i", "incomplete_expired", nil)))
	f.on("POST", "/v1/subscriptions/sub_a", 200, sub("sub_a", "active", nil))
	f.on("POST", "/v1/subscriptions/sub_p", 200, sub("sub_p", "past_due", nil))
	if err := a.AttachDefaultPaymentMethod(context.Background(), cus, "pm_9", "idem-pm"); err != nil {
		t.Fatal(err)
	}
	at := f.only("POST", "/v1/payment_methods/pm_9/attach")
	wantForm(t, at, "customer", "cus_1")
	def := f.only("POST", "/v1/customers/cus_1")
	wantForm(t, def, "invoice_settings[default_payment_method]", "pm_9")
	wantForm(t, f.only("GET", "/v1/subscriptions"), "customer", "cus_1")
	if at.Idem != "idem-pm:attach" || def.Idem != "idem-pm:default" {
		t.Errorf("idempotency keys %q %q", at.Idem, def.Idem)
	}
	for _, id := range []string{"sub_a", "sub_p"} {
		r := f.only("POST", "/v1/subscriptions/"+id)
		wantForm(t, r, "default_payment_method", "pm_9")
		if r.Idem != "idem-pm:"+id {
			t.Errorf("%s idempotency key %q", id, r.Idem)
		}
	}
	for _, id := range []string{"sub_c", "sub_i"} {
		if len(f.sent("POST", "/v1/subscriptions/"+id)) != 0 {
			t.Errorf("%s is not live and must not be updated", id)
		}
	}
}

func TestCreateSubscriptionOutcomes(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want port.PaymentStatus
		inv  string
	}{
		{"active", sub("sub_1", "active", nil), port.PaymentPaid, ""},
		{"trialing", sub("sub_1", "trialing", nil), port.PaymentPaid, ""},
		{"incomplete, invoice open", sub("sub_1", "incomplete", map[string]any{"latest_invoice": invoice("in_1", "open", nil)}), port.PaymentPending, "in_1"},
		{"incomplete, invoice void", sub("sub_1", "incomplete", map[string]any{"latest_invoice": invoice("in_1", "void", nil)}), port.PaymentDeclined, "in_1"},
		{"incomplete, no invoice", sub("sub_1", "incomplete", nil), port.PaymentPending, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a := newFakeStripe(t)
			f.on("POST", "/v1/subscriptions", 200, c.resp)
			res, err := a.CreateSubscription(context.Background(), port.SubscriptionInput{Customer: cus, PriceRef: "price_A", Quantity: 3,
				DefaultPaymentMethod: "pm_1", Metadata: map[string]string{"tenant_id": "t1"}}, "idem-sub")
			if err != nil || res.Outcome.Status != c.want || res.LatestInvoice.ProviderInvoiceID != c.inv || res.Ref.ProviderSubscriptionID != "sub_1" {
				t.Fatalf("got %+v %v", res, err)
			}
			if !res.PeriodStart.Equal(fixedNow) || !res.PeriodEnd.Equal(fixedNow.AddDate(0, 1, 0)) {
				t.Errorf("period from the seat item: %v – %v", res.PeriodStart, res.PeriodEnd)
			}
			r := f.only("POST", "/v1/subscriptions")
			wantForm(t, r, "customer", "cus_1", "items[0][price]", "price_A", "items[0][quantity]", "3",
				"payment_behavior", "default_incomplete", "default_payment_method", "pm_1", "metadata[tenant_id]", "t1", "expand[0]", "latest_invoice")
			if r.Idem != "idem-sub" {
				t.Errorf("idempotency key %q", r.Idem)
			}
		})
	}
}

func TestConfirmInitialPayment(t *testing.T) {
	ctx := context.Background()
	t.Run("no latest invoice yet", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "incomplete", nil))
		out, err := a.ConfirmInitialPayment(ctx, subRef, "k")
		if err != nil || out.Status != port.PaymentPending {
			t.Fatalf("got %+v %v", out, err)
		}
		wantForm(t, f.only("GET", "/v1/subscriptions/sub_1"), "expand[0]", "latest_invoice")
	})
	t.Run("already paid: no second payment", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", map[string]any{"latest_invoice": invoice("in_1", "paid", nil)}))
		out, err := a.ConfirmInitialPayment(ctx, subRef, "k")
		if err != nil || out.Status != port.PaymentPaid {
			t.Fatalf("got %+v %v", out, err)
		}
	})
	for _, c := range []struct {
		status string
		want   port.PaymentStatus
	}{{"paid", port.PaymentPaid}, {"uncollectible", port.PaymentDeclined}, {"open", port.PaymentPending}} {
		t.Run("pay → "+c.status, func(t *testing.T) {
			f, a := newFakeStripe(t)
			f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "incomplete", map[string]any{"latest_invoice": invoice("in_1", "open", nil)}))
			f.on("POST", "/v1/invoices/in_1/pay", 200, invoice("in_1", c.status, nil))
			out, err := a.ConfirmInitialPayment(ctx, subRef, "idem-pay")
			if err != nil || out.Status != c.want {
				t.Fatalf("got %+v %v", out, err)
			}
			r := f.only("POST", "/v1/invoices/in_1/pay")
			wantForm(t, r, "off_session", "true")
			if r.Idem != "idem-pay" {
				t.Errorf("idempotency key %q", r.Idem)
			}
		})
	}
}

// §8.6.3 renewal top-up keeps seats even when unpaid; a pending update
// awaiting payment is never reported as paid.
func TestChangeQuantityKeepIfUnpaidAndPendingUpdate(t *testing.T) {
	f, a := newFakeStripe(t)
	at := fixedNow.Add(-time.Minute)
	f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", nil))
	f.on("POST", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", map[string]any{
		"pending_update": map[string]any{"expires_at": fixedNow.Add(23 * time.Hour).Unix()},
		"latest_invoice": invoice("in_5", "paid", nil),
	}))
	res, err := a.ChangeQuantity(context.Background(), port.QuantityChange{Sub: subRef, From: 3, To: 5, ProrationInstant: at, KeepIfUnpaid: true}, "idem-q")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pending || res.Outcome.Status != port.PaymentPending || res.Invoice.ProviderInvoiceID != "in_5" {
		t.Errorf("got %+v, want pending with invoice in_5", res)
	}
	r := f.only("POST", "/v1/subscriptions/sub_1")
	wantForm(t, r, "items[0][id]", "si_1", "items[0][quantity]", "5", "payment_behavior", "allow_incomplete",
		"proration_behavior", "always_invoice", "proration_date", unix(at), "expand[0]", "latest_invoice")
	if r.Idem != "idem-q" {
		t.Errorf("idempotency key %q", r.Idem)
	}
}

func TestChangePriceWithoutInvoiceIsPaid(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", nil))
	f.on("POST", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", nil))
	res, err := a.ChangePrice(context.Background(), port.PriceChange{Sub: subRef, ToPriceRef: "price_B"}, "idem-p")
	if err != nil || res.Pending || res.Outcome.Status != port.PaymentPaid || res.Invoice.ProviderInvoiceID != "" {
		t.Fatalf("got %+v %v", res, err)
	}
	r := f.only("POST", "/v1/subscriptions/sub_1")
	wantForm(t, r, "items[0][id]", "si_1", "items[0][price]", "price_B", "payment_behavior", "pending_if_incomplete", "proration_behavior", "always_invoice")
	wantNoForm(t, r, "proration_date", "items[0][quantity]")
}

// An existing schedule is updated in place, not recreated (BIL-SUB-8).
func TestScheduleAtPeriodEndReusesSchedule(t *testing.T) {
	f, a := newFakeStripe(t)
	renew := fixedNow.AddDate(0, 1, 0)
	f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", map[string]any{"schedule": "sub_sched_7"}))
	f.on("POST", "/v1/subscription_schedules/sub_sched_7", 200, map[string]any{"id": "sub_sched_7", "object": "subscription_schedule"})
	ref, err := a.ScheduleAtPeriodEnd(context.Background(), port.PeriodEndChange{Sub: subRef, ToPriceRef: "price_B", ToQuantity: 2, At: renew}, "idem-sch")
	if err != nil || ref.ProviderScheduleID != "sub_sched_7" {
		t.Fatalf("got %+v %v", ref, err)
	}
	if len(f.sent("POST", "/v1/subscription_schedules")) != 0 {
		t.Error("an existing schedule must not be recreated")
	}
	r := f.only("POST", "/v1/subscription_schedules/sub_sched_7")
	wantForm(t, r, "end_behavior", "release",
		"phases[0][items][0][price]", "price_A", "phases[0][items][0][quantity]", "3",
		"phases[0][start_date]", unix(fixedNow), "phases[0][end_date]", unix(renew),
		"phases[1][items][0][price]", "price_B", "phases[1][items][0][quantity]", "2",
		"phases[1][start_date]", unix(renew), "phases[1][proration_behavior]", "none")
	if r.Idem != "idem-sch:phases" {
		t.Errorf("idempotency key %q", r.Idem)
	}
}

// BIL-SUB-7: pause holds new invoices as drafts and stops retries on open
// ones; resume clears pause_collection and restores auto_advance.
func TestPauseAndResumeCollection(t *testing.T) {
	for _, pause := range []bool{true, false} {
		t.Run(strconv.FormatBool(pause), func(t *testing.T) {
			f, a := newFakeStripe(t)
			f.on("POST", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", nil))
			f.on("GET", "/v1/invoices", 200, list("/v1/invoices", invoice("in_1", "open", nil), invoice("in_2", "open", nil)))
			f.on("POST", "/v1/invoices/in_1", 200, invoice("in_1", "open", nil))
			f.on("POST", "/v1/invoices/in_2", 200, invoice("in_2", "open", nil))
			if err := a.PauseCollection(context.Background(), subRef, pause, "idem-pc"); err != nil {
				t.Fatal(err)
			}
			r := f.only("POST", "/v1/subscriptions/sub_1")
			if pause {
				wantForm(t, r, "pause_collection[behavior]", "keep_as_draft")
			} else {
				wantForm(t, r, "pause_collection", "")
			}
			wantForm(t, f.only("GET", "/v1/invoices"), "subscription", "sub_1", "status", "open")
			for _, id := range []string{"in_1", "in_2"} {
				ir := f.only("POST", "/v1/invoices/"+id)
				wantForm(t, ir, "auto_advance", strconv.FormatBool(!pause))
				if ir.Idem != "idem-pc:"+id {
					t.Errorf("idempotency key %q", ir.Idem)
				}
			}
		})
	}
}

func TestPreviews(t *testing.T) {
	ctx := context.Background()
	at := fixedNow.Add(-time.Second)
	periodEnd := fixedNow.AddDate(0, 1, 0)
	preview := invoice("upcoming_in", "draft", map[string]any{"amount_due": 4600, "period_end": periodEnd.Unix()})

	f, a := newFakeStripe(t)
	f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", nil))
	f.on("POST", "/v1/invoices/create_preview", 200, preview)
	pv, err := a.PreviewQuantityChange(ctx, port.QuantityChange{Sub: subRef, To: 5, ProrationInstant: at})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Amount != (domain.Money{AmountMinor: 4600, Currency: "USD"}) || pv.RenewalAmount != (domain.Money{AmountMinor: 2300 * 5, Currency: "USD"}) ||
		!pv.ProrationInstant.Equal(at) || !pv.PeriodEnd.Equal(periodEnd) {
		t.Errorf("preview %+v", pv)
	}
	wantForm(t, f.only("POST", "/v1/invoices/create_preview"), "subscription", "sub_1",
		"subscription_details[items][0][id]", "si_1", "subscription_details[items][0][quantity]", "5",
		"subscription_details[proration_behavior]", "always_invoice", "subscription_details[proration_date]", unix(at))

	f, a = newFakeStripe(t)
	f.on("GET", "/v1/subscriptions/sub_1", 200, sub("sub_1", "active", nil))
	f.on("POST", "/v1/invoices/create_preview", 200, preview)
	pv, err = a.PreviewPriceChange(ctx, port.PriceChange{Sub: subRef, ToPriceRef: "price_B"})
	if err != nil || pv.Amount.AmountMinor != 4600 || !pv.ProrationInstant.IsZero() {
		t.Fatalf("got %+v %v", pv, err)
	}
	r := f.only("POST", "/v1/invoices/create_preview")
	wantForm(t, r, "subscription_details[items][0][price]", "price_B")
	wantNoForm(t, r, "subscription_details[proration_date]")
}

// An amount-based one-off (no price-book price) sends amount + currency; an
// empty description is omitted.
func TestChargeOneOffByAmount(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("POST", "/v1/invoices", 200, invoice("in_8", "draft", nil))
	f.on("POST", "/v1/invoiceitems", 200, map[string]any{"id": "ii_1", "object": "invoiceitem"})
	f.on("POST", "/v1/invoices/in_8/pay", 200, invoice("in_8", "paid", nil))
	res, err := a.ChargeOneOff(context.Background(), port.OneOffCharge{Customer: cus, Amount: domain.Money{AmountMinor: 1999, Currency: "AED"},
		Metadata: map[string]string{"purpose": "token_addon"}}, "idem-oo")
	if err != nil || res.Invoice.ProviderInvoiceID != "in_8" || res.Outcome.Status != port.PaymentPaid {
		t.Fatalf("got %+v %v", res, err)
	}
	ir := f.only("POST", "/v1/invoices")
	wantForm(t, ir, "customer", "cus_1", "collection_method", "charge_automatically", "auto_advance", "false", "metadata[purpose]", "token_addon")
	wantNoForm(t, ir, "description")
	item := f.only("POST", "/v1/invoiceitems")
	wantForm(t, item, "customer", "cus_1", "invoice", "in_8", "amount", "1999", "currency", "aed")
	wantNoForm(t, item, "pricing[price]", "quantity")
	pay := f.only("POST", "/v1/invoices/in_8/pay")
	wantForm(t, pay, "off_session", "true")
	if ir.Idem != "idem-oo:invoice" || item.Idem != "idem-oo:item" || pay.Idem != "idem-oo:pay" {
		t.Errorf("idempotency keys %q %q %q", ir.Idem, item.Idem, pay.Idem)
	}
}

func TestInvoiceLinks(t *testing.T) {
	ctx := context.Background()
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "open", map[string]any{
		"invoice_pdf": "https://pay.stripe.com/invoice/x/pdf", "hosted_invoice_url": "https://invoice.stripe.com/i/x"}))
	pdf, err := a.InvoicePDFLink(ctx, invRef)
	if err != nil || pdf.URL != "https://pay.stripe.com/invoice/x/pdf" || !pdf.ExpiresAt.Equal(fixedNow.Add(invoiceLinkTTL)) {
		t.Fatalf("pdf %+v %v", pdf, err)
	}
	pay, err := a.PayOpenInvoiceLink(ctx, invRef)
	if err != nil || pay.URL != "https://invoice.stripe.com/i/x" || !pay.ExpiresAt.Equal(fixedNow.Add(invoiceLinkTTL)) {
		t.Fatalf("pay %+v %v", pay, err)
	}

	f, a = newFakeStripe(t)
	f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "draft", nil))
	if _, err := a.InvoicePDFLink(ctx, invRef); !errors.Is(err, port.ErrNotFound) {
		t.Errorf("no PDF yet: %v", err)
	}
	if _, err := a.PayOpenInvoiceLink(ctx, invRef); !errors.Is(err, port.ErrNotFound) {
		t.Errorf("no hosted page yet: %v", err)
	}
}

// The invoice mirror (§4.2.13): kind, money in upper-case currency, times.
func TestGetInvoiceView(t *testing.T) {
	paidAt := fixedNow.Add(time.Hour)
	cases := []struct {
		extra map[string]any
		want  domain.InvoiceKind
	}{
		{map[string]any{"metadata": map[string]any{"purpose": "token_addon"}, "billing_reason": "manual"}, domain.InvoiceTokenAddon},
		{map[string]any{"billing_reason": "subscription_create"}, domain.InvoiceSubscriptionCreate},
		{map[string]any{"billing_reason": "subscription_cycle"}, domain.InvoiceSubscriptionCycle},
		{map[string]any{"billing_reason": "subscription_update"}, domain.InvoiceProration},
		{map[string]any{"billing_reason": "manual"}, domain.InvoiceManual},
	}
	for _, c := range cases {
		f, a := newFakeStripe(t)
		inv := invoice("in_1", "paid", map[string]any{"number": "INV-1", "amount_due": 6900, "amount_paid": 6900,
			"period_start": fixedNow.Unix(), "period_end": fixedNow.AddDate(0, 1, 0).Unix(), "created": fixedNow.Unix(),
			"status_transitions": map[string]any{"paid_at": paidAt.Unix()}, "hosted_invoice_url": "https://invoice.stripe.com/i/x"})
		for k, v := range c.extra {
			inv[k] = v
		}
		f.on("GET", "/v1/invoices/in_1", 200, inv)
		v, err := a.GetInvoice(context.Background(), invRef)
		if err != nil {
			t.Fatal(err)
		}
		if v.Kind != c.want || v.Number != "INV-1" || v.Status != "paid" || v.AmountDue != (domain.Money{AmountMinor: 6900, Currency: "USD"}) ||
			v.PaidAt == nil || !v.PaidAt.Equal(paidAt) || v.IssuedAt == nil || v.PeriodStart == nil || v.PeriodEnd == nil ||
			v.HostedURL != "https://invoice.stripe.com/i/x" {
			t.Errorf("%v: view %+v", c.want, v)
		}
	}
	f, a := newFakeStripe(t)
	f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "draft", nil))
	v, err := a.GetInvoice(context.Background(), invRef)
	if err != nil || v.PaidAt != nil || v.IssuedAt != nil || v.PeriodStart != nil || v.PeriodEnd != nil {
		t.Fatalf("unset times stay nil: %+v %v", v, err)
	}
}

// Refunds go against the invoice's PaymentIntent (from its payments).
func TestRefundRequest(t *testing.T) {
	for _, reason := range []port.RefundReason{port.RefundReasonRequestedByCustomer, ""} {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "paid", map[string]any{"payments": list("/v1/invoice_payments",
			map[string]any{"id": "inpay_0", "object": "invoice_payment", "payment": map[string]any{"type": "charge", "charge": "ch_0"}},
			map[string]any{"id": "inpay_1", "object": "invoice_payment", "payment": map[string]any{"type": "payment_intent", "payment_intent": "pi_1"}})}))
		f.on("POST", "/v1/refunds", 200, map[string]any{"id": "re_1", "object": "refund", "status": "pending"})
		res, err := a.Refund(context.Background(), port.RefundInput{Invoice: invRef, Amount: usd5, Reason: reason}, "idem-re")
		if err != nil || res.ProviderRefundID != "re_1" || res.Status != "pending" {
			t.Fatalf("got %+v %v", res, err)
		}
		wantForm(t, f.only("GET", "/v1/invoices/in_1"), "expand[0]", "payments")
		r := f.only("POST", "/v1/refunds")
		wantForm(t, r, "payment_intent", "pi_1", "amount", "500")
		if reason != "" {
			wantForm(t, r, "reason", string(reason))
		} else {
			wantNoForm(t, r, "reason")
		}
		if r.Idem != "idem-re" {
			t.Errorf("idempotency key %q", r.Idem)
		}
	}
}

func TestCreditBalanceRequest(t *testing.T) {
	f, a := newFakeStripe(t)
	f.on("POST", "/v1/customers/cus_1/balance_transactions", 200, map[string]any{"id": "cbtxn_1", "object": "customer_balance_transaction"})
	id, err := a.CreditBalance(context.Background(), port.BalanceAdjustment{Customer: cus, Amount: domain.Money{AmountMinor: -1500, Currency: "USD"}, Description: "goodwill"}, "idem-cb")
	if err != nil || id != "cbtxn_1" {
		t.Fatalf("got %q %v", id, err)
	}
	r := f.only("POST", "/v1/customers/cus_1/balance_transactions")
	wantForm(t, r, "amount", "-1500", "currency", "usd", "description", "goodwill")
	if r.Idem != "idem-cb" {
		t.Errorf("idempotency key %q", r.Idem)
	}
}

// BIL-PAY-4 re-reads: the fields each handler relies on.
func TestFetchCurrentFields(t *testing.T) {
	ctx := context.Background()
	t.Run("subscription without items", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/subscriptions/sub_1", 200, map[string]any{"id": "sub_1", "object": "subscription", "status": "canceled",
			"items": list("/v1/subscription_items")})
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectSubscription, ID: "sub_1"})
		if err != nil || st.Status != "canceled" || !st.Fields["current_period_start"].(time.Time).IsZero() || st.Fields["quantity"] != nil {
			t.Fatalf("got %+v %v", st, err)
		}
	})
	t.Run("invoice without parent", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/invoices/in_1", 200, invoice("in_1", "open", map[string]any{"billing_reason": "manual"}))
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_1"})
		if err != nil || st.Fields["subscription"] != "" || st.Fields["kind"] != string(domain.InvoiceManual) {
			t.Fatalf("got %+v %v", st, err)
		}
	})
	t.Run("invoice names its Billing subscription", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/invoices/in_2", 200, invoice("in_2", "paid", map[string]any{"billing_reason": "subscription_create",
			"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{
				"subscription": "sub_9", "metadata": map[string]any{"billing_subscription_id": "bs_1", "tenant_id": "t1"}}}}))
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_2"})
		if err != nil || st.Fields["subscription"] != "sub_9" || st.Fields["billing_subscription_id"] != "bs_1" {
			t.Fatalf("got %+v %v", st, err)
		}
	})
	t.Run("invoice carries its total and charged lines (BIL-PAY-6, BIL-PAY-7)", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/invoices/in_3", 200, invoice("in_3", "paid", map[string]any{"billing_reason": "subscription_update",
			"amount_paid": 4000, "total": 4600, "lines": map[string]any{"object": "list", "url": "/v1/invoices/in_3/lines", "data": []any{
				map[string]any{"id": "il_1", "object": "line_item", "amount": -23000, "quantity": 10},
				map[string]any{"id": "il_2", "object": "line_item", "amount": 27600, "quantity": 12,
					"pricing": map[string]any{"type": "price_details", "price_details": map[string]any{"price": "price_pro"}}}}}}))
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_3"})
		qs, _ := st.Fields["charged_line_quantities"].([]int64)
		prices, _ := st.Fields["charged_line_prices"].([]string)
		if err != nil || st.Fields["total_minor"] != int64(4600) || st.Fields["amount_paid_minor"] != int64(4000) ||
			len(qs) != 1 || qs[0] != 12 || len(prices) != 1 || prices[0] != "price_pro" {
			t.Fatalf("got %+v %v", st, err)
		}
	})
	t.Run("subscription checkout session carries its first invoice", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/checkout/sessions/cs_2", 200, map[string]any{"id": "cs_2", "object": "checkout.session", "status": "complete",
			"mode": "subscription", "subscription": "sub_2",
			"invoice": invoice("in_7", "paid", map[string]any{"lines": list("/v1/invoices/in_7/lines", map[string]any{
				"id": "il_1", "object": "line_item", "quantity": 4,
				"period": map[string]any{"start": fixedNow.Unix(), "end": fixedNow.AddDate(0, 1, 0).Unix()}})})})
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: "cs_2"})
		if err != nil || st.Fields["invoice"] != "in_7" || st.Fields["invoice_status"] != "paid" || st.Fields["invoice_line_quantity"] != int64(4) ||
			!st.Fields["invoice_line_period_start"].(time.Time).Equal(fixedNow) || st.Fields["subscription"] != "sub_2" {
			t.Fatalf("got %+v %v", st, err)
		}
		wantForm(t, f.only("GET", "/v1/checkout/sessions/cs_2"), "expand[1]", "invoice")
	})
	t.Run("setup checkout session", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/checkout/sessions/cs_1", 200, map[string]any{"id": "cs_1", "object": "checkout.session", "status": "complete", "mode": "setup",
			"metadata":     map[string]any{"tenant_id": "t1"},
			"setup_intent": map[string]any{"id": "seti_1", "object": "setup_intent", "payment_method": "pm_7"},
			"subscription": "sub_1"})
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectCheckoutSession, ID: "cs_1"})
		if err != nil || st.Status != "complete" || st.Fields["mode"] != "setup" || st.Fields["setup_payment_method"] != "pm_7" ||
			st.Fields["subscription"] != "sub_1" || st.Metadata["tenant_id"] != "t1" {
			t.Fatalf("got %+v %v", st, err)
		}
		wantForm(t, f.only("GET", "/v1/checkout/sessions/cs_1"), "expand[0]", "setup_intent")
	})
	t.Run("refund", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/refunds/re_1", 200, map[string]any{"id": "re_1", "object": "refund", "status": "failed", "amount": 500, "currency": "aed",
			"metadata": map[string]any{"billing_refund_id": "r1"}})
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectRefund, ID: "re_1"})
		if err != nil || st.Status != "failed" || st.Fields["amount_minor"] != int64(500) || st.Fields["currency"] != "AED" || st.Metadata["billing_refund_id"] != "r1" {
			t.Fatalf("got %+v %v", st, err)
		}
	})
	t.Run("dispute with charge and payment intent", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/disputes/dp_1", 200, map[string]any{"id": "dp_1", "object": "dispute", "status": "needs_response", "reason": "fraudulent",
			"amount": 6900, "created": fixedNow.Unix(), "charge": "ch_1", "payment_intent": "pi_1"})
		f.on("GET", "/v1/invoice_payments", 200, list("/v1/invoice_payments",
			map[string]any{"id": "inpay_1", "object": "invoice_payment", "invoice": "in_9"}))
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectDispute, ID: "dp_1"})
		if err != nil || st.Status != "needs_response" || st.Fields["reason"] != "fraudulent" || st.Fields["amount_minor"] != int64(6900) ||
			!st.Fields["created"].(time.Time).Equal(fixedNow) || st.Fields["payment_ref"] != "ch_1" || st.Fields["payment_intent"] != "pi_1" ||
			st.Fields["invoice"] != "in_9" {
			t.Fatalf("got %+v %v", st, err)
		}
		wantForm(t, f.only("GET", "/v1/invoice_payments"), "payment[type]", "payment_intent", "payment[payment_intent]", "pi_1", "limit", "1")
	})
	// A failed invoice-payments lookup is retryable, never "no invoice": a
	// dispute applied without its invoice would miss the CD-10 conversion
	// payment check (bug fixed 2026-11-27).
	t.Run("dispute with payment intent only, invoice lookup failing", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/disputes/dp_2", 200, map[string]any{"id": "dp_2", "object": "dispute", "status": "lost", "payment_intent": "pi_2"})
		f.on("GET", "/v1/invoice_payments", 500, unavailable)
		_, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectDispute, ID: "dp_2"})
		if !errors.Is(err, port.ErrProviderUnavailable) {
			t.Fatalf("err %v, want ErrProviderUnavailable", err)
		}
	})
	t.Run("dispute with payment intent and no invoice payment", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/disputes/dp_3", 200, map[string]any{"id": "dp_3", "object": "dispute", "status": "lost", "payment_intent": "pi_3"})
		f.on("GET", "/v1/invoice_payments", 200, list("/v1/invoice_payments"))
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectDispute, ID: "dp_3"})
		if err != nil || st.Fields["payment_ref"] != "pi_3" || st.Fields["invoice"] != nil {
			t.Fatalf("got %+v %v", st, err)
		}
	})
	t.Run("early fraud warning on a bare charge", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/radar/early_fraud_warnings/issfr_1", 200, map[string]any{"id": "issfr_1", "object": "radar.early_fraud_warning",
			"fraud_type": "unauthorized_use_of_card", "created": fixedNow.Unix(), "charge": "ch_3"})
		st, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectEarlyFraudWarning, ID: "issfr_1"})
		if err != nil || st.Status != "open" || st.Fields["reason"] != "unauthorized_use_of_card" || st.Fields["payment_ref"] != "ch_3" ||
			st.Fields["payment_intent"] != nil || st.Fields["invoice"] != nil {
			t.Fatalf("got %+v %v", st, err)
		}
		if len(f.sent("GET", "/v1/invoice_payments")) != 0 {
			t.Error("no PaymentIntent: no invoice-payment lookup")
		}
	})
}

// §8.13: 100 per page, the cursor continues, events are normalised for repair.
func TestListSincePaging(t *testing.T) {
	ctx := context.Background()
	since := fixedNow.Add(-24 * time.Hour)
	page := func(prefix, object string, n int, extra func(i int) map[string]any) map[string]any {
		items := make([]any, n)
		for i := range items {
			it := map[string]any{"id": prefix + strconv.Itoa(i), "object": object}
			for k, v := range extra(i) {
				it[k] = v
			}
			items[i] = it
		}
		l := list("/v1/"+object+"s", items...)
		l["has_more"] = true
		return l
	}
	none := func(int) map[string]any { return nil }
	for _, c := range []struct {
		kind       port.ReconKind
		path, obj  string
		wantStatus string
	}{
		{port.ReconSubscriptions, "/v1/subscriptions", "subscription", ""},
		{port.ReconInvoices, "/v1/invoices", "invoice", ""},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			f, a := newFakeStripe(t)
			f.on("GET", c.path, 200, page("x_", c.obj, 100, none))
			pg, err := a.ListSince(ctx, c.kind, since, "x_prev")
			if err != nil || len(pg.Items) != 100 || pg.NextCursor != "x_99" {
				t.Fatalf("got %d items, cursor %q, %v", len(pg.Items), pg.NextCursor, err)
			}
			r := f.only("GET", c.path)
			wantForm(t, r, "starting_after", "x_prev", "limit", "100", "created[gte]", unix(since))
			if c.kind == port.ReconSubscriptions {
				wantForm(t, r, "status", "all")
			}
		})
	}
	t.Run("events", func(t *testing.T) {
		f, a := newFakeStripe(t)
		f.on("GET", "/v1/events", 200, page("evt_", "event", 100, func(i int) map[string]any {
			return map[string]any{"type": "invoice.paid", "created": fixedNow.Unix(), "livemode": i == 1,
				"data": map[string]any{"object": map[string]any{"id": "in_" + strconv.Itoa(i), "object": "invoice", "customer": "cus_1"}}}
		}))
		pg, err := a.ListSince(ctx, port.ReconEvents, since, "evt_prev")
		if err != nil || len(pg.Items) != 100 || pg.NextCursor != "evt_99" {
			t.Fatalf("got %d items, cursor %q, %v", len(pg.Items), pg.NextCursor, err)
		}
		wantForm(t, f.only("GET", "/v1/events"), "starting_after", "evt_prev", "limit", "100")
		ev, ok := pg.Items[0].Fields["event"].(domain.PaymentEvent)
		if !ok || ev.Kind != domain.KindInvoicePaid || ev.Object.ID != "in_0" || ev.ProviderEventID != "evt_0" || pg.Items[0].Status != "invoice.paid" {
			t.Errorf("event 0 not normalised: %+v", pg.Items[0])
		}
		if pg.Items[1].Fields != nil {
			t.Errorf("a livemode event in a test-mode environment must not be normalised: %+v", pg.Items[1])
		}
	})
	t.Run("unsupported kind", func(t *testing.T) {
		_, a := newFakeStripe(t)
		if _, err := a.ListSince(ctx, "charges", since, ""); !errors.Is(err, port.ErrUnsupported) {
			t.Fatalf("got %v", err)
		}
	})
}

// B-O11 / B-O12 price verification.
func TestVerifyPrice(t *testing.T) {
	month := port.PriceExpectation{Amount: domain.Money{AmountMinor: 2300, Currency: "USD"}, Interval: "month"}
	oneOff := port.PriceExpectation{Amount: domain.Money{AmountMinor: 2300, Currency: "USD"}}
	recurring := func(iv string) map[string]any { return map[string]any{"interval": iv, "interval_count": 1} }
	cases := []struct {
		name     string
		price    map[string]any
		want     port.PriceExpectation
		problems []string // "" = match
	}{
		{"monthly match", map[string]any{"active": true, "unit_amount": 2300, "currency": "usd", "recurring": recurring("month")}, month, nil},
		{"one-off match", map[string]any{"active": true, "unit_amount": 2300, "currency": "usd"}, oneOff, nil},
		{"everything differs", map[string]any{"active": false, "unit_amount": 2500, "currency": "aed", "recurring": recurring("year")}, month,
			[]string{"inactive", "amount 2500 ≠ 2300", "currency AED ≠ USD", "interval differs"}},
		{"recurring for a pack", map[string]any{"active": true, "unit_amount": 2300, "currency": "usd", "recurring": recurring("month")}, oneOff,
			[]string{"recurring price for a one-off pack"}},
		{"one-off for a plan", map[string]any{"active": true, "unit_amount": 2300, "currency": "usd"}, month, []string{"interval differs"}},
		// A 3-monthly price is not "month" (bug fixed 2026-11-27).
		{"every 3 months for a monthly plan", map[string]any{"active": true, "unit_amount": 2300, "currency": "usd",
			"recurring": map[string]any{"interval": "month", "interval_count": 3}}, month, []string{"interval count 3 ≠ 1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a := newFakeStripe(t)
			c.price["id"], c.price["object"] = "price_A", "price"
			f.on("GET", "/v1/prices/price_A", 200, c.price)
			err := a.VerifyPrice(context.Background(), "price_A", c.want)
			if c.problems == nil {
				if err != nil {
					t.Fatalf("match: %v", err)
				}
				return
			}
			if !errors.Is(err, port.ErrPriceMismatch) {
				t.Fatalf("got %v, want ErrPriceMismatch", err)
			}
			if !strings.HasSuffix(err.Error(), strings.Join(c.problems, "; ")) {
				t.Errorf("problems: %v, want %q", err, strings.Join(c.problems, "; "))
			}
		})
	}
}
