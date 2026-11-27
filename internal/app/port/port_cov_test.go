package port

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func TestProviderErrorToDomainPriceAndNotFound(t *testing.T) {
	for in, want := range map[error]string{
		fmt.Errorf("price: %w", ErrPriceMismatch): "price: " + ErrPriceMismatch.Error(),
		fmt.Errorf("gone: %w", ErrNotFound):       "the provider object does not exist",
	} {
		var de *domain.DomainError
		if !errors.As(ProviderErrorToDomain(in), &de) || de.Code != domain.ErrProviderPriceMismatch || de.Message != want {
			t.Errorf("%v → %+v", in, de)
		}
	}
}

func TestErrorStringsAndCurrency(t *testing.T) {
	if got := (&IgnoredEvent{Reason: "stale"}).Error(); got != "ignored: stale" {
		t.Errorf("IgnoredEvent = %q", got)
	}
	if got := (ErrDeclined{Code: "card_declined"}).Error(); got != "payment declined: card_declined" {
		t.Errorf("ErrDeclined = %q", got)
	}
	c := ProviderCapabilities{Currencies: []string{"USD", "EUR"}}
	if !c.SupportsCurrency("EUR") || c.SupportsCurrency("GBP") {
		t.Error("SupportsCurrency wrong")
	}
}

// NopMetrics accepts every call without effect (a Service built without metrics).
func TestNopMetricsAcceptsEveryCall(t *testing.T) {
	var m Metrics = NopMetrics{}
	m.PurchaseToEvent("seats", time.Second)
	m.SagaConfirmed("ok", time.Second)
	m.RPConvertAttempt("ok")
	m.UnmatchedConversion()
	m.DowngradeBlocked("seats")
	m.RepricingScheduled("pro")
	m.SeatChange("purchase", "ok")
	m.SeatProjectionMismatch()
	m.ProrationDeferred("seats", "USD", 1)
	m.AddonPurchase("tokens_1m", "ok")
	m.PaymentFailure("card_declined")
	m.DunningTransition("past_due")
	m.InvoiceDisposition("paid")
	m.WebhookProcessed("stripe", "invoice_paid", "ok", time.Second)
	m.SagasOverdue(0)
	m.ReconciliationFindings(nil)
	m.CrossTenantJobRun("recon", "ok")
}
