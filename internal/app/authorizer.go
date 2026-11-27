package app

import (
	"slices"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// Caller is the identity of an API request (LLD §5.1): gateway- or
// mesh-injected headers only, never the body (BIL-API-1).
type Caller struct {
	UserID    string   // x-user-id (operator sub on operator routes)
	TenantID  string   // x-tenant-id
	Roles     []string // x-tenant-roles
	Principal string   // mTLS SPIFFE principal (internal route)
	ClientIP  string
	UserAgent string
	// IdempotencyKey is the request's Idempotency-Key, stored on purchase
	// and change rows (uq_*_key, §9.2).
	IdempotencyKey string
}

// PlatformTenantID is the tenant id cross-tenant operator work is recorded
// under (CD-21; the platform_tenant of pending_audit_entries' RLS policy).
const PlatformTenantID = "00000000-0000-0000-0000-0000000000b1"

// RejectSystemCaller is BIL-AUTH-4: commercial-system is Billing's outbound
// identity only; inbound it is refused on every route.
func RejectSystemCaller(c Caller) error {
	if c.UserID == domain.CommercialSystemPrincipal || slices.Contains(c.Roles, domain.RoleCommercialSystem) {
		return domain.NewError(domain.ErrInsufficientRole, "commercial-system is not accepted inbound")
	}
	return nil
}

// RequireTenantMatch is BIL-API-2: x-tenant-id must equal the path tenant.
func RequireTenantMatch(c Caller, pathTenantID string) error {
	if c.TenantID == "" || c.TenantID != pathTenantID {
		return domain.NewError(domain.ErrInsufficientRole, "x-tenant-id does not match the tenant in the path")
	}
	return nil
}

// RequirePermission is BIL-AUTH-1 / BIL-AUTH-7: the route's one permission,
// mapped from the caller's tenant roles. platform_operator grants nothing
// here (BIL-AUTH-2).
func RequirePermission(c Caller, p domain.Permission) error {
	if !domain.HasPermission(c.Roles, p) {
		return domain.NewError(domain.ErrInsufficientPermission, "the caller's roles do not grant the permission this route requires").
			With("required_permission", string(p))
	}
	return nil
}

// RequirePlatformOperator is BIL-AUTH-2 / BIL-AUTH-6 for operator routes:
// platform_operator is required, and every operator route is refused while
// the operator path is not live, except in dev and test.
func RequirePlatformOperator(c Caller, operatorPathLive, devLike bool) error {
	if !operatorPathLive && !devLike {
		return domain.NewError(domain.ErrInsufficientRole, "the operator path is not enabled in this environment")
	}
	if !slices.Contains(c.Roles, domain.RolePlatformOperator) {
		return domain.NewError(domain.ErrInsufficientRole, "platform_operator role required")
	}
	return nil
}

// RequirePCIReviewer is BIL-PROV-4's role check on B-O17, on top of
// RequirePlatformOperator.
func RequirePCIReviewer(c Caller) error {
	if !slices.Contains(c.Roles, domain.RolePCIOwnerReviewer) {
		return domain.NewError(domain.ErrInsufficientRole, "pci_owner_reviewer role required")
	}
	return nil
}

// RequireBFFCaller is BIL-AUTH-3: B-I1 admits only the Signup BFF's mTLS
// principal.
func RequireBFFCaller(c Caller, allowed string) error {
	if allowed == "" || c.Principal != allowed {
		return domain.NewError(domain.ErrInsufficientRole, "caller principal is not allowed on this route")
	}
	return nil
}

// CallerPermissions lists the permissions the caller holds, in the order
// of domain.AllPermissions (SubscriptionView.caller_permissions).
func CallerPermissions(c Caller) []string {
	held := domain.PermissionsFor(c.Roles)
	out := make([]string, 0, len(held))
	for _, p := range domain.AllPermissions {
		if held[p] {
			out = append(out, string(p))
		}
	}
	return out
}
