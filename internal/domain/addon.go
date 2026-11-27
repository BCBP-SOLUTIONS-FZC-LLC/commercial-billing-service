package domain

// Token add-on eligibility (LLD §8.7, BIL-ADD-1). Billing reads the effective
// llm_token_quota_monthly (or, once Entitlements ships it, per-resource
// eligibility for llm_tokens; BL-OQ-3) and applies its own rules. A generic
// addon_eligible flag is never used.

// IneligibleReason is B-P19's ineligible_reason and B-P20's 409 extension.
type IneligibleReason string

// Reasons (§5.4 B-P19).
const (
	IneligibleTrial            IneligibleReason = "trial"
	IneligibleNotActive        IneligibleReason = "not_active"
	IneligibleUnlimitedQuota   IneligibleReason = "unlimited_quota"
	IneligibleCollectionPaused IneligibleReason = "collection_paused"
	IneligibleNotAvailable     IneligibleReason = "not_available"
)

// AddonEligibilityInput is what the decision needs.
type AddonEligibilityInput struct {
	SubscriptionStatus SubscriptionStatus // "" when the tenant has no subscription
	CollectionPaused   bool
	IAMLifecycle       IAMLifecycle
	// EntitlementsKnown is false when Entitlements could not be read: the
	// pack list shows not_available and a purchase fails closed with 503.
	EntitlementsKnown bool
	// TokenQuotaFinite is the effective llm_token_quota_monthly != null.
	TokenQuotaFinite bool
}

// AddonEligibility returns whether a tenant may buy a token add-on and, if
// not, the first failing reason in a fixed order: trial, not_active,
// collection_paused, not_available, unlimited_quota.
func AddonEligibility(in AddonEligibilityInput) (bool, IneligibleReason) {
	switch {
	case in.IAMLifecycle == LifecycleTrial || in.SubscriptionStatus == "":
		// ENT-D6: a trial tenant is prompted to convert, never sold an add-on.
		return false, IneligibleTrial
	case in.SubscriptionStatus != StatusActive || in.IAMLifecycle != LifecycleActive:
		return false, IneligibleNotActive
	case in.CollectionPaused:
		return false, IneligibleCollectionPaused
	case !in.EntitlementsKnown:
		return false, IneligibleNotAvailable
	case !in.TokenQuotaFinite:
		return false, IneligibleUnlimitedQuota
	}
	return true, ""
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
