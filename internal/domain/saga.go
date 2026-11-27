package domain

import "slices"

// SagaKind is the kind of conversion saga (LLD §4.1).
type SagaKind string

// Saga kinds.
const (
	SagaTrialConversion SagaKind = "trial_conversion"
	SagaDirectSignup    SagaKind = "direct_signup"
	SagaOperatorStub    SagaKind = "operator_stub"
)

// SagaState is a conversion saga state (§9.3.1).
type SagaState string

// Saga states.
const (
	SagaPaymentConfirmed        SagaState = "payment_confirmed"
	SagaRPConvertPending        SagaState = "rp_convert_pending"
	SagaAwaitingTenantConverted SagaState = "awaiting_tenant_converted"
	SagaConfirmed               SagaState = "confirmed"
	SagaFailed                  SagaState = "failed"
	SagaRefundPending           SagaState = "refund_pending"
	SagaRefunded                SagaState = "refunded"
	SagaReverted                SagaState = "reverted"
)

// AllSagaStates lists every state, for exhaustive tests.
var AllSagaStates = []SagaState{
	SagaPaymentConfirmed, SagaRPConvertPending, SagaAwaitingTenantConverted, SagaConfirmed,
	SagaFailed, SagaRefundPending, SagaRefunded, SagaReverted,
}

// Terminal reports whether no further transition is possible. confirmed is
// an end state of the conversion but can still be reverted (B-O8), so it is
// not terminal here.
func (s SagaState) Terminal() bool {
	return s == SagaRefunded || s == SagaReverted
}

// CountsForOverdueAlert reports whether a saga in this state counts toward
// the 30-minute alert (BIL-OPS-5, §9.3.1 table).
func (s SagaState) CountsForOverdueAlert() bool {
	return s == SagaPaymentConfirmed || s == SagaRPConvertPending || s == SagaAwaitingTenantConverted
}

// InitialSagaState returns the state a new saga starts in (§9.3.1): a trial
// checkout payment, a B-O1 operator stub, or a B-I1 direct signup.
func InitialSagaState(kind SagaKind) (SagaState, bool) {
	switch kind {
	case SagaTrialConversion:
		return SagaPaymentConfirmed, true
	case SagaOperatorStub:
		return SagaRPConvertPending, true
	case SagaDirectSignup:
		return SagaAwaitingTenantConverted, true
	}
	return "", false
}

// sagaTransitions is the §9.3.1 state diagram.
var sagaTransitions = map[SagaState][]SagaState{
	SagaPaymentConfirmed:        {SagaRPConvertPending},
	SagaRPConvertPending:        {SagaAwaitingTenantConverted, SagaRPConvertPending, SagaFailed, SagaConfirmed, SagaRefundPending},
	SagaAwaitingTenantConverted: {SagaConfirmed, SagaRefundPending},
	SagaFailed:                  {SagaRPConvertPending, SagaRefundPending},
	SagaRefundPending:           {SagaRefunded},
	SagaConfirmed:               {SagaReverted},
}

// CanSagaTransition reports whether from → to is allowed (§9.3.1).
func CanSagaTransition(from, to SagaState) bool {
	return slices.Contains(sagaTransitions[from], to)
}

// RP3Action is what the saga worker does with a Realm Provisioner RP-3 answer.
type RP3Action string

// RP-3 actions (§9.3.2).
const (
	// RP3Advance moves rp_convert_pending → awaiting_tenant_converted.
	RP3Advance RP3Action = "advance"
	// RP3Retry stays in rp_convert_pending with backoff and the same key.
	RP3Retry RP3Action = "retry"
	// RP3Fail moves to failed and raises SEV-2.
	RP3Fail RP3Action = "fail"
)

// RP3Answer is a normalised RP-3 response: the HTTP status (0 for a
// transport error or client timeout) and the problem errorCode, if any.
type RP3Answer struct {
	Status    int
	ErrorCode string
}

// ClassifyRP3 maps an RP-3 answer to the saga action of §9.3.2.
//
//	200                                         → advance
//	409 realm_already_dedicated                 → advance (a lost response; the 30-min alert covers it)
//	409 operation_in_progress                   → retry
//	502 conversion_failed / keycloak_unavailable → retry
//	503, transport error, client timeout        → retry
//	409 tenant_offboarded                       → fail
//	400, 403, 404                               → fail
//
// Anything else is unexpected and fails the saga so an operator looks at
// it, rather than retrying forever.
func ClassifyRP3(a RP3Answer) RP3Action {
	switch {
	case a.Status == 200:
		return RP3Advance
	case a.Status == 409 && a.ErrorCode == "realm_already_dedicated":
		return RP3Advance
	case a.Status == 409 && a.ErrorCode == "operation_in_progress":
		return RP3Retry
	case a.Status == 409 && a.ErrorCode == "tenant_offboarded":
		return RP3Fail
	case a.Status == 502, a.Status == 503, a.Status == 0:
		return RP3Retry
	case a.Status == 400, a.Status == 403, a.Status == 404:
		return RP3Fail
	}
	return RP3Fail
}

// RP3IdempotencyKey is the RP-3 key of BIL-SAGA-2.
func RP3IdempotencyKey(tenantID, billingSubscriptionID string) string {
	return "convert:" + tenantID + ":" + billingSubscriptionID
}
