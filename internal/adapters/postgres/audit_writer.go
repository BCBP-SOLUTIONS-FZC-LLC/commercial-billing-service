package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
)

// AuditWriter is port.AuditWriter on platform-audit's outbox Enqueuer.
type AuditWriter struct{ enq *auditoutbox.Enqueuer }

var _ port.AuditWriter = (*AuditWriter)(nil)

// NewAuditWriter wraps the Enqueuer built from AUDIT_* config.
func NewAuditWriter(enq *auditoutbox.Enqueuer) *AuditWriter { return &AuditWriter{enq: enq} }

// Write implements port.AuditWriter.
func (w *AuditWriter) Write(ctx context.Context, e port.AuditEntry) error {
	tx, ok := txFrom(ctx)
	if !ok {
		return domain.NewError(domain.ErrInternal, "audit entry written outside a transaction")
	}
	tenant, err := uuid.Parse(e.TenantID)
	if err != nil {
		return domain.NewError(domain.ErrInternal, "audit entry tenant is not a UUID")
	}
	// "" and commercial-system's own sentinel are the service account, not a
	// user: e.g. a B-O8 reversal completed without an operator (Audit Log
	// §10.3: non-IAM producers use {service_account, …00c1}).
	actor := audit.CommercialSystemActor()
	if e.ActorID != "" && e.ActorID != domain.CommercialSystemPrincipal {
		id, err := uuid.Parse(e.ActorID)
		if err != nil {
			return domain.NewError(domain.ErrInternal, "audit actor is not a UUID")
		}
		actor = audit.UserActor(id)
	}
	entry := audit.Entry{TenantID: tenant, EntryType: e.EntryType, Action: e.Action, Actor: actor,
		OccurredAt: e.OccurredAt, SourceEventType: e.SourceEventType, IPAddress: e.IPAddress, UserAgent: e.UserAgent}
	if e.TargetType != "" {
		entry.Target = &audit.Target{Type: e.TargetType, ID: e.TargetID}
	}
	if len(e.Metadata) > 0 {
		md, err := audit.Metadata(e.Metadata)
		if err != nil {
			return err
		}
		entry.Metadata = md
	}
	return w.enq.Enqueue(ctx, tx, entry, w.enq.NewKeyStrict(e.SourceEventType, e.NaturalID))
}
