package app

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope of Billing's spans (§11.3).
const TracerName = "commercial-billing"

// Span attribute keys (§11.3). Never amounts with card data, never a
// client_secret.
const (
	AttrTenant       = attribute.Key("tenant.id")
	AttrSubscription = attribute.Key("billing.subscription_id")
	AttrProvider     = attribute.Key("billing.provider")
	AttrSagaState    = attribute.Key("billing.saga_state")
	AttrEventID      = attribute.Key("billing.provider_event_id")
)

// StartSpan starts a span named name (billing.<area>.<action>) and returns
// a finisher that records err on the span when non-nil. Usage:
//
//	ctx, end := StartSpan(ctx, "billing.seats.purchase", AttrTenant.String(id))
//	defer func() { end(err) }()
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	return StartLinkedSpan(ctx, name, nil, attrs...)
}

// StartLinkedSpan is StartSpan with links to the spans that caused this
// work asynchronously (a consumed event, a ledger row).
func StartLinkedSpan(ctx context.Context, name string, links []trace.Link, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, span := otel.Tracer(TracerName).Start(ctx, name, trace.WithAttributes(attrs...), trace.WithLinks(links...))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
