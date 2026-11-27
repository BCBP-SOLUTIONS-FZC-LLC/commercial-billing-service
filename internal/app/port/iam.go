package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// SeatUsage is I-11's answer (other fields are ignored, D-4).
type SeatUsage struct {
	ActiveUsers        int `json:"active_users"`
	PendingInvitations int `json:"pending_invitations"`
	LicensedSeats      int `json:"licensed_seats"`
}

// InUse is the seat floor: active users plus pending invitations (BIL-SEAT-3).
func (u SeatUsage) InUse() int { return u.ActiveUsers + u.PendingInvitations }

// PlanChangeCheck is I-18's answer.
type PlanChangeCheck struct {
	Allowed  bool              `json:"allowed"`
	Blockers []PlanChangeBlock `json:"blockers"`
}

// PlanChangeBlock is one I-18 blocker.
type PlanChangeBlock struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// RealmView is RP-11's answer (operator view only).
type RealmView struct {
	Strategy      string `json:"strategy"`
	Status        string `json:"status"`
	KeycloakShard string `json:"keycloak_shard"`
	RealmName     string `json:"realm_name"`
	RecordVersion int64  `json:"record_version"`
	IDPCount      int    `json:"idp_count"`
}

// ConvertRequest is RP-3's body.
type ConvertRequest struct {
	Plan                  domain.PlanCode `json:"plan"`
	BillingSubscriptionID string          `json:"billing_subscription_id"`
}

// CoreClient calls Org & Membership as commercial-system (§18.2). Failures
// are DomainErrors: 503 IAM_UNAVAILABLE (transport, 5xx, timeout; fail
// closed) and 409 TENANT_NOT_KNOWN_TO_IAM (404).
type CoreClient interface {
	SeatUsage(ctx context.Context, tenantID string) (SeatUsage, error)                                     // I-11
	PlanChangeCheck(ctx context.Context, tenantID string, target domain.PlanCode) (PlanChangeCheck, error) // I-18
}

// RealmProvisionerClient calls RP as commercial-system (§18.1). RP-3 and
// RP-4 return the normalised answer (status 0 for transport errors) for
// the saga to classify; they never retry themselves.
type RealmProvisionerClient interface {
	Convert(ctx context.Context, tenantID string, r ConvertRequest, idemKey string) domain.RP3Answer // RP-3
	RevertConversion(ctx context.Context, tenantID, reason, idemKey string) domain.RP3Answer         // RP-4
	Realm(ctx context.Context, tenantID string) (RealmView, error)                                   // RP-11
}

// TokenQuota is the part of the effective entitlement set Billing reads
// (§18.4, BL-OQ-3).
type TokenQuota struct {
	// MonthlyLimit is limits.llm_token_quota_monthly.value; nil = unlimited.
	MonthlyLimit *int64
	// Eligibility is addon_eligibility.llm_tokens when Entitlements serves
	// it (W-1): "eligible" or a reason (trial, not_active, unlimited_quota).
	// Empty until then; Billing applies the interim finite-quota rule.
	Eligibility string
}

// EntitlementsReader reads GET /api/v1/internal/tenants/:id/entitlements.
// Failure is 503 ENTITLEMENTS_UNAVAILABLE (fail closed, BIL-ADD-1).
type EntitlementsReader interface {
	TokenQuota(ctx context.Context, tenantID string) (TokenQuota, error)
}
