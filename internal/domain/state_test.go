package domain

import "testing"

// TestBIL_SUB_4_EveryStatusPair checks every from × to pair against §8.14:
// exactly these ten moves are allowed, each with its event.
func TestBIL_SUB_4_EveryStatusPair(t *testing.T) {
	want := map[[2]SubscriptionStatus]EventType{
		{StatusIncomplete, StatusActive}: "",
		{StatusIncomplete, StatusEnded}:  "",
		{StatusActive, StatusPastDue}:    EventTenantPaymentPastDue,
		{StatusPastDue, StatusActive}:    EventTenantReactivated,
		{StatusActive, StatusCancelled}:  EventTenantSubscriptionCancelled,
		{StatusPastDue, StatusCancelled}: EventTenantSubscriptionCancelled,
		{StatusCancelled, StatusActive}:  EventTenantReactivated,
		{StatusActive, StatusEnded}:      "",
		{StatusPastDue, StatusEnded}:     "",
		{StatusCancelled, StatusEnded}:   "",
	}
	for _, from := range AllSubscriptionStatuses {
		for _, to := range AllSubscriptionStatuses {
			ev, allowed := want[[2]SubscriptionStatus{from, to}]
			tr, ok := LookupTransition(from, to)
			if ok != allowed {
				t.Errorf("%s → %s: allowed = %v, want %v", from, to, ok, allowed)
				continue
			}
			if ok && tr.Event != ev {
				t.Errorf("%s → %s: event = %q, want %q", from, to, tr.Event, ev)
			}
		}
	}
	if len(SubscriptionTransitions()) != len(want) {
		t.Errorf("table has %d transitions, want %d", len(SubscriptionTransitions()), len(want))
	}
}

func TestEndedIsTerminalAndNotLive(t *testing.T) {
	for _, to := range AllSubscriptionStatuses {
		if CanTransition(StatusEnded, to) {
			t.Errorf("ended → %s must be rejected", to)
		}
	}
	// BIL-SUB-3: only non-ended subscriptions count as live.
	for _, s := range AllSubscriptionStatuses {
		if s.Live() == (s == StatusEnded) {
			t.Errorf("%s.Live() wrong", s)
		}
	}
}

// BIL-EVT-8: incomplete is never projected to IAM, so no move into or out
// of it produces an event.
func TestBIL_EVT_8_IncompleteNeverProjected(t *testing.T) {
	for _, tr := range SubscriptionTransitions() {
		if (tr.From == StatusIncomplete || tr.To == StatusIncomplete) && tr.Event != "" {
			t.Errorf("%s → %s emits %s", tr.From, tr.To, tr.Event)
		}
	}
}

func TestSagaEveryStatePair(t *testing.T) {
	allowed := map[[2]SagaState]bool{
		{SagaPaymentConfirmed, SagaRPConvertPending}:        true,
		{SagaRPConvertPending, SagaAwaitingTenantConverted}: true,
		{SagaRPConvertPending, SagaRPConvertPending}:        true,
		{SagaRPConvertPending, SagaFailed}:                  true,
		{SagaRPConvertPending, SagaConfirmed}:               true,
		{SagaRPConvertPending, SagaRefundPending}:           true,
		{SagaAwaitingTenantConverted, SagaConfirmed}:        true,
		{SagaAwaitingTenantConverted, SagaRefundPending}:    true,
		{SagaFailed, SagaRPConvertPending}:                  true,
		{SagaFailed, SagaRefundPending}:                     true,
		{SagaRefundPending, SagaRefunded}:                   true,
		{SagaConfirmed, SagaReverting}:                      true,
		{SagaConfirmed, SagaRefunded}:                       true,
		{SagaReverting, SagaReverting}:                      true,
		{SagaReverting, SagaReverted}:                       true,
		{SagaReverting, SagaConfirmed}:                      true,
	}
	for _, from := range AllSagaStates {
		for _, to := range AllSagaStates {
			if got, want := CanSagaTransition(from, to), allowed[[2]SagaState{from, to}]; got != want {
				t.Errorf("%s → %s: %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range []SagaState{SagaRefunded, SagaReverted} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	if SagaConfirmed.Terminal() {
		t.Error("confirmed can still be reverted (B-O8)")
	}
}

// §9.3.2: every RP-3 answer.
func TestRP3AnswerHandling(t *testing.T) {
	cases := []struct {
		a    RP3Answer
		want RP3Action
	}{
		{RP3Answer{200, ""}, RP3Advance},
		{RP3Answer{409, "realm_already_dedicated"}, RP3Advance},
		{RP3Answer{409, "operation_in_progress"}, RP3Retry},
		{RP3Answer{502, "conversion_failed"}, RP3Retry},
		{RP3Answer{502, "keycloak_unavailable"}, RP3Retry},
		{RP3Answer{503, "db_unavailable"}, RP3Retry},
		{RP3Answer{503, "request_timeout"}, RP3Retry},
		{RP3Answer{0, ""}, RP3Retry}, // transport error or client timeout
		{RP3Answer{409, "tenant_offboarded"}, RP3Fail},
		{RP3Answer{400, "missing_idempotency_key"}, RP3Fail},
		{RP3Answer{403, "tenant_mismatch"}, RP3Fail},
		{RP3Answer{404, "tenant_realm_not_found"}, RP3Fail},
		{RP3Answer{418, ""}, RP3Fail}, // unexpected: an operator looks
	}
	for _, tc := range cases {
		if got := ClassifyRP3(tc.a); got != tc.want {
			t.Errorf("%d %s → %s, want %s", tc.a.Status, tc.a.ErrorCode, got, tc.want)
		}
	}
}

// RP's documented transient answers (internal_handler.go RP-3: 409
// optimistic_lock_conflict, 500, 503 token_service_unavailable, a gateway
// 504) keep the saga pending with backoff instead of failing it.
func TestBIL_SAGA_1_RP3TransientAnswersRetry(t *testing.T) {
	for _, a := range []RP3Answer{{409, "optimistic_lock_conflict"}, {500, ""}, {500, "INTERNAL"}, {503, "token_service_unavailable"}, {504, ""}} {
		if got := ClassifyRP3(a); got != RP3Retry {
			t.Errorf("%d %s → %s, want retry", a.Status, a.ErrorCode, got)
		}
	}
	for _, a := range []RP3Answer{{409, "realm_name_unavailable"}, {409, "credential_replay_expired"}, {409, ""}} {
		if got := ClassifyRP3(a); got != RP3Fail {
			t.Errorf("%d %s → %s, want fail", a.Status, a.ErrorCode, got)
		}
	}
}

func TestBIL_SAGA_2_RP3Key(t *testing.T) {
	if got := RP3IdempotencyKey("t1", "s1"); got != "convert:t1:s1" {
		t.Fatalf("got %q", got)
	}
}

func TestBIL_OPS_5_AlertStates(t *testing.T) {
	want := map[SagaState]bool{SagaPaymentConfirmed: true, SagaRPConvertPending: true, SagaAwaitingTenantConverted: true}
	for _, s := range AllSagaStates {
		if s.CountsForOverdueAlert() != want[s] {
			t.Errorf("%s CountsForOverdueAlert = %v", s, s.CountsForOverdueAlert())
		}
	}
}

// C-8 (rev 0.73): the standard-library UUID check matches the canonical form
// the google/uuid check accepted for a refund's purchase_ref.
func TestCanonicalUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"3fa85f64-5717-4562-b3fc-2c963f66afa6":  true,
		"3FA85F64-5717-4562-B3FC-2C963F66AFA6":  true,
		"3fa85f6457174562b3fc2c963f66afa6":      false,
		"3fa85f64-5717-4562-b3fc-2c963f66afag":  false,
		"3fa85f64_5717-4562-b3fc-2c963f66afa6":  false,
		"{3fa85f64-5717-4562-b3fc-2c963f66afa}": false,
		"":                                      false,
	} {
		if got := canonicalUUID(s); got != want {
			t.Errorf("canonicalUUID(%q) = %v, want %v", s, got, want)
		}
	}
}
