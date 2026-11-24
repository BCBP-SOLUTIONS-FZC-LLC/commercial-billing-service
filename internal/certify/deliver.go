package certify

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// Deliverer plays the provider's webhook delivery into Billing's own B-W1
// handler and processor (§8.15). The suite runs without a public endpoint,
// so it reads each event from the provider's event list in its raw form,
// signs it with the run's webhook secret as the provider does, and POSTs
// it to B-W1 in process; the processor then applies it with FetchCurrent
// (BIL-PAY-4). The bodies are the provider's; only the transport is local.
type Deliverer struct {
	API       StripeAPI
	Secret    string
	Handler   http.Handler // the B-W1 route
	Path      string       // /api/v1/webhooks/stripe
	Processor *app.WebhookProcessor
	Provider  port.PaymentProvider
	Now       func() time.Time
	Since     time.Time

	delivered map[string]bool
}

// Post sends body to B-W1 signed at ts (sig overrides the signature when
// set) and returns the status code.
func (d *Deliverer) Post(ctx context.Context, body []byte, ts time.Time, sig string) int {
	if sig == "" {
		sig = Sign(d.Secret, ts, body)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, d.Path, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.Handler.ServeHTTP(rec, req)
	return rec.Code
}

// DeliverNew delivers every provider event since the run started that was
// not delivered yet, then drains the processor. It returns the event ids.
func (d *Deliverer) DeliverNew(ctx context.Context) ([]string, error) {
	if d.delivered == nil {
		d.delivered = map[string]bool{}
	}
	var ids []string
	cursor := ""
	for {
		page, err := d.Provider.ListSince(ctx, port.ReconEvents, d.Since, cursor)
		if err != nil {
			return ids, err
		}
		for _, it := range page.Items {
			if d.delivered[it.Ref.ID] {
				continue
			}
			raw, err := d.API.RawEvent(ctx, it.Ref.ID)
			if err != nil {
				return ids, err
			}
			if code := d.Post(ctx, raw, d.Now(), ""); code != http.StatusOK {
				return ids, fmt.Errorf("B-W1 answered %d for %s", code, it.Ref.ID)
			}
			d.delivered[it.Ref.ID] = true
			ids = append(ids, it.Ref.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return ids, d.Drain(ctx)
}

// Drain processes ledger rows until none is claimable.
func (d *Deliverer) Drain(ctx context.Context) error {
	for range 50 {
		n, err := d.Processor.ProcessBatch(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}
