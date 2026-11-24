// Package observability is Billing's one boundary to tracing and log
// correlation (LLD §11.3, §11.4). Every span starts on the tracer
// platform-gincommon provides (gincommon.NewTracer, configured by
// gincommon.InitTracingWithConfig), errors are recorded with
// gincommon.RecordSpanError, and the trace id comes from
// gincommon.SpanTraceID. Span attributes, links and the span id have no
// gincommon helper yet, so this package — and adapters/metrics for the
// registry — is the only place allowed to name OpenTelemetry or
// Prometheus types (depguard in .golangci.yml).
package observability

import (
	"context"
	"encoding/hex"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// Tracer implements port.Tracer on gincommon's tracer, looked up per span
// so it is always the provider gincommon.InitTracingWithConfig installed.
type Tracer struct{ scope string }

var _ port.Tracer = Tracer{}

// NewTracer returns the tracer for scope (the service name).
func NewTracer(scope string) Tracer { return Tracer{scope: scope} }

// Start implements port.Tracer.
func (t Tracer) Start(ctx context.Context, name string, attrs ...port.Attr) (context.Context, func(error)) {
	return t.start(ctx, name, nil, attrs)
}

// StartLinked implements port.Tracer: the link names the producer's trace
// (the envelope carries no span id, so the span id is derived from the
// message id — stable per message, rev 0.38).
func (t Tracer) StartLinked(ctx context.Context, name string, l port.TraceLink, attrs ...port.Attr) (context.Context, func(error)) {
	var links []trace.Link
	if tid, err := trace.TraceIDFromHex(l.TraceID); err == nil && tid.IsValid() {
		var sid trace.SpanID
		if h := strings.ReplaceAll(l.MessageID, "-", ""); len(h) >= 16 {
			if b, err := hex.DecodeString(h[len(h)-16:]); err == nil {
				copy(sid[:], b)
			}
		}
		if !sid.IsValid() {
			sid[7] = 1
		}
		links = append(links, trace.Link{
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}),
			Attributes:  []attribute.KeyValue{attribute.String("messaging.producer.trace_id", l.TraceID)},
		})
	}
	return t.start(ctx, name, links, attrs)
}

func (t Tracer) start(ctx context.Context, name string, links []trace.Link, attrs []port.Attr) (context.Context, func(error)) {
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if a.Value != "" {
			kv = append(kv, attribute.String(a.Key, a.Value))
		}
	}
	ctx, span := gincommon.NewTracer(t.scope).Start(ctx, name, trace.WithAttributes(kv...), trace.WithLinks(links...))
	return ctx, func(err error) {
		if err != nil {
			gincommon.RecordSpanError(ctx, err, err.Error())
		}
		span.End()
	}
}

// LogFields implements port.Tracer (§11.4 trace_id, span_id).
func (Tracer) LogFields(ctx context.Context) map[string]any {
	tid := gincommon.SpanTraceID(ctx)
	if tid == "" {
		return nil
	}
	return map[string]any{"trace_id": tid, "span_id": trace.SpanContextFromContext(ctx).SpanID().String()}
}
