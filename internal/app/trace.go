package app

import (
	"context"
	"sync/atomic"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// TracerName is the instrumentation scope of Billing's spans (§11.3).
const TracerName = "commercial-billing"

// Span attribute keys (§11.3).
const (
	attrTenant       = "tenant.id"
	attrSubscription = "billing.subscription_id"
	attrProvider     = "billing.provider"
	attrEventID      = "billing.provider_event_id"
	attrSagaID       = "billing.saga_id"
)

// AttrTenant, AttrSubscription, AttrProvider, AttrEventID and AttrSagaID
// build the §11.3 attributes.
func AttrTenant(v string) port.Attr       { return port.Attr{Key: attrTenant, Value: v} }
func AttrSubscription(v string) port.Attr { return port.Attr{Key: attrSubscription, Value: v} }
func AttrProvider(v string) port.Attr     { return port.Attr{Key: attrProvider, Value: v} }
func AttrEventID(v string) port.Attr      { return port.Attr{Key: attrEventID, Value: v} }
func AttrSagaID(v string) port.Attr       { return port.Attr{Key: attrSagaID, Value: v} }

// tracer is the process tracer: gincommon's, installed once at startup by
// the composition root (SetTracer). Until then spans are not recorded.
var tracer atomic.Pointer[port.Tracer]

// SetTracer installs the process tracer (adapters/observability).
func SetTracer(t port.Tracer) { tracer.Store(&t) }

// Tracer returns the process tracer.
func Tracer() port.Tracer {
	if t := tracer.Load(); t != nil {
		return *t
	}
	return port.NopTracer{}
}

// StartSpan starts a span named name (billing.<area>.<action>); the
// returned func records err when non-nil and ends it.
func StartSpan(ctx context.Context, name string, attrs ...port.Attr) (context.Context, func(error)) {
	return Tracer().Start(ctx, name, attrs...)
}

// StartLinkedSpan is StartSpan linked to the producer of an async hop.
func StartLinkedSpan(ctx context.Context, name string, link port.TraceLink, attrs ...port.Attr) (context.Context, func(error)) {
	return Tracer().StartLinked(ctx, name, link, attrs...)
}
