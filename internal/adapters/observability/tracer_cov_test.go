package observability_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/observability"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// A message id too short to derive a span id still links, with a fixed
// valid span id (a link needs one).
func TestLinkedSpanShortMessageID(t *testing.T) {
	exp := spans(t)
	_, end := observability.NewTracer("commercial-billing").StartLinked(context.Background(), "billing.consume X",
		port.TraceLink{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", MessageID: "m-1"})
	end(nil)
	l := exp.GetSpans()[0].Links
	if len(l) != 1 || !l[0].SpanContext.SpanID().IsValid() || l[0].SpanContext.SpanID().String() != "0000000000000001" {
		t.Fatalf("links %+v", l)
	}
}
