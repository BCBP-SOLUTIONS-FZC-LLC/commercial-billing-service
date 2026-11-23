package manual

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// The exempt record in migration 000004 must name exactly AdapterVersion.
func TestAdapterVersionMatchesMigration(t *testing.T) {
	sql, err := os.ReadFile("../../postgres/migrations/000004_provider_certifications.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), "'manual', '"+AdapterVersion+"'") {
		t.Fatalf("migration 000004 does not insert the exempt record for %s", AdapterVersion)
	}
}

func TestCreateSubscriptionIsPaidNow(t *testing.T) {
	a := New()
	fixed := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return fixed }
	res, err := a.CreateSubscription(context.Background(), port.SubscriptionInput{Metadata: map[string]string{"interval": "month"}}, "k")
	if err != nil || res.Outcome.Status != port.PaymentPaid || !res.PeriodStart.Equal(fixed) || !res.PeriodEnd.After(fixed) {
		t.Fatalf("got %+v %v", res, err)
	}
	year, _ := a.CreateSubscription(context.Background(), port.SubscriptionInput{Metadata: map[string]string{"interval": "year"}}, "k")
	if !year.PeriodEnd.Equal(fixed.AddDate(1, 0, 0)) {
		t.Fatalf("annual stub period end %v", year.PeriodEnd)
	}
}

func TestOtherMutationsUnsupported(t *testing.T) {
	a, ctx := New(), context.Background()
	checks := map[string]error{
		"ChangeQuantity":  func() error { _, err := a.ChangeQuantity(ctx, port.QuantityChange{}, "k"); return err }(),
		"CancelNow":       a.CancelNow(ctx, port.SubscriptionRef{}, "k"),
		"PauseCollection": a.PauseCollection(ctx, port.SubscriptionRef{}, true, "k"),
		"ChargeOneOff":    func() error { _, err := a.ChargeOneOff(ctx, port.OneOffCharge{}, "k"); return err }(),
		"Refund":          func() error { _, err := a.Refund(ctx, port.RefundInput{}, "k"); return err }(),
		"FinalizeInvoice": a.FinalizeInvoice(ctx, port.InvoiceRef{}, "k"),
	}
	for name, err := range checks {
		if !errors.Is(err, port.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
	if _, err := a.VerifyAndNormalise(ctx, nil, nil); !errors.Is(err, port.ErrInvalidSignature) {
		t.Error("manual sends no callbacks")
	}
	if len(a.Capabilities().MissingRequired()) == 0 {
		t.Error("manual is exempt, not certifiable: it declares no required capability")
	}
}
