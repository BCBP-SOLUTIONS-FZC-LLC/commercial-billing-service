# Decided Items Waiting on Entitlements — BL-OQ-3, BL-OQ-4, BL-OQ-20

| Field | Value |
|---|---|
| Source | `commercial-lld-billing-service.md` rev 0.23, §16.1 (base HLD v0.65) |
| Date | 2026-11-23 |
| From | Commercial Team (Billing) |
| To | Commercial Team (Entitlements service); also the Billing service, Commercial IaC and Audit Log for their parts |
| Decided by | Commercial TL, 2026-11-19. Confirmed by BL-OQ-23 (c) on 2026-11-23 and HLD v0.65 (D-8) |
| Related | `billing-open-items-entitlements.md` (E-1, E-2, E-4, E-5, E-7: payloads and tests in full) |
| Deployment state | No service is in production; all run in dev only. "Done" means merged and promoted with each environment |

## What this document is

Three items in the Billing register are **decided, not open**. The Commercial TL made the decision on 2026-11-19. What's left is the build, mostly in the **Entitlements service**. Nothing here needs a meeting or a new decision. Each team needs to implement its part and confirm it.

They don't block the Billing design sign-off. They do block **token add-ons**, which stay off (`FEATURE_TOKEN_ADDONS=false`) until all three are built, along with BL-OQ-18 (pack definitions) and BL-OQ-19 (Model Gateway proof).

## The rule behind all three

> **Billing owns commercial transactions. Entitlements owns quota state. The service that owns a business action publishes its event.** (Commercial TL, 2026-11-19)

| Business action | Owner | How it travels | Audit |
|---|---|---|---|
| Token add-on bought (payment confirmed) | Billing | `TenantQuotaAddOnChanged {reason: purchase}` on `billing.events` | Audit consumes the bus event → `billing.token_addon.purchased` |
| Add-on invoice refunded | Billing | `TenantQuotaAddOnChanged {reason: refund}` on `billing.events` | Audit consumes the bus event → `billing.token_addon.refunded` |
| Goodwill grant, migration credit, manual correction | Entitlements | Entitlements' own operator route. **No bus event** | AL-5 (`billing.token_addon.granted` / `.adjusted` / `.revoked`) |
| Add-on expiry (12 months) | Entitlements | Entitlements' expiry job. **No bus event** | AL-5 (`billing.token_addon.expired`) |

## Who owns what

### Teams and services

| Service | Owning team | Repository | Namespace / ServiceAccount (pod label) | Role in these items |
|---|---|---|---|---|
| **Entitlements** | Commercial Team (CD-4), Entitlements service | `BCBP-SOLUTIONS-FZC-LLC/commercial-entitlements-service`¹ | `commercial` / `commercial-entitlements` | **Builds almost everything here**: the eligibility API, access for Billing, the event consumer, refund handling. No code yet: the repository holds only a README |
| **Billing** | Commercial Team (CD-4), Billing service | `BCBP-SOLUTIONS-FZC-LLC/commercial-billing` | `billing` / `commercial-billing` | Publishes `TenantQuotaAddOnChanged`; calls the Entitlements read route. Designed in the LLD; built with Billing |
| **Commercial IaC** | Commercial Team, with Platform / SRE | Commercial infrastructure-as-code | — | Creates `billing-entitlements-q` and its SNS subscription to `billing-events` (HLD §15.1) |
| **Audit Log** | IAM (Audit Log) | `iam-audit-log` | `iam` | Already built. Deploys the release with `billing.token_addon.refunded` per environment (BL-OQ-23 b) |

¹ HLD §15.2 names the repository `commercial-entitlements`; the actual GitHub repository is `commercial-entitlements-service`. The service name, envelope `source` and metric prefix stay `commercial-entitlements` (CD-17).

### Work by owner

| # | Work | Owner (service) | Item |
|---|---|---|---|
| W-1 | Return `addon_eligibility` per resource on the effective entitlement set | **Entitlements** | BL-OQ-3 |
| W-2 | Admit Billing, read-only, on `GET /api/v1/internal/tenants/:id/entitlements` only (NetworkPolicy, Istio AuthorizationPolicy, app allow-list) | **Entitlements** (its Helm chart) | BL-OQ-3 |
| W-3 | Withdraw Entitlements' own `tenant_quota_add_on_changed` everywhere | **Entitlements** | BL-OQ-4 |
| W-4 | Consume `TenantQuotaAddOnChanged {reason: purchase}` from `billing-entitlements-q`; grant tokens; expiry from `purchased_at` | **Entitlements** | BL-OQ-4 |
| W-5 | Consume `TenantQuotaAddOnChanged {reason: refund}`; remove unused tokens only | **Entitlements** | BL-OQ-20 |
| W-6 | Corrections and expiry as Entitlements actions, audited through AL-5, no bus event | **Entitlements** | BL-OQ-20 |
| W-7 | Entitlements LLD states W-1 to W-6 (consumed schema, reason ownership, no add-on event) | **Entitlements** | BL-OQ-4, BL-OQ-20 (E-7) |
| W-8 | Publish `TenantQuotaAddOnChanged` for `purchase` and `refund`; register the schema in Glue `billing-events` | **Billing** | BL-OQ-4, BL-OQ-20 |
| W-9 | Read eligibility from Entitlements; fail closed; interim finite-quota check | **Billing** | BL-OQ-3 |
| W-10 | Egress from `billing` to Entitlements in `commercial`, on that one route | **Billing** (its Helm chart) | BL-OQ-3 |
| W-11 | `billing-entitlements-q` with DLQ and SNS subscription to `billing-events` | **Commercial IaC** | BL-OQ-4 |
| W-12 | Deploy Audit Log at or after `0e12f2c` (refund entry type) | **Audit Log (IAM)** | BL-OQ-20 (via BL-OQ-23 b) |

W-8 to W-10 are already in the Billing LLD and ship with Billing. W-12 is merged and only needs deploying.

---

## BL-OQ-3 — Add-on eligibility and Billing's read access

**Owner:** Entitlements service (W-1, W-2). Billing service consumes it (W-9, W-10).

**What was asked.** Before selling token add-ons, Billing must know whether the tenant's token quota is finite (`BIL-ADD-1`). Entitlements is the system of record for the effective entitlement set.

**What Entitlements first proposed.** A single `addon_eligible: bool`, true when the tenant is active and **either** its token quota **or** its API-request budget is finite.

**Decision (TL, 2026-11-19).** Rejected. A tenant with unlimited tokens and a finite API budget would show `true`, and Billing would sell tokens the tenant can't use. Instead:

**W-1 — Entitlements returns eligibility per resource** on `GET /api/v1/internal/tenants/:id/entitlements`:

```json
"addon_eligibility": {
  "llm_tokens":   { "eligible": true,  "reason": null },
  "api_requests": { "eligible": false, "reason": "unlimited_quota" }
}
```

`reason` is one of `trial`, `not_active` or `unlimited_quota`. Billing reads `llm_tokens` only in v1.

**W-2 — Entitlements admits Billing, read-only, on that one route.** Billing runs in namespace `billing`, not `commercial`, so it isn't admitted today.

| Layer | Rule |
|---|---|
| Istio AuthorizationPolicy | Principal `cluster.local/ns/billing/sa/commercial-billing`; method `GET`; path `/api/v1/internal/tenants/*/entitlements` only |
| NetworkPolicy | Ingress from namespace `billing`, pod label `app.kubernetes.io/name: commercial-billing` |
| App allow-list | `commercial-billing` on that route only |
| Not allowed | Any write route, any other internal route, any operator route |

**Billing's side (W-9, W-10; already designed).**

| Call | How |
|---|---|
| Pack list (B-P19) | Cached read, 60 s |
| Purchase (B-P20) | Always a fresh read; `ENTITLEMENTS_TIMEOUT_MS=1000` |
| Entitlements unreachable | Purchase fails closed with `503 ENTITLEMENTS_UNAVAILABLE`; pack list shows `eligible: false` |
| Until W-1 ships | Billing checks that `llm_token_quota_monthly` in the same response is finite (`BIL-ADD-1`) |
| Billing's own checks (always) | Subscription `active` and not collection-paused; IAM lifecycle `active`; not on trial (ENT-D6) |
| Egress | Billing's NetworkPolicy allows Entitlements in `commercial` on this `GET` only (LLD §10) |

**Done when:** in each environment, a Billing pod gets `200` with `addon_eligibility` on that route and `403` on any other Entitlements route.

---

## BL-OQ-4 — Billing is the only publisher of `TenantQuotaAddOnChanged`

**Owner:** Entitlements service (W-3, W-4). Billing service publishes (W-8). Commercial IaC provides the queue (W-11).

**What Entitlements first proposed.** Entitlements would produce its own `tenant_quota_add_on_changed` (`action` granted, revoked or expired), with `purchased_at` taken from the time Entitlements created the add-on row.

**Decision (TL, 2026-11-19).** Billing is the **only** publisher of `TenantQuotaAddOnChanged`, on `billing.events`. `purchased_at` is Billing's payment-confirmation time. Entitlements consumes the event and publishes nothing under that name.

**W-3 — Entitlements withdraws its own event.** The names below come from Entitlements' reply of 2026-11-19; the repository has no code yet, so they apply when it is written:
- remove `tenant_quota_add_on_changed` from `producedEvents`, `schemas/`, `glue_codec.go` and `api/asyncapi.yaml`;
- `AddonService.AddAddon`, `RemoveAddon` and `ExpireAddons` emit no add-on event under any name.

**W-4 — Entitlements consumes the purchase.**
- Queue `billing-entitlements-q` (subscribed to `billing-events`; created by W-11).
- On `reason: purchase`, create the grant for `delta` tokens. Idempotent on `billing_ref` (= `purchase_id`): a replay grants nothing.
- The 12-month expiry runs from Billing's `purchased_at`, not from when Entitlements wrote the row.
- The add-on balance is drawn down after the plan quota, oldest-expiring first (CD-55).

**Payload (Billing LLD §7.3.6; Glue registry `billing-events`, owned by Billing):**

| Field | Purchase | Refund |
|---|---|---|
| `tenant_id` | uuid (also in the envelope, D-1) | uuid |
| `resource` | `llm_tokens` (the only value in v1) | `llm_tokens` |
| `delta` | > 0, tokens added | < 0, tokens to reverse |
| `reason` | `purchase` | `refund` |
| `billing_ref` | `purchase_id` | `refund_id` |
| `purchase_ref` | — | `purchase_id` of the grant being reversed |
| `purchased_at` | payment-confirmation instant | — |
| `pack_code`, `pack_quantity` | set | — |

The schema is `additionalProperties: true`, so ignore fields you don't use.

**Billing's side (W-8; already designed).** One event per confirmed purchase, in the same commit as the purchase (outbox), with `billing_ref = purchase_id` (`BIL-ADD-2`). Envelope `source = commercial-billing`.

**Done when:** Entitlements publishes no add-on event under any name, and a purchase on `billing-entitlements-q` raises the tenant's add-on balance exactly once, including when replayed.

---

## BL-OQ-20 — Refunds and corrections

**Owner:** Entitlements service (W-5, W-6, W-7). Billing service publishes refunds (W-8). Audit Log deploys the refund label (W-12).

**What Entitlements first said.** Billing must not emit this event type, or any `action` values, in v1.

**Decision (TL, 2026-11-19; audit path confirmed 2026-11-23).** Billing owns the two money-moving reasons, `purchase` and `refund`. Entitlements owns everything else that changes an add-on balance (goodwill, migration credit, manual correction, expiry) as its own operator actions. These are audited through AL-5 and **never published as a bus event**. That last point was confirmed by the TL and Entitlements on 2026-11-23 (BL-OQ-23 c) and written into HLD v0.65 (D-8).

**W-5 — Entitlements applies a refund** (`reason: refund`, `delta < 0`, `purchase_ref`):
1. Find the grant created by `purchase_ref`.
2. Remove `min(|delta|, unused tokens of that grant)`. Never go below zero, and never remove tokens already consumed.
3. Idempotent on `billing_ref` (= `refund_id`).
4. If Entitlements' design records a balance reduction in AL-5, write that entry. Audit already records Billing's refund event as `billing.token_addon.refunded`.

Billing never computes the remaining balance (`BIL-ADD-4`). Refunds of add-on invoices are operator-only, for `billing_error`, `duplicate_charge` or `fraud` (`BIL-ADD-3`), so they're rare.

**W-6 — Corrections and expiry stay with Entitlements.**

| Action | Entitlements does | Never |
|---|---|---|
| Goodwill, migration credit, manual correction | Its own operator route; AL-5 `billing.token_addon.granted` / `.adjusted` / `.revoked` | `TenantQuotaAddOnChanged`; any event on `entitlement-events` |
| Expiry after 12 months | Its expiry job; AL-5 `billing.token_addon.expired` | Same |

The HLD's former reserved reasons (`promotional_grant`, `operator_correction`, `migration_credit`, `contract_entitlement`) are no longer on Billing's event (HLD v0.65 §9.4). Billing never calls Entitlements' operator route.

**W-7 — Entitlements LLD.** When it is written, it should state:
- the consumed `reason` enum is `purchase` | `refund`, with `delta` non-zero and `purchase_ref` optional;
- corrections and expiry are Entitlements actions audited through AL-5;
- *"Entitlements does not publish add-on balance change events on `entitlement-events`. Audit visibility for goodwill grants, corrections, expiries and similar balance changes is provided exclusively through AL-5 audit submissions."* (wording approved for S-8).

**Audit's side (W-12).** The `billing.token_addon.refunded` type is merged ([iam-audit-log#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24)). Until that Audit Log release is deployed in an environment, refunds there are stored as `billing.token_addon.purchased`. They're mislabelled, but nothing is lost: `reason` and the negative `delta` are kept in the metadata.

**Done when:** a refund event reduces that grant's unused balance and never goes below zero, replays are no-ops, and Entitlements emits no bus event for corrections or expiry.

---

## Responsibility matrix

R = does the work · A = accountable for sign-off · C = consulted · I = informed

| Work | Entitlements service | Billing service | Commercial IaC / SRE | Audit Log (IAM) | Commercial TL |
|---|---|---|---|---|---|
| W-1 eligibility per resource | R, A | C | | | I |
| W-2 admit Billing read-only | R, A | C | | | I |
| W-3 withdraw own add-on event | R, A | I | | I | I |
| W-4 consume purchase | R, A | C | | | I |
| W-5 apply refund | R, A | C | | I | I |
| W-6 corrections and expiry via AL-5 only | R, A | I | | C | I |
| W-7 Entitlements LLD text | R, A | C | | | C |
| W-8 publish purchase and refund | C | R, A | | I | |
| W-9 read eligibility, fail closed | C | R, A | | | |
| W-10 Billing egress | C | R, A | | | |
| W-11 `billing-entitlements-q` | I | C | R, A | | |
| W-12 deploy refund entry type | | I | | R, A | |

## Related IAM items

IAM has one item tied to this work, now resolved, and two follow-ups from the sign-off that are **not** part of it. They're listed here so readers know where they're tracked.

| IAM item | Register | Relation to this document | Status | Tracked in |
|---|---|---|---|---|
| **A-6:** Audit adds no bus subscription to Entitlements add-on events | BL-OQ-23 (c) | **Related.** It underpins W-6: corrections and expiry reach Audit only through AL-5. IAM confirmed it on condition that Entitlements publishes no add-on event | **Done.** The condition was met when the TL and Entitlements approved option 1 (sign-off S-8, 2026-11-23). No Audit Log change | `billing-iam-actions.md` A-6 |
| **A-5:** Audit entry type `billing.token_addon.refunded` | BL-OQ-23 (b) | **Related.** It is W-12 here | Merged ([iam-audit-log#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24)); deploy per environment | `billing-iam-actions.md` A-5 |
| **A-9:** B-O16 in the OP-G4 test; Audit type `billing.invoice.suspension_disposition` | BL-OQ-25 | **Not related.** From S-2: the operator's decision on invoices held during a suspension | Requested | `billing-iam-actions.md` A-9; `billing-register-open-items.md` |
| **A-10:** role `pci_owner_reviewer`; B-O17 in the OP-G4 test; Audit type `billing.provider.certification_reviewed` | BL-OQ-26 | **Not related.** From S-7: payment-provider certification approval | Requested | `billing-iam-actions.md` A-10; `billing-register-open-items.md` |

The operator path (BL-OQ-14: OP-G1 to OP-G4) also affects Entitlements' own operator routes, including the add-on correction route in W-6. That joint test is Entitlements item E-6 in `billing-open-items-entitlements.md`, not a Billing build item.

## Order of work

1. **Entitlements LLD (W-7)** written with these contracts, so the code follows the decision rather than the old reply.
2. **W-11** queue created per environment. W-4 and W-5 can't receive events before it exists.
3. **W-3** withdrawal, then **W-4 / W-5** consumer. Build these together: a consumer that also still publishes would create two sources for the same balance.
4. **W-1 / W-2** eligibility and access, tested from a Billing pod (W-9, W-10).
5. **W-12** deployed before add-on refunds are possible in that environment.
6. Token add-ons switch on (`FEATURE_TOKEN_ADDONS=true`) only once these are done **and** BL-OQ-18 (packs) and BL-OQ-19 (Model Gateway proof) are closed.

## Checklist

**Entitlements service**
- [ ] W-1: `addon_eligibility` per resource on the effective set (`trial`, `not_active`, `unlimited_quota`)
- [ ] W-2: `commercial-billing` admitted read-only on `GET /api/v1/internal/tenants/:id/entitlements` only, in each environment
- [ ] W-3: `tenant_quota_add_on_changed` removed from produced events, schemas, codec and AsyncAPI
- [ ] W-4: purchase consumed from `billing-entitlements-q`; idempotent on `billing_ref`; expiry from `purchased_at`
- [ ] W-5: refund consumed; unused tokens of `purchase_ref` only; never below zero; idempotent on `billing_ref`
- [ ] W-6: corrections and expiry audited through AL-5 only; no event on `entitlement-events`
- [ ] W-7: Entitlements LLD states the consumed schema, reason ownership and the no-event rule

**Billing service** (in the LLD; built with Billing)
- [ ] W-8: `TenantQuotaAddOnChanged` for `purchase` and `refund` (`BIL-ADD-2`, `BIL-ADD-4`); schema in Glue `billing-events`
- [ ] W-9: fresh read on purchase, 60 s cache on pack list, `503` fail-closed, interim finite-quota check
- [ ] W-10: egress to Entitlements on that `GET` only

**Commercial IaC / SRE**
- [ ] W-11: `billing-entitlements-q`, DLQ and SNS subscription to `billing-events`, per environment

**Audit Log (IAM)**
- [ ] W-12: release at or after `0e12f2c` deployed per environment
