# IAM Reply — Billing Actions A-1 to A-8

| Field | Value |
|---|---|
| Re | `billing-iam-actions.md` (Billing LLD rev 0.11) |
| From | IAM (AuthZ Enrichment, Audit Log, Org & Membership, Realm Provisioner) |
| To | Commercial Team (Billing) |
| Date | 2026-11-19 |
| Status | **Merged 2026-11-19:** [iam-authz-enrichment#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43) (A-1, `f4af854`), [iam-audit-log#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24) (A-5, `0e12f2c`). Follow-up [iam-realm-provisioner#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60) (A-2 OP-G1, `9e8fcab`) also merged |
| Deployment state | All services dev-only; nothing below is a production change |

## Summary

| # | Action | Result | Billing item |
|---|---|---|---|
| A-1 | Extend OP-G4 to Billing routes B-O6 to B-O15 | **Done (AuthZ half).** Tests added for all 15 Billing operator routes, unit and e2e, passing. No AuthZ code change needed. Gateway half waits on OP-G2/OP-G3 | BL-OQ-9: IAM side complete; closes when Billing's `HTTPRoute` lists B-O1 to B-O15 |
| A-2 | OP-G1 platform realm, OP-G3 issuer, OP-G6 date | **Not done — blocked.** OP-G3 needs the issuer URL from platform-edge's OP-G2. OP-G1 code exists but is off everywhere. OP-G6 date not set | BL-OQ-14 open |
| A-3 | Joint OP-G4 run per environment | **Not started** — needs A-2 | BL-OQ-14 open |
| A-4 | `billing-audit-q` receives `TenantQuotaAddOnChanged` | **Confirmed, no change.** Catch-all subscription, no filter policy (AL-EVT-2) | BL-OQ-23 (a), SRE-8: closable |
| A-5 | Entry type `billing.token_addon.refunded` | **Done.** New type; classified by `data.reason`; vocabulary 101 → 102; tests passing | BL-OQ-23 (b): closes on merge and release |
| A-6 | No subscription to Entitlements add-on events | **Confirmed, with one caveat** (see A-6) | BL-OQ-23 (c): needs Entitlements to confirm C-4 |
| A-7 | Core `000004` and `parked-events-replay` before direct signup | **Confirmed.** Both exist in Core's repo and chart; ordering rule accepted | SRE-3 |
| A-8 | Billing admission on I-11, I-18, RP-3, RP-4, RP-11 | **Confirmed, no change.** Admitted in dev, staging and prod charts | CD-19 |

---

## A-1 — OP-G4 coverage for Billing's operator routes

**Finding confirmed:** AuthZ Enrichment has no per-route list. `domain.OperatorRoutePrefix = "/api/v1/operator/"` admits every operator path with `platform_operator`. Routes under `/tenants/{id}/` get that tenant as `x-tenant-id`; every other operator route gets the nil UUID.

**Changes (`iam-authz-enrichment`, branch `test/billing-operator-routes`):**

| File | Change |
|---|---|
| `internal/usecase/operator_test.go` | Table of Billing B-O1..B-O15; `TestOperator_BillingRoutes_AllowedWithRole`, `TestOperator_BillingRoutes_DeniedWithoutRole`, `TestOperator_BillingTenantRoutes_MalformedTenantDenied` |
| `test/e2e/operator_test.go` | `TestOperator_BillingRoutes`: one subtest per route through the real binary over gRPC, allow and deny |
| `docs/lld/iam-lld-authz-enrichment.md` | v1.50; §8.9.1 OP-G4 now lists Billing B-O1..B-O15 |
| `CHANGELOG.md` | Unreleased entry |

**Results:**
- Unit (`go test ./internal/... ./cmd/...`): pass.
- e2e (`go test -tags=e2e ./test/e2e/... -run TestOperator`, real binary, local Valkey and SQS): pass.

| Check per route | Result |
|---|---|
| `platform_operator` → Allow; `x-tenant-roles: platform_operator`; `x-user-id` = operator `sub` | 15/15 |
| `x-tenant-id` = path tenant (B-O1–B-O4, B-O7–B-O10) | 8/8 |
| `x-tenant-id` = nil UUID (B-O5, B-O6, B-O11–B-O15) | 7/7 |
| No `platform_operator` → `DENY_ROLE_INSUFFICIENT` | 15/15 |
| Non-UUID path tenant → `DENY_MALFORMED_IDENTITY` (HTTP 401) | 8/8 |

**Still open for OP-G4:** the same checks through the real gateway, with a real platform-realm token. That needs OP-G2 and OP-G3 (A-2).

## A-2 — Turning the operator path on

| Step | State | Owner | Next |
|---|---|---|---|
| OP-G1 platform realm | Code done (`EnsurePlatformRealm`, run at Realm Provisioner startup when `PLATFORM_REALM_NAME` is set). The variable is commented out in every environment | IAM | Set `PLATFORM_REALM_NAME` in dev once OP-G2 is scheduled. Not enabled now: on its own it unlocks nothing, and it would create a realm in the shared dev Keycloak |
| OP-G2 gateway `jwt_authn` provider | Not started | **Platform edge** | Sole external blocker |
| OP-G3 `OPERATOR_REALM_ISSUER` | `""` in AuthZ values (path off) | IAM | Needs the issuer URL from OP-G1 and the provider from OP-G2 |
| OP-G6 target date | **Not set** | IAM lead with platform edge | IAM cannot commit to a date until platform edge dates OP-G2 |

Housekeeping found: Realm Provisioner's startup log says "CD-52 OP-G3 pending" when it skips the platform realm. In the AuthZ LLD numbering that step is OP-G1. A wording fix, not done in this change.

## A-3 — Joint OP-G4 run

Not started; depends on A-2. The tests added in A-1 are the per-route expectations for the gateway run.

## A-4 — `billing-audit-q` receives Billing's `TenantQuotaAddOnChanged`

**Confirmed, no change.** `scripts/init-floci.sh` subscribes `billing-audit-q` to `billing-events` with no filter, and Audit LLD **AL-EVT-2** makes every Audit subscription catch-all. Billing's `TenantQuotaAddOnChanged` already arrives and maps to `billing.token_addon.purchased` (IAM-7, since Audit LLD rev 0.32). The "add to the filter" step in SRE-8 does not apply to Audit queues. **SRE-8 can be closed.**

## A-5 — `billing.token_addon.refunded`

**Changes (`iam-audit-log`, branch `feat/token-addon-refunded`):**

| File | Change |
|---|---|
| `internal/core/domain/taxonomy.go` | New entry type `billing.token_addon.refunded` (`security_3y`, bus-sourced, not direct-write). New `ClassifyBusPayload`: for `billing.events` `TenantQuotaAddOnChanged`, `data.reason == "refund"` → `billing.token_addon.refunded`; `purchase`, missing or any other reason → `billing.token_addon.purchased` (never dropped) |
| `internal/core/domain/bus.go` | `BuildBusEntry` decodes the payload first, then classifies with `ClassifyBusPayload`. `action` becomes `refunded`; `source_event_type` stays `TenantQuotaAddOnChanged` (AL-INV-5); `purchase_ref` is kept in metadata |
| `internal/core/domain/taxonomy_test.go`, `bus_test.go` | Refinement table test; end-to-end `BuildBusEntry` refund test; vocabulary 101 → 102 |
| `api/taxonomy.json` | Regenerated (`make taxonomy-json`); platform-audit's `audittypecheck` reads it |
| `api/asyncapi.yaml` | Message summary names both entry types |
| `docs/lld/iam-lld-audit-log-service.md` | Rev 0.37: §7.1 row split by `reason`, §25 vocabulary |
| `CHANGELOG.md` | Unreleased entry |

**Results:** `go vet ./...` and `make lint` clean; unit suites (`./internal/...`, `./api/...`, `./test/unit/...`, `./cmd/...`) pass, including the AsyncAPI and taxonomy-drift tests.

**Schema:** Audit's consumed schema for this message is open (`additionalProperties: true`, no required fields), so a negative `delta`, `reason: "refund"` and `purchase_ref` are already accepted. No schema change was needed.

**Until it ships**, Billing's refund events are stored as `billing.token_addon.purchased` (the existing mapping ignores `reason`): mislabelled, not lost; the payload is in metadata.

**Release:** this ships in the next Audit Log release (suggested `v0.3.0`, MINOR: one taxonomy row). platform-audit should then move its pinned Audit Log release and regenerate its taxonomy copy.

## A-6 — No subscription to Entitlements add-on events

**Confirmed:** Audit adds no new subscription. Entitlements' corrections and expiry already arrive by AL-5 (`billing.token_addon.granted`, `.adjusted`, `.revoked`, `.expired`).

**Caveat Billing and Entitlements must know.** Audit already has `entitlement-audit-q`, subscribed catch-all to `entitlement-events` (AL-EVT-2). Audit cannot skip one type on that topic. If Entitlements publishes **any** add-on balance event on `entitlement-events`, whether today's `tenant_quota_add_on_changed` or a renamed `TenantAddOnBalanceChanged` (corrections C-4/C-5), Audit stores each as `entitlement.unknown` and alarms (AL-EVT-4), duplicating the AL-5 entries.

Requested decision (Commercial TL with Entitlements), one of:
1. Entitlements does not publish an add-on balance event on `entitlement-events` (AL-5 is its only audit path). **Recommended.**
2. Entitlements publishes it, and Audit maps it to an explicit "skip" or to an entry type of its own. That is a new Audit LLD decision, so tell us.

## A-7 — Core migration ordering (SRE-3)

**Confirmed.** `000004_cd50_pending_tenant_events` is in Core's migrations, and the `parked-events-replay` CronJob (every 5 minutes) is in Core's base chart values, so it ships with every environment's install. Ordering rule accepted: in each environment, Core at or after this release is deployed before Billing turns on `FEATURE_DIRECT_SIGNUP`.

## A-8 — Billing admission on IAM routes (CD-19)

**Confirmed, no change.** Principal `cluster.local/ns/billing/sa/commercial-billing`:

| Route | Service | Method, path | NetworkPolicy | Istio AuthorizationPolicy | App check |
|---|---|---|---|---|---|
| I-11 | Core | `GET …/seat-usage` | dev, staging, prod (`commercialCallers.enabled: true`) | Yes (I-11 and I-18 only) | `RequireInternalCaller` accepts `commercial-system` |
| I-18 | Core | `GET …/plan-change-check` | same | same | same |
| RP-3 | Realm Provisioner | `POST /api/v1/internal/tenants/:id/convert` | dev, staging, prod | staging, prod (dev has no Istio CRDs) | commercial allow-list group |
| RP-4 | Realm Provisioner | `POST …/tenants/:id/revert-conversion` | same | same | same |
| RP-11 | Realm Provisioner | `GET …/tenants/:id/realm` | same | same | same |

Every other Core and Realm Provisioner internal route stays `iam-system` only. Billing's CD-19 smoke test should expect `403` on RP-10 and RP-14.

---

## Changes made (merged)

| Repo | Branch | Files |
|---|---|---|
| `iam-authz-enrichment` ([#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43)) | `test/billing-operator-routes` (`d94149d`) | `internal/usecase/operator_test.go`, `test/e2e/operator_test.go`, `docs/lld/iam-lld-authz-enrichment.md`, `CHANGELOG.md` |
| `iam-audit-log` ([#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24)) | `feat/token-addon-refunded` (`1ce82ab`) | `internal/core/domain/{taxonomy,bus}.go` and their tests, `api/taxonomy.json`, `api/asyncapi.yaml`, `docs/lld/iam-lld-audit-log-service.md`, `CHANGELOG.md` |
| `iam-org-membership`, `iam-realm-provisioner` | — | none (A-7, A-8 confirmed as built) |

## What Billing can close

| Item | Close? |
|---|---|
| BL-OQ-9 | IAM side done (A-1). Close when Billing's operator `HTTPRoute` lists B-O1 to B-O15 and the A-1 branch is merged |
| BL-OQ-23 (a) / SRE-8 | **Yes** — no change needed |
| BL-OQ-23 (b) | When the A-5 branch is merged and released |
| BL-OQ-23 (c) | After the A-6 decision |
| SRE-3 | Stays open as a per-environment ordering rule |
| CD-19 admission | **Confirmed** |
| BL-OQ-14 | Open — OP-G2 (platform edge), then OP-G1, OP-G3, OP-G4; OP-G6 date pending |
