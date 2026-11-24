package metrics

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// CountingEnqueuer counts commercial_events_emitted_total{event_type} for
// each event enqueued in the outbox. The count is taken at enqueue, inside
// the business transaction, so a rolled-back transaction is counted too;
// platform_outbox_* covers what was actually published.
type CountingEnqueuer struct {
	Next    port.EventEnqueuer
	Metrics *Metrics
}

var _ port.EventEnqueuer = CountingEnqueuer{}

// Enqueue implements port.EventEnqueuer.
func (c CountingEnqueuer) Enqueue(ctx context.Context, ev domain.EventPayload, m port.EventMeta) (string, error) {
	id, err := c.Next.Enqueue(ctx, ev, m)
	if err == nil {
		c.Metrics.EventEmitted(string(ev.EventType()))
	}
	return id, err
}
