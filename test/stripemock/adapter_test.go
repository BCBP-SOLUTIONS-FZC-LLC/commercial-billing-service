//go:build integration

// Package stripemock_test runs the Stripe adapter against stripe-mock (LLD
// §14.2): stripe-mock validates every request against Stripe's OpenAPI spec
// and answers with fixtures, so these tests prove the request shapes and the
// response mapping, not Stripe's behaviour (that is the certification suite,
// §14.7, in Stripe test mode).
package stripemock_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	stripeadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/stripe"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

var adapter *stripeadapter.Adapter

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "stripe/stripe-mock:latest",
			ExposedPorts: []string{"12111/tcp"},
			WaitingFor:   wait.ForListeningPort("12111/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stripe-mock: %v\n", err)
		return 1
	}
	defer func() { _ = c.Terminate(ctx) }()
	host, _ := c.Host(ctx)
	p, _ := c.MappedPort(ctx, "12111")
	adapter, err = stripeadapter.New(stripeadapter.Config{
		APIKey: "sk_test_123", WebhookSecrets: []string{"whsec_test"}, Livemode: false,
		BaseURL: fmt.Sprintf("http://%s:%s", host, p.Port()), Timeout: 10 * time.Second, MaxRPS: 50,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "adapter: %v\n", err)
		return 1
	}
	return m.Run()
}

func ok(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

func TestCustomersAndPaymentMethods(t *testing.T) {
	ctx := context.Background()
	ref, err := adapter.EnsureCustomer(ctx, port.CustomerInput{TenantID: "3fa85f64-5717-4562-b3fc-2c963f66afa6", Currency: "USD", Name: "Acme", Email: "billing@example.com"}, "idem-cust-1")
	ok(t, "EnsureCustomer", err)
	if ref.ProviderCustomerID == "" {
		t.Fatal("customer id")
	}
	ok(t, "UpdateCustomer", adapter.UpdateCustomer(ctx, ref, port.CustomerInput{Name: "Acme FZ", Email: "ap@example.com"}, "idem-cust-2"))
	_, err = adapter.ListPaymentMethods(ctx, ref)
	ok(t, "ListPaymentMethods", err)
	ok(t, "AttachDefaultPaymentMethod", adapter.AttachDefaultPaymentMethod(ctx, ref, "pm_card_visa", "idem-pm-1"))
	s, err := adapter.CreatePaymentMethodSetup(ctx, port.SetupInput{Customer: ref, SuccessURL: "https://app.example.com/billing?setup=ok", CancelURL: "https://app.example.com/billing"}, "idem-setup-1")
	ok(t, "CreatePaymentMethodSetup", err)
	if s.ProviderSessionID == "" {
		t.Error("setup session id")
	}
}

func TestCheckoutSessions(t *testing.T) {
	ctx := context.Background()
	ref := port.CustomerRef{ProviderCustomerID: "cus_123"}
	for _, purpose := range []port.CheckoutPurpose{port.CheckoutConversion, port.CheckoutReactivation, port.CheckoutAddonPayment} {
		s, err := adapter.CreateHostedCheckout(ctx, port.CheckoutInput{Customer: ref, Purpose: purpose, PriceRef: "price_123", Quantity: 10, Currency: "USD",
			Metadata:   map[string]string{"tenant_id": "t1", "billing_subscription_id": "s1", "purpose": string(purpose)},
			SuccessURL: "https://app.example.com/billing?checkout=ok", CancelURL: "https://app.example.com/billing",
			ExpiresAt: time.Now().Add(time.Hour)}, "idem-co-"+string(purpose))
		ok(t, "CreateHostedCheckout "+string(purpose), err)
		if err == nil && (s.ProviderSessionID == "" || s.URL == "") {
			t.Errorf("%s: session %+v", purpose, s)
		}
	}
}

func TestSubscriptionLifecycleCalls(t *testing.T) {
	ctx := context.Background()
	sub := port.SubscriptionRef{ProviderSubscriptionID: "sub_123"}
	res, err := adapter.CreateSubscription(ctx, port.SubscriptionInput{Customer: port.CustomerRef{ProviderCustomerID: "cus_123"}, PriceRef: "price_123", Quantity: 5,
		Currency: "USD", DefaultPaymentMethod: "pm_123", Metadata: map[string]string{"tenant_id": "t1"}}, "idem-sub-1")
	ok(t, "CreateSubscription", err)
	if err == nil && res.Ref.ProviderSubscriptionID == "" {
		t.Error("subscription id")
	}
	_, err = adapter.ConfirmInitialPayment(ctx, sub, "idem-confirm-1")
	ok(t, "ConfirmInitialPayment", err)
	_, err = adapter.ChangeQuantity(ctx, port.QuantityChange{Sub: sub, From: 5, To: 7, ProrationInstant: time.Now(), InvoiceNow: true}, "idem-qty-1")
	ok(t, "ChangeQuantity", err)
	_, err = adapter.ChangePrice(ctx, port.PriceChange{Sub: sub, ToPriceRef: "price_456", ProrationInstant: time.Now()}, "idem-price-1")
	ok(t, "ChangePrice", err)
	_, err = adapter.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: sub, ToPriceRef: "price_789", ToQuantity: 4, At: time.Now().Add(20 * 24 * time.Hour)}, "idem-sched-1")
	ok(t, "ScheduleAtPeriodEnd", err)
	ok(t, "ReleaseSchedule", adapter.ReleaseSchedule(ctx, port.ScheduleRef{ProviderScheduleID: "sub_sched_123"}, "idem-rel-1"))
	ok(t, "SetCancelAtPeriodEnd", adapter.SetCancelAtPeriodEnd(ctx, sub, true, "idem-cape-1"))
	ok(t, "PauseCollection pause", adapter.PauseCollection(ctx, sub, true, "idem-pause-1"))
	ok(t, "PauseCollection resume", adapter.PauseCollection(ctx, sub, false, "idem-resume-1"))
	ok(t, "CancelNow", adapter.CancelNow(ctx, sub, "idem-cancel-1"))
}

func TestPreviewsAndCharges(t *testing.T) {
	ctx := context.Background()
	sub := port.SubscriptionRef{ProviderSubscriptionID: "sub_123"}
	pv, err := adapter.PreviewQuantityChange(ctx, port.QuantityChange{Sub: sub, From: 5, To: 7, ProrationInstant: time.Now()})
	ok(t, "PreviewQuantityChange", err)
	if err == nil && !domain.ValidCurrency(pv.Amount.Currency) {
		t.Errorf("preview currency %q", pv.Amount.Currency)
	}
	_, err = adapter.PreviewPriceChange(ctx, port.PriceChange{Sub: sub, ToPriceRef: "price_456", ProrationInstant: time.Now()})
	ok(t, "PreviewPriceChange", err)
	_, err = adapter.ChargeOneOff(ctx, port.OneOffCharge{Customer: port.CustomerRef{ProviderCustomerID: "cus_123"}, PriceRef: "price_pack", Quantity: 2,
		Description: "Token add-on", Metadata: map[string]string{"purpose": "token_addon", "purchase_id": "p1"}}, "idem-addon-1")
	ok(t, "ChargeOneOff", err)
}

func TestInvoicesRefundsAdjustments(t *testing.T) {
	ctx := context.Background()
	inv := port.InvoiceRef{ProviderInvoiceID: "in_123"}
	v, err := adapter.GetInvoice(ctx, inv)
	ok(t, "GetInvoice", err)
	if err == nil && v.Ref.ProviderInvoiceID == "" {
		t.Error("invoice id")
	}
	_, err = adapter.InvoicePDFLink(ctx, inv)
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		t.Errorf("InvoicePDFLink: %v", err)
	}
	_, err = adapter.PayOpenInvoiceLink(ctx, inv)
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		t.Errorf("PayOpenInvoiceLink: %v", err)
	}
	ok(t, "FinalizeInvoice", adapter.FinalizeInvoice(ctx, inv, "idem-fin-1"))
	ok(t, "VoidInvoice", adapter.VoidInvoice(ctx, inv, "idem-void-1"))
	_, err = adapter.Refund(ctx, port.RefundInput{Invoice: inv, Amount: domain.Money{AmountMinor: 500, Currency: "USD"}, Reason: port.RefundReasonDuplicate}, "idem-ref-1")
	if err != nil && !errors.Is(err, port.ErrNotFound) { // fixture invoice may carry no payment
		t.Errorf("Refund: %v", err)
	}
	_, err = adapter.CreditBalance(ctx, port.BalanceAdjustment{Customer: port.CustomerRef{ProviderCustomerID: "cus_123"}, Amount: domain.Money{AmountMinor: -500, Currency: "USD"}, Description: "Service credit"}, "idem-bal-1")
	ok(t, "CreditBalance", err)
}

func TestFetchCurrentAndListSince(t *testing.T) {
	ctx := context.Background()
	for _, ref := range []domain.ProviderObjectRef{
		{Kind: domain.ObjectSubscription, ID: "sub_123"},
		{Kind: domain.ObjectInvoice, ID: "in_123"},
		{Kind: domain.ObjectCheckoutSession, ID: "cs_123"},
	} {
		st, err := adapter.FetchCurrent(ctx, ref)
		ok(t, "FetchCurrent "+string(ref.Kind), err)
		if err == nil && st.Status == "" {
			t.Errorf("%s: no status", ref.Kind)
		}
	}
	if _, err := adapter.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: "charge", ID: "ch_1"}); !errors.Is(err, port.ErrUnsupported) {
		t.Errorf("unknown kind: %v", err)
	}
	since := time.Now().Add(-24 * time.Hour)
	for _, k := range []port.ReconKind{port.ReconSubscriptions, port.ReconInvoices, port.ReconEvents} {
		_, err := adapter.ListSince(ctx, k, since, "")
		ok(t, "ListSince "+string(k), err)
	}
}
