package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

var fixedNow = time.Date(2026, 11, 20, 9, 14, 3, 0, time.UTC)

func newTestAdapter(t *testing.T, livemode bool, secrets ...string) *Adapter {
	t.Helper()
	if len(secrets) == 0 {
		secrets = []string{"whsec_current"}
	}
	a, err := New(Config{APIKey: "rk_test_x", WebhookSecrets: secrets, Tolerance: 300 * time.Second, Livemode: livemode,
		Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sign(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func header(sig string) http.Header {
	h := http.Header{}
	h.Set(SignatureHeader, sig)
	return h
}

func event(t *testing.T, typ string, livemode bool, object map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": "evt_1", "type": typ, "created": fixedNow.Unix(), "livemode": livemode,
		"data": map[string]any{"object": object},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBIL_PAY_2_SignatureChecks(t *testing.T) {
	a := newTestAdapter(t, false, "whsec_new", "whsec_old")
	body := event(t, "invoice.paid", false, map[string]any{"id": "in_1", "object": "invoice", "customer": "cus_1"})
	ctx := context.Background()
	cases := map[string]struct {
		h  http.Header
		ok bool
	}{
		"current secret":             {header(sign("whsec_new", fixedNow, body)), true},
		"rotated secret still valid": {header(sign("whsec_old", fixedNow, body)), true},
		"unknown secret":             {header(sign("whsec_other", fixedNow, body)), false},
		"stale timestamp":            {header(sign("whsec_new", fixedNow.Add(-301*time.Second), body)), false},
		"future timestamp":           {header(sign("whsec_new", fixedNow.Add(301*time.Second), body)), false},
		"missing header":             {http.Header{}, false},
		"tampered body":              {header(sign("whsec_new", fixedNow, append([]byte(" "), body...))), false},
	}
	for name, c := range cases {
		_, err := a.VerifyAndNormalise(ctx, c.h, body)
		if c.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.ok && !errors.Is(err, port.ErrInvalidSignature) {
			t.Errorf("%s: got %v, want ErrInvalidSignature", name, err)
		}
	}
}

func TestBIL_PAY_2_LivemodeMustMatch(t *testing.T) {
	prod := newTestAdapter(t, true)
	body := event(t, "invoice.paid", false, map[string]any{"id": "in_1", "object": "invoice"})
	if _, err := prod.VerifyAndNormalise(context.Background(), header(sign("whsec_current", fixedNow, body)), body); !errors.Is(err, port.ErrInvalidSignature) {
		t.Fatalf("a test-mode event in prod must be refused: %v", err)
	}
}

// §7.4 mapping.
func TestNormalisedKinds(t *testing.T) {
	cases := []struct {
		typ, mode string
		want      domain.NormalisedKind
	}{
		{"checkout.session.completed", "subscription", domain.KindCheckoutCompleted},
		{"checkout.session.completed", "payment", domain.KindCheckoutCompleted},
		{"checkout.session.completed", "setup", domain.KindPaymentMethodSet},
		{"checkout.session.expired", "subscription", domain.KindCheckoutExpired},
		{"invoice.created", "", domain.KindInvoiceUpdated},
		{"invoice.finalized", "", domain.KindInvoiceUpdated},
		{"invoice.voided", "", domain.KindInvoiceUpdated},
		{"invoice.marked_uncollectible", "", domain.KindInvoiceUpdated},
		{"invoice.paid", "", domain.KindInvoicePaid},
		{"invoice.payment_failed", "", domain.KindInvoicePaymentFailed},
		{"invoice.payment_action_required", "", domain.KindPaymentActionRequired},
		{"customer.subscription.updated", "", domain.KindSubscriptionUpdated},
		{"customer.subscription.deleted", "", domain.KindSubscriptionEnded},
		{"subscription_schedule.released", "", domain.KindScheduleUpdated},
		{"subscription_schedule.completed", "", domain.KindScheduleUpdated},
		{"charge.refunded", "", domain.KindRefundUpdated},
		{"refund.failed", "", domain.KindRefundUpdated},
		{"charge.dispute.created", "", domain.KindDisputeOpened},
		{"charge.dispute.updated", "", domain.KindDisputeUpdated},
		{"charge.dispute.closed", "", domain.KindDisputeClosed},
		{"radar.early_fraud_warning.created", "", domain.KindFraudWarning},
		{"customer.updated", "", domain.KindCustomerUpdated},
		{"payment_method.attached", "", domain.KindCustomerUpdated},
		{"product.created", "", domain.KindIgnored},
	}
	for _, c := range cases {
		if got, _ := normaliseKind(c.typ, c.mode); got != c.want {
			t.Errorf("%s (%s) → %s, want %s", c.typ, c.mode, got, c.want)
		}
	}
}

var panPattern = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)

// BIL-PCI-3: the stored payload is allow-listed; card data, billing details
// and emails never reach the ledger; expanded objects shrink to their id.
func TestBIL_PCI_3_PayloadAllowList(t *testing.T) {
	a := newTestAdapter(t, false)
	obj := map[string]any{
		"id": "in_1", "object": "invoice", "status": "paid", "amount_paid": 6900, "currency": "usd",
		"customer":               map[string]any{"id": "cus_1", "email": "owner@example.com", "name": "Ada"},
		"customer_email":         "owner@example.com",
		"metadata":               map[string]any{"tenant_id": "t1", "billing_subscription_id": "s1"},
		"payment_method_details": map[string]any{"card": map[string]any{"number": "4242 4242 4242 4242", "last4": "4242"}},
		"billing_details":        map[string]any{"address": map[string]any{"line1": "1 Street"}},
	}
	body := event(t, "invoice.paid", false, obj)
	ev, err := a.VerifyAndNormalise(context.Background(), header(sign("whsec_current", fixedNow, body)), body)
	if err != nil {
		t.Fatal(err)
	}
	p := string(ev.Payload)
	for _, leak := range []string{"owner@example.com", "Ada", "4242", "1 Street", "payment_method_details", "billing_details"} {
		if regexp.MustCompile(regexp.QuoteMeta(leak)).MatchString(p) {
			t.Errorf("payload leaks %q: %s", leak, p)
		}
	}
	if panPattern.MatchString(p) {
		t.Errorf("payload contains a PAN-like number: %s", p)
	}
	if ev.CustomerRef != "cus_1" || ev.Kind != domain.KindInvoicePaid || ev.Object.ID != "in_1" ||
		ev.Metadata["tenant_id"] != "t1" || !ev.ProviderCreatedAt.Equal(fixedNow) || ev.ProviderEventID != "evt_1" {
		t.Errorf("normalised event %+v", ev)
	}
}

func TestCustomerEventResolvesItsOwnId(t *testing.T) {
	a := newTestAdapter(t, false)
	body := event(t, "customer.updated", false, map[string]any{"id": "cus_9", "object": "customer", "email": "x@example.com"})
	ev, err := a.VerifyAndNormalise(context.Background(), header(sign("whsec_current", fixedNow, body)), body)
	if err != nil || ev.CustomerRef != "cus_9" {
		t.Fatalf("customer event: %+v %v", ev, err)
	}
}
