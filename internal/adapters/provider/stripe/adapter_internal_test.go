package stripe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	stripego "github.com/stripe/stripe-go/v87"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func TestNewRejectsBadConfig(t *testing.T) {
	cases := map[string]Config{
		"no API key":             {WebhookSecrets: []string{"s"}},
		"no webhook secret":      {APIKey: "rk_test_x"},
		"test clock in livemode": {APIKey: "rk_test_x", WebhookSecrets: []string{"s"}, Livemode: true, TestClock: "clock_1"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New must fail", name)
		}
	}
	a, err := New(Config{APIKey: "rk_test_x", WebhookSecrets: []string{"s"}})
	if err != nil || a.cfg.Tolerance != 300*time.Second || a.cfg.Timeout != 8*time.Second || a.limiter != nil || a.Name() != Name ||
		!strings.HasPrefix(a.AdapterVersion(), adapterSemver+"+stripeapi.") || len(a.Capabilities().MissingRequired()) != 0 {
		t.Fatalf("defaults: %+v %v", a, err)
	}
}

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "dial tcp: i/o timeout" }
func (fakeNetErr) Timeout() bool   { return true }
func (fakeNetErr) Temporary() bool { return true }

var _ net.Error = fakeNetErr{}

// §18.8 error mapping: transient → ErrProviderUnavailable, never terminal.
func TestMapErr(t *testing.T) {
	if mapErr(nil) != nil {
		t.Fatal("nil stays nil")
	}
	for name, err := range map[string]error{
		"net error":         fmt.Errorf("post: %w", fakeNetErr{}),
		"deadline":          fmt.Errorf("post: %w", context.DeadlineExceeded),
		"other client side": errors.New("unexpected EOF"),
		"429":               &stripego.Error{HTTPStatusCode: 429, Type: stripego.ErrorTypeInvalidRequest, Code: stripego.ErrorCodeRateLimit},
		"500":               &stripego.Error{HTTPStatusCode: 500, Type: stripego.ErrorTypeAPI},
		"503":               &stripego.Error{HTTPStatusCode: 503, Type: stripego.ErrorTypeAPI},
		"no status":         &stripego.Error{Type: stripego.ErrorTypeAPI},
	} {
		if got := mapErr(err); !errors.Is(got, port.ErrProviderUnavailable) {
			t.Errorf("%s: %v, want ErrProviderUnavailable", name, got)
		}
	}
	if got := mapErr(&stripego.Error{HTTPStatusCode: 402, Type: stripego.ErrorTypeCard, Code: stripego.ErrorCodeAuthenticationRequired}); !errors.As(got, new(port.ErrActionRequired)) {
		t.Errorf("authentication_required without a PaymentIntent: %v", got)
	}
	var d port.ErrDeclined
	if got := mapErr(&stripego.Error{HTTPStatusCode: 402, Type: stripego.ErrorTypeCard, Code: stripego.ErrorCodeCardDeclined, DeclineCode: "do_not_honor"}); !errors.As(got, &d) || d.Code != "do_not_honor" {
		t.Errorf("decline code wins over the error code: %v", got)
	}
	if got := mapErr(&stripego.Error{HTTPStatusCode: 400, Type: stripego.ErrorTypeIdempotency}); !errors.Is(got, port.ErrIdempotencyMismatch) {
		t.Errorf("idempotency_error: %v", got)
	}
	// A request whose key is still being processed (409, retryable per
	// stripe-go) is transient, never "parameters differ": the caller must
	// retry with the same key (bug fixed 2026-11-27).
	for name, se := range map[string]*stripego.Error{
		"409 idempotency_error":      {HTTPStatusCode: 409, Type: stripego.ErrorTypeIdempotency},
		"409 invalid_request":        {HTTPStatusCode: 409, Type: stripego.ErrorTypeInvalidRequest},
		"idempotency_key_in_use 400": {HTTPStatusCode: 400, Type: stripego.ErrorTypeIdempotency, Code: stripego.ErrorCodeIdempotencyKeyInUse},
	} {
		got := mapErr(se)
		if !errors.Is(got, port.ErrProviderUnavailable) || errors.Is(got, port.ErrIdempotencyMismatch) {
			t.Errorf("%s: %v, want ErrProviderUnavailable", name, got)
		}
	}
	if got := mapErr(&stripego.Error{HTTPStatusCode: 404, Type: stripego.ErrorTypeInvalidRequest, Code: stripego.ErrorCodeResourceMissing}); !errors.Is(got, port.ErrNotFound) {
		t.Errorf("resource_missing: %v", got)
	}
	// Other invalid requests: an internal error naming only the request id,
	// never the message or parameters.
	got := mapErr(&stripego.Error{HTTPStatusCode: 400, Type: stripego.ErrorTypeInvalidRequest, Code: stripego.ErrorCodeParameterInvalidInteger,
		Msg: "Invalid integer: billing@example.com", Param: "quantity", RequestLogURL: "https://dashboard.stripe.com/test/logs/req_abc"})
	if got == nil || errors.Is(got, port.ErrProviderUnavailable) || !strings.Contains(got.Error(), "req_abc") ||
		strings.Contains(got.Error(), "example.com") || strings.Contains(got.Error(), "quantity") {
		t.Errorf("invalid request: %v", got)
	}
	if requestRef("req_plain") != "req_plain" {
		t.Error("a log URL without a slash is returned as is")
	}
}

// A rate-limited call whose context ends gives up as ErrProviderUnavailable
// without reaching Stripe (§21.2).
func TestLimiterContextEnds(t *testing.T) {
	_, a := newFakeStripe(t) // any request would fail the test: no routes
	a.limiter = &limiter{rate: 1, tokens: 0, last: fixedNow, now: func() time.Time { return fixedNow }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func() error{
		"call":                  func() error { _, err := a.EnsureCustomer(ctx, port.CustomerInput{}, "k"); return err },
		"ListPaymentMethods":    func() error { _, err := a.ListPaymentMethods(ctx, cus); return err },
		"ConfirmInitialPayment": func() error { _, err := a.ConfirmInitialPayment(ctx, subRef, "k"); return err },
		"ScheduleAtPeriodEnd":   func() error { _, err := a.ScheduleAtPeriodEnd(ctx, port.PeriodEndChange{Sub: subRef}, "k"); return err },
		"Refund":                func() error { _, err := a.Refund(ctx, port.RefundInput{Invoice: invRef}, "k"); return err },
		"FetchCurrent": func() error {
			_, err := a.FetchCurrent(ctx, domain.ProviderObjectRef{Kind: domain.ObjectInvoice, ID: "in_1"})
			return err
		},
		"ListSince": func() error { _, err := a.ListSince(ctx, port.ReconInvoices, fixedNow, ""); return err },
	}
	for name, fn := range calls {
		if err := fn(); !errors.Is(err, port.ErrProviderUnavailable) || !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A correctly signed body that is not a Stripe event, or one without an id
// or type, is refused as malformed (BIL-PAY-2).
func TestMalformedSignedEventRefused(t *testing.T) {
	a := newTestAdapter(t, false)
	for _, body := range [][]byte{[]byte("not json"), []byte(`{"type":"invoice.paid","livemode":false}`), []byte(`{"id":"evt_1","livemode":false}`)} {
		if _, err := a.VerifyAndNormalise(context.Background(), header(sign("whsec_current", fixedNow, body)), body); !errors.Is(err, port.ErrInvalidSignature) {
			t.Errorf("%s: got %v", body, err)
		}
	}
}

// A signed event whose data.object is not an object is refused (BIL-PAY-2).
func TestMalformedEventObjectRefused(t *testing.T) {
	a := newTestAdapter(t, false)
	body := []byte(fmt.Sprintf(`{"id":"evt_1","type":"invoice.paid","created":%d,"livemode":false,"data":{"object":"in_1"}}`, fixedNow.Unix()))
	if _, err := a.VerifyAndNormalise(context.Background(), header(sign("whsec_current", fixedNow, body)), body); !errors.Is(err, port.ErrInvalidSignature) {
		t.Fatalf("got %v", err)
	}
}
