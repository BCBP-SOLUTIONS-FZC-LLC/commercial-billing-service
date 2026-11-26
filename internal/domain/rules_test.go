package domain

import (
	"errors"
	"testing"
	"time"
)

// §14.1 Permissions: every route × role.
func TestPermissionsEveryRouteAndRole(t *testing.T) {
	ownerOnly := map[Permission]bool{
		PermSubscriptionCancel: true, PermSubscriptionChangePlan: true, PermPaymentMethodManage: true,
	}
	if len(TenantRoutePermission) != 26 {
		t.Fatalf("want 26 tenant routes, got %d", len(TenantRoutePermission))
	}
	for route, perm := range TenantRoutePermission {
		if !HasPermission([]string{RoleTenantOwner}, perm) {
			t.Errorf("%s: owner must hold %s", route, perm)
		}
		if got := HasPermission([]string{RoleTenantAdmin}, perm); got == ownerOnly[perm] {
			t.Errorf("%s: tenant_admin holds %s = %v", route, perm, got)
		}
		for _, r := range []string{RoleTenderAdmin, RoleMember, RolePlatformOperator, RolePCIOwnerReviewer, RoleCommercialSystem, ""} {
			if HasPermission([]string{r}, perm) {
				t.Errorf("%s: role %q must hold no billing permission (BIL-AUTH-2)", route, r)
			}
		}
	}
}

// BIL-AUTH-7: the owner-only permissions are never implied by others, and
// the four "new contractual commitment" routes need change_plan.
func TestBIL_AUTH_7_OwnerOnlyNeverImplied(t *testing.T) {
	admin := PermissionsFor([]string{RoleTenantAdmin, RoleTenderAdmin, RoleMember})
	for _, p := range []Permission{PermSubscriptionChangePlan, PermPaymentMethodManage, PermSubscriptionCancel} {
		if admin[p] {
			t.Errorf("non-owner roles together grant %s", p)
		}
	}
	for _, r := range NewContractualCommitmentRoutes {
		if TenantRoutePermission[r] != PermSubscriptionChangePlan {
			t.Errorf("%s must require change_plan", r)
		}
	}
	for _, r := range []string{"B-P7", "B-P10", "B-P11", "B-P20"} {
		if TenantRoutePermission[r] != PermSubscriptionManage {
			t.Errorf("%s stays under subscription.manage (Finance overlap rule)", r)
		}
	}
	for _, r := range []string{"B-P16", "B-P26"} {
		if TenantRoutePermission[r] != PermPaymentMethodManage {
			t.Errorf("%s must require payment_method.manage", r)
		}
	}
}

// §14.1 Error mapping: every §17 code → status.
func TestErrorCatalogueMatchesLLD(t *testing.T) {
	want := map[int][]ErrorCode{
		400: {ErrValidation, ErrTenantMismatch, ErrInvalidSignature},
		402: {ErrPaymentDeclined, ErrPaymentActionRequired},
		403: {ErrInsufficientPermission, ErrInsufficientRole},
		404: {ErrSubscriptionNotFound, ErrPurchaseNotFound, ErrInvoiceNotFound, ErrSagaNotFound, ErrScheduleNotFound, ErrPlanChangeNotFound, ErrRunNotFound, ErrLedgerEventNotFound, ErrCertificationNotFound},
		409: {ErrSubscriptionAlreadyExists, ErrSubscriptionRequired, ErrSubscriptionNotActive, ErrSubscriptionNotLapsed, ErrSubscriptionEnding, ErrAccountClosed, ErrCollectionPaused, ErrInvoiceNotHeld, ErrPlanChangeBlocked, ErrPlanChangePending, ErrSamePlan, ErrTenantNotKnownToIAM, ErrSeatPurchaseInProgress, ErrPreviewStale, ErrScheduleBeingApplied, ErrScheduleAlreadyApplied, ErrAddonNotEligible, ErrAddonNotAvailable, ErrAddonCapReached, ErrPurchaseNotAwaitingPayment, ErrProviderOperationUnsupported, ErrInvoiceNotFinalised, ErrOptimisticLockConflict, ErrConcurrentUpdate, ErrRequestInProgress, ErrRevertWindowExpired, ErrSagaAlreadyConfirmed, ErrReconciliationRunning, ErrPriceOverlap, ErrCertificationExists, ErrCertificationNotActive, ErrCertificationNotPending},
		410: {ErrChargeExpired, ErrPreviewExpired},
		413: {ErrPayloadTooLarge},
		415: {ErrUnsupportedMediaType},
		422: {ErrIdempotencyKeyReused, ErrPriceNotAvailable, ErrSeatsBelowInUse, ErrTargetNotBelowCurrent, ErrTermsNotAccepted, ErrCurrencyMismatch, ErrRefundExceedsPaid, ErrAddonRefundCategory, ErrProviderPriceMismatch, ErrSuiteVersionMismatch, ErrCertificationIncomplete, ErrSelfReviewNotAllowed},
		500: {ErrInternal},
		503: {ErrProviderUnavailable, ErrProviderNotEnabled, ErrIAMUnavailable, ErrEntitlementsUnavailable, ErrDBUnavailable, ErrRequestTimeout},
	}
	n := 0
	for status, codes := range want {
		for _, c := range codes {
			n++
			if c.Status() != status {
				t.Errorf("%s → %d, want %d", c, c.Status(), status)
			}
		}
	}
	if n != len(ErrorCatalogue()) {
		t.Errorf("catalogue has %d codes, test lists %d", len(ErrorCatalogue()), n)
	}
	if ErrorCode("NOPE").Known() || ErrorCode("NOPE").Status() != 500 {
		t.Error("unknown code must be unknown and map to 500")
	}
}

func TestDomainErrorWithKeepsOriginal(t *testing.T) {
	base := NewError(ErrSeatPurchaseInProgress, "in progress")
	ext := base.With("purchase_id", "p1")
	if base.Extensions != nil || ext.Extensions["purchase_id"] != "p1" || ext.Status() != 409 {
		t.Fatal("With must copy, not mutate")
	}
	cause := errors.New("boom")
	if !errors.Is(&DomainError{Code: ErrInternal, Cause: cause}, cause) {
		t.Fatal("Unwrap must expose the cause")
	}
}

func TestBIL_SEAT_3_RenewalSeats(t *testing.T) {
	// HLD §8.5.1 example: 100 seats, target 60, 55 in use → 60; 75 in use → 75.
	if got := RenewalSeatCount(60, 55); got != 60 {
		t.Errorf("got %d, want 60", got)
	}
	if got := RenewalSeatCount(60, 75); got != 75 {
		t.Errorf("got %d, want 75", got)
	}
	// Top-up when members were added inside the lead window.
	if got := RenewalTopUp(60, 64); got != 4 {
		t.Errorf("top-up = %d, want 4", got)
	}
	if got := RenewalTopUp(60, 58); got != 0 {
		t.Errorf("top-up = %d, want 0", got)
	}
	for renewal := 1; renewal < 50; renewal += 7 {
		for inUse := 0; inUse < 60; inUse += 5 {
			if FinalRenewalSeats(renewal, inUse) < inUse {
				t.Fatalf("final %d below in-use %d", FinalRenewalSeats(renewal, inUse), inUse)
			}
		}
	}
	if SeatsInUse(50, 5) != 55 {
		t.Error("seats in use = active users + pending invitations")
	}
	if !ValidReductionTarget(60, 100) || ValidReductionTarget(100, 100) || ValidReductionTarget(0, 100) {
		t.Error("reduction target must be 1 ≤ target < current")
	}
}

func TestPlanDirection(t *testing.T) {
	cases := []struct {
		from, to PlanCode
		want     PlanChangeDirection
	}{
		{PlanStarter, PlanPro, DirectionUpgrade},
		{PlanPro, PlanEnterprise, DirectionUpgrade},
		{PlanEnterprise, PlanStarter, DirectionDowngrade},
		{PlanPro, PlanPro, DirectionSame},
	}
	for _, tc := range cases {
		if got := DirectionOf(tc.from, tc.to); got != tc.want {
			t.Errorf("%s → %s = %s, want %s", tc.from, tc.to, got, tc.want)
		}
	}
}

// §14.1 Add-on eligibility: each ineligible_reason; cap boundary (BIL-ADD-1).
func TestBIL_ADD_1_Eligibility(t *testing.T) {
	ok := AddonEligibilityInput{SubscriptionStatus: StatusActive, IAMLifecycle: LifecycleActive, EntitlementsKnown: true, TokenQuotaFinite: true}
	mod := func(f func(*AddonEligibilityInput)) AddonEligibilityInput { c := ok; f(&c); return c }
	cases := []struct {
		in   AddonEligibilityInput
		want IneligibleReason
	}{
		{ok, ""},
		{mod(func(i *AddonEligibilityInput) { i.IAMLifecycle = LifecycleTrial }), IneligibleTrial},
		{mod(func(i *AddonEligibilityInput) { i.SubscriptionStatus = "" }), IneligibleTrial},
		{mod(func(i *AddonEligibilityInput) { i.SubscriptionStatus = StatusPastDue }), IneligibleNotActive},
		{mod(func(i *AddonEligibilityInput) { i.IAMLifecycle = LifecycleSuspended }), IneligibleNotActive},
		{mod(func(i *AddonEligibilityInput) { i.CollectionPaused = true }), IneligibleCollectionPaused},
		{mod(func(i *AddonEligibilityInput) { i.EntitlementsKnown = false }), IneligibleNotAvailable},
		{mod(func(i *AddonEligibilityInput) { i.TokenQuotaFinite = false }), IneligibleUnlimitedQuota},
	}
	for _, tc := range cases {
		eligible, reason := AddonEligibility(tc.in)
		if eligible != (tc.want == "") || reason != tc.want {
			t.Errorf("%+v → %v %q, want %q", tc.in, eligible, reason, tc.want)
		}
	}

	limit := int64(10_000_000)
	if !WithinCap(&limit, 5_000_000, 5_000_000) || WithinCap(&limit, 5_000_000, 5_000_001) {
		t.Error("cap boundary: confirmed + tokens ≤ cap")
	}
	if !WithinCap(nil, 1<<40, 1<<40) {
		t.Error("no cap means no limit")
	}
	if r, has := CapRemaining(&limit, 12_000_000); !has || r != 0 {
		t.Errorf("remaining never negative: %d", r)
	}
	if _, has := CapRemaining(nil, 0); has {
		t.Error("no cap → ok=false")
	}
}

func TestBIL_ADD_3_RefundCategories(t *testing.T) {
	for _, c := range []AddonRefundCategory{RefundBillingError, RefundDuplicateCharge, RefundFraud} {
		if !AddonRefundAllowed(c) {
			t.Errorf("%s must be allowed", c)
		}
	}
	for _, c := range []AddonRefundCategory{"customer_request", "goodwill", ""} {
		if AddonRefundAllowed(c) {
			t.Errorf("%q must be refused (no customer refund path)", c)
		}
	}
}

func TestMoney(t *testing.T) {
	for code, want := range map[string]int{"USD": 2, "AED": 2, "JPY": 0, "KWD": 3, "CLF": 4} {
		if got, err := MinorUnitExponent(code); err != nil || got != want {
			t.Errorf("%s exponent = %d (%v), want %d", code, got, err, want)
		}
	}
	for _, bad := range []string{"usd", "US", "USDX", ""} {
		if _, err := NewMoney(1, bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	a, _ := NewMoney(6900, "USD")
	if s, err := a.Add(Money{100, "USD"}); err != nil || s.AmountMinor != 7000 {
		t.Error("add same currency")
	}
	if _, err := a.Add(Money{100, "AED"}); err == nil {
		t.Error("currencies never convert")
	}
	if a.MulInt(5).AmountMinor != 34500 {
		t.Error("multiply")
	}
}

// §14.1 Events: payload builders enforce the schema rules.
func TestEventPayloadValidation(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 14, 3, 0, time.UTC)
	ref, seats, deferred := "p1", 50, int64(115)
	valid := []interface{ Validate() error }{
		TenantPlanChanged{TenantID: "t", Plan: PlanEnterprise, PreviousPlan: PlanPro, Direction: DirectionUpgrade, EffectiveAt: now, BillingRef: "c1"},
		TenantSeatsChanged{TenantID: "t", LicensedSeats: 51, PreviousLicensedSeats: &seats, Reason: SeatsPurchase, BillingRef: &ref, DeferredAmountMinor: &deferred, DeferralReason: DeferralBelowProviderMinimum},
		TenantSeatsChanged{TenantID: "t", LicensedSeats: 60, Reason: SeatsRenewal},
		TenantPaymentPastDue{TenantID: "t", InvoiceRef: "i1", AmountDue: &Money{345000, "USD"}, PastDueSince: now},
		TenantSubscriptionCancelled{TenantID: "t", CancelledAt: now, Reason: CancelDunningExhausted},
		TenantReactivated{TenantID: "t", Source: ReactivationSource, ReactivatedAt: now, PreviousStatus: StatusPastDue},
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: 10_000_000, Reason: AddonPurchase, BillingRef: "p1", PurchasedAt: &now, PackCode: "tokens_5m", PackQuantity: 2},
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: -5_000_000, Reason: AddonRefund, BillingRef: "r1", PurchaseRef: "0193a8f0-1c2d-7e3f-9a4b-000000000001"},
	}
	for i, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("valid[%d]: %v", i, err)
		}
	}
	invalidCases := []interface{ Validate() error }{
		TenantPlanChanged{TenantID: "t", Plan: "gold", PreviousPlan: PlanPro, EffectiveAt: now, BillingRef: "c1"},
		TenantSeatsChanged{TenantID: "t", LicensedSeats: 0, Reason: SeatsPurchase},
		TenantSeatsChanged{TenantID: "t", LicensedSeats: 5, Reason: "manual"},
		TenantSeatsChanged{TenantID: "t", LicensedSeats: 5, Reason: SeatsPurchase, DeferredAmountMinor: &deferred},                                              // reason missing
		TenantSeatsChanged{TenantID: "t", LicensedSeats: 5, Reason: SeatsRenewal, DeferredAmountMinor: &deferred, DeferralReason: DeferralBelowProviderMinimum}, // not a purchase
		TenantPaymentPastDue{TenantID: "t", PastDueSince: now},
		TenantSubscriptionCancelled{TenantID: "t", CancelledAt: now, Reason: CancelOffboarded},
		TenantReactivated{TenantID: "t", Source: "operator", ReactivatedAt: now},
		TenantQuotaAddOnChanged{TenantID: "t", Resource: "api_requests", Delta: 1, Reason: AddonPurchase, BillingRef: "p1", PurchasedAt: &now},
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: 1, Reason: AddonPurchase, BillingRef: "p1"},                   // no purchased_at
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: 5, Reason: AddonRefund, BillingRef: "r1", PurchaseRef: "p1"},  // positive refund
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: -5, Reason: AddonRefund, BillingRef: "r1", PurchaseRef: "p1"}, // non-UUID purchase_ref
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: -5, Reason: AddonRefund, BillingRef: "r1"},                    // no purchase_ref
		TenantQuotaAddOnChanged{TenantID: "t", Resource: ResourceLLMTokens, Delta: 5, Reason: "operator_correction", BillingRef: "x", PurchasedAt: &now},
	}
	for i, p := range invalidCases {
		if err := p.Validate(); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("invalid[%d] accepted: %+v", i, p)
		}
	}
	if len(ProducedEventTypes) != 6 {
		t.Error("six produced event types; BillingPeriodRolled is not emitted")
	}
}
