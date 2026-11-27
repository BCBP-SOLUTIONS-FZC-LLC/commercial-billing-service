package port

import "context"

// Observability ports (LLD §11). Every log line, metric and span goes
// through platform-gincommon: the implementations live in
// adapters/observability (tracer, log correlation) and adapters/metrics,
// and obtain their tracer, registry and logger from gincommon. No other
// package imports OpenTelemetry or Prometheus (lint: depguard).

// Attr is one span attribute (§11.3): string values only, never card
// data, a client_secret or contact PII.
type Attr struct{ Key, Value string }

// TraceLink names the span an asynchronous hop came from: the producer's
// trace (envelope trace_id) and the message that carried it.
type TraceLink struct {
	TraceID   string // W3C hex trace id; empty → no link
	MessageID string // event id; identifies the hop
}

// Tracer starts spans. end records err (when non-nil) and ends the span.
type Tracer interface {
	Start(ctx context.Context, name string, attrs ...Attr) (context.Context, func(err error))
	StartLinked(ctx context.Context, name string, link TraceLink, attrs ...Attr) (context.Context, func(err error))
	// LogFields are the §11.4 correlation fields of ctx (trace_id,
	// span_id), empty outside a span.
	LogFields(ctx context.Context) map[string]any
}

// NopTracer records nothing (tests, and before the server installs one).
type NopTracer struct{}

// Start implements Tracer.
func (NopTracer) Start(ctx context.Context, _ string, _ ...Attr) (context.Context, func(error)) {
	return ctx, func(error) {}
}

// StartLinked implements Tracer.
func (NopTracer) StartLinked(ctx context.Context, _ string, _ TraceLink, _ ...Attr) (context.Context, func(error)) {
	return ctx, func(error) {}
}

// LogFields implements Tracer.
func (NopTracer) LogFields(context.Context) map[string]any { return nil }
