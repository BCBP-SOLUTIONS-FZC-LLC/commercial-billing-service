package domain

import "time"

// IAM tenant lifecycle projection (LLD §7.1, BIL-EVT-3).

// Suspension sources carried by TenantSuspended.
const (
	SuspensionBillingLapse = "billing_lapse"
	SuspensionOperator     = "operator"
)

// LifecycleSkip says why an IAM lifecycle event is not applied.
type LifecycleSkip string

// Skip reasons (logged and counted; the message is acknowledged).
const (
	SkipNone       LifecycleSkip = ""
	SkipNoAccount  LifecycleSkip = "no_account" // a trial tenant Billing has no account for
	SkipOffboarded LifecycleSkip = "offboarded" // offboarded is terminal regardless of time
	SkipStale      LifecycleSkip = "stale"      // time not newer than iam_last_event_at
)

// LifecycleDecision is BIL-EVT-3: last-writer-wins on the envelope time
// against iam_last_event_at, with offboarded terminal. An equal time is
// stale, the same tie rule as Core's EVT-14.
func LifecycleDecision(a BillingAccount, eventTime time.Time) LifecycleSkip {
	if a.IAMLifecycle == LifecycleOffboarded {
		return SkipOffboarded
	}
	if a.IAMLastEventAt != nil && !eventTime.After(*a.IAMLastEventAt) {
		return SkipStale
	}
	return SkipNone
}

// FutureTimeClamped reports an event stamped more than skew ahead of now: a
// poison pill that would block every later event for the tenant, so it goes
// to the DLQ (reason future_time_clamp) instead of being applied (BIL-EVT-3,
// Core EVT-15).
func FutureTimeClamped(eventTime, now time.Time, skew time.Duration) bool {
	return eventTime.After(now.Add(skew))
}

// LifecycleChange is the account write for one applied event.
type LifecycleChange struct {
	To               IAMLifecycle
	SuspensionSource *string    // set iff To = suspended
	ClosedAt         *time.Time // set iff To = offboarded
	EventAt          time.Time  // becomes iam_last_event_at
}

// UsagePeriodRecord is one UsagePeriodClosed, kept for reference (§4.2.21).
type UsagePeriodRecord struct {
	TenantID         string
	PeriodStart      time.Time
	PeriodEnd        time.Time
	TokensTotal      int64
	APIRequestsTotal int64
	AddonConsumed    int64
	Limits           []byte // JSON object
	SourceEventID    string
	SourceEventTime  time.Time
}

// Delayed emissions (§4.2.20, BIL-EVT-6).
const (
	BuilderSeatsAfterConversion = "seats_after_conversion"
	BuilderSeatsResync          = "seats_resync"
)

// ScheduledEmission is one row of scheduled_emissions.
type ScheduledEmission struct {
	EmissionID     string
	TenantID       string
	EventType      EventType
	PayloadBuilder string
	RefID          string // billing_subscription_id
	DueAt          time.Time
}

// SeatsReasonFor maps a builder to TenantSeatsChanged.reason.
func SeatsReasonFor(builder string) (SeatsChangeReason, bool) {
	switch builder {
	case BuilderSeatsAfterConversion:
		return SeatsConversion, true
	case BuilderSeatsResync:
		return SeatsResync, true
	}
	return "", false
}

// SeatsProjectable reports whether a delayed seat event may still be
// published for a subscription in status s: only while it is paid for
// (active, past_due). Never for incomplete (BIL-EVT-8); a cancelled or
// ended subscription's seat count no longer matters to Core.
func SeatsProjectable(s SubscriptionStatus) bool {
	return s == StatusActive || s == StatusPastDue
}
