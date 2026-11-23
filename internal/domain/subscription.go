package domain

// SubscriptionStatus is Billing's subscription status (LLD §4.1, §8.14).
// incomplete is never projected to IAM (BIL-EVT-8); ended is terminal.
type SubscriptionStatus string

// Subscription statuses.
const (
	StatusIncomplete SubscriptionStatus = "incomplete"
	StatusActive     SubscriptionStatus = "active"
	StatusPastDue    SubscriptionStatus = "past_due"
	StatusCancelled  SubscriptionStatus = "cancelled"
	StatusEnded      SubscriptionStatus = "ended"
)

// AllSubscriptionStatuses lists every status, for exhaustive tests.
var AllSubscriptionStatuses = []SubscriptionStatus{
	StatusIncomplete, StatusActive, StatusPastDue, StatusCancelled, StatusEnded,
}

// Live reports whether the subscription counts toward BIL-SUB-3 (at most one
// non-ended subscription per tenant).
func (s SubscriptionStatus) Live() bool { return s != StatusEnded && s != "" }

// Transition is one allowed status move and the event it produces.
type Transition struct {
	From, To SubscriptionStatus
	// Event is the billing.events type this move emits, or "" for none.
	Event EventType
}

// subscriptionTransitions is the single source of the §8.14 state machine
// (BIL-SUB-4). Every other move is rejected: a code path gets 500 INTERNAL,
// a provider callback is recorded ignored (BIL-PAY-4).
var subscriptionTransitions = []Transition{
	{StatusIncomplete, StatusActive, ""},                               // first payment confirmed (conversion saga carries the IAM change)
	{StatusIncomplete, StatusEnded, ""},                                // checkout expired / declined / 3-D Secure abandoned
	{StatusActive, StatusPastDue, EventTenantPaymentPastDue},           // cycle invoice payment failed
	{StatusPastDue, StatusActive, EventTenantReactivated},              // invoice paid
	{StatusActive, StatusCancelled, EventTenantSubscriptionCancelled},  // customer / auto-renew off
	{StatusPastDue, StatusCancelled, EventTenantSubscriptionCancelled}, // dunning exhausted
	{StatusCancelled, StatusActive, EventTenantReactivated},            // settled via a new provider subscription
	{StatusActive, StatusEnded, ""},                                    // TenantOffboarded / RP-4 reversal / signup refund
	{StatusPastDue, StatusEnded, ""},                                   // TenantOffboarded / RP-4 reversal
	{StatusCancelled, StatusEnded, ""},                                 // TenantOffboarded
}

var transitionIndex = func() map[[2]SubscriptionStatus]Transition {
	m := make(map[[2]SubscriptionStatus]Transition, len(subscriptionTransitions))
	for _, t := range subscriptionTransitions {
		m[[2]SubscriptionStatus{t.From, t.To}] = t
	}
	return m
}()

// LookupTransition returns the allowed transition from → to (BIL-SUB-4).
func LookupTransition(from, to SubscriptionStatus) (Transition, bool) {
	t, ok := transitionIndex[[2]SubscriptionStatus{from, to}]
	return t, ok
}

// CanTransition reports whether from → to is allowed (BIL-SUB-4).
func CanTransition(from, to SubscriptionStatus) bool {
	_, ok := LookupTransition(from, to)
	return ok
}

// SubscriptionTransitions returns a copy of the transition table.
func SubscriptionTransitions() []Transition {
	out := make([]Transition, len(subscriptionTransitions))
	copy(out, subscriptionTransitions)
	return out
}
