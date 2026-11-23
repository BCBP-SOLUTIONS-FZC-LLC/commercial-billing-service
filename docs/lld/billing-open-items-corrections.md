# Billing Open Items — Corrections to Team Responses

| Field | Value |
|---|---|
| Re | `billing-open-items-team-response.md` (2026-11-19) |
| From | Commercial Team (Billing), on behalf of the Commercial TL |
| Date | 2026-11-19 |
| Billing LLD | `commercial-lld-billing-service.md` rev 0.8 |
| Audience | IAM — Audit Log, IAM platform/SRE (`billing-audit-q`), Commercial Entitlements |

## Decision principle

**Billing owns commercial transactions. Entitlements owns quota state. An event originates in the service that owns the business action.**

| Business action | Owner | Event |
|---|---|---|
| Token add-on purchased (payment confirmed) | Billing | `TenantQuotaAddOnChanged {reason: "purchase"}` on `billing.events` |
| Add-on invoice refunded (refund succeeded) | Billing | `TenantQuotaAddOnChanged {reason: "refund"}` on `billing.events` (new) |
| Goodwill grant, migration credit, manual correction | Entitlements | Entitlements' own event (not `TenantQuotaAddOnChanged`); audit via AL-5 |
| Add-on expiry (12 months) | Entitlements | Entitlements' own event; audit via AL-5 |

This keeps HLD v0.64 §5.1, §8.6.2, §9.1 and §9.4 for the purchase. It adds a `refund` reason to Billing's event and moves the reserved correction reasons off it. The HLD amendment is requested as Billing D-8.

---

## Part 1 — IAM (Audit Log, `billing-audit-q`)

The IAM Core responses (BL-OQ-2, D-3, D-4, SRE-6) are accepted as written; thank you. The corrections below are for Audit Log and the `billing-audit-q` subscription (SRE-8). The Entitlements reply asked IAM to filter `tenant_quota_add_on_changed`. Please don't apply that.

### C-1 — SRE-8: filter on Billing's event (correction)

| | |
|---|---|
| **Response said** | `billing-audit-q` subscription filter must add `tenant_quota_add_on_changed` (Entitlements' event) to its `FilterPolicy` |
| **Correct** | `billing-audit-q` subscribes to SNS `billing-events` (topic owned by Billing) and must receive **`TenantQuotaAddOnChanged`**, the event Billing publishes. No subscription to any Entitlements add-on event |
| **Why** | TL decision BL-OQ-4: Billing is the only publisher of `TenantQuotaAddOnChanged`. Entitlements withdraws `tenant_quota_add_on_changed` |
| **Change** | Audit Log's convention is that its queues have **no** filter policy (catch-all sink, AL-EVT-2). If `billing-audit-q` follows it, no Terraform change is needed: confirm only that the consumer maps the type (C-2). If `billing-audit-q` was created with a filter (Commercial HLD v0.64 §9.1 says "IAM extends the filter"), add `TenantQuotaAddOnChanged` to the filter on the `EventType` message attribute. Please confirm which applies |

### C-2 — Map the new `refund` reason (new request, BL-OQ-23 b)

| | |
|---|---|
| **Today** | Audit consumer maps `TenantQuotaAddOnChanged` → `billing.token_addon.purchased` |
| **Needed** | Map by `data.reason`: `purchase` → `billing.token_addon.purchased`; `refund` → **new** `billing.token_addon.refunded` (`security_3y`, bus-sourced, producer Billing) |
| **Payload (refund)** | `{tenant_id, resource: "llm_tokens", delta: -<tokens>, reason: "refund", billing_ref: <refund_id>, purchase_ref: <purchase_id>}`. `delta` is negative for a refund |
| **Schema** | Billing's `TenantQuotaAddOnChanged.json`: `delta` is a non-zero integer (was `minimum: 1`); `reason` enum is `purchase` \| `refund`; new optional `purchase_ref`. Please update the consumed-schema copy |
| **Until shipped** | Refund events stored as `billing.token_addon.purchased` (Audit's existing mapping ignores `reason`): mislabelled as a purchase, not lost; the payload (`reason`, negative `delta`) is kept in metadata |

### C-3 — No consumer for Entitlements' add-on changes (confirm, BL-OQ-23 c)

Entitlements' corrections and expiry already reach Audit through AL-5 direct-write (`billing.token_addon.granted`, `.adjusted`, `.revoked`, `.expired`, HLD v0.64 §11.6). Subscribing to an Entitlements bus event for the same changes would record each one twice. Please confirm Audit adds no such subscription.

### IAM checklist

- [ ] SRE-8: confirm `billing-audit-q` receives `TenantQuotaAddOnChanged` (no filter, or `EventType` filter extended) (C-1)
- [ ] Audit taxonomy: `billing.token_addon.refunded`, mapping by `reason` (C-2)
- [ ] Consumed schema updated: `delta` non-zero, `reason` enum `purchase | refund`, `purchase_ref` (C-2)
- [ ] Confirm no subscription to Entitlements add-on events (C-3)

---

## Part 2 — Commercial Entitlements

### C-4 — BL-OQ-4: Billing is the only publisher of `TenantQuotaAddOnChanged`

| | |
|---|---|
| **Response said** | Entitlements now produces `tenant_quota_add_on_changed` (`action` granted / revoked / expired, `purchased_at = addon.CreatedAt`) |
| **Correct** | Billing publishes `TenantQuotaAddOnChanged` on `billing.events`. Entitlements **consumes** it on `billing-entitlements-q`, idempotent on `billing_ref`. `purchased_at` is Billing's payment-confirmation time; Entitlements stores it as the 12-month expiry basis |
| **Please change** | Remove `tenant_quota_add_on_changed` from `producedEvents`, `schemas/`, `glue_codec.go` and `api/asyncapi.yaml`, or rename it (C-5). `AddonService.AddAddon` / `RemoveAddon` / `ExpireAddons` must not emit `TenantQuotaAddOnChanged` |

### C-5 — BL-OQ-20: reason ownership (misread)

| | |
|---|---|
| **Response said** | "Billing must not emit this event type or any `action` values in v1" |
| **Correct** | Billing owns `purchase` and `refund`. Entitlements owns correction (`promotional_grant`, `operator_correction`, `migration_credit`, `contract_entitlement`) and expiry |
| **Please change** | If Entitlements publishes balance changes it makes itself, use a separate event, for example `TenantAddOnBalanceChanged` on `entitlement.events`, with `action` granted / adjusted / revoked / expired. Keep AL-5 as the audit path for those |
| **New consumer behaviour** | On `TenantQuotaAddOnChanged {reason: "refund", delta < 0, purchase_ref}`: remove at most the unused tokens of grant `purchase_ref`, never below zero and never consumed tokens. Idempotent on `billing_ref` (= refund id). Billing does not compute the balance |

### C-6 — BL-OQ-3: per-resource eligibility, not `addon_eligible`

| | |
|---|---|
| **Response said** | `addon_eligible: bool`, true when the tenant is active and **either** the token quota **or** the API-request budget is finite |
| **Correct** | Generic `addon_eligible` is rejected. A tenant with unlimited tokens and a finite API budget would show `true`, and Billing would sell tokens the tenant cannot use |
| **Please change** | Return eligibility per resource on the effective set, for example `"addon_eligibility": {"llm_tokens": {"eligible": true, "reason": null}, "api_requests": {"eligible": false, "reason": "unlimited_quota"}}`. Reasons: `trial`, `not_active`, `unlimited_quota`. Billing reads `llm_tokens` only in v1 |
| **Until shipped** | Billing reads `llm_token_quota_monthly` from the same response |

### C-7 — Billing → Entitlements access (approved, read-only)

| | |
|---|---|
| **Response said** | "No NetworkPolicy or mTLS change required" |
| **Correct** | Billing runs in namespace `billing`, not `commercial`, so it needs admission. TL approved **read-only** access |
| **Please change** | Admit `cluster.local/ns/billing/sa/commercial-billing` (namespace `billing`, pod label `app.kubernetes.io/name: commercial-billing`) on **`GET /api/v1/internal/tenants/:id/entitlements` only**: NetworkPolicy ingress plus Istio AuthorizationPolicy (method `GET`, that path). No write routes |

### Entitlements checklist

- [ ] Withdraw or rename `tenant_quota_add_on_changed`; never emit `TenantQuotaAddOnChanged` (C-4, C-5)
- [ ] Consume `TenantQuotaAddOnChanged` `reason: refund` (C-5)
- [ ] Replace `addon_eligible` with per-resource eligibility (C-6)
- [ ] Admit Billing read-only on the entitlements GET route (C-7)

---

## What Billing changed (LLD rev 0.8)

- `TenantQuotaAddOnChanged` schema: `reason` enum `purchase | refund`; `delta` non-zero; new `purchase_ref` (§7.3.6).
- New invariant `BIL-ADD-4`: a succeeded refund of an add-on invoice emits `reason: refund` once, in the same transaction (`refunds.addon_purchase_id`, `addon_tokens`, `addon_event_id`).
- §8.11 and B-O3: the operator no longer calls Entitlements to reverse tokens after an add-on refund.
- `BIL-ADD-1` reads per-resource eligibility once Entitlements ships it.
- Register: BL-OQ-3, BL-OQ-4, BL-OQ-20 decided. New BL-OQ-23 (Audit Log). New D-8 (HLD reason list).
