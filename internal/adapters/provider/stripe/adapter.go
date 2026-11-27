// Package stripe is the Stripe payment adapter (LLD §3.4, §18.8). It is the
// only package allowed to import github.com/stripe (BIL-PCI-1); no Stripe type
// leaves it. The Stripe API version is the one stripe-go pins, so a pinned
// version change is a new AdapterVersion and needs a new certification
// (BIL-PROV-2).
package stripe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	stripego "github.com/stripe/stripe-go/v87"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// Name is the provider name stored in billing_accounts.provider.
const Name = "stripe"

// adapterSemver is this adapter's own version; bump it with any mapping change.
const adapterSemver = "1.1.0"

// AdapterVersion is "<adapter semver>+stripeapi.<pinned Stripe API version>" (§3.5).
var AdapterVersion = adapterSemver + "+stripeapi." + stripego.APIVersion

// Config configures the adapter (§12).
type Config struct {
	APIKey         string        // STRIPE_API_KEY (restricted key)
	WebhookSecrets []string      // STRIPE_WEBHOOK_SECRETS (1–2, rotation overlap)
	Tolerance      time.Duration // WEBHOOK_TOLERANCE
	// Livemode is what callbacks must report: APP_ENV == prod (§5.4.3).
	Livemode   bool
	Timeout    time.Duration // STRIPE_TIMEOUT_MS
	MaxRetries int64         // STRIPE_MAX_RETRIES (network errors only; requests carry idempotency keys)
	MaxRPS     int           // STRIPE_MAX_RPS, client-side per pod (§21.2)
	// BaseURL overrides the API endpoint (stripe-mock in tests); empty in production.
	BaseURL string
	// HTTPClient is the otelhttp client from adapters/httpx; nil uses a plain client.
	HTTPClient *http.Client
	Now        func() time.Time
	// TestClock attaches every new customer to a Stripe Test Clock, so the
	// certification suite can advance time (§14.7, cmd/certify). Test mode
	// only: New refuses it with Livemode. Never set by the server.
	TestClock string
}

// Adapter implements port.PaymentProvider for Stripe.
type Adapter struct {
	sc      *stripego.Client
	cfg     Config
	limiter *limiter
	now     func() time.Time
}

var _ port.PaymentProvider = (*Adapter)(nil)

// New builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("stripe: API key is required")
	}
	if len(cfg.WebhookSecrets) == 0 {
		return nil, errors.New("stripe: at least one webhook secret is required")
	}
	if cfg.TestClock != "" && cfg.Livemode {
		return nil, errors.New("stripe: a test clock is test mode only")
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = 300 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	hc = &http.Client{Transport: hc.Transport, Timeout: cfg.Timeout, CheckRedirect: hc.CheckRedirect, Jar: hc.Jar}
	bc := &stripego.BackendConfig{HTTPClient: hc, MaxNetworkRetries: stripego.Int64(cfg.MaxRetries)}
	if cfg.BaseURL != "" {
		bc.URL = stripego.String(cfg.BaseURL)
	}
	backends := stripego.NewBackendsWithConfig(bc)
	return &Adapter{
		sc:      stripego.NewClient(cfg.APIKey, stripego.WithBackends(backends)),
		cfg:     cfg,
		limiter: newLimiter(cfg.MaxRPS, cfg.Now),
		now:     cfg.Now,
	}, nil
}

// Name returns "stripe".
func (a *Adapter) Name() string { return Name }

// AdapterVersion returns the certification key (§3.5).
func (a *Adapter) AdapterVersion() string { return AdapterVersion }

// Capabilities: Stripe meets the full capability bar with native proration
// (§3.4 adapter table). The certification suite verifies each (§14.7).
func (a *Adapter) Capabilities() port.ProviderCapabilities {
	return port.ProviderCapabilities{
		RecurringSubscriptions: true, WebhookRedelivery: true, StableEventIDs: true, Proration: port.ProrationNative,
		InvoiceRetrieval: true, HostedPaymentMethodUpdate: true, Refunds: true, RecurringRetries: true,
		OffSessionCharges: true, Disputes: true, PendingUpdates: true, Schedules: true, CollectionPause: true,
		HostedCardEntry: true, Currencies: []string{"USD", "AED"},
	}
}

// mapErr maps a Stripe error to the port's adapter errors (§18.8):
// card_error → ErrDeclined (decline code); authentication_required /
// requires_action → ErrActionRequired; rate limit, connection and 5xx →
// ErrProviderUnavailable; 409 Conflict / idempotency_key_in_use (a request
// with the same key still in flight, retryable per stripe-go) →
// ErrProviderUnavailable, so the caller retries with the same key;
// idempotency_error (same key, different parameters) → ErrIdempotencyMismatch;
// resource_missing → ErrNotFound; other invalid_request_error → an internal
// error carrying only the request log URL, never the request body.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var se *stripego.Error
	if !errors.As(err, &se) {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", port.ErrProviderUnavailable, err)
		}
		return fmt.Errorf("%w: %v", port.ErrProviderUnavailable, err)
	}
	switch {
	case se.Code == stripego.ErrorCodeAuthenticationRequired:
		secret := ""
		if se.PaymentIntent != nil {
			secret = se.PaymentIntent.ClientSecret
		}
		return port.ErrActionRequired{ClientSecret: secret}
	case se.Type == stripego.ErrorTypeCard:
		code := string(se.DeclineCode)
		if code == "" {
			code = string(se.Code)
		}
		return port.ErrDeclined{Code: code}
	case se.HTTPStatusCode == http.StatusConflict || se.Code == stripego.ErrorCodeIdempotencyKeyInUse:
		// Same key still being processed: transient, never "parameters differ".
		return fmt.Errorf("%w: stripe %d %s", port.ErrProviderUnavailable, se.HTTPStatusCode, se.Code)
	case se.Type == stripego.ErrorTypeIdempotency:
		return port.ErrIdempotencyMismatch
	case se.HTTPStatusCode == http.StatusTooManyRequests || se.HTTPStatusCode >= 500 || se.HTTPStatusCode == 0:
		return fmt.Errorf("%w: stripe %d", port.ErrProviderUnavailable, se.HTTPStatusCode)
	case se.Code == stripego.ErrorCodeResourceMissing:
		return port.ErrNotFound
	}
	return fmt.Errorf("stripe %s (%s) request %s", se.Type, se.Code, requestRef(se.RequestLogURL))
}

func requestRef(logURL string) string {
	if i := strings.LastIndex(logURL, "/"); i >= 0 {
		return logURL[i+1:]
	}
	return logURL
}

// limiter is a small token bucket (STRIPE_MAX_RPS per pod, §21.2).
type limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newLimiter(rps int, now func() time.Time) *limiter {
	if rps <= 0 {
		return nil
	}
	return &limiter{rate: float64(rps), tokens: float64(rps), last: now(), now: now}
}

// wait blocks until a token is available or ctx ends.
func (l *limiter) wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	for {
		l.mu.Lock()
		now := l.now()
		l.tokens = min(l.rate, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", port.ErrProviderUnavailable, ctx.Err())
		case <-time.After(need):
		}
	}
}

// call applies the rate limit, then fn, then error mapping.
func call[T any](ctx context.Context, a *Adapter, fn func() (T, error)) (T, error) {
	var zero T
	if err := a.limiter.wait(ctx); err != nil {
		return zero, err
	}
	v, err := fn()
	if err != nil {
		return zero, mapErr(err)
	}
	return v, nil
}

// params returns base params with the idempotency key set.
func idem(key string) stripego.Params {
	p := stripego.Params{}
	if key != "" {
		p.IdempotencyKey = stripego.String(key)
	}
	return p
}
