package domain

import "time"

// Token add-on eligibility (LLD §8.7, BIL-ADD-1). Billing reads the effective
// llm_token_quota_monthly and addon_eligibility.llm_tokens (BL-OQ-3,
// BL-OQ-31) and applies its own rules. A generic addon_eligible flag is never
// used.

// IneligibleReason is B-P19's ineligible_reason and B-P20's 409 extension.
type IneligibleReason string

// Reasons (§5.4.1 B-P19, B-P20; B-O18's not_trial). "trial" is no longer
// returned (rev 0.75): a trial tenant is either eligible, trial_inactive or
// owner_only.
const (
	IneligibleTrialInactive    IneligibleReason = "trial_inactive"
	IneligibleOwnerOnly        IneligibleReason = "owner_only"
	IneligibleNotActive        IneligibleReason = "not_active"
	IneligibleUnlimitedQuota   IneligibleReason = "unlimited_quota"
	IneligibleCollectionPaused IneligibleReason = "collection_paused"
	IneligibleNotAvailable     IneligibleReason = "not_available"
	IneligibleNotTrial         IneligibleReason = "not_trial"
)

// entitlementsEligible is addon_eligibility.llm_tokens when Entitlements
// allows the purchase.
const entitlementsEligible = "eligible"

// legacyTrialReason is the reason Entitlements gave a trial tenant before
// BL-OQ-31 (a). It no longer reaches the caller: it means the trial is not
// reported active.
const legacyTrialReason = "trial"

// AddonEligibilityInput is what the decision needs.
type AddonEligibilityInput struct {
	SubscriptionStatus SubscriptionStatus // "" when the tenant has no live subscription
	CollectionPaused   bool
	IAMLifecycle       IAMLifecycle // LifecycleTrial when the tenant has no billing account yet
	// EntitlementsKnown is false when Entitlements could not be read: the
	// pack list shows not_available and a purchase fails closed with 503.
	EntitlementsKnown bool
	// TokenQuotaFinite is the effective llm_token_quota_monthly != null.
	TokenQuotaFinite bool
	// EntitlementsReason is addon_eligibility.llm_tokens: "eligible", a
	// reason (trial_inactive, not_active, unlimited_quota, …), or "" when
	// Entitlements does not serve it.
	EntitlementsReason string
	// Owner is a caller holding commercial.payment_method.manage (trial
	// tenants only, P-5c: the first card on file is the owner's).
	Owner bool
}

// IsTrialTenant reports whether a tenant is on trial for add-ons
// (BIL-ADD-1 (b), B-O18): no live subscription and IAM lifecycle trial (a
// tenant without a billing account is passed as trial).
func IsTrialTenant(subscriptionStatus SubscriptionStatus, lifecycle IAMLifecycle) bool {
	return subscriptionStatus == "" && lifecycle == LifecycleTrial
}

// AddonEligibility returns whether a tenant may buy a token add-on and, if
// not, the first failing reason. Paid tenant (BIL-ADD-1 (a)): not_active,
// collection_paused, not_available, unlimited_quota, then Entitlements'
// reason. Trial tenant (BIL-ADD-1 (b)): not_available, trial_inactive (or
// Entitlements' own reason) unless Entitlements reports llm_tokens eligible,
// unlimited_quota, owner_only. The currency and the BIL-ADD-5 cap are
// checked by the purchase.
func AddonEligibility(in AddonEligibilityInput) (bool, IneligibleReason) {
	reason := in.EntitlementsReason
	if reason == legacyTrialReason {
		reason = string(IneligibleTrialInactive)
	}
	if IsTrialTenant(in.SubscriptionStatus, in.IAMLifecycle) {
		switch {
		case !in.EntitlementsKnown:
			return false, IneligibleNotAvailable
		case reason == "":
			// Entitlements must report the trial active (P-5f): fail closed.
			return false, IneligibleTrialInactive
		case reason != entitlementsEligible:
			return false, IneligibleReason(reason)
		case !in.TokenQuotaFinite:
			return false, IneligibleUnlimitedQuota
		case !in.Owner:
			return false, IneligibleOwnerOnly
		}
		return true, ""
	}
	switch {
	case in.SubscriptionStatus != StatusActive || in.IAMLifecycle != LifecycleActive:
		return false, IneligibleNotActive
	case in.CollectionPaused:
		return false, IneligibleCollectionPaused
	case !in.EntitlementsKnown:
		return false, IneligibleNotAvailable
	case !in.TokenQuotaFinite:
		return false, IneligibleUnlimitedQuota
	case reason != "" && reason != entitlementsEligible:
		// A W-1 reason from Entitlements wins over the finite-quota rule.
		return false, IneligibleReason(reason)
	}
	return true, ""
}

// Trial add-on cap (BIL-ADD-5, §4.2.25): the default when no operator
// override exists, and the ceiling an override (B-O18) never exceeds.
// Amounts are DEFAULT_CURRENCY (USD) minor units.
const (
	TrialCapDefaultPacks       = 1
	TrialCapDefaultAmountMinor = int64(5000)
	TrialCapCeilingPacks       = 10
	TrialCapCeilingAmountMinor = int64(200000)
)

// TrialAddonCap is a trial_addon_caps row (§4.2.25), or the default cap
// (RecordVersion 0) when the tenant has none.
type TrialAddonCap struct {
	TenantID       string
	MaxPacks       int
	MaxAmountMinor int64
	Reason         string
	SetBy          string
	SetAt          time.Time
	RecordVersion  int64
}

// DefaultTrialAddonCap is the cap in force without an override.
func DefaultTrialAddonCap(tenantID string) TrialAddonCap {
	return TrialAddonCap{TenantID: tenantID, MaxPacks: TrialCapDefaultPacks, MaxAmountMinor: TrialCapDefaultAmountMinor}
}

// ValidTrialCapLimits reports whether an override stays within 1 to 10
// packs and 1 to 200000 minor units (BIL-ADD-5 ceiling, chk on §4.2.25).
func ValidTrialCapLimits(maxPacks int, maxAmountMinor int64) bool {
	return maxPacks >= 1 && maxPacks <= TrialCapCeilingPacks && maxAmountMinor >= 1 && maxAmountMinor <= TrialCapCeilingAmountMinor
}

// TrialAddonUsage is what a trial tenant's purchases that count toward the
// cap add up to: trial_purchase rows not failed, expired or cancelled.
type TrialAddonUsage struct {
	Packs       int
	AmountMinor int64
}

// WithinTrialCap reports whether a purchase of packs for amountMinor keeps
// the tenant within the cap together with what it already used; whichever
// limit is reached first refuses it (BIL-ADD-5).
func WithinTrialCap(c TrialAddonCap, used TrialAddonUsage, packs int, amountMinor int64) bool {
	return used.Packs+packs <= c.MaxPacks && used.AmountMinor+amountMinor <= c.MaxAmountMinor
}

// CapRemaining returns the tokens still purchasable this UTC month under a
// pack's monthly cap; ok is false when there is no cap (§8.7).
func CapRemaining(monthlyCapTokens *int64, confirmedThisMonth int64) (remaining int64, ok bool) {
	if monthlyCapTokens == nil {
		return 0, false
	}
	return max(*monthlyCapTokens-confirmedThisMonth, 0), true
}

// WithinCap reports whether buying tokens keeps the tenant within the cap:
// confirmed tokens since the 1st 00:00 UTC + tokens ≤ cap (409 ADDON_CAP_REACHED otherwise).
func WithinCap(monthlyCapTokens *int64, confirmedThisMonth, tokens int64) bool {
	if monthlyCapTokens == nil {
		return true
	}
	return confirmedThisMonth+tokens <= *monthlyCapTokens
}

// AddonRefundCategory is the operator refund category (§8.11).
type AddonRefundCategory string

// Refund categories allowed on an add-on invoice (BIL-ADD-3).
const (
	RefundBillingError    AddonRefundCategory = "billing_error"
	RefundDuplicateCharge AddonRefundCategory = "duplicate_charge"
	RefundFraud           AddonRefundCategory = "fraud"
)

// AddonRefundAllowed reports whether an add-on invoice may be refunded in
// this category. There is no customer refund path (BIL-ADD-3).
func AddonRefundAllowed(c AddonRefundCategory) bool {
	return c == RefundBillingError || c == RefundDuplicateCharge || c == RefundFraud
}
