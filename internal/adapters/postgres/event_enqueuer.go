package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
)

// systemIP is the envelope ip_address for work no caller drove (§7.3.1).
const systemIP = "system"

// EventEnqueuer is port.EventEnqueuer on platform-events' ordered outbox.
type EventEnqueuer struct {
	source, specVersion string
	validate            func(eventType string, payload []byte) error
}

var _ port.EventEnqueuer = (*EventEnqueuer)(nil)

// NewEventEnqueuer builds the enqueuer. validate is the produced-schema
// check (eventbus.Schemas.Validate); source and specVersion are the
// envelope constants (§7.3.1).
func NewEventEnqueuer(source, specVersion string, validate func(eventType string, payload []byte) error) *EventEnqueuer {
	return &EventEnqueuer{source: source, specVersion: specVersion, validate: validate}
}

// Enqueue implements port.EventEnqueuer.
func (e *EventEnqueuer) Enqueue(ctx context.Context, p domain.EventPayload, meta port.EventMeta) error {
	st, ok := txStateFrom(ctx)
	if !ok {
		// BIL-EVT-2: an event is only ever written with its state change.
		return domain.NewError(domain.ErrInternal, "event enqueued outside a transaction")
	}
	if err := p.Validate(); err != nil {
		return domain.NewError(domain.ErrInternal, err.Error())
	}
	typ := string(p.EventType())
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", typ, err)
	}
	if err := e.validate(typ, data); err != nil {
		return domain.NewError(domain.ErrInternal, err.Error())
	}
	at, err := nextEventTime(ctx, st)
	if err != nil {
		return mapDBError(err)
	}

	actor, ip, ua := meta.Actor, meta.IPAddress, meta.UserAgent
	if actor == "" {
		actor = port.SystemActor
	}
	if ip == "" {
		ip = systemIP
	}
	opts := []events.EnvelopeOpt{
		events.WithTenantID(p.Tenant()), events.WithSubject(p.Tenant()), events.WithSchemaVersion(e.specVersion),
		events.WithActor(actor), events.WithIPAddress(ip),
	}
	if ua != "" {
		opts = append(opts, events.WithUserAgent(ua))
	}
	if meta.CorrelationID != "" {
		opts = append(opts, events.WithCorrelationID(meta.CorrelationID))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		opts = append(opts, events.WithTraceID(sc.TraceID().String()))
	}
	env := events.NewEnvelope(typ, e.source, json.RawMessage(data), opts...)
	env.Timestamp = at
	// BIL-EVT-5: ordered per tenant. Callers hold the tenant's subscription
	// row lock (SubscriptionRepo.Transition / ForUpdate) before enqueuing,
	// so two transactions for one tenant cannot interleave their inserts.
	return mapDBError(outbox.EnqueueOrdered(ctx, st.tx, env, p.Tenant()))
}

// nextEventTime is BIL-EVT-1: Billing's clock read in this transaction,
// at millisecond precision, strictly after the previous event of the
// same transaction (+1 ms), because Core skips an equal time.
func nextEventTime(ctx context.Context, st *txCtx) (time.Time, error) {
	var now time.Time
	if err := st.tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	at := now.UTC().Truncate(time.Millisecond)
	if !st.lastEventTime.IsZero() && !at.After(st.lastEventTime) {
		at = st.lastEventTime.Add(time.Millisecond)
	}
	st.lastEventTime = at
	return at, nil
}
