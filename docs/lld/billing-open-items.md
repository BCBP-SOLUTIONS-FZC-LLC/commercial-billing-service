# Billing LLD — Open Items

| Field | Value |
|---|---|
| Billing LLD | `commercial-lld-billing-service.md` rev 0.61 (base HLD v0.65). The LLD's §16.1 register is the source of truth for every BL-OQ item; this file holds the per-team detail and actions for the items that are **not closed** |
| Date | 2026-11-26 (IAM status updated from IAM's replies in the hand-off doc "Billing — IAM Open Items Hand-off", 2026-11-26); Entitlements findings F-5, F-6 and the RP queue-name note added 2026-11-27 (contract check, Billing LLD rev 0.59) |
| From | Commercial Team (Billing) |
| Deployment state | No service is deployed to any environment. "Done" means merged **and** promoted with each environment (dev → staging → production) |
| Replaces | `billing-design-signoff-blockers.md`, `billing-register-open-items.md`, `billing-open-items-by-team.md`, `billing-open-items-handoff.md`, `billing-open-items-entitlements.md`, `billing-open-items-corrections.md`, `billing-decided-items-entitlements-build.md`, `billing-iam-actions.md`, `billing-iam-actions-reply.md`, `op-g2-platform-edge-request.md`. Their closed items and history are in git (removed in the commit that added this file) |

## 1. Where things stand

| Area | State |
|---|---|
| **Design decisions** | All nine partner-team decisions approved on 2026-11-23 (S-1 to S-9). Commercial Team additions to S-2, S-3, S-5, S-6 acknowledged by Finance, Product and Legal. No design question is open |
| **HLD alignment** | D-1, D-5, D-8, D-9 accepted and written into HLD v0.65 (§17.8). v0.65 is not merged yet (§3.1) |
| **Final sign-off** | Not given yet. Two steps remain (§3.1, §3.2) |
| **Go-live** | Blocked by items owned by other teams (§4). Billing's design behaves safely until each closes: a feature flag stays off, a route returns `403`, or a call fails closed with `503` |

**None of the items below blocks the design sign-off** except §3.1 and §3.2. Everything in §4 is a go-live gate or a build item.

**Status words:** **Open** = needs an answer or work · **Requested** = asked of the team, no reply yet · **Decided** = decision made, needs building · **Fail** = built, but checked and wrong · **Blocked** = waits on another item · **Deferred** = later phase.

**The rule behind the add-on items** (Commercial TL, 2026-11-19): *Billing owns commercial transactions. Entitlements owns quota state. The service that owns a business action publishes its event.*

| Business action | Owner | How it travels | Audit |
|---|---|---|---|
| Token add-on bought (payment confirmed) | Billing | `TenantQuotaAddOnChanged {reason: purchase}` on `billing.events` | Bus → `billing.token_addon.purchased` |
| Add-on invoice refunded | Billing | `TenantQuotaAddOnChanged {reason: refund}` on `billing.events` | Bus → `billing.token_addon.refunded` |
| Goodwill grant, migration credit, manual correction | Entitlements | Entitlements' operator route. **No bus event** | AL-5 `billing.token_addon.granted` / `.adjusted` / `.revoked` |
| Add-on expiry (12 months) | Entitlements | Entitlements' expiry job. **No bus event** | AL-5 `billing.token_addon.expired` |

---

## 2. Summary by team

| Team | Service / repository | Open items | Holds back |
|---|---|---|---|
| **Commercial HLD owner** | `commercial-hld-docs` | Merge and tag HLD v0.65; BL-OQ-29 (outbox retry window, HLD §9.3) | **Final sign-off** |
| **PCI owner group** | — | Re-confirm §4 and §5.4.4 (rev 0.21+); name `pci_owner_reviewer` holders; approve first certification | **Final sign-off**; first provider certification |
| **Platform-edge** | Envoy Gateway (ingress) | BL-OQ-14: **OP-G2** | **Every operator route**, so paid launch too |
| **IAM: Realm Provisioner** | `iam-realm-provisioner` | OP-G1 rollout and operator users; confirm A-10 (a) role (#62) and RP #63 live per environment (both merged) | Operator sign-in; provider certification; reversal reset |
| **IAM: AuthZ Enrichment** | `iam-authz-enrichment` | OP-G3 payload key (issuer already set); OP-G4 runs (A-3); OP-G6 date. A-9 (a), A-10 (a, b) **done** (#45, v0.2.0) | Operator routes per environment |
| **IAM: Audit Log** | `iam-audit-log` | Release and deploy A-9 (b), A-10 (c) (merged #25); deploy refund type (BL-OQ-23 b / W-12). `paused` field **confirmed** | Correct audit for B-O16, B-O17, add-on refunds |
| **IAM: Core (Org & Membership)** | `iam-org-membership` | SRE-3 ordering per environment (no code left); BL-OQ-27 (a) deploy | Direct paid signup; reversal reset |
| **Commercial Entitlements** | `commercial-entitlements-service` | **BL-OQ-28: F-5 (critical, Glue decode)**, F-6; W-2 (F-1, F-2); F-3; consumer event names; E-6; BL-OQ-27 (b) | **Token add-ons**; plan changes reaching Entitlements |
| **Commercial TL** | — | F-4 (`TenantEntitlementsChanged {cause: addon_change}`) | Nothing in Billing |
| **Commercial IaC / Platform-SRE** | IaC; clusters | Topics, registry, queues (incl. W-11); SRE-1, SRE-3, SRE-7; audit DB role; Gateway test cluster; Bedrock access; `platform-audit` probe | Billing events in any environment; token add-ons |
| **AI team** | Model Gateway | BL-OQ-19 (AI-1, AI-3, AI-5) | Token add-ons |
| **Web / growth** | Signup BFF | BL-OQ-8 | Direct paid signup |
| **Notification** | Notification service | BL-OQ-12 | Customer emails; price increases for live subscribers |
| **Usage & Metering** | `commercial-usage-metering-service` | BL-OQ-24 | Billing's usage-record consumer |
| **Commercial Team (Billing)** | `commercial-billing` | W-8, W-9, W-10 built (not deployed); F-2 header contract; Stripe adapter 1.1.0 certification; Pact provider verification for B-I1 (BL-OQ-8); LLD housekeeping; final approval; BL-OQ-15 (deferred) | Its own build |
| Finance, Product, Legal, Security | — | None (all closed 2026-11-23) | — |

---

## 3. Steps to final sign-off

Signed by the **Commercial Team** (whole document, CD-4) and the **PCI owner group** (§4, §5.4.4, §10).

### 3.1 Commercial HLD owner — merge and tag HLD v0.65

- **What:** v0.65 corrects the four differences the LLD had with the HLD (D-1, D-5, D-8, D-9). It is on the unpushed branch `docs/hld-v0.65-billing-lld-notes` in `commercial-hld-docs`.
- **Why it matters:** the LLD names v0.65 as its base, and the signed HLD version is a tagged commit on `main` (CD-3a).
- **Action:** review `hld-commercial-domain-v0.65.md` (text-only; the changelog row and §17.8 list every change), merge to `main`, tag it if HLD versions are signed by tag.
- **Done when:** v0.65 is on `main`, tagged if that's the practice.

### 3.2 PCI owner group — re-confirm §4 and §5.4.4

- **What:** the PCI review (S-7) covered rev 0.20. To meet PCI's two recommendations, rev 0.21 changed sections in PCI's scope:
  - §4.2.24: new statuses `pending_review` and `rejected`, review columns, trigger `trg_pc_guard`, constraint `chk_pc_two_people`;
  - §5.4.4: B-O14 now only records a run; new route **B-O17** for the reviewer's decision, role `pci_owner_reviewer`, invariant `BIL-PROV-4`.
- **Why it matters:** the sign-off should cover the text being signed. The changes tighten the control; they don't loosen it.
- **Action:** read LLD §4.2.24, B-O14, B-O17 and `BIL-PROV-4`; reply "confirmed" or with changes.
- **Done when:** the PCI owner group confirms rev 0.21 or later.

---

## 4. Open items by team

### 4.1 Platform-edge — BL-OQ-14 (OP-G2), the main external blocker

- **What:** a `jwt_authn` provider on the ingress Envoy Gateway for platform realm `bcbp-platform`, on `/api/v1/operator/*` only.
- **Why it matters:** until it exists, every operator route in every Commercial service answers `403`: the conversion stub (B-O1), refunds, credits, collection pause, the price book, and **provider certification** (B-O14, B-O17). Without certification Stripe takes no new accounts, so there is no paid launch.
- **Billing meanwhile:** operator routes answer `403` outside dev and test (`BIL-AUTH-6`). No Billing-side bypass.
- **Depends on:** OP-G1 (merged). OP-G1 and OP-G2 can proceed in parallel: the issuer URL is fixed by the realm name.

**The provider must:**

1. Trust the platform realm: issuer `https://auth.bcbpsolutions.com/realms/bcbp-platform`; JWKS `https://auth.bcbpsolutions.com/realms/bcbp-platform/protocol/openid-connect/certs`. Confirm the dev Keycloak's public host; the issuer must match the `iss` dev tokens carry.
2. Apply only on `/api/v1/operator/*`. Platform tokens must not authenticate tenant routes; tenant tokens must not authenticate operator routes (AuthZ also enforces this, `DENY_ROLE_INSUFFICIENT`, but the gateway is the first control).
3. Keep `ext_authz` (AuthZ Enrichment) on operator routes; it injects `x-tenant-roles: platform_operator`, `x-user-id`, `x-tenant-id`.
4. Write the verified payload (`sub`, `iss`, `realm_access.roles`) into `jwt_authn` filter metadata, and tell IAM the metadata key (the provider name). AuthZ reads it from `JWT_OPERATOR_PAYLOAD_KEY`.
5. Strip client-supplied identity headers (`x-user-id`, `x-tenant-id`, `x-tenant-roles`, …) as on tenant routes.

Suggested shape (final form is platform-edge's call):

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: operator-platform-jwt            # one per operator HTTPRoute, or one listing all of them
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: <service>-operator           # e.g. Billing's operator HTTPRoute (LLD §13.1)
  mergeType: StrategicMerge              # keep the Gateway-level ext_authz (Envoy Gateway v1.8+)
  jwt:
    providers:
      - name: platform                   # → JWT_OPERATOR_PAYLOAD_KEY=platform in AuthZ (OP-G3)
        issuer: https://auth.bcbpsolutions.com/realms/bcbp-platform
        remoteJWKS:
          uri: https://auth.bcbpsolutions.com/realms/bcbp-platform/protocol/openid-connect/certs
```

The Gateway-level tenant providers must not also accept tenant tokens on these routes; if a merged policy keeps them, attach the operator routes to a listener or policy without them.

- **Done when:** in each environment, a `bcbp-platform` token (user with `platform_operator`, OTP completed) reaches `/api/v1/operator/...` and AuthZ logs `operator_access`; a tenant-realm token there gets `403`; a platform token on a tenant route gets `403`; platform-edge has told IAM the provider/metadata key.
- **Asks of platform-edge:** (1) target date for OP-G2 in dev (also answers IAM's OP-G6); (2) the dev Keycloak public host, if not `auth.bcbpsolutions.com`; (3) the metadata key / provider name.

### 4.2 IAM

#### Realm Provisioner / Keycloak (`iam-realm-provisioner`)

| # | What | Action | Done when |
|---|---|---|---|
| OP-G1 rollout | Platform realm `bcbp-platform` merged ([#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60), `9e8fcab`); created on the next deploy | Deploy per environment; create operator users (manual today) | Operators can sign in to `bcbp-platform` in each environment |
| A-10 (a) / BL-OQ-26 | Role **`pci_owner_reviewer`** in the platform realm, next to `platform_operator` | **Merged** in #62 (`f30facf`); Realm Provisioner creates the role at startup. IAM ticked it deployed (2026-11-26). Remaining: confirm it exists in each environment; grant it only to people the PCI owner group names; audit grants and removals as other platform-realm roles | Role exists per environment, held only by named PCI members |
| BL-OQ-27 | `reason` field on `TrialReactivated` (needed by Org & Membership's reset) | **Merged** in #63 (`47539e2`). IAM ticked it deployed per environment (2026-11-26); confirm before relying on Org & Membership's reset | `TrialReactivated{reason: conversion_revert}` emitted per environment |

#### AuthZ Enrichment (`iam-authz-enrichment`)

| # | What | Action | Done when |
|---|---|---|---|
| OP-G3 | Point AuthZ at the platform realm | `OPERATOR_REALM_ISSUER: "https://auth.bcbpsolutions.com/realms/bcbp-platform"` is **already set** in the staging and prod Helm values (#45). Remaining: after OP-G2, set `JWT_OPERATOR_PAYLOAD_KEY` (the OP-G2 provider name; empty if it shares the tenant key) per environment and redeploy. Startup refuses an operator issuer equal to a tenant realm's | Operator tokens accepted; `x-tenant-roles` carries every platform-realm role the operator holds, including `platform_operator` |
| A-3 / OP-G4 | Joint end-to-end operator test through the gateway | Run with Commercial (Billing **and** Entitlements, E-6) in each environment as it is promoted. Per-route expectations for B-O1 to B-O15 are already pinned in AuthZ tests ([#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43), merged) | Allow and deny cases pass for every Billing operator route |
| A-9 (a) / BL-OQ-25 | B-O16 in the OP-G4 list | **Done.** `POST /api/v1/operator/tenants/:id/invoices/:invoice_id/disposition` (tenant in path): allow, deny and non-UUID-tenant tests merged in #45 (`3e5fd75`), released v0.2.0. No AuthZ code change was needed. The gateway half runs with A-3 | Cases below pass (unit/e2e done; gateway run with A-3) |
| A-10 (a, b) / BL-OQ-26 | Forward `pci_owner_reviewer`; B-O17 in OP-G4 | **Done.** A code change was needed, merged in #45 (`3e5fd75`), released v0.2.0: AuthZ forwards **every** platform-realm role in `x-tenant-roles`, comma-joined (e.g. `platform_operator,pci_owner_reviewer`), without keeping a role list. `POST /api/v1/operator/billing/provider-certifications/:id/review` (no tenant in path) allow and deny cases tested. The gateway half runs with A-3 | Cases below pass (unit/e2e done; gateway run with A-3) |
| OP-G6 | Target date | Give Commercial a date for OP-G1 to OP-G4 (asked 28 Oct; gates Phase 1) | Date published |

OP-G4 test cases for B-O16:

| Case | Expected |
|---|---|
| Platform token with `platform_operator` | Allow; `x-tenant-roles` = all the operator's platform-realm roles, including `platform_operator`; `x-user-id` = operator `sub`, `x-tenant-id` = path tenant |
| Platform token without `platform_operator` | `403` (`DENY_ROLE_INSUFFICIENT`) |
| Tenant-realm token, any role | `403` |
| Non-UUID tenant in path | `401` (`DENY_MALFORMED_IDENTITY`) |

OP-G4 test cases for B-O17:

| Case | Expected |
|---|---|
| Platform token with `platform_operator` and `pci_owner_reviewer` | Allow; both roles in `x-tenant-roles`, `x-user-id` = reviewer `sub` |
| Platform token with `platform_operator` only | Passes AuthZ; Billing answers `403 INSUFFICIENT_ROLE` (the test asserts the `403`) |
| Platform token without `platform_operator` | `403` (`DENY_ROLE_INSUFFICIENT`) |
| Tenant-realm token, any role | `403` |

#### Audit Log (`iam-audit-log`)

| # | What | Action | Done when |
|---|---|---|---|
| A-9 (b) / BL-OQ-25 | Entry type `billing.invoice.suspension_disposition` | **Merged** in #25 (`e995889`) as specified below; not yet in a release (v0.2.0 predates it). Release and deploy per environment | Type released and deployed |
| A-9 (b) confirm | `billing.collection.paused` accepts the extra metadata field `paused` (`true` \| `false`) written by B-O10 | **Confirmed** (2026-11-26): the type was added in #25, and AL-5 metadata is open JSON (only the `X-Max-Metadata-Bytes` cap applies), so `paused` and the reason are stored as sent | Done |
| A-10 (c) / BL-OQ-26 | Entry type `billing.provider.certification_reviewed` | **Merged** in #25 (`e995889`) as specified below; not yet in a release. Release and deploy per environment | Type released and deployed |
| W-12 / BL-OQ-23 (b) | `billing.token_addon.refunded` (code merged [#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24) `0e12f2c`; AL-5 direct-write types for Entitlements present at `e995889`, verified 2026-11-26) | Deploy a release at or after `0e12f2c` in each environment (v0.3.0 suggested), before add-on refunds are possible there | Refund events stored as `refunded`, not `purchased` |

| Field | `billing.invoice.suspension_disposition` | `billing.provider.certification_reviewed` |
|---|---|---|
| Tier | `security_3y` | `security_3y` |
| Source | AL-5 direct-write via `platform-audit` v0.1.2 outbox; producer `commercial-billing` (admitted since Audit Log rev 0.34). Not bus-sourced | Same |
| Actor | Operator `sub` (`x-user-id`) | Reviewer `sub` (`x-user-id`) |
| Tenant | Path tenant | `platform_tenant` (global, as `billing.provider.certified`) |
| Written | Once per B-O16 decision, same transaction | Once per B-O17 decision, same transaction |
| Metadata | `invoice_id`, `provider_invoice_id`, `action` (`finalize` \| `void`), `amount_minor`, `currency`, `reason` (10–1000 chars) | `certification_id`, `decision` (`approve` \| `reject`), `provider`, `adapter_version`, `suite_version`, `certified_by` (submitter `sub`), `reason` |

**Billing meanwhile:** AL-5 entries wait in `pending_audit_entries` and drain once the type exists; nothing is lost. Add-on refunds are stored as `purchased` until W-12 is deployed, with `reason` and the negative `delta` kept in metadata.

#### Core / Org & Membership (`iam-org-membership`)

| # | What | Action | Done when |
|---|---|---|---|
| SRE-3 (with Platform-SRE) | Table `pending_tenant_events` (CD-50 / EVT-18) and the `parked-events-replay` CronJob (`*/5 * * * *`). The table was migration `000004`; it is now part of Core's consolidated base migration `000000_initial_schema`. The CronJob is on by default in the chart for every environment. No code change left | Deploy in each environment **before** `FEATURE_DIRECT_SIGNUP=true` there | Confirmed per environment |
| BL-OQ-27 (a) | Reset `plan` (from `tenants.trial_plan`) and `licensed_seats` (trial allowance, T-8) on `TrialReactivated{reason: conversion_revert}`; seat and plan watermarks raised to the revert time | **Merged** (PRs #69, #70, #71, `8150ea1`, O&M LLD rev 2.65); checked on `main` at `7fcc435`: the revert raises both watermarks, and a tenant with no `trial_plan` keeps its current plan with a logged warning. **Not deployed.** Deploy per environment, after RP #63 is deployed there. Direct paid signups are covered: every tenant is created as `trial` with its signup plan (I-1) | A reverted tenant shows the trial plan and trial seats in each environment |

**Billing meanwhile (BL-OQ-27):** a reverted tenant keeps the paid plan and seats until (a) is live; the operator refunds add-ons by hand (runbook `reversal-decision.md`). Billing sends no `billing.events` after a reversal.

### 4.3 PCI owner group

| # | What | Action | Done when |
|---|---|---|---|
| Sign-off | Re-confirm §4 and §5.4.4 at rev 0.21+ | See §3.2 | Confirmed |
| BL-OQ-26 | Who holds `pci_owner_reviewer` | Name the members to IAM | Role granted to named members |
| First certification | Approve the first Stripe certification run | After `cmd/certify` runs and B-O14 records the run, a member other than the submitter approves it through B-O17 | Stripe's record is `passed`; the registry enables Stripe |

### 4.4 Commercial Entitlements

**Service:** `commercial-entitlements-service` (namespace `commercial`, ServiceAccount `commercial-entitlements`). Decided items BL-OQ-3, BL-OQ-4, BL-OQ-20 (TL, 2026-11-19), work items W-1 to W-7.

**Last checked 2026-11-26** against `main` at `dc9b654` (PR #2, Entitlements LLD rev 1.20). Branch `compatibility-check` (`d83fec4`) is not merged and not counted.

| # | Item | Result 2026-11-26 |
|---|---|---|
| W-1 | `addon_eligibility` per resource (`llm_tokens`, `api_requests`; reasons `trial`, `not_active`, `unlimited_quota`) — BL-OQ-3 | **Pass.** `computeAddonEligibility` in `core/service/effective_set.go`; `addon_eligible: bool` gone; Billing's `entitlementsclient` reads the shape |
| W-2 | Billing admitted read-only on `GET /api/v1/internal/tenants/:id/entitlements` only — BL-OQ-3 | **Fail: F-1, F-2.** NetworkPolicy rule (namespace `billing`, pod label `commercial-billing`, port 8080) is correct |
| W-3 | Entitlements' own `tenant_quota_add_on_changed` withdrawn — BL-OQ-4 | **Pass.** Only the inbound schema remains; AsyncAPI lists the event as consumed |
| W-4 | Consume `reason: purchase` from `billing-entitlements-q`; idempotent on `billing_ref`; expiry `purchased_at` + 12 months — BL-OQ-4 | **Pass.** `billing_consumer.go`, `ApplyBillingPurchase`, migration `000002_addon_billing_ref`. Can't receive events in an environment until W-11 exists there |
| W-5 | Consume `reason: refund`; unused tokens of the `purchase_ref` grant only; never below zero — BL-OQ-20 | **Pass, gap F-3** |
| W-6 | Corrections and expiry via AL-5 only, no add-on bus event — BL-OQ-20 | **Pass, question F-4** (with the TL) |
| W-7 | Entitlements LLD states the consumed schema, reason ownership, no-event rule | **Pass** (LLD rev 1.20 §7) |

**Open findings**

- **F-1 (W-2, blocking): the Istio policy admits Billing on the wrong paths.** `deploy/helm/templates/authz-policy.yaml` allows `GET` on `*/effective-set` and `*/quota-ceilings`, neither of which exists in `router.go`. The route Billing calls isn't listed, and `billing` isn't in `existingCallerNamespaces`. Istio is on in staging and prod, so every Billing call is denied there and add-on purchase fails closed with `503 ENTITLEMENTS_UNAVAILABLE` (dev has Istio off, so it doesn't show). **Fix (Entitlements):** set the rule's `paths` to `/api/v1/internal/tenants/*/entitlements` only; fix the comment pointing at the non-existent `commercialSystemAllowedRoutes`.
- **F-2 (W-2, blocking, both teams): the internal route rejects Billing before the allow-list runs.** `internalServiceRequired()` returns `401` without a non-empty `x-service-token`; `billingReadOnly()` applies only when `x-service-name` is `commercial-billing`. Billing sends neither header, so it gets `401` everywhere. If Billing sent the token but not the name, the allow-list would not run and Billing would reach every internal route, including `POST …/entitlements/recompute`. A caller-declared header can't be the identity check. **Proposed fix:** Entitlements identifies Billing by its mesh identity (SPIFFE principal `cluster.local/ns/billing/sa/commercial-billing` from `x-forwarded-client-cert`). If Entitlements keeps the header scheme, Billing sends both headers and `billingReadOnly` must deny by default any caller with a token and no recognised name. Either way, the agreed header contract goes into Billing LLD §10 and the Entitlements LLD. **Decision: approved (2026-11-26)**, as a cross-service contract documented in both LLDs, not inferred from code:
    - **Authentication:** mTLS through the Istio mesh. Billing's identity is the XFCC principal (`cluster.local/ns/billing/sa/commercial-billing`), never a header the caller writes.
    - **Caller headers:** the contract lists which headers are required, which are optional, and which values are trusted only when injected by the mesh. Proposed set (adjust to the platform standard where one exists): `X-Tenant-Id`, `X-Request-Id`, and `X-Correlation-Id` if that is the platform standard.
    - **Tenant context:** tenant id header, request/correlation id propagation, and actor/system identity if applicable.
    - **Authorization:** Entitlements validates the Billing principal; only `commercial-billing` may call the eligibility endpoint. Billing does not rely on user-supplied identity headers.
    - If Entitlements keeps a header-based scheme, Billing sends the documented headers on every eligibility request.

  **F-2 closes when:** Billing LLD §10 documents the contract; the Entitlements LLD documents the same contract, word for word; both services implement the same header names and identity source; integration tests pass with the agreed header set; no undocumented header is needed for an eligibility check.
- **F-3 (W-5, minor): refund idempotency uses the event ID, not `billing_ref`.** `ApplyBillingRefund` relies on `processOnce` with the envelope ID. Safe today (Billing reuses `addon_event_id`), but the decision is "idempotent on `billing_ref` (= `refund_id`)". **Fix:** record applied `refund_id`s (e.g. a `quota_addon_refunds` row keyed on `billing_ref`).
- **F-4 (W-6, Commercial TL decision): add-on changes show on `entitlement-events` via the effective set.** The effective set adds the remaining add-on balance to `llm_token_quota_monthly`, so a grant, revocation, expiry, purchase, refund or **drawdown** publishes `TenantEntitlementsChanged {cause: addon_change}`. S-8 forbids *add-on balance change events*, not effective-set changes. Decide (a) whether `cause: addon_change` is allowed under D-8, and (b) whether a drawdown should publish at all (one event per consumption from an add-on). Neither changes Billing.

**Findings of 2026-11-27 — BL-OQ-28** (contract check against `main` at `dc9b654`, Billing LLD rev 0.59):

- **F-5 (CRITICAL, would block every Billing event in staging and prod): the SQS consumer cannot decode Glue-framed messages.** `internal/adapter/inbound/sqspipeline/consumer.go:76-88` builds the consumer without `events.WithConsumerCodec(events.GlueDecodeCodec{})`. platform-events v2.3.0 (`internal/adapter/outbound/sqs/consumer.go:1597`) rejects any message carrying a `schema_id` when no codec is set, so every Glue-framed Billing event (`TenantQuotaAddOnChanged`, `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated`) would be retried and dead-lettered once Billing publishes Glue-framed events (staging, prod); dev-like environments publish plain JSON. Nothing is deployed yet. **Fix (Entitlements):** add the one option, as Core and Realm Provisioner do. **Billing follow-up:** `contract-live` to test Glue-framed delivery, not only the decoded payload.
- **F-1 and F-2 re-checked: still open.** `deploy/helm/templates/authz-policy.yaml:58-60` admits only `*/effective-set` and `*/quota-ceilings`; `billing` is not in `existingCallerNamespaces` in `values-staging.yaml` / `values-prod.yaml`; `middleware.go:39-47` returns `401` without `x-service-token`. Every add-on purchase gets `503 ENTITLEMENTS_UNAVAILABLE` until both are fixed. Billing calls `GET /api/v1/internal/tenants/:id/entitlements` with its mTLS principal (Billing LLD §10.2).
- **F-6 (race): a `TenantPlanChanged` processed before Core applies the change is never corrected.** `billing_consumer.go:134-148` re-reads I-17 instead of using the event's plan; `membership_consumer.go:66-75` ignores Core's `TenantStateChanged` unless its cause is `TrialReactivated`, so nothing triggers a second recompute. **Fix (Entitlements):** apply the plan from the event, or recompute on Core's plan-change event too.

**Other Entitlements items**

| # | What | Action | Done when |
|---|---|---|---|
| Consumer event names | The Billing consumer pushed on 2026-11-19 handled `BillingSubscriptionActivated` / `…Updated` / `…Suspended` / `…Reactivated`, `BillingTrialStarted`, `BillingTrialExpired`. **Billing publishes none of these.** Not re-checked on 2026-11-26 | Handle `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged` (HLD §9.4, Billing LLD §7.3). Unknown types are acked and dropped, so every Billing event would be lost | Each of the four types is applied in a test |
| E-6 / BL-OQ-14 | Joint OP-G4 run for Entitlements' own operator routes (incl. add-on correction and shortfall routes) | **Approved** (2026-11-26) as a coordination milestone, no code. On Billing's signal (§4.10), schedule the joint run with IAM and Billing, then run it | Joint OP-G4 run completed per environment |
| BL-OQ-27 (b) | After a conversion reversal (`billing-note-to-entitlements-revert-plan.md`, E-1 to E-4 of that note) | Consume `TrialReactivated` on `tenant-entitlements-q` (not in its filter today); recompute a trial snapshot from I-17; handle reading I-17 before Org & Membership has applied the event; decide whether an add-on balance not refunded survives the reversal | **Awaiting reply** |
| E-8 | Staff role `commercial_trial_manager` (OP-G5) | Only if Product asks (CD-51). Not a Billing need | — |

**Review points on the consumer** (from the 2026-11-23 review; recheck against `main`): a malformed purchase event goes to the DLQ and alerts, never logged-and-acked (customer paid, no tokens); a failed recompute after a grant is retried or reconciled; a `billing_ref` arriving for a different tenant alerts rather than retry-looping; a refund arriving before its purchase is retried, not acked.

**What closes W-2:** F-1 fixed, F-2 agreed and built, then from a Billing pod in each environment: `200` with `addon_eligibility` on the route, and `403` on `POST …/entitlements/recompute` and `GET …/plans/current`.

**Billing meanwhile:** `FEATURE_TOKEN_ADDONS=false`. Without eligibility or access, an add-on purchase fails closed (`503`). Until W-1 is live in an environment, Billing checks that `llm_token_quota_monthly` is finite (`BIL-ADD-1`).

### 4.5 Commercial IaC / Platform-SRE

| # | What | Action | Done when |
|---|---|---|---|
| IaC | SNS `billing-events`, Glue registry `billing-events`; SQS `tenant-billing-q`, `usage-billing-q` with DLQs (HLD §16 action 2; LLD §7.3.9) | Create per environment | Billing publishes and consumes in each environment |
| W-11 | `billing-entitlements-q` + DLQ + SNS subscription to `billing-events` | Create per environment | Entitlements receives Billing events |
| DB | `billing_audit_drainer` role and secret (`PG_AUDIT_DRAINER_*`); `MIGRATION_DATABASE_URL` on the direct RDS endpoint, not PgBouncer | Create per environment | Migrations and audit drainer run |
| `platform-audit` | `Client.Probe` returns `ProbeAdmitted` from a Billing pod; `platform-audit` alert rules loaded and routed to Billing on-call | Per environment | Probe passes; alerts routed |
| SRE-1 | Istio `STRICT` mTLS in staging | Enable | Staging mesh is strict |
| SRE-3 | Core `000004` + `parked-events-replay` before direct signup (with Core, §4.2) | Order per environment | Confirmed per environment |
| SRE-7 | Remove any `membership-billing-q` (retired by CD-54) | Delete if present | Queue absent |
| BL-OQ-19 support | Cluster with Model Gateway v1.2.0, the rate-limit service and Valkey; Bedrock access in `ap-south-1` (later `me-central-1`) | Provide to the AI team | AI team can run AI-1, AI-3, AI-5 |

### 4.6 AI team — BL-OQ-19, Model Gateway limit hot reload on a cluster

- **What:** when a tenant buys tokens, the Gateway must raise that tenant's limit. So far shown only without a cluster (0.3 s reload, Envoy fail-open); nothing has run through Gateway v1.2.0.
- **Billing meanwhile:** `FEATURE_TOKEN_ADDONS=false`.
- **Action:** on the §4.5 cluster, on the pinned v1.2.0 image: **AI-1** an entitlement change reaches the Gateway within 60 s; **AI-3** calls continue and the SEV-2 alert fires when the rate-limit service fails; **AI-5** a purchased add-on raises the tenant's allowance (plan entitlement plus add-on). AI-5 is required: AI-1 and AI-3 alone don't prove the commercial path.
- **Done when:** all three pass on a named cluster, with evidence.
- PD-LLM-1 R-1, R-2, R-4 (model list, evaluation set, Opus-tier rule) are the AI team's own Model Gateway work, not Billing items.

### 4.7 Web / growth — BL-OQ-8, Signup BFF alignment (SGN-2)

- **What:** direct paid signup calls Billing's B-I1 (LLD §5.4.2).
- **Billing meanwhile:** `FEATURE_DIRECT_SIGNUP=false`; signup goes through trial conversion only.
- **Action:** (1) `BILLING_TIMEOUT_MS` ≥ 15000; (2) map `PaymentToken` → `payment_method_ref`, `Plan` → `plan_code`, `Seats` → `licensed_seats`; (3) 3-D Secure: `402 PAYMENT_ACTION_REQUIRED` → browser authenticates → retry with the **same** idempotency key; (4) follow the go-live order in LLD §18.7.
- **Done when:** the Pact contract test is green and the Test-Clock end-to-end run passes in staging.
- **Depends on:** SRE-3.
- **Status (2026-11-27, BFF reply):** alignment **done and merged**: `bff-signup` PR #6 (BFF LLD v1.3 §19), merged 2026-11-26 (`c2e9a1d`); not yet promoted to staging. All checklist items done except the Pact contract. The BFF never provisions on `409 SUBSCRIPTION_ALREADY_EXISTS` (LLD §5.4.2 updated, rev 0.52). Billing's answers were sent to the BFF team on 2026-11-27.
- **Still open:**
  - Billing: Pact broker URL, credentials path, provider name and verification branch (Q2), and the Pact provider verification in Billing's CI, which doesn't exist yet;
  - Billing: Test-Clock E2E slot in staging (Q4);
  - Web / growth: promote PR #6 dev → staging (date to confirm), then publish the Pact contract;
  - Platform-SRE: Istio sidecar injection on namespace `bff`, without which B-I1 answers `403`.

### 4.8 Notification — BL-OQ-12, Billing messages (NTF-3)

- **What:** messages for seats added; seat reduction applied (including renewal above target); token add-on purchased; downgrade blocked at renewal; renewal top-up charged; **renewal price change scheduled** (current and new unit price, effective date). Add-on expiry reminder is Entitlements-driven.
- **Why it matters:** Product requires customers to be told before a renewal uses a higher price (BL-OQ-16). Until this ships, the change is shown only on the Billing page.
- **Billing meanwhile:** only past-due and cancelled emails are sent.
- **Action:** agree the triggers with the Commercial Team (event or outbox message per case), then build the templates. Notification resolves recipients; Billing puts no personal data on the bus.
- **Done when:** each message is sent in staging from a real Billing trigger. **Don't apply price increases to live subscribers until the price-change message is live.**

### 4.9 Usage & Metering — BL-OQ-24, `UsagePeriodClosed` contract

- **What:** Billing consumes `period_start`, `period_end`, `tokens_total`, `api_requests_total`, `addon_consumed`, `limits` (LLD §7.1), taken from the HLD because the Usage & Metering LLD isn't written.
- **Billing meanwhile:** records are reference-only; nothing is priced from them. A mismatch affects operator and Finance views, not charges.
- **Action:** confirm the payload in the Usage & Metering LLD and register the schema in Glue `usage-events`.
- **Done when:** schema registered and Billing's contract test passes against it, before `usage-billing-q` is switched on.

### 4.10 Commercial Team (Billing) — own items

| # | What | Action | Done when |
|---|---|---|---|
| W-8 | Publish `TenantQuotaAddOnChanged` (`purchase`, `refund`) through the outbox (`BIL-ADD-2`, `BIL-ADD-4`, LLD §7.3.6) | **Approved** (2026-11-26): emitted through the Billing outbox and published only on a successful add-on purchase confirmation or refund confirmation. Schema definition, payload validation and Glue registration delivered in Phase 4; event production delivered with the add-on purchase and refund flows in Phase 6 | Phase 6 merged: exactly one event per confirmed purchase and per confirmed refund; schema registered per environment |
| W-9 | Read Entitlements eligibility: fresh on purchase (B-P20, `ENTITLEMENTS_TIMEOUT_MS=1000`), 60 s cache for the pack list (B-P19), `503 ENTITLEMENTS_UNAVAILABLE` fail-closed, interim finite-quota check | **Approved** (2026-11-26). Build as LLD §8.7: a fresh Entitlements check on every purchase; eligibility caching only for pack discovery (B-P19); Entitlements unavailable blocks the purchase (`503 ENTITLEMENTS_UNAVAILABLE`); the interim finite-quota rule (`BIL-ADD-1`) stays until Entitlements is established as the sole source of eligibility truth | Works against Entitlements in each environment |
| W-10 | Egress from `billing` to Entitlements in `commercial`, on that `GET` only | **Approved** (2026-11-26): least-privilege egress, solely for the eligibility `GET /api/v1/internal/tenants/:id/entitlements` used by B-P19 and B-P20. The Billing Helm chart and its NetworkPolicy and service-mesh configuration restrict Billing → Entitlements traffic to that endpoint only (LLD §10) | From a Billing pod in each environment, the eligibility route is reachable and every other Billing → Entitlements route is not |
| F-2 | Header / identity contract with Entitlements | **Approved** (2026-11-26; decision in §4.4). Agree the exact header names with Entitlements (platform standard first); write the contract into LLD §10, identical to the Entitlements LLD; send the documented headers on every eligibility request if the header scheme is kept; add integration tests | Contract identical in both LLDs; both services implement it; integration tests pass with the agreed headers; no undocumented header required |
| Contract test | Entitlements' consumption of `TenantQuotaAddOnChanged` | **Done** (approved 2026-11-26): `test/contract/entitlements_addon_test.go`. Schema compatibility both ways, decode into Entitlements' inbound structs, producer guards; vendored schema from `dc9b654`; daily run against Entitlements' `main` (`.github/workflows/contract-live.yml`) | CI fails if Billing and Entitlements disagree ✔ |
| E-6 signal | Readiness hand-off for the joint OP-G4 run | **Approved** (2026-11-26): a cross-team readiness milestone, no code change. Once OP-G2 and OP-G3 are complete **and validated in the target environment**, Billing formally notifies Entitlements that the operator path is ready for shared testing. Receiving the signal authorises scheduling the joint OP-G4 run. Sequence: OP-G2 complete + OP-G3 complete → E-6 signal sent → joint OP-G4 scheduled → joint test completed. Owners: Billing + Entitlements | OP-G2 and OP-G3 complete, Entitlements notified, and the joint OP-G4 run scheduled, per environment |
| Housekeeping | LLD §1.2 cites `billing-open-items-team-response.md`, `billing-reply-ai-team.md`, `billing-note-to-entitlements-revert-plan.md` and `billing-reply-to-org-membership-revert-plan.md`, which aren't in this repository | Add them or say where they're held | Citations resolve |
| Approve | Final sign-off (CD-4) | After §3.1 and §3.2 | Signed |
| BL-OQ-15 | **Deferred (Phase 3).** Per-tenant provider choice; moving customers off a removed provider (design in §19.3). With Finance | None now | Phase 3 plan |

---

## 5. What each launch needs

| Launch | Needs closed |
|---|---|
| **Final sign-off** | §3.1 HLD v0.65 merged · §3.2 PCI re-confirmation |
| **Any Billing event in an environment** | IaC topic, registry and queues · Entitlements consumer event names (or Entitlements misses every plan change) |
| **Paid subscriptions through Stripe** | OP-G1 rollout · **OP-G2** · OP-G3 · OP-G4 · A-10 role + named holders · first certification approved (B-O17) |
| **Direct paid signup** | All of the above · BL-OQ-8 · SRE-3 |
| **Token add-ons** (`FEATURE_TOKEN_ADDONS=true`) | Stripe launch · W-2 (F-1, F-2) · W-8 to W-11 · BL-OQ-19 (AI-1, AI-3, AI-5) · W-12 deployed |
| **Price increases for live subscribers** | BL-OQ-12 price-change message |
| **Usage-record consumer** | BL-OQ-24 |
| **Correct audit labels** | A-9 (b), A-10 (c), W-12 deployed |
| **Correct plan/seats after a reversal** | BL-OQ-27 (a) deployed with RP #63; (b) Entitlements |

## 6. Critical path

```
OP-G1 rollout ─► OP-G2 (platform-edge, not started) ─► OP-G3 ─► OP-G4 joint run ─┐
A-10 role + PCI names holders ───────────────────────────────────────────────────┴─► first certification (B-O17) ─► Stripe live
```

**Hygiene (RP, 2026-11-27):** Realm Provisioner's `deploy/iam/sqs.tf.example` names Billing's queue `iam-billing-realm-q`, while Billing's `api/asyncapi.yaml` lists `billing-realm-q`. Align one name with IaC before the queue is created.

**OP-G2 is the longest pole.** It hasn't started, and nothing operator-side can be proven without it. Ask IAM for the OP-G6 date and escalate OP-G2 first.

## 7. Tracker

| # | Item | Team | Status (2026-11-26) |
|---|---|---|---|
| 3.1 | Merge and tag HLD v0.65 | Commercial HLD owner | Open |
| 3.2 | Re-confirm §4, §5.4.4 | PCI owner group | Open |
| BL-OQ-14 | OP-G2 gateway provider | Platform-edge | **Not started** |
| BL-OQ-14 | OP-G1 rollout, operator users | IAM Realm Provisioner | Merged; rollout open |
| BL-OQ-14 | OP-G3 (payload key; issuer set), OP-G4, OP-G6 | IAM AuthZ Enrichment | Blocked on OP-G2 |
| BL-OQ-25 | A-9 (a) B-O16 tests | IAM AuthZ | **Done** (#45, v0.2.0) |
| BL-OQ-25 | A-9 (b) Audit type | IAM Audit Log | Merged (#25); release + deploy open |
| BL-OQ-26 | A-10 (a, b) role, forwarding, B-O17 tests | IAM RP, AuthZ | **Done** (#62, #45 v0.2.0); confirm role per environment |
| BL-OQ-26 | A-10 (c) Audit type | IAM Audit Log | Merged (#25); release + deploy open |
| BL-OQ-26 | Name `pci_owner_reviewer` holders | PCI owner group | Open |
| BL-OQ-23 (b) / W-12 | Refund type deployed | IAM Audit Log | Merged; deploy open |
| BL-OQ-27 (a) | Plan/seat reset on reversal | IAM Org & Membership; RP | O&M merged (`8150ea1`), not deployed; RP #63 merged (`47539e2`), reported deployed |
| BL-OQ-27 (b) | Trial limits after reversal | Entitlements | Awaiting reply |
| SRE-3 | Core `000004` before direct signup | IAM Core + SRE | Code ready |
| BL-OQ-3 | W-1 eligibility | Entitlements | **Done** (`dc9b654`) |
| BL-OQ-3 | W-2 Billing access | Entitlements + Billing | **Fail** (F-1, F-2; re-checked 2026-11-27, still open) |
| BL-OQ-28 / F-5 | Glue decode codec on the SQS consumer | Entitlements | **Critical**, open (2026-11-27) |
| BL-OQ-28 / F-6 | `TenantPlanChanged` processed before Core applies the plan | Entitlements | Open (2026-11-27) |
| BL-OQ-29 | `OUTBOX_MAX_ATTEMPTS=10`, `OUTBOX_RETRY_BACKOFF=30s`, `OUTBOX_MAX_RETRY_BACKOFF=5m` (today an SNS / Glue outage dead-letters every row in about a minute); needs an HLD §9.3 change | Commercial HLD owner | Open (2026-11-27) |
| — | Glue-framed delivery in `contract-live` | Billing | Open (2026-11-27) |
| — | RP queue name `iam-billing-realm-q` vs `billing-realm-q` | IAM Realm Provisioner | Open (2026-11-27) |
| F-2 | Header / identity contract | Billing + Entitlements | **Approved** 2026-11-26; Billing LLD §10.2 text proposed (rev 0.50); waits on Entitlements' agreement, Entitlements LLD copy, build, integration tests |
| BL-OQ-4 | W-3, W-4 single publisher, purchase consumer | Entitlements | **Done** (`dc9b654`) |
| BL-OQ-20 | W-5, W-6, W-7 refunds, corrections, LLD | Entitlements | **Done**; F-3 open; F-4 with TL |
| — | Entitlements consumer event names | Entitlements | Open (not re-checked 2026-11-26) |
| E-6 | Signal, then joint OP-G4 run | Billing + Entitlements (+ IAM) | **Approved** 2026-11-26; signal not sent: waits on OP-G2 (not started) and OP-G3 |
| IaC | Topic, registry, queues incl. W-11; DB role; probe | Commercial IaC / SRE | Open |
| SRE-1, SRE-7 | Istio strict in staging; remove old queue | Platform-SRE | Open |
| BL-OQ-19 | AI-1, AI-3, AI-5 on a cluster | AI team (+ SRE cluster) | Not proven |
| BL-OQ-8 | BFF alignment | Web / growth (+ Billing for Pact, E2E) | BFF code merged (PR #6, `c2e9a1d`), not in staging; Pact and Test-Clock E2E pending |
| BL-OQ-12 | NTF-3 messages | Notification + Commercial | Open |
| BL-OQ-24 | `UsagePeriodClosed` contract | Usage & Metering | Open (known risk) |
| W-8 | Add-on event publish | Billing | **Built** (Phase 6, `feat/billing-phase-6-flows`): purchase and refund events through the outbox; not deployed; Glue registration per environment open |
| W-9 | Eligibility read | Billing | **Built** (`adapters/entitlementsclient`, B-P19 / B-P20, LLD §8.7); not deployed; works once Entitlements closes F-1, F-2 |
| W-10 | Egress to Entitlements | Billing | **Built** in the chart's server NetworkPolicy (namespace `commercial`, pod `commercial-entitlements`, LLD §10.2); not deployed. A NetworkPolicy cannot limit the path: restricting Billing to the one `GET` needs an AuthorizationPolicy on the Entitlements side (open, with F-2) |
| — | Contract test incl. Entitlements schema | Billing | **Done** 2026-11-26 |
| — | LLD citations | Billing | Open |
| BL-OQ-15 | Multi-provider follow-ups | Billing + Finance | Deferred (Phase 3) |

Closed and not repeated here (see LLD §16.1 and git history): BL-OQ-1, 2, 5, 6, 7, 9, 10, 11, 13, 16, 17, 18, 21, 22, 23 (a, c); S-1 to S-9 and their acknowledgements; D-1 to D-9; IAM A-1, A-4 to A-8; Entitlements E-3; PD-LLM-1 and ENT-D6 approvals.
