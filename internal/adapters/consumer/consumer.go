// Package consumer is Billing's inbound bus adapter (LLD §7.1): the
// tenant-billing-q and usage-billing-q SQS consumers. Each message is
// Glue-decoded by platform-events, checked against Billing's embedded
// consumed schema, decoded into the app's event type and handed to the use
// case. Decode failures, schema violations and future-time poison pills go
// straight to the queue's DLQ; other failures are retried by SQS and
// dead-lettered by platform-events at receive 4.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// DLQ reasons (§7.1).
const (
	ReasonFutureTime      = "future_time_clamp"
	ReasonSchemaViolation = "schema_violation"
)

// errSchemaViolation marks a payload that fails its consumed schema or
// cannot be decoded: retrying cannot help.
var errSchemaViolation = errors.New(ReasonSchemaViolation)

// Validator checks a consumed payload against its embedded schema
// (eventbus.Schemas.ValidateConsumed).
type Validator func(eventType string, payload []byte) error

// TenantBilling is the tenant-billing-q handler.
func TenantBilling(uc *app.LifecycleConsumer, validate Validator, log port.Logger) events.Handler {
	return traced(tenantBilling(uc, validate, log))
}

func tenantBilling(uc *app.LifecycleConsumer, validate Validator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		switch env.Type {
		case app.EventTenantConverted, app.EventTenantSuspended, app.EventTenantReactivated, app.EventTenantOffboarded:
		default:
			// The SNS filter should never deliver it; acknowledge so an
			// added upstream type cannot block the queue.
			ignored(log, "tenant-billing-q", env)
			return nil
		}
		if err := validate(env.Type, env.Payload); err != nil {
			return fmt.Errorf("%w: %w", errSchemaViolation, err)
		}
		var d struct {
			TenantID              string     `json:"tenant_id"`
			Source                string     `json:"source"`
			OffboardedAt          *time.Time `json:"offboarded_at"`
			BillingSubscriptionID string     `json:"billing_subscription_id"`
			Plan                  string     `json:"plan"`
			DirectSignup          bool       `json:"direct_signup"`
		}
		if err := json.Unmarshal(env.Payload, &d); err != nil {
			return fmt.Errorf("%w: %w", errSchemaViolation, err)
		}
		if err := sameTenant(env, d.TenantID); err != nil {
			return err
		}
		ev := app.TenantLifecycleEvent{
			EventID: env.ID, Type: env.Type, Time: env.Timestamp, TenantID: d.TenantID, Source: d.Source,
			BillingSubscriptionID: d.BillingSubscriptionID, Plan: domain.PlanCode(d.Plan), DirectSignup: d.DirectSignup,
		}
		if d.OffboardedAt != nil {
			ev.OffboardedAt = *d.OffboardedAt
		}
		return uc.Handle(ctx, ev)
	}
}

// UsageBilling is the usage-billing-q handler.
func UsageBilling(uc *app.UsageConsumer, validate Validator, log port.Logger) events.Handler {
	return traced(usageBilling(uc, validate, log))
}

func usageBilling(uc *app.UsageConsumer, validate Validator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		if env.Type != "UsagePeriodClosed" {
			ignored(log, "usage-billing-q", env)
			return nil
		}
		if err := validate(env.Type, env.Payload); err != nil {
			return fmt.Errorf("%w: %w", errSchemaViolation, err)
		}
		var d struct {
			TenantID         string          `json:"tenant_id"`
			PeriodStart      time.Time       `json:"period_start"`
			PeriodEnd        time.Time       `json:"period_end"`
			TokensTotal      int64           `json:"tokens_total"`
			APIRequestsTotal int64           `json:"api_requests_total"`
			AddonConsumed    int64           `json:"addon_consumed"`
			Limits           json.RawMessage `json:"limits"`
		}
		if err := json.Unmarshal(env.Payload, &d); err != nil {
			return fmt.Errorf("%w: %w", errSchemaViolation, err)
		}
		if err := sameTenant(env, d.TenantID); err != nil {
			return err
		}
		return uc.Handle(ctx, app.UsagePeriodClosedEvent{EventID: env.ID, Time: env.Timestamp, Record: domain.UsagePeriodRecord{
			TenantID: d.TenantID, PeriodStart: d.PeriodStart, PeriodEnd: d.PeriodEnd, TokensTotal: d.TokensTotal,
			APIRequestsTotal: d.APIRequestsTotal, AddonConsumed: d.AddonConsumed, Limits: d.Limits,
		}})
	}
}

// sameTenant rejects an envelope whose tenant_id disagrees with data's.
func sameTenant(env events.Envelope[json.RawMessage], dataTenant string) error {
	if env.TenantID != "" && env.TenantID != dataTenant {
		return fmt.Errorf("%w: envelope tenant_id %s differs from data.tenant_id %s", errSchemaViolation, env.TenantID, dataTenant)
	}
	return nil
}

func ignored(log port.Logger, queue string, env events.Envelope[json.RawMessage]) {
	if log != nil {
		log.Warn("unexpected event type acknowledged without processing", map[string]any{
			"queue": queue, "event_type": env.Type, "event_id": env.ID})
	}
}

// dlqReason classifies errors that must not be retried.
func dlqReason(err error) (string, bool) {
	switch {
	case errors.Is(err, app.ErrFutureTime):
		return ReasonFutureTime, true
	case errors.Is(err, errSchemaViolation):
		return ReasonSchemaViolation, true
	}
	return "", false
}

// RouteRejectsToDLQ sends non-retryable failures straight to the queue's
// DLQ with their reason. If the DLQ send fails the original error is
// returned, so the message stays on the queue for retry (IAM's pattern).
func RouteRejectsToDLQ(h events.Handler, dlq events.DLQPublisher, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := h(ctx, env)
		reason, reject := dlqReason(err)
		if !reject {
			return err
		}
		src, ok := events.SourceMessageFromContext(ctx)
		if !ok || dlq == nil {
			return err
		}
		if sErr := dlq.SendToDLQ(ctx, src.QueueURL, src.Body, maps.Clone(src.Attributes), reason); sErr != nil {
			if log != nil {
				log.Warn("DLQ send failed; message stays on the queue", map[string]any{
					"event_id": env.ID, "event_type": env.Type, "reason": reason, "error": sErr.Error()})
			}
			return err
		}
		if log != nil {
			log.Warn("message dead-lettered", map[string]any{
				"event_id": env.ID, "event_type": env.Type, "reason": reason, "error": err.Error()})
		}
		return nil
	}
}

// traced runs a handler in a "billing.consume <type>" span linked to the
// producer's trace through the envelope's trace_id (§11.3).
func traced(h events.Handler) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		ctx, end := app.StartLinkedSpan(ctx, "billing.consume "+env.Type, port.TraceLink{TraceID: env.TraceID, MessageID: env.ID},
			app.AttrTenant(env.TenantID), port.Attr{Key: "messaging.message.id", Value: env.ID})
		err := h(ctx, env)
		end(err)
		return err
	}
}
