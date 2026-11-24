package app

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// emissionUserAgent is the envelope user_agent of delayed events (§7.3.1).
const emissionUserAgent = "commercial-billing/worker:emissions"

// EmissionWorker moves due scheduled_emissions into the outbox (BIL-EVT-6).
// Each payload is built when it is emitted, from the current subscription
// (a snapshot, BIL-EVT-4), and stamped then (BIL-EVT-1).
type EmissionWorker struct {
	Emissions port.EmissionRepo
	Subs      port.SubscriptionRepo
	Tx        port.TxRunner
	Lock      port.TenantLocker
	Events    port.EventEnqueuer
	BatchSize int // tenants per pass
	Now       func() time.Time
	Log       port.Logger
	Metrics   port.Metrics
}

// RunOnce emits what is due; it returns the number of tenants handled.
func (w *EmissionWorker) RunOnce(ctx context.Context) (int, error) {
	now := w.now()
	tenants, err := w.Emissions.DueTenants(ctx, now, w.batch())
	if err != nil {
		return 0, err
	}
	for _, t := range tenants {
		err := w.Tx.RunInTenantTx(ctx, t, func(ctx context.Context) error {
			if err := w.Lock.LockTenant(ctx, t); err != nil {
				return err
			}
			due, err := w.Emissions.ClaimDue(ctx, t, now)
			if err != nil {
				return err
			}
			for _, e := range due {
				if err := w.emit(ctx, e); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil && w.Log != nil {
			w.Log.Warn("scheduled emission failed; retried on the next pass", map[string]any{"tenant_id": t, "error": err.Error()})
		}
	}
	return len(tenants), nil
}

func (w *EmissionWorker) emit(ctx context.Context, e domain.ScheduledEmission) (err error) {
	ctx, end := StartSpan(ctx, "billing.emission "+e.PayloadBuilder, AttrTenant.String(e.TenantID), AttrSubscription.String(e.RefID))
	defer func() { end(err) }()
	reason, ok := domain.SeatsReasonFor(e.PayloadBuilder)
	if !ok || e.EventType != domain.EventTenantSeatsChanged {
		w.cancelled(e, "unknown_builder")
		return w.Emissions.Finish(ctx, e.EmissionID, nil)
	}
	sub, err := w.Subs.Get(ctx, e.RefID)
	if domain.HasCode(err, domain.ErrSubscriptionNotFound) {
		w.cancelled(e, "subscription_not_found")
		return w.Emissions.Finish(ctx, e.EmissionID, nil)
	}
	if err != nil {
		return err
	}
	if !domain.SeatsProjectable(sub.Status) {
		w.cancelled(e, "subscription_"+string(sub.Status))
		return w.Emissions.Finish(ctx, e.EmissionID, nil)
	}
	item, err := w.Subs.Item(ctx, e.RefID)
	if domain.HasCode(err, domain.ErrSubscriptionNotFound) {
		w.cancelled(e, "seat_item_not_found")
		return w.Emissions.Finish(ctx, e.EmissionID, nil)
	}
	if err != nil {
		return err
	}
	id, err := w.Events.Enqueue(ctx, domain.TenantSeatsChanged{
		TenantID: e.TenantID, LicensedSeats: item.Quantity, Reason: reason, BillingSubscriptionID: sub.BillingSubscriptionID,
	}, port.EventMeta{UserAgent: emissionUserAgent})
	if err != nil {
		return err
	}
	if w.Metrics != nil && e.PayloadBuilder == domain.BuilderSeatsAfterConversion && sub.PaymentConfirmedAt != nil {
		// Includes the deliberate SEATS_AFTER_CONVERSION_DELAY (BIL-EVT-6).
		w.Metrics.PurchaseToEvent(port.PurchaseKindConversionSeats, w.now().Sub(*sub.PaymentConfirmedAt))
	}
	return w.Emissions.Finish(ctx, e.EmissionID, &id)
}

func (w *EmissionWorker) cancelled(e domain.ScheduledEmission, why string) {
	if w.Log != nil {
		w.Log.Info("scheduled emission cancelled", map[string]any{
			"tenant_id": e.TenantID, "emission_id": e.EmissionID, "builder": e.PayloadBuilder, "reason": why})
	}
}

// Run polls every interval until ctx ends; a full batch polls again at once.
func (w *EmissionWorker) Run(ctx context.Context, interval time.Duration) {
	for {
		n, err := w.RunOnce(ctx)
		if err != nil && w.Log != nil && ctx.Err() == nil {
			w.Log.Warn("scheduled emission scan failed", map[string]any{"error": err.Error()})
		}
		if err == nil && n >= w.batch() {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (w *EmissionWorker) batch() int {
	if w.BatchSize > 0 {
		return w.BatchSize
	}
	return 50
}

func (w *EmissionWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
