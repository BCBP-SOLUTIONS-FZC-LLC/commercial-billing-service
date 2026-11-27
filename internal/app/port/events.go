package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// SystemActor is the envelope actor for changes no person made: webhook,
// job and consumer-driven work (LLD §7.3.1). It is commercial-system's
// sentinel UUID (CD-19), the one spelling on every Billing event, so Audit
// Log records it as {service_account, …00c1}.
const SystemActor = domain.CommercialSystemPrincipal

// EventMeta carries the envelope fields that describe who caused a change
// (§7.3.1). Empty Actor / IPAddress default to the system values; UserAgent
// for system work is commercial-billing/<webhook|job:name|consumer:queue>.
type EventMeta struct {
	Actor         string
	IPAddress     string
	UserAgent     string
	CorrelationID string
}

// EventEnqueuer writes a billing.events event into the ordered outbox in the
// transaction already in ctx (BIL-EVT-2): it never opens its own, so the
// state change and its event commit together. Events are keyed by tenant
// (BIL-EVT-5) and stamped with Billing's clock in that transaction, strictly
// increasing within it (BIL-EVT-1). A payload failing its schema is
// rejected and the transaction must roll back. It returns the envelope id.
type EventEnqueuer interface {
	Enqueue(ctx context.Context, payload domain.EventPayload, meta EventMeta) (eventID string, err error)
}
