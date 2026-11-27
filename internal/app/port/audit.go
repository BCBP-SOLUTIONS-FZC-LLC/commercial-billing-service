package port

import (
	"context"
	"time"
)

// AuditEntry is one AL-5 direct-write entry (LLD §10.6): operator actions
// and the cross-tenant job entry only; everything else reaches Audit on the
// bus.
type AuditEntry struct {
	TenantID        string // the entry's tenant, or the platform tenant
	EntryType       string // e.g. billing.refund.issued
	Action          string // e.g. refund.issued
	ActorID         string // operator sub; "" = commercial-system
	TargetType      string // subscription | invoice | saga | price_book | certification
	TargetID        string
	SourceEventType string // e.g. RefundIssued (also the key's first part)
	NaturalID       string // makes the key unique: the refund id, adjustment id, …
	OccurredAt      time.Time
	IPAddress       string
	UserAgent       string
	Metadata        map[string]any // bounded: amounts, category, reason
}

// AuditWriter enqueues an entry in the transaction in ctx (outbox mode),
// so the entry commits with the change it records. A duplicate key is a
// no-op. It never blocks on Audit Log (AUDIT-CALLER-1).
type AuditWriter interface {
	Write(ctx context.Context, e AuditEntry) error
}
