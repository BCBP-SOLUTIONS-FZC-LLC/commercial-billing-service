package port

import (
	"errors"
	"fmt"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func TestProviderErrorToDomain(t *testing.T) {
	cases := []struct {
		in   error
		want domain.ErrorCode
	}{
		{ErrDeclined{Code: "insufficient_funds"}, domain.ErrPaymentDeclined},
		{fmt.Errorf("wrapped: %w", ErrActionRequired{ClientSecret: "pi_secret"}), domain.ErrPaymentActionRequired},
		{fmt.Errorf("wrapped: %w", ErrProviderUnavailable), domain.ErrProviderUnavailable},
		{ErrUnsupported, domain.ErrProviderOperationUnsupported},
		{ErrInvalidSignature, domain.ErrInvalidSignature},
		{ErrIdempotencyMismatch, domain.ErrInternal},
		{errors.New("unknown"), domain.ErrInternal},
	}
	for _, c := range cases {
		var de *domain.DomainError
		if !errors.As(ProviderErrorToDomain(c.in), &de) || de.Code != c.want {
			t.Errorf("%v → %v, want %s", c.in, ProviderErrorToDomain(c.in), c.want)
		}
	}
	var de *domain.DomainError
	errors.As(ProviderErrorToDomain(ErrDeclined{Code: "card_declined"}), &de)
	if de.Extensions["decline_reason"] != "card_declined" {
		t.Error("decline_reason extension (§17)")
	}
	if ProviderErrorToDomain(nil) != nil {
		t.Error("nil stays nil")
	}
}

func TestMissingRequiredCapabilities(t *testing.T) {
	full := ProviderCapabilities{RecurringSubscriptions: true, WebhookRedelivery: true, StableEventIDs: true,
		Proration: ProrationNative, InvoiceRetrieval: true, HostedPaymentMethodUpdate: true, Refunds: true,
		RecurringRetries: true, OffSessionCharges: true, HostedCardEntry: true}
	if m := full.MissingRequired(); len(m) != 0 {
		t.Fatalf("full bar reported missing %v", m)
	}
	// Disputes, PendingUpdates, Schedules and CollectionPause are not required.
	noProration := full
	noProration.Proration = ""
	if m := noProration.MissingRequired(); len(m) != 1 || m[0] != "Proration" {
		t.Fatalf("missing proration: %v", m)
	}
	fallback := full
	fallback.Proration = ProrationFallback
	if len(fallback.MissingRequired()) != 0 {
		t.Fatal("a declared fallback satisfies Proration")
	}
}
