package domain

import (
	"errors"
	"slices"
	"strings"
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
		// A live subscription with IAM lifecycle trial (paid, TenantConverted
		// not yet received) and no subscription with an active lifecycle are
		// neither paid-active nor trial tenants.
		{mod(func(i *AddonEligibilityInput) { i.IAMLifecycle = LifecycleTrial }), IneligibleNotActive},
		{mod(func(i *AddonEligibilityInput) { i.SubscriptionStatus = "" }), IneligibleNotActive},
		{mod(func(i *AddonEligibilityInput) { i.EntitlementsReason = "eligible" }), ""},
		{mod(func(i *AddonEligibilityInput) { i.EntitlementsReason = "tenant_suspended" }), "tenant_suspended"},
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

	// BIL-ADD-1 (b), rev 0.75: a trial tenant (no live subscription,
	// lifecycle trial) is eligible while Entitlements reports llm_tokens
	// eligible, the quota is finite and the caller is the owner.
	trial := AddonEligibilityInput{IAMLifecycle: LifecycleTrial, EntitlementsKnown: true, TokenQuotaFinite: true, EntitlementsReason: "eligible", Owner: true}
	tmod := func(f func(*AddonEligibilityInput)) AddonEligibilityInput { c := trial; f(&c); return c }
	for _, tc := range []struct {
		in   AddonEligibilityInput
		want IneligibleReason
	}{
		{trial, ""},
		{tmod(func(i *AddonEligibilityInput) { i.EntitlementsKnown = false }), IneligibleNotAvailable},
		{tmod(func(i *AddonEligibilityInput) { i.EntitlementsReason = "" }), IneligibleTrialInactive},
		{tmod(func(i *AddonEligibilityInput) { i.EntitlementsReason = "trial" }), IneligibleTrialInactive},
		{tmod(func(i *AddonEligibilityInput) { i.EntitlementsReason = "trial_inactive"; i.Owner = false }), IneligibleTrialInactive},
		{tmod(func(i *AddonEligibilityInput) { i.EntitlementsReason = "unlimited_quota" }), IneligibleUnlimitedQuota},
		{tmod(func(i *AddonEligibilityInput) { i.TokenQuotaFinite = false }), IneligibleUnlimitedQuota},
		{tmod(func(i *AddonEligibilityInput) { i.Owner = false }), IneligibleOwnerOnly},
	} {
		eligible, reason := AddonEligibility(tc.in)
		if eligible != (tc.want == "") || reason != tc.want {
			t.Errorf("trial %+v → %v %q, want %q", tc.in, eligible, reason, tc.want)
		}
	}
	if !IsTrialTenant("", LifecycleTrial) || IsTrialTenant(StatusActive, LifecycleTrial) || IsTrialTenant("", LifecycleActive) {
		t.Error("trial tenant: no live subscription and lifecycle trial")
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

// BIL-ADD-5 (rev 0.75): the default trial cap is 1 pack / 5000; an
// override stays within 10 packs / 200000; whichever limit is reached first
// refuses a purchase, counting what is already used.
func TestBIL_ADD_5_TrialCap(t *testing.T) {
	d := DefaultTrialAddonCap("t")
	if d.MaxPacks != 1 || d.MaxAmountMinor != 5000 || d.TenantID != "t" || d.RecordVersion != 0 {
		t.Fatalf("default %+v", d)
	}
	if !WithinTrialCap(d, TrialAddonUsage{}, 1, 4900) || WithinTrialCap(d, TrialAddonUsage{}, 2, 200) ||
		WithinTrialCap(d, TrialAddonUsage{}, 1, 5001) || WithinTrialCap(d, TrialAddonUsage{Packs: 1, AmountMinor: 4900}, 1, 1) {
		t.Error("default cap: packs and amount, whichever first")
	}
	if !WithinTrialCap(d, TrialAddonUsage{}, 1, 5000) {
		t.Error("the cap boundary is allowed")
	}
	raised := TrialAddonCap{MaxPacks: 5, MaxAmountMinor: 50000}
	if !WithinTrialCap(raised, TrialAddonUsage{Packs: 4, AmountMinor: 45000}, 1, 5000) || WithinTrialCap(raised, TrialAddonUsage{Packs: 4, AmountMinor: 45100}, 1, 5000) {
		t.Error("raised cap by amount")
	}
	for _, c := range []struct {
		packs  int
		amount int64
		ok     bool
	}{{1, 1, true}, {10, 200000, true}, {0, 5000, false}, {11, 5000, false}, {5, 0, false}, {5, 200001, false}} {
		if ValidTrialCapLimits(c.packs, c.amount) != c.ok {
			t.Errorf("limits %d/%d valid=%v", c.packs, c.amount, !c.ok)
		}
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

// BIL-SUB-1: suspended, trial_expired and offboarded are IAM's. Billing
// produces exactly its six event types, none of which sets them; no Billing
// subscription status is one of them, so no status-bearing payload can carry
// one; and offboarding ends a subscription without a cancellation event.
func TestBIL_SUB_1_NoEventSetsAnIAMOwnedStatus(t *testing.T) {
	want := []EventType{EventTenantPlanChanged, EventTenantSeatsChanged, EventTenantPaymentPastDue,
		EventTenantSubscriptionCancelled, EventTenantReactivated, EventTenantQuotaAddOnChanged}
	if !slices.Equal(ProducedEventTypes, want) {
		t.Fatalf("produced event types %v, want %v", ProducedEventTypes, want)
	}
	iamOwned := []string{"suspended", "trial_expired", "offboarded"}
	for _, et := range ProducedEventTypes {
		name := strings.ToLower(string(et))
		for _, s := range []string{"suspend", "trialexpired", "offboard"} {
			if strings.Contains(name, s) {
				t.Errorf("Billing produces %s", et)
			}
		}
	}
	for _, st := range AllSubscriptionStatuses {
		if slices.Contains(iamOwned, string(st)) {
			t.Errorf("Billing subscription status %q is IAM's", st)
		}
	}
	now := time.Date(2026, 11, 20, 9, 14, 3, 0, time.UTC)
	if err := (TenantSubscriptionCancelled{TenantID: "t", CancelledAt: now, Reason: CancelOffboarded}).Validate(); !errors.Is(err, ErrInvalidPayload) {
		t.Error("a cancellation event for offboarding was accepted")
	}
	for _, s := range iamOwned {
		p := TenantReactivated{TenantID: "t", Source: ReactivationSource, ReactivatedAt: now, PreviousStatus: SubscriptionStatus(s)}
		if err := p.Validate(); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("TenantReactivated with previous_status %q accepted", s)
		}
	}
}
