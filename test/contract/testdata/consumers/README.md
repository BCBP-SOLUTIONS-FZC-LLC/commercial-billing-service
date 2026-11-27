# Consumer schemas of billing.events

Copies of the consumed-schema extracts that subscribers of `billing-events`
embed and validate against. `billing_events_test.go` checks that every payload
Billing produces passes them, so a consumer that tightens its schema is caught
here before it breaks delivery. Refresh them when a consumer's schema changes,
and record the source commit below.

| Directory | Source | Commit |
|---|---|---|
| `iam-org-membership/` (Core, `billing-orgm-q`) | `internal/adapter/outbound/eventbus/schemas/tenant_*.json` | `5a643fb` (main) |
| `iam-audit-log/` (`billing-audit-q`) | `internal/eventschema/billing_audit_*.json` | `0e12f2c` (main) |
| `iam-realm-provisioner/` (`billing-realm-q`) | `internal/eventschema/billing_tenant_reactivated.json` | `9e8fcab` (main) |
| `commercial-entitlements/` (`billing-entitlements-q`) | `commercial-entitlements-service` `internal/adapter/outbound/eventbus/schemas/inbound_tenant_quota_add_on_changed.json`; consumer structs mirrored in `entitlements_addon_test.go` from `internal/core/domain/event.go` and `entitlement.go` | `dc9b654` (main) |

Entitlements publishes a consumed schema for `TenantQuotaAddOnChanged` only;
its consumer of Billing's other events (`TenantPlanChanged`,
`TenantSubscriptionCancelled`, `TenantReactivated`) has none yet. AuthZ
Enrichment and Notification publish none. Add them here when they do.

`entitlements_addon_test.go` also checks schema compatibility both ways and
that every produced add-on payload decodes into Entitlements' inbound structs.
When Entitlements changes `event.go` or `billingResourceKeys`, update the
mirrors there too. The `Contract (live consumers)` workflow runs the suite daily
against the schema on Entitlements' `main` (`CONTRACT_CONSUMERS_DIR`).
