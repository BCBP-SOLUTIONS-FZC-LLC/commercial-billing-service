package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// outboxPruneBatch bounds one delete (lock hold time).
const outboxPruneBatch = 5000

// PruneOutbox deletes published outbox_events older than olderThan with
// outbox.Runner.PrunePublished (outbox-prune, §13.2). The runner is never
// started, so its publisher is never called.
func PruneOutbox(ctx context.Context, appPool *pgcommon.Pool, olderThan time.Duration) (int64, error) {
	r, err := outbox.NewRunner(outbox.Config{Pool: appPool, Publisher: refusingPublisher{}})
	if err != nil {
		return 0, err
	}
	var total int64
	for {
		n, err := r.PrunePublished(ctx, olderThan, outboxPruneBatch)
		total += n
		if err != nil || n < outboxPruneBatch {
			return total, err
		}
	}
}

// refusingPublisher satisfies outbox.Config for a prune-only runner.
type refusingPublisher struct{}

var errPruneOnly = errors.New("outbox: prune-only runner does not publish")

func (refusingPublisher) Publish(context.Context, events.Envelope[json.RawMessage]) error {
	return errPruneOnly
}

func (refusingPublisher) PublishBatch(context.Context, []events.Envelope[json.RawMessage]) error {
	return errPruneOnly
}
