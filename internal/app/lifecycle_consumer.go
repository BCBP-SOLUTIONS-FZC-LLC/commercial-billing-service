package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Inbox consumer names (§7.1; processed_events.consumer).
const (
	ConsumerTenantBilling = "tenant-billing"
	ConsumerUsageBilling  = "usage-billing"
)

// IAM lifecycle event types on tenant-billing-q.
const (
	EventTenantConverted   = "TenantConverted"
	EventTenantSuspended   = "TenantSuspended"
	EventTenantReactivated = "TenantReactivated"
	EventTenantOffboarded  = "TenantOffboarded"
)

// ErrFutureTime marks an event stamped beyond MAX_LIFECYCLE_EVENT_SKEW: the
// consumer routes it straight to the DLQ (reason future_time_clamp) rather
// than retrying it (BIL-EVT-3).
var ErrFutureTime = errors.New("future_time_clamp")

// ErrUnsupportedEvent marks a type the queue's filter should never deliver.
var ErrUnsupportedEvent = errors.New("unsupported event type")

// TenantLifecycleEvent is one tenant-billing-q message, decoded by the
// consumer adapter. Only the fields Billing reads are carried; export_s3_key
// never is (§7.1).
type TenantLifecycleEvent struct {
	EventID  string
	Type     string
	Time     time.Time // envelope time: the recency key
	TenantID string

	// TenantSuspended / TenantReactivated
	Source string

	// TenantOffboarded
	OffboardedAt time.Time

	// TenantConverted
	BillingSubscriptionID string
	Plan                  domain.PlanCode
	DirectSignup          bool
}

// ConversionConfirmer is the saga side of TenantConverted (§9.3), added in
// Phase 6. It runs in the event's transaction under the tenant lock and is
// exempt from the recency skip. matched=false (no saga for the
// subscription) means Billing did not initiate the conversion: no write.
type ConversionConfirmer interface {
	ConfirmConversion(ctx context.Context, ev TenantLifecycleEvent) (matched bool, err error)
}

// LifecycleConsumer projects IAM tenant lifecycle onto billing_accounts
// (§7.1): exactly once per event (inbox), under the tenant lock
// (BIL-CONC-1), last-writer-wins on envelope time (BIL-EVT-3).
type LifecycleConsumer struct {
	Inbox    port.InboxRunner
	Lock     port.TenantLocker
	Accounts port.LifecycleRepo
	Subs     port.SubscriptionRepo
	// Conversions is nil until Phase 6; the lifecycle projection then
	// applies to any tenant with an account.
	Conversions ConversionConfirmer
	MaxSkew     time.Duration // MAX_LIFECYCLE_EVENT_SKEW_SECONDS
	Now         func() time.Time
	Log         port.Logger
}

// Handle applies one event.
func (c *LifecycleConsumer) Handle(ctx context.Context, ev TenantLifecycleEvent) error {
	switch ev.Type {
	case EventTenantConverted, EventTenantSuspended, EventTenantReactivated, EventTenantOffboarded:
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedEvent, ev.Type)
	}
	if domain.FutureTimeClamped(ev.Time, c.now(), c.MaxSkew) {
		return fmt.Errorf("%w: %s %s at %s", ErrFutureTime, ev.Type, ev.EventID, ev.Time.Format(time.RFC3339Nano))
	}
	return c.Inbox.RunOnce(ctx, ConsumerTenantBilling, ev.EventID, ev.Type, ev.TenantID, func(ctx context.Context) error {
		if err := c.Lock.LockTenant(ctx, ev.TenantID); err != nil {
			return err
		}
		if ev.Type == EventTenantConverted && c.Conversions != nil {
			matched, err := c.Conversions.ConfirmConversion(ctx, ev)
			if err != nil {
				return err
			}
			if !matched {
				return nil
			}
		}
		acct, found, err := c.Accounts.AccountForUpdate(ctx, ev.TenantID)
		if err != nil {
			return err
		}
		if !found {
			c.skipped(ev, domain.SkipNoAccount)
			return nil
		}
		if skip := domain.LifecycleDecision(acct, ev.Time); skip != domain.SkipNone {
			c.skipped(ev, skip)
			return nil
		}
		if err := c.Accounts.ApplyLifecycle(ctx, ev.TenantID, change(ev)); err != nil {
			return err
		}
		if ev.Type == EventTenantSuspended && ev.Source == domain.SuspensionBillingLapse {
			return c.checkLapse(ctx, ev)
		}
		return nil
	})
}

// change is the account write for an applied event.
func change(ev TenantLifecycleEvent) domain.LifecycleChange {
	c := domain.LifecycleChange{EventAt: ev.Time}
	switch ev.Type {
	case EventTenantSuspended:
		src := ev.Source
		c.To, c.SuspensionSource = domain.LifecycleSuspended, &src
	case EventTenantOffboarded:
		at := ev.OffboardedAt
		if at.IsZero() {
			at = ev.Time
		}
		c.To, c.ClosedAt = domain.LifecycleOffboarded, &at
	default: // TenantConverted, TenantReactivated
		c.To = domain.LifecycleActive
	}
	return c
}

// checkLapse: a billing-lapse suspension follows Billing's own cancellation,
// so a subscription that is not cancelled is a mismatch for an operator.
func (c *LifecycleConsumer) checkLapse(ctx context.Context, ev TenantLifecycleEvent) error {
	if c.Subs == nil {
		return nil
	}
	s, found, err := c.Subs.Live(ctx, ev.TenantID)
	if err != nil {
		return err
	}
	if found && s.Status != domain.StatusCancelled && c.Log != nil {
		c.Log.Error("lifecycle_mismatch: IAM suspended the tenant for billing lapse but Billing's subscription is not cancelled", map[string]any{
			"tenant_id": ev.TenantID, "event_id": ev.EventID, "billing_subscription_id": s.BillingSubscriptionID,
			"status": string(s.Status), "severity": "SEV-3"})
	}
	return nil
}

func (c *LifecycleConsumer) skipped(ev TenantLifecycleEvent, why domain.LifecycleSkip) {
	if c.Log != nil {
		c.Log.Info("IAM lifecycle event not applied", map[string]any{
			"tenant_id": ev.TenantID, "event_id": ev.EventID, "event_type": ev.Type, "reason": string(why)})
	}
}

func (c *LifecycleConsumer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// UsagePeriodClosedEvent is one usage-billing-q message.
type UsagePeriodClosedEvent struct {
	EventID string
	Time    time.Time
	Record  domain.UsagePeriodRecord
}

// UsageConsumer stores period totals for reference; nothing is priced from
// them (§7.1, §8.12). Trial tenants are stored too.
type UsageConsumer struct {
	Inbox port.InboxRunner
	Usage port.UsageRecordRepo
	Log   port.Logger
}

// Handle stores one period, replacing a stored one only when newer.
func (c *UsageConsumer) Handle(ctx context.Context, ev UsagePeriodClosedEvent) error {
	r := ev.Record
	r.SourceEventID, r.SourceEventTime = ev.EventID, ev.Time
	return c.Inbox.RunOnce(ctx, ConsumerUsageBilling, ev.EventID, "UsagePeriodClosed", r.TenantID, func(ctx context.Context) error {
		applied, err := c.Usage.Upsert(ctx, r)
		if err == nil && !applied && c.Log != nil {
			c.Log.Info("UsagePeriodClosed older than the stored period; kept the stored one", map[string]any{
				"tenant_id": r.TenantID, "event_id": ev.EventID, "period_start": r.PeriodStart})
		}
		return err
	})
}
