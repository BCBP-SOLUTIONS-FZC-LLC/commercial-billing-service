package manual

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Every provider operation beyond recording a subscription is refused as
// 409 PROVIDER_OPERATION_UNSUPPORTED (LLD §3.4, §8.1.4).
func TestEveryOtherOperationUnsupported(t *testing.T) {
	a, ctx := New(), context.Background()
	checks := map[string]func() error{
		"UpdateCustomer": func() error { return a.UpdateCustomer(ctx, port.CustomerRef{}, port.CustomerInput{}, "k") },
		"CreateHostedCheckout": func() error {
			_, err := a.CreateHostedCheckout(ctx, port.CheckoutInput{}, "k")
			return err
		},
		"ExpireCheckout": func() error { return a.ExpireCheckout(ctx, "cs_1", "k") },
		"CreatePaymentMethodSetup": func() error {
			_, err := a.CreatePaymentMethodSetup(ctx, port.SetupInput{}, "k")
			return err
		},
		"AttachDefaultPaymentMethod": func() error { return a.AttachDefaultPaymentMethod(ctx, port.CustomerRef{}, "pm_1", "k") },
		"ConfirmInitialPayment": func() error {
			_, err := a.ConfirmInitialPayment(ctx, port.SubscriptionRef{}, "k")
			return err
		},
		"ChangePrice": func() error { _, err := a.ChangePrice(ctx, port.PriceChange{}, "k"); return err },
		"ScheduleAtPeriodEnd": func() error {
			_, err := a.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{}, "k")
			return err
		},
		"ReleaseSchedule":       func() error { return a.ReleaseSchedule(ctx, port.ScheduleRef{}, "k") },
		"SetCancelAtPeriodEnd":  func() error { return a.SetCancelAtPeriodEnd(ctx, port.SubscriptionRef{}, true, "k") },
		"PreviewQuantityChange": func() error { _, err := a.PreviewQuantityChange(ctx, port.QuantityChange{}); return err },
		"PreviewPriceChange":    func() error { _, err := a.PreviewPriceChange(ctx, port.PriceChange{}); return err },
		"GetInvoice":            func() error { _, err := a.GetInvoice(ctx, port.InvoiceRef{}); return err },
		"InvoicePDFLink":        func() error { _, err := a.InvoicePDFLink(ctx, port.InvoiceRef{}); return err },
		"PayOpenInvoiceLink":    func() error { _, err := a.PayOpenInvoiceLink(ctx, port.InvoiceRef{}); return err },
		"VoidInvoice":           func() error { return a.VoidInvoice(ctx, port.InvoiceRef{}, "k") },
		"CreditBalance": func() error {
			_, err := a.CreditBalance(ctx, port.BalanceAdjustment{}, "k")
			return err
		},
		"FetchCurrent": func() error {
			_, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_1"})
			return err
		},
	}
	for name, fn := range checks {
		if err := fn(); !errors.Is(err, port.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
}

// Reads that have nothing to return succeed empty: no customer, no payment
// methods, nothing to reconcile, no provider price to verify.
func TestNoOpReads(t *testing.T) {
	a, ctx := New(), context.Background()
	if a.Name() != Name || a.AdapterVersion() != AdapterVersion {
		t.Errorf("identity %s %s", a.Name(), a.AdapterVersion())
	}
	if ref, err := a.EnsureCustomer(ctx, port.CustomerInput{TenantID: "t1"}, "k"); err != nil || ref.ProviderCustomerID != "" {
		t.Errorf("EnsureCustomer: %+v %v (manual accounts have no provider customer)", ref, err)
	}
	if pms, err := a.ListPaymentMethods(ctx, port.CustomerRef{}); err != nil || len(pms) != 0 {
		t.Errorf("ListPaymentMethods: %+v %v", pms, err)
	}
	if pg, err := a.ListSince(ctx, port.ReconInvoices, time.Now(), ""); err != nil || len(pg.Items) != 0 || pg.NextCursor != "" {
		t.Errorf("ListSince: %+v %v", pg, err)
	}
	if err := a.VerifyPrice(ctx, "", port.PriceExpectation{}); err != nil {
		t.Errorf("VerifyPrice: %v", err)
	}
}
