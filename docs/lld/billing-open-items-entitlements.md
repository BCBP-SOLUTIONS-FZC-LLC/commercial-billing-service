# Billing — Open Items for Commercial Entitlements

| Field | Value |
|---|---|
| From | Commercial Team (Billing) |
| To | Commercial Entitlements; Commercial TL (one decision, E-3) |
| Date | 2026-11-23 |
| Billing LLD | `commercial-lld-billing-service.md` rev 0.22 (base HLD v0.64) |
| Background | TL decisions of 2026-11-19 (LLD rev 0.8); corrections C-4 to C-7 in `billing-open-items-corrections.md`; IAM caveat on A-6 in `billing-iam-actions-reply.md` |
| Deployment state | No service is in production; all run in dev only. "Done" means merged and promoted with the environment |
| Status source | Status reflects the last Entitlements reply (2026-11-19). The Entitlements repository has no code yet, so nothing below could be checked against an implementation |

## Principle (TL, 2026-11-19)

**Billing owns commercial transactions. Entitlements owns quota state. The service that owns a business action publishes its event.**

| Business action | Owner | How it travels |
|---|---|---|
| Token add-on purchased (payment confirmed) | Billing | `TenantQuotaAddOnChanged {reason: "purchase"}` on `billing.events` |
| Add-on invoice refunded (refund succeeded) | Billing | `TenantQuotaAddOnChanged {reason: "refund"}` on `billing.events` |
| Goodwill grant, migration credit, manual correction | Entitlements | Entitlements operator route; audit through AL-5. Never `TenantQuotaAddOnChanged` |
| Add-on expiry (12 months) | Entitlements | Entitlements' expiry job; audit through AL-5. Never `TenantQuotaAddOnChanged` |

## Summary

| # | Item | Billing ID | Type | Blocks | Status |
|---|---|---|---|---|---|
| E-1 | Withdraw Entitlements' `tenant_quota_add_on_changed`; consume Billing's `TenantQuotaAddOnChanged` (`reason: purchase`) | BL-OQ-4 | Code | Token add-on sales | **Decided**, to implement |
| E-2 | Apply `TenantQuotaAddOnChanged {reason: "refund"}` | BL-OQ-20 | Code | Add-on refunds (B-O3) | **Decided**, to implement |
| E-3 | Should Entitlements publish **any** add-on balance event on `entitlement-events`? | BL-OQ-23 (c) | **Decision** (TL + Entitlements) | Billing design sign-off (Section B) | **Closed** 2026-11-23: no (option 1) |
| E-4 | Replace `addon_eligible: bool` with per-resource eligibility | BL-OQ-3 | Code (API) | Correct add-on eligibility | **Decided**, to implement |
| E-5 | Give Billing read-only access to the effective entitlements route | BL-OQ-3 (access) | Config | Add-on purchase (fails closed `503` otherwise) | **Decided**, to implement |
| E-6 | Joint OP-G4 operator test in each environment | BL-OQ-14 (A-3) | Test | Operator routes per environment | Waits on OP-G2 (platform edge) and OP-G3 (IAM) |
| E-7 | Entitlements LLD text: reserved reasons move off `TenantQuotaAddOnChanged` | D-8 | Docs | — | Open |
| E-8 | Staff role `commercial_trial_manager` | OP-G5 | Config | — | Only if Product asks (CD-51) |

E-3 was the only item that blocked **design sign-off**; it was closed on 2026-11-23 (option 1). Nothing left here blocks design sign-off. E-1, E-2, E-4 and E-5 are **go-live gates** for token add-ons, which already sit behind `FEATURE_TOKEN_ADDONS=false` (BL-OQ-19).

---

## E-1 — Billing is the only publisher of `TenantQuotaAddOnChanged` (BL-OQ-4, C-4)

**Last reply said:** Entitlements produces `tenant_quota_add_on_changed` (`action` granted / revoked / expired, `purchased_at = addon.CreatedAt`).

**Decided:** Billing publishes `TenantQuotaAddOnChanged` on `billing.events`. Entitlements **consumes** it on `billing-entitlements-q`.

**Please change:**
- Remove `tenant_quota_add_on_changed` from `producedEvents`, `schemas/`, `glue_codec.go` and `api/asyncapi.yaml`.
- `AddonService.AddAddon` / `RemoveAddon` / `ExpireAddons` must not emit `TenantQuotaAddOnChanged` under any name.
- On `reason: "purchase"`: create the grant, idempotent on `billing_ref` (= `purchase_id`).
- Use Billing's `purchased_at` (payment-confirmation time) as the 12-month expiry basis, not the time Entitlements created the row.

**Payload (Billing LLD §7.3.6, Glue registry `billing-events`):**

| Field | Purchase | Refund |
|---|---|---|
| `tenant_id` | uuid | uuid |
| `resource` | `llm_tokens` (only value Billing emits in v1) | `llm_tokens` |
| `delta` | > 0, tokens added | < 0, tokens to reverse |
| `reason` | `purchase` | `refund` |
| `billing_ref` | `purchase_id` | `refund_id` |
| `purchase_ref` | — | `purchase_id` of the grant being reversed |
| `purchased_at` | payment confirmation instant | — |
| `pack_code`, `pack_quantity` | set | — |

The schema is `additionalProperties: true`; ignore fields you don't use.

**Done when:** Entitlements publishes no add-on event under Billing's type name, and a purchase on `billing-entitlements-q` raises the tenant's add-on balance exactly once.

## E-2 — Apply the `refund` reason (BL-OQ-20, C-5)

**Last reply said:** "Billing must not emit this event type or any `action` values in v1."

**Decided:** Billing owns `purchase` and `refund`. Entitlements owns correction and expiry.

**Consumer behaviour on `{reason: "refund", delta < 0, purchase_ref}`:**
1. Find the grant created by `purchase_ref`.
2. Remove `min(|delta|, unused tokens of that grant)`. Never go below zero, and never remove tokens already consumed.
3. Idempotent on `billing_ref` (= `refund_id`).
4. Write the AL-5 entry your design uses for a balance reduction, if any. Audit already records Billing's event as `billing.token_addon.refunded` (iam-audit-log#24).

Billing never computes the remaining balance (`BIL-ADD-4`). Refunds of add-on invoices exist only as operator actions for `billing_error`, `duplicate_charge` or `fraud` (`BIL-ADD-3`).

**Done when:** a refund event reduces that grant's unused balance and replays are no-ops.

## E-3 — Does Entitlements publish an add-on balance event? (BL-OQ-23 c) — closed

**Conflict to resolve.** Correction C-5 (19 Nov) suggested that Entitlements *may* publish its own `TenantAddOnBalanceChanged` on `entitlement.events`. IAM's reply (A-6) then flagged that Audit's `entitlement-audit-q` is catch-all (AL-EVT-2). Any add-on event there would be stored as `entitlement.unknown`, raise an alarm (AL-EVT-4) and duplicate the AL-5 entries (`billing.token_addon.granted` / `.adjusted` / `.revoked` / `.expired`) that Entitlements already writes.

| Option | Effect |
|---|---|
| **1. No add-on balance event on `entitlement-events`; AL-5 is the only audit path (recommended, IAM's preference)** | No Audit change; no duplicates. Billing needs no such event either |
| 2. Publish one | Audit needs a new LLD decision: an explicit skip, or a new entry type. Needs a named consumer to justify it |

**Asked of the TL and Entitlements:** confirm option 1, or name the consumer that needs option 2 and raise it with Audit Log.

**Decision: ✅ Option 1 approved by the Commercial TL and Entitlements, 2026-11-23.** No `TenantAddOnBalanceChanged` (or equivalent) on `entitlement-events`; AL-5 is the only audit path. No Audit Log change and no new consumer. Billing LLD §7.3.6 and §8.11 (rev 0.22) now say: *"Entitlements does not publish add-on balance change events on `entitlement-events`. Audit visibility for goodwill grants, corrections, expiries and similar balance changes is provided exclusively through AL-5 audit submissions."* **For the Entitlements LLD:** please state the same, and keep writing the AL-5 entries (`billing.token_addon.granted` / `.adjusted` / `.revoked` / `.expired`) for those actions.

## E-4 — Per-resource eligibility instead of `addon_eligible` (BL-OQ-3, C-6)

**Last reply said:** `addon_eligible: bool`, true when the tenant is active and **either** the token quota **or** the API-request budget is finite.

**Why rejected:** a tenant with unlimited tokens and a finite API budget would show `true`, and Billing would sell tokens the tenant cannot use.

**Please return**, on the effective set from `GET /api/v1/internal/tenants/:id/entitlements`:

```json
"addon_eligibility": {
  "llm_tokens":   { "eligible": true,  "reason": null },
  "api_requests": { "eligible": false, "reason": "unlimited_quota" }
}
```

Reasons: `trial`, `not_active`, `unlimited_quota`. Billing reads `llm_tokens` only in v1.

**Until shipped:** Billing checks that `llm_token_quota_monthly` is finite from the same response (`BIL-ADD-1`). Billing still checks its own rules: subscription `active` and not collection-paused, IAM lifecycle `active`, not on trial (ENT-D6).

**How Billing calls it:** pack list (B-P19) uses a 60 s cached read; purchase (B-P20) always re-reads fresh, with `ENTITLEMENTS_TIMEOUT_MS=1000`. If Entitlements is unreachable the purchase fails closed with `503 ENTITLEMENTS_UNAVAILABLE`, and the pack list shows `eligible: false`.

## E-5 — Billing → Entitlements read-only access (C-7)

**Last reply said:** "No NetworkPolicy or mTLS change required."

**Why it is needed:** Billing runs in namespace `billing`, not `commercial`.

**Please admit**, on `GET /api/v1/internal/tenants/:id/entitlements` **only**:
- principal `cluster.local/ns/billing/sa/commercial-billing`;
- NetworkPolicy ingress from namespace `billing`, pod label `app.kubernetes.io/name: commercial-billing`;
- Istio AuthorizationPolicy limited to method `GET` and that path. No write routes, no operator routes.

**Done when:** a Billing pod gets `200` on that route and `403` on any other Entitlements route, in each environment.

## E-6 — Joint OP-G4 run (BL-OQ-14, A-3)

When OP-G2 (platform edge) and OP-G3 (IAM) are done, run the end-to-end operator test through the gateway in each environment, jointly with IAM and Billing. Entitlements covers its own operator routes, including the add-on correction and shortfall routes. Until then every `/api/v1/operator/*` route returns `403`.

## E-7 — Entitlements LLD text (D-8)

HLD §9.4 still lists the reserved reasons `promotional_grant`, `operator_correction`, `migration_credit` and `contract_entitlement` on `TenantQuotaAddOnChanged`. After the TL decision they belong to Entitlements' own operator actions. Please make the Entitlements LLD match: consumed `reason` enum `purchase | refund`, `delta` non-zero, optional `purchase_ref`. The HLD note is requested separately from the HLD owner.

## E-8 — Staff role `commercial_trial_manager` (OP-G5)

Only needed if Product asks for staff, other than platform operators, to assign trial profiles (CD-51). Not a Billing need.

## Already agreed (for reference, no action)

| Item | State |
|---|---|
| `billing-entitlements-q` receives `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantQuotaAddOnChanged`. Entitlements re-reads I-17 and never reads Billing | As in HLD §5.2 and Billing LLD §7.3.9 |
| Model tier and trial-profile values (PD-LLM-1, ENT-D6) are Catalog / Entitlements data. Billing sends only the plan in `TenantPlanChanged` | No Billing change (LLD rev 0.6) |
| Drawdown shortfalls (ENT-D9 / D11 / D12) and deferred expiry (ENT-D8) are Entitlements records. A "billing escalation" disposition is actioned by an operator through B-O2 | No Billing route or event (LLD rev 0.14, §1.1) |

## Checklist

- [ ] E-1: withdraw `tenant_quota_add_on_changed`; consume Billing's purchase event, idempotent on `billing_ref`; expiry from `purchased_at`
- [ ] E-2: consume `reason: refund` (unused tokens of `purchase_ref` only, never below zero)
- [x] E-3: TL + Entitlements confirmed option 1 (no add-on event on `entitlement-events`), 2026-11-23
- [ ] E-4: `addon_eligibility` per resource on the effective set
- [ ] E-5: admit `commercial-billing` read-only on the entitlements GET route, in each environment
- [ ] E-6: joint OP-G4 run in each environment (after OP-G2 / OP-G3)
- [ ] E-7: Entitlements LLD consumed schema and reason ownership updated
