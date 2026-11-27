package domain

// Billing authorizes by permission, mapped from the IAM roles in
// x-tenant-roles (LLD §5.2). The mapping below is the constant table Finance
// approved (BIZ-6 / BL-OQ-13, 2026-11-23). A Finance policy change is a pull
// request to this file.

// Permission is a commercial permission.
type Permission string

// Permissions (§5.2).
const (
	PermSubscriptionManage     Permission = "commercial.subscription.manage"
	PermSubscriptionCancel     Permission = "commercial.subscription.cancel"
	PermBillingView            Permission = "commercial.billing.view"
	PermBillingDownloadInvoice Permission = "commercial.billing.download_invoice"
	PermSubscriptionChangePlan Permission = "commercial.subscription.change_plan"
	PermPaymentMethodManage    Permission = "commercial.payment_method.manage"
)

// AllPermissions lists the six permissions.
var AllPermissions = []Permission{
	PermSubscriptionManage, PermSubscriptionCancel, PermBillingView,
	PermBillingDownloadInvoice, PermSubscriptionChangePlan, PermPaymentMethodManage,
}

// IAM roles Billing reads (x-tenant-roles).
const (
	RoleTenantOwner      = "tenant_owner"
	RoleTenantAdmin      = "tenant_admin"
	RoleTenderAdmin      = "tender_admin"
	RoleMember           = "member"
	RolePlatformOperator = "platform_operator"
	RolePCIOwnerReviewer = "pci_owner_reviewer"
	RoleCommercialSystem = "commercial-system"
)

// CommercialSystemPrincipal is Billing's outbound identity; inbound it is
// rejected on every route (BIL-AUTH-4).
const CommercialSystemPrincipal = "00000000-0000-0000-0000-0000000000c1"

// rolePermissions is the Finance-approved mapping. Tender roles get none;
// platform_operator grants nothing on tenant routes (BIL-AUTH-2).
var rolePermissions = map[string][]Permission{
	RoleTenantOwner: AllPermissions,
	RoleTenantAdmin: {PermSubscriptionManage, PermBillingView, PermBillingDownloadInvoice},
}

// PermissionsFor returns the union of permissions the roles grant.
func PermissionsFor(roles []string) map[Permission]bool {
	out := map[Permission]bool{}
	for _, r := range roles {
		for _, p := range rolePermissions[r] {
			out[p] = true
		}
	}
	return out
}

// HasPermission reports whether the roles grant p. Each permission is
// checked on its own: none implies another (BIL-AUTH-7).
func HasPermission(roles []string, p Permission) bool {
	return PermissionsFor(roles)[p]
}

// TenantRoutePermission is the one permission each tenant route requires
// (§5.3, BIL-AUTH-7).
var TenantRoutePermission = map[string]Permission{
	"B-P1":  PermBillingView,
	"B-P2":  PermSubscriptionChangePlan,
	"B-P3":  PermSubscriptionChangePlan,
	"B-P4":  PermSubscriptionChangePlan,
	"B-P5":  PermSubscriptionChangePlan,
	"B-P6":  PermSubscriptionManage,
	"B-P7":  PermSubscriptionManage,
	"B-P8":  PermBillingView,
	"B-P9":  PermBillingView,
	"B-P10": PermSubscriptionManage,
	"B-P11": PermSubscriptionManage,
	"B-P12": PermSubscriptionCancel,
	"B-P13": PermSubscriptionCancel,
	"B-P14": PermSubscriptionChangePlan,
	"B-P15": PermBillingView,
	"B-P16": PermPaymentMethodManage,
	"B-P17": PermBillingDownloadInvoice,
	"B-P18": PermBillingDownloadInvoice,
	"B-P19": PermBillingView,
	"B-P20": PermSubscriptionManage,
	"B-P21": PermBillingView,
	"B-P22": PermBillingView,
	"B-P23": PermSubscriptionManage,
	"B-P24": PermBillingView,
	"B-P25": PermBillingView,
	"B-P26": PermPaymentMethodManage,
}

// NewContractualCommitmentRoutes are the routes Finance's overlap rule makes
// owner-only through subscription.change_plan (§5.2, BIL-AUTH-7).
var NewContractualCommitmentRoutes = []string{"B-P2", "B-P4", "B-P5", "B-P14"}
