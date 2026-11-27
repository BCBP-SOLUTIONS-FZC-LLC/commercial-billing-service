package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDomainErrorWithCause(t *testing.T) {
	e := &DomainError{Code: ErrValidation, Message: "bad", Cause: errors.New("root")}
	if got := e.Error(); got != "VALIDATION_ERROR: bad: root" {
		t.Errorf("Error() = %q", got)
	}
}

func TestProblemTypeAndInternalProblem(t *testing.T) {
	if _, ok := ProblemType(ErrValidation); !ok {
		t.Error("VALIDATION_ERROR has no problem type")
	}
	if _, ok := ProblemType("NO_SUCH_CODE"); ok {
		t.Error("unknown code has a problem type")
	}
	for _, code := range []ErrorCode{ErrInternal, "NO_SUCH_CODE"} {
		p := (&DomainError{Code: code, Message: "secret detail"}).Problem()
		if p.Status() != 500 || p.Code != string(ErrInternal) {
			t.Errorf("%s: problem %d %s, want 500 INTERNAL", code, p.Status(), p.Code)
		}
	}
}

func TestIntervalValidAndCurrencyExponent(t *testing.T) {
	if !IntervalYear.Valid() || BillingInterval("week").Valid() {
		t.Error("interval validity wrong")
	}
	if _, err := MinorUnitExponent("us"); err == nil {
		t.Error("malformed currency accepted")
	}
}

func TestSeatsReasonForUnknownBuilder(t *testing.T) {
	if r, ok := SeatsReasonFor(BuilderSeatsResync); !ok || r != SeatsResync {
		t.Errorf("resync → %q %v", r, ok)
	}
	if _, ok := SeatsReasonFor("nope"); ok {
		t.Error("unknown builder mapped")
	}
}

func TestCanSagaTransitionOrStayUnknownState(t *testing.T) {
	if CanSagaTransitionOrStay(ConversionSaga{State: "bogus"}) {
		t.Error("unknown state may stay")
	}
	if !CanSagaTransitionOrStay(ConversionSaga{State: SagaConfirmed}) {
		t.Error("confirmed may stay")
	}
}

func TestCheckoutProviderCreatedAndBackoff(t *testing.T) {
	if (CheckoutSession{ProviderSessionID: PendingSessionPrefix + "x"}).ProviderCreated() {
		t.Error("pending session reported created")
	}
	if !(CheckoutSession{ProviderSessionID: "cs_1"}).ProviderCreated() {
		t.Error("provider session reported pending")
	}
	if got := SagaBackoff(3, time.Minute); got != 8*time.Second {
		t.Errorf("backoff(3) = %s", got)
	}
	if got := SagaBackoff(20, time.Minute); got != time.Minute {
		t.Errorf("backoff(20) = %s", got)
	}
}

func TestCertOutcome(t *testing.T) {
	if _, _, known := CertOutcome("cert-9.9", "native", "native", nil); known {
		t.Error("unknown suite known")
	}
	missing, ok, known := CertOutcome("cert-1.0", "fallback", "fallback",
		[]CertResult{{Capability: "proration", Case: "calculator_charged", Passed: true}})
	if !known || !ok || len(missing) == 0 {
		t.Errorf("fallback: missing %d ok %v known %v", len(missing), ok, known)
	}
	if _, ok, _ := CertOutcome("cert-1.0", "fallback", "native", nil); ok {
		t.Error("declared native proved by fallback")
	}
}

func TestPaymentEventKinds(t *testing.T) {
	if KindIgnored.ChangesState() || !KindInvoicePaid.ChangesState() {
		t.Error("ChangesState wrong")
	}
	for k, want := range map[NormalisedKind]ProviderObjectKind{
		KindSubscriptionUpdated: ObjectSubscription, KindSubscriptionEnded: ObjectSubscription,
		KindScheduleUpdated: ObjectSchedule, KindRefundUpdated: ObjectRefund,
		KindDisputeOpened: ObjectDispute, KindDisputeClosed: ObjectDispute,
		KindFraudWarning: ObjectEarlyFraudWarning, KindCustomerUpdated: ObjectCustomer,
		KindCheckoutExpired: ObjectCheckoutSession, KindIgnored: "",
	} {
		if got := ObjectKindFor(k); got != want {
			t.Errorf("%s → %q, want %q", k, got, want)
		}
	}
	if got := (ProviderObjectRef{Kind: ObjectInvoice, ID: "in_1"}).String(); got != "in_1" {
		t.Errorf("String() = %q", got)
	}
}

func wantInvalid(t *testing.T, name string, err error, fragment string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidPayload) || !strings.Contains(err.Error(), fragment) {
		t.Errorf("%s: err %v, want invalid payload containing %q", name, err, fragment)
	}
}

func TestEventValidationRejections(t *testing.T) {
	now := time.Now()
	neg, one := -1, int64(1)
	wantInvalid(t, "plan values", TenantPlanChanged{TenantID: "t", BillingRef: "b", EffectiveAt: now, Plan: "x", PreviousPlan: PlanPro}.Validate(), "plan and previous_plan")
	wantInvalid(t, "plan required", TenantPlanChanged{TenantID: "t"}.Validate(), "required")
	wantInvalid(t, "direction", TenantPlanChanged{TenantID: "t", BillingRef: "b", EffectiveAt: now, Plan: PlanPro, PreviousPlan: PlanStarter, Direction: "sideways"}.Validate(), "direction")

	wantInvalid(t, "seats tenant", TenantSeatsChanged{LicensedSeats: 1, Reason: SeatsPurchase}.Validate(), "tenant_id")
	wantInvalid(t, "previous seats", TenantSeatsChanged{TenantID: "t", LicensedSeats: 1, PreviousLicensedSeats: &neg, Reason: SeatsPurchase}.Validate(), "previous_licensed_seats")
	wantInvalid(t, "deferral pair", TenantSeatsChanged{TenantID: "t", LicensedSeats: 1, Reason: SeatsPurchase, DeferredAmountMinor: &one}.Validate(), "go together")
	wantInvalid(t, "deferral reason", TenantSeatsChanged{TenantID: "t", LicensedSeats: 1, Reason: SeatsRenewal, DeferredAmountMinor: &one, DeferralReason: DeferralBelowProviderMinimum}.Validate(), "only reported on reason=purchase")

	wantInvalid(t, "deferral amount", TenantSeatsChanged{TenantID: "t", LicensedSeats: 1, Reason: SeatsPurchase, DeferredAmountMinor: &one, DeferralReason: "other"}.Validate(), "below_provider_minimum")
	wantInvalid(t, "past due required", TenantPaymentPastDue{TenantID: "t"}.Validate(), "required")
	wantInvalid(t, "past due currency", TenantPaymentPastDue{TenantID: "t", InvoiceRef: "i", PastDueSince: now, AmountDue: &Money{AmountMinor: 1, Currency: "usd"}}.Validate(), "ISO 4217")

	wantInvalid(t, "cancel required", TenantSubscriptionCancelled{TenantID: "t", Reason: CancelCustomer}.Validate(), "required")
	wantInvalid(t, "cancel reason", TenantSubscriptionCancelled{TenantID: "t", CancelledAt: now, Reason: "whim"}.Validate(), "not allowed")

	wantInvalid(t, "reactivated required", TenantReactivated{TenantID: "t", Source: ReactivationSource}.Validate(), "required")
	wantInvalid(t, "reactivated source", TenantReactivated{TenantID: "t", ReactivatedAt: now, Source: "core"}.Validate(), "source")

	wantInvalid(t, "reactivated previous", TenantReactivated{TenantID: "t", ReactivatedAt: now, Source: ReactivationSource, PreviousStatus: "active"}.Validate(), "previous_status")
	wantInvalid(t, "addon resource", TenantQuotaAddOnChanged{TenantID: "t", BillingRef: "b", Resource: "seats"}.Validate(), "llm_tokens")
	wantInvalid(t, "addon purchase ref", TenantQuotaAddOnChanged{TenantID: "t", BillingRef: "b", Resource: ResourceLLMTokens, Reason: AddonPurchase, Delta: 1, PurchasedAt: &now, PurchaseRef: "x"}.Validate(), "refund-only")
	wantInvalid(t, "addon pack qty", TenantQuotaAddOnChanged{TenantID: "t", BillingRef: "b", Resource: ResourceLLMTokens, Reason: AddonPurchase, Delta: 1, PurchasedAt: &now, PackQuantity: -1}.Validate(), "pack_quantity")
}
