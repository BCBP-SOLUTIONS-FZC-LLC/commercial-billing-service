package observability_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/observability"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// spans installs an in-memory provider behind the global gincommon reads.
func spans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

// Spans from the app layer are gincommon spans with the §11.3 attributes;
// errors are recorded; log lines get trace_id and span_id.
func TestTracerThroughGincommon(t *testing.T) {
	exp := spans(t)
	app.SetTracer(observability.NewTracer("commercial-billing"))
	t.Cleanup(func() { app.SetTracer(port.NopTracer{}) })

	ctx, end := app.StartSpan(context.Background(), "billing.seats.purchase", app.AttrTenant("t-1"), app.AttrProvider(""))
	f := app.Tracer().LogFields(ctx)
	end(errors.New("declined"))
	got := exp.GetSpans()
	if len(got) != 1 || got[0].Name != "billing.seats.purchase" {
		t.Fatalf("spans %+v", got)
	}
	s := got[0]
	if s.Status.Code != codes.Error {
		t.Error("error not recorded")
	}
	if len(s.Attributes) != 1 || string(s.Attributes[0].Key) != "tenant.id" || s.Attributes[0].Value.AsString() != "t-1" {
		t.Errorf("attributes %v (empty values must be dropped)", s.Attributes)
	}
	if f["trace_id"] != s.SpanContext.TraceID().String() || f["span_id"] != s.SpanContext.SpanID().String() {
		t.Errorf("log fields %v", f)
	}
	if len(app.Tracer().LogFields(context.Background())) != 0 {
		t.Error("log fields outside a span")
	}
}

// A consumed event's span links to the producer's trace (§11.3).
func TestLinkedSpan(t *testing.T) {
	exp := spans(t)
	tr := observability.NewTracer("commercial-billing")
	_, end := tr.StartLinked(context.Background(), "billing.consume TenantConverted",
		port.TraceLink{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", MessageID: "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"})
	end(nil)
	l := exp.GetSpans()[0].Links
	if len(l) != 1 || l[0].SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("links %+v", l)
	}
	_, end = tr.StartLinked(context.Background(), "billing.consume X", port.TraceLink{TraceID: "not-hex"})
	end(nil)
	if n := len(exp.GetSpans()[1].Links); n != 0 {
		t.Errorf("invalid trace id linked (%d)", n)
	}
}

// Outbound calls: a client span, W3C traceparent injected, the call
// reported to the dependency observer.
func TestOutboundClient(t *testing.T) {
	exp := spans(t)
	var traceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	var dep, op, outcome string
	httpx.SetObserver(func(d, o, out string, _ time.Duration) { dep, op, outcome = d, o, out })
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/subscriptions/sub_1QAbCdEfGh", nil)
	resp, err := httpx.New("stripe", 0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if traceparent == "" {
		t.Error("no traceparent injected")
	}
	if dep != "stripe" || op != "subscriptions.update" || outcome != "error" {
		t.Errorf("observed %s %s %s", dep, op, outcome)
	}
	s := exp.GetSpans()
	if len(s) != 1 || s[0].Name != "stripe subscriptions.update" || s[0].Status.Code != codes.Error {
		t.Fatalf("spans %+v", s)
	}
}
