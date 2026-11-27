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

AuthZ Enrichment, Entitlements and Notification do not yet publish consumed
schemas for Billing's events; add them here when they do.
