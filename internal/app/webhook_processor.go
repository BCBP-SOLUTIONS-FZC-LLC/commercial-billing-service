// Package app holds Billing's use cases and background processors (LLD §3).
// It depends only on domain and port.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// WebhookProcessor drains payment_event_ledger (LLD §8.15): claim due rows
// on the system pool, then for each row resolve the tenant, cross-check the
// metadata (BIL-PAY-3), re-read the provider object (BIL-PAY-4), dispatch to
// the handler for its kind inside a tenant transaction, and record the
// outcome. One tenant's rows are processed in provider order (BIL-PAY-5,
// enforced by the claim).
//
// The domain change commits on the app pool and the ledger outcome on the
// system pool (billing_app has no ledger grant, §4.3), so a crash between the
// two replays the row; handlers are idempotent because they compare with
// Billing's current state and the re-read object.
type WebhookProcessor struct {
	Ledger      port.LedgerRepo
	Providers   port.ProviderLookup
	Tx          port.TxRunner
	Handlers    map[domain.NormalisedKind]port.PaymentEventHandler
	MaxAttempts int           // WEBHOOK_MAX_ATTEMPTS
	BatchSize   int           // rows per claim (20 in the §8.15 query)
	ClaimFor    time.Duration // a claim older than this is reclaimable (60 s)
	Now         func() time.Time
	Log         port.Logger
	Metrics     port.Metrics // commercial_billing_webhooks_total, _processing_seconds
}

// Outcomes recorded in the ledger.
const (
	outcomeUnknownCustomer  = "failed:unknown_customer"
	outcomeMetadataMismatch = "failed:metadata_mismatch"
	outcomeUnhandledType    = "ignored:unhandled_type"
	outcomeNoHandler        = "ignored:no_handler"
	outcomeProviderUnloaded = "failed:provider_not_loaded"
)

// ProcessBatch claims and processes one batch; it returns how many rows it handled.
func (p *WebhookProcessor) ProcessBatch(ctx context.Context) (int, error) {
	rows, err := p.Ledger.Claim(ctx, p.batch(), p.claimFor())
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		p.processOne(ctx, row)
	}
	return len(rows), nil
}

// Run processes until ctx ends, sleeping interval when idle (WEBHOOK_POLL_INTERVAL).
func (p *WebhookProcessor) Run(ctx context.Context, interval time.Duration) {
	for {
		n, err := p.ProcessBatch(ctx)
		if err != nil && p.Log != nil && ctx.Err() == nil {
			p.Log.Warn("webhook claim failed", map[string]any{"error": err.Error()})
		}
		if n > 0 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (p *WebhookProcessor) processOne(ctx context.Context, row domain.LedgerEntry) {
	attrs := []port.Attr{AttrProvider(row.Provider), AttrEventID(row.ProviderEventID)}
	if row.TenantID != nil {
		attrs = append(attrs, AttrTenant(*row.TenantID))
	}
	ctx, end := StartSpan(ctx, "billing.webhook.process "+row.NormalisedKind, attrs...)
	defer end(nil)
	finish := func(status domain.LedgerStatus, outcome string, next *time.Time) {
		if p.Metrics != nil {
			p.Metrics.WebhookProcessed(row.Provider, row.NormalisedKind, string(status), p.now().Sub(row.ReceivedAt))
		}
		if err := p.Ledger.Finish(ctx, row.Provider, row.ProviderEventID, status, outcome, next); err != nil && p.Log != nil {
			p.Log.Error("ledger outcome not recorded; the row will be reclaimed", map[string]any{
				"provider": row.Provider, "provider_event_id": row.ProviderEventID, "error": err.Error()})
		}
	}
	kind := domain.NormalisedKind(row.NormalisedKind)
	if kind == domain.KindIgnored {
		finish(domain.LedgerIgnored, outcomeUnhandledType, nil)
		return
	}
	// BIL-PAY-3: the tenant comes from Billing's customer mapping only.
	if row.TenantID == nil || *row.TenantID == "" {
		p.alert("webhook for an unknown provider customer", row)
		finish(domain.LedgerFailed, outcomeUnknownCustomer, nil) // no retry (§8.15 step 4)
		return
	}
	tenantID := *row.TenantID
	ev := eventFromRow(row)
	if md := ev.Metadata["tenant_id"]; md != "" && md != tenantID {
		p.alert("webhook metadata names another tenant (BIL-PAY-3, SEV-3)", row)
		finish(domain.LedgerFailed, outcomeMetadataMismatch, nil)
		return
	}
	provider, ok := p.Providers.Loaded(row.Provider)
	if !ok {
		finish(domain.LedgerFailed, outcomeProviderUnloaded, nil)
		return
	}
	handler, ok := p.Handlers[kind]
	if !ok {
		finish(domain.LedgerIgnored, outcomeNoHandler, nil)
		return
	}
	// BIL-PAY-4: apply the provider's current object, not the snapshot.
	current, err := provider.FetchCurrent(ctx, ev.Object)
	if err != nil {
		p.retryOrPark(row, err, finish)
		return
	}
	var outcome string
	err = p.Tx.RunInTenantTx(ctx, tenantID, func(ctx context.Context) error {
		var herr error
		outcome, herr = handler.Handle(ctx, tenantID, ev, current)
		return herr
	})
	var ignored *port.IgnoredEvent
	var parked *port.ParkedEvent
	switch {
	case err == nil:
		if outcome == "" {
			outcome = "applied:" + string(kind)
		}
		finish(domain.LedgerProcessed, outcome, nil)
	case errors.As(err, &ignored):
		finish(domain.LedgerIgnored, "ignored:"+ignored.Reason, nil)
	case errors.As(err, &parked):
		// An operator decides (BIL-OPS-6): failed, no retry; the handler
		// raised its own alert.
		finish(domain.LedgerFailed, "failed:"+parked.Reason, nil)
	default:
		p.retryOrPark(row, err, finish)
	}
}

// retryOrPark backs off 1 s, 2 s, 4 s … capped at 5 min; after MaxAttempts
// the row stays failed (parked) with a SEV-3 for the runbook (§20.2).
func (p *WebhookProcessor) retryOrPark(row domain.LedgerEntry, err error, finish func(domain.LedgerStatus, string, *time.Time)) {
	code := "error"
	var de *domain.DomainError
	if errors.As(err, &de) || errors.As(port.ProviderErrorToDomain(err), &de) {
		code = string(de.Code)
	}
	if row.Attempts >= p.maxAttempts() {
		p.alert("webhook processing exhausted its attempts (SEV-3)", row)
		finish(domain.LedgerFailed, "failed:"+code, nil)
		return
	}
	next := p.now().Add(Backoff(row.Attempts))
	finish(domain.LedgerFailed, "failed:"+code, &next)
}

// Backoff returns the wait after the given attempt: 1 s doubling, max 5 min.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second
	for i := 1; i < attempt && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

func (p *WebhookProcessor) alert(msg string, row domain.LedgerEntry) {
	if p.Log != nil {
		p.Log.Error(msg, map[string]any{"provider": row.Provider, "provider_event_id": row.ProviderEventID, "event_type": row.EventType})
	}
}

// eventFromRow rebuilds the normalised event from a ledger row.
func eventFromRow(row domain.LedgerEntry) domain.PaymentEvent {
	kind := domain.NormalisedKind(row.NormalisedKind)
	ev := domain.PaymentEvent{
		Provider: row.Provider, ProviderEventID: row.ProviderEventID, EventType: row.EventType, Kind: kind,
		Livemode: row.Livemode, ProviderCreatedAt: row.ProviderCreatedAt, Payload: row.Payload,
		Object: domain.ProviderObjectRef{Kind: domain.ObjectKindFor(kind)},
	}
	if row.ProviderObjectRef != nil {
		ev.Object.ID = *row.ProviderObjectRef
	}
	var p struct {
		Object struct {
			Customer string            `json:"customer"`
			Metadata map[string]string `json:"metadata"`
		} `json:"object"`
	}
	if json.Unmarshal(row.Payload, &p) == nil {
		ev.CustomerRef, ev.Metadata = p.Object.Customer, p.Object.Metadata
	}
	return ev
}

func (p *WebhookProcessor) batch() int {
	if p.BatchSize > 0 {
		return p.BatchSize
	}
	return 20
}

func (p *WebhookProcessor) claimFor() time.Duration {
	if p.ClaimFor > 0 {
		return p.ClaimFor
	}
	return 60 * time.Second
}

func (p *WebhookProcessor) maxAttempts() int {
	if p.MaxAttempts > 0 {
		return p.MaxAttempts
	}
	return 10
}

func (p *WebhookProcessor) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
