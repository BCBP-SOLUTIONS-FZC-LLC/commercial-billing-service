# Billing — Actions Required from IAM

| Field | Value |
|---|---|
| From | Commercial Team (Billing) |
| To | IAM: operator gateway / AuthZ Enrichment, Audit Log, Core (Org & Membership), Realm Provisioner |
| Date | 2026-11-19 |
| Billing LLD | `commercial-lld-billing-service.md` rev 0.21 (A-1 to A-8 written against rev 0.13; A-9 added 2026-11-23 from rev 0.16; A-10 added 2026-11-23 from rev 0.21) |
| Status | **IAM replied 2026-11-19** (`billing-iam-actions-reply.md`); #43, #24 and #60 merged the same day. Status column and checklist below reflect the reply. **A-9 and A-10 are new (2026-11-23), not yet answered** |
| Ingress | Envoy Gateway is the only ingress. There is no IAM-side operator route list: the platform-realm `jwt_authn` provider and AuthZ Enrichment admit the whole `/api/v1/operator/` prefix, and each service exposes its own paths with its own `HTTPRoute` |
| OP-G numbering | AuthZ Enrichment LLD §8.9.1 (OP-G1 platform realm, OP-G2 gateway provider, OP-G3 issuer, OP-G4 end-to-end test, OP-G5 staff role). The HLD's CD-52 row numbers them differently (Billing D-9) |
| Deployment state | No service is in production; every service, Billing included, runs in dev only. "Deploy" below means promotion through dev → staging → production |

## Summary

| # | Action | IAM owner | Billing item | Type | Blocks | Status (IAM reply) |
|---|---|---|---|---|---|---|
| A-1 | Extend the end-to-end operator test (OP-G4) to Billing routes B-O6 to B-O15 | AuthZ Enrichment + Commercial | BL-OQ-9 (approved) | Test | Closing BL-OQ-9 | **Done** — [iam-authz-enrichment#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43) merged (`f4af854`) |
| A-2 | Platform realm (OP-G1), then set `OPERATOR_REALM_ISSUER` and deploy (OP-G3) once platform-edge ships the gateway provider (OP-G2) | Realm Provisioner / Keycloak, AuthZ Enrichment | BL-OQ-14 | Config | Every operator route outside dev/test | OP-G1 **done** ([iam-realm-provisioner#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60) merged, `9e8fcab`); OP-G2 requested from platform edge (`op-g2-platform-edge-request.md`); OP-G3 last; OP-G6 date not set |
| A-3 | Run OP-G4 jointly with Commercial in each environment as it is promoted | AuthZ Enrichment + Commercial | BL-OQ-14 | Test | Operator routes per environment | Not started (needs A-2) |
| A-4 | Confirm `billing-audit-q` receives Billing's `TenantQuotaAddOnChanged` | Audit Log / IAM SRE | BL-OQ-23 (a), SRE-8 | Config confirm | Add-on purchase audit | **Done** — catch-all, no change |
| A-5 | Add entry type `billing.token_addon.refunded`, mapping by `reason` | Audit Log | BL-OQ-23 (b) | Code (taxonomy) | Add-on refund audit | **Done** — [iam-audit-log#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24) merged (`0e12f2c`); live per environment on Audit Log deploy |
| A-6 | Confirm no subscription to Entitlements add-on events | Audit Log | BL-OQ-23 (c) | Confirm | — | Confirmed, caveat: TL to confirm Entitlements publishes no add-on event |
| A-7 | Deploy Core `000004` and `parked-events-replay` before direct signup, in each environment | Core + Platform/SRE | SRE-3 | Deploy ordering | Direct paid signup per environment | **Done** — confirmed in Core's repo and chart |
| A-8 | Confirm Billing's admission on the five IAM routes | Core, Realm Provisioner | CD-19 | Confirm | Conversion, downgrade, seats, reversal | **Done** — admitted in dev, staging, prod |
| A-9 | **New.** Cover Billing's new operator route B-O16 in OP-G4, and add AL-5 entry type `billing.invoice.suspension_disposition` | AuthZ Enrichment + Commercial (a); Audit Log (b) | BL-OQ-25 (from BL-OQ-1 / CD-24) | Test (a); Code, taxonomy (b) | B-O16 through the gateway; its audit entries | **Requested 2026-11-23** |
| A-10 | **New.** Platform-realm role `pci_owner_reviewer` passed to Billing; OP-G4 coverage of B-O17; AL-5 entry type `billing.provider.certification_reviewed` | Realm Provisioner / Keycloak + AuthZ Enrichment (a); AuthZ Enrichment + Commercial (b); Audit Log (c) | BL-OQ-26 (from BL-OQ-21) | Config + Test + Code (taxonomy) | First payment-provider certification | Not started |

Already closed (no action): BL-OQ-2, D-3, D-4, SRE-6 (Core); BL-OQ-10 (Audit Log rev 0.34: Billing admitted to AL-5, seven entry types added).

---

## A-1 — Extend OP-G4 coverage to Billing's operator routes (BL-OQ-9, approved)

**Decision (2026-11-19):** approved, and reframed after checking how the operator path is built.

**No IAM route list to change.** Envoy Gateway is the only ingress. Its platform-realm `jwt_authn` provider (OP-G2) and AuthZ Enrichment's `ext_authz` check (`domain.OperatorRoutePrefix`) both admit the whole `/api/v1/operator/` prefix with the `platform_operator` role, and give routes without `/tenants/:id` the nil-UUID tenant sentinel. Each service exposes its own paths with its own `HTTPRoute`, so **Billing** lists B-O1 to B-O15 in its operator `HTTPRoute` (Billing chart, LLD §13.1). Today's `403` comes from the operator path being off (OP-G1 to OP-G3 not done, `BIL-AUTH-6`), not from a missing route.

**IAM's part:** OP-G4 (AuthZ Enrichment LLD §8.9.1) covers "Commercial's 10" from the 28 Oct reply, which include only Billing B-O1 to B-O5 (`conversions`, `adjustments`, `refunds`, `invoices`, `billing/reconciliation-runs`). Please add:

| # | Method | Path | Tenant in path |
|---|---|---|---|
| B-O6 | `GET` | `/api/v1/operator/billing/reconciliation-runs/:run_id` | No (nil-UUID sentinel) |
| B-O7 | `GET` | `/api/v1/operator/tenants/:id/subscription` | Yes |
| B-O8 | `POST` | `/api/v1/operator/tenants/:id/conversion-reversals` | Yes |
| B-O9 | `POST` | `/api/v1/operator/tenants/:id/conversion-sagas/:saga_id/actions` | Yes |
| B-O10 | `PUT` | `/api/v1/operator/tenants/:id/collection-pause` | Yes |
| B-O11 | `POST` | `/api/v1/operator/billing/plan-prices` | No |
| B-O12 | `POST` | `/api/v1/operator/billing/token-packs` | No |
| B-O13 | `POST` | `/api/v1/operator/billing/webhook-events/:provider/:provider_event_id/replay` | No |
| B-O14 | `POST` | `/api/v1/operator/billing/provider-certifications` | No |
| B-O15 | `POST` | `/api/v1/operator/billing/provider-certifications/:id/revoke` | No |

B-O14 and B-O15 were added in Billing LLD rev 0.3, after BL-OQ-9 was raised; they are requested in the same change.

Test cases per route:

| Case | Expected |
|---|---|
| Platform-realm token with `platform_operator` | Reaches Billing (`2xx`, or a Billing validation `4xx`; never a gateway `403`) |
| Platform-realm token without `platform_operator` | `403` (`DENY_ROLE_INSUFFICIENT`) |
| Tenant-realm token, any role | `403` |
| Malformed tenant id (B-O7 to B-O10) | `401` (`DENY_MALFORMED_IDENTITY`, IAM HLD §5.4) |
| Route without a tenant (B-O6, B-O11 to B-O15) | Allowed with the nil-UUID tenant sentinel |

**BL-OQ-9 closes** when Billing's `HTTPRoute` lists B-O1 to B-O15 (Billing) and IAM confirms OP-G4 covers B-O6 to B-O15.

## A-2 — Turn the operator path on (BL-OQ-14)

| Step | What | Owner | Status |
|---|---|---|---|
| OP-G1 | Keycloak platform realm: role `platform_operator`, MFA, short sessions, operator users | IAM (Realm Provisioner `EnsurePlatformRealm`, code done) | Per environment |
| OP-G2 | Gateway `jwt_authn` provider for the platform realm, on `/api/v1/operator/*` only | **Platform-edge** | Open — sole external blocker |
| OP-G3 | Set `OPERATOR_REALM_ISSUER` (and `JWT_OPERATOR_PAYLOAD_KEY` if OP-G2 needs it) in AuthZ Enrichment values; deploy | IAM | Blocked on OP-G1, OP-G2 |
| OP-G6 | Target date for OP-G1 to OP-G4, given to Commercial (asked 28 Oct; gates Phase 1) | IAM | Open |

Until then Billing's operator routes answer `403 INSUFFICIENT_ROLE` outside dev and test (`BIL-AUTH-6`); there is no Billing-side bypass.

## A-3 — Joint OP-G4 run per environment (BL-OQ-14)

Run OP-G4 jointly with Commercial (Billing and Entitlements) in each environment as it is promoted (all services are dev-only today).

## A-4 — `billing-audit-q` receives `TenantQuotaAddOnChanged` (BL-OQ-23 a, SRE-8)

TL decision: Billing is the only publisher of `TenantQuotaAddOnChanged`, on SNS `billing-events`. Audit Log's convention is that its queues have no filter policy. Please confirm either:
- `billing-audit-q` has no filter: no change, or
- it has a filter on the `EventType` message attribute: add `TenantQuotaAddOnChanged`.

Do **not** add Entitlements' `tenant_quota_add_on_changed`; Entitlements is withdrawing it.

## A-5 — New Audit entry type `billing.token_addon.refunded` (BL-OQ-23 b)

| `TenantQuotaAddOnChanged.data.reason` | Entry type | Tier |
|---|---|---|
| `purchase` | `billing.token_addon.purchased` (exists) | `security_3y` |
| `refund` (new) | **`billing.token_addon.refunded`** (new) | `security_3y` |

Schema changes in Billing's `TenantQuotaAddOnChanged.json`: `delta` is a non-zero integer (negative for a refund); `reason` enum is `purchase | refund`; new optional `purchase_ref`. Please update Audit's consumed-schema copy. Until this ships, refund events stored as `billing.token_addon.purchased` (Audit's existing mapping ignores `reason`): mislabelled as a purchase, not lost; the payload (`reason`, negative `delta`) is kept in metadata.

## A-6 — No subscription to Entitlements add-on events (BL-OQ-23 c)

Entitlements' corrections and expiry already reach Audit through AL-5 (`billing.token_addon.granted`, `.adjusted`, `.revoked`, `.expired`). Please confirm Audit adds no bus subscription for the same changes, so nothing is recorded twice.

## A-7 — Core migration ordering per environment (SRE-3)

Code is in Core's repo. In each environment, deploy Core migration `000004` and the `parked-events-replay` CronJob **before** Billing's direct signup is enabled there (`FEATURE_DIRECT_SIGNUP`). This is an ordering rule for promotion, not a production fix.

## A-8 — Confirm Billing's admission on IAM routes (CD-19)

Billing calls IAM as `commercial-system` from namespace `billing` (`cluster.local/ns/billing/sa/commercial-billing`) on exactly five routes. Core confirmed I-18 (`RequireInternalCaller()`, rev 2.24). Please confirm the rest in each environment:

| Route | Service | Use | Confirmed |
|---|---|---|---|
| I-11 seat usage | Core | Seat floor, renewal reduction | To confirm |
| I-18 plan-change check | Core | Downgrade check | ✅ Core rev 2.24 |
| RP-3 convert | Realm Provisioner | Trial → paid conversion | To confirm |
| RP-4 revert conversion | Realm Provisioner | Operator-approved reversal (CD-10) | To confirm |
| RP-11 realm status | Realm Provisioner | Operator view | To confirm |

## A-9 — Billing operator route B-O16 and its audit entry type (BL-OQ-25)

**Why this is new.** Finance approved CD-24 on 2026-11-23 (Billing BL-OQ-1, LLD rev 0.16). When an operator suspends a tenant, Billing pauses collection, and renewal invoices created during the pause are held as drafts. Finance requires an explicit, audited operator decision on each held draft (charge it or void it), never an automatic charge on reactivation. Billing adds operator route **B-O16** for that decision.

### (a) OP-G4 coverage — AuthZ Enrichment + Commercial

No AuthZ code change is expected: B-O16 sits under the `/api/v1/operator/` prefix that AuthZ already admits. Billing adds it to its own operator `HTTPRoute` (LLD §13.1). Please add it to the OP-G4 per-route tests, as A-1 did for B-O6 to B-O15:

| # | Method | Path | Tenant in path |
|---|---|---|---|
| B-O16 | `POST` | `/api/v1/operator/tenants/:id/invoices/:invoice_id/disposition` | Yes |

| Case | Expected |
|---|---|
| Platform-realm token with `platform_operator` | Allow; `x-tenant-roles: platform_operator`, `x-user-id` = operator `sub`, `x-tenant-id` = path tenant |
| Platform-realm token without `platform_operator` | `403` (`DENY_ROLE_INSUFFICIENT`) |
| Tenant-realm token, any role | `403` |
| Non-UUID tenant in path | `401` (`DENY_MALFORMED_IDENTITY`) |

The gateway half runs with A-3, once OP-G2 and OP-G3 are done.

### (b) New Audit entry type — Audit Log

| Field | Value |
|---|---|
| Entry type | `billing.invoice.suspension_disposition` |
| Tier | `security_3y` (as Billing's other operator-action types) |
| Source | AL-5 direct-write through `platform-audit` v0.1.2 outbox mode; producer `commercial-billing` (already admitted, Audit Log rev 0.34). Not bus-sourced |
| Actor | Operator `sub` (`x-user-id`) |
| Tenant | Path tenant |
| Written | Once per B-O16 decision, in the same transaction as the decision |
| Metadata | `invoice_id`, `provider_invoice_id`, `action` (`finalize` \| `void`), `amount_minor`, `currency`, `reason` (operator text, 10–1000 characters) |

**Until it ships:** entries wait in Billing's `pending_audit_entries` and drain once the type exists, as with BL-OQ-10. Nothing is lost.

**No change needed for B-O10.** It keeps using `billing.collection.paused` (already in the taxonomy). The reason is now mandatory, and the metadata gains `paused` (`true` \| `false`) so "continue billing during a suspension" is recorded too. Please confirm `billing.collection.paused` accepts that extra metadata field.

**BL-OQ-25 closes** when OP-G4 lists B-O16 and the Audit entry type is released. It is a go-live gate for B-O16, not a blocker for Billing's design sign-off.

---

## A-10 — PCI reviewer role and Billing operator route B-O17 (BL-OQ-26)

**Why this is new.** The PCI owner group approved Billing's certification rule on 2026-11-23 (Billing BL-OQ-21, LLD rev 0.21): a Billing engineer runs the payment-provider certification suite, and a **different** PCI owner group member approves it. PCI recommended that approval be restricted to the PCI owner group. Billing therefore splits submission from approval: B-O14 records a run as `pending_review`, and new route **B-O17** lets the reviewer approve or reject it. Billing must be able to tell that the caller is a PCI owner group member, which needs a role from IAM.

### (a) Role `pci_owner_reviewer` — Realm Provisioner / Keycloak + AuthZ Enrichment

| Item | Request |
|---|---|
| Realm | Platform realm (OP-G1), alongside `platform_operator` |
| Holders | PCI owner group members only. The PCI owner group names them; it is never granted by default |
| Header | AuthZ Enrichment passes it in `x-tenant-roles` on the operator path, for example `x-tenant-roles: platform_operator,pci_owner_reviewer`. Billing checks both on B-O17 only |
| Precedent | Same shape as the staff role in OP-G5 (`commercial_trial_manager`) |
| Audit | Role grants and removals audited as for other platform-realm roles |

Please confirm whether AuthZ Enrichment forwards additional platform-realm roles in `x-tenant-roles` today, or whether this needs a change.

### (b) OP-G4 coverage — AuthZ Enrichment + Commercial

| # | Method | Path | Tenant in path |
|---|---|---|---|
| B-O17 | `POST` | `/api/v1/operator/billing/provider-certifications/:id/review` | No (global) |

| Case | Expected |
|---|---|
| Platform-realm token with `platform_operator` and `pci_owner_reviewer` | Allow; both roles in `x-tenant-roles`, `x-user-id` = reviewer `sub` |
| Platform-realm token with `platform_operator` only | Allowed through AuthZ; Billing answers `403 INSUFFICIENT_ROLE` (end-to-end test asserts the `403`) |
| Platform-realm token without `platform_operator` | `403` (`DENY_ROLE_INSUFFICIENT`) |
| Tenant-realm token, any role | `403` |

### (c) New Audit entry type — Audit Log

| Field | Value |
|---|---|
| Entry type | `billing.provider.certification_reviewed` |
| Tier | `security_3y` |
| Source | AL-5 direct-write through `platform-audit` v0.1.2 outbox mode; producer `commercial-billing`. Not bus-sourced |
| Actor | Reviewer `sub` (`x-user-id`) |
| Tenant | `platform_tenant` (global record, as `billing.provider.certified`) |
| Written | Once per B-O17 decision, in the same transaction |
| Metadata | `certification_id`, `decision` (`approve` \| `reject`), `provider`, `adapter_version`, `suite_version`, `certified_by` (submitter `sub`), `reason` |

**Until it ships:** entries wait in Billing's `pending_audit_entries`, as with A-9.

**BL-OQ-26 closes** when the role is live and passed to Billing in each environment, OP-G4 lists B-O17, and the Audit entry type is released. It is a go-live gate for the first provider certification, not a blocker for Billing's design sign-off.

---

## Checklist for IAM

- [x] A-1 (#43 merged): OP-G4 covers B-O6 to B-O15 → with Billing's `HTTPRoute` change, **BL-OQ-9 closed**
- [~] A-2: OP-G1 done ([iam-realm-provisioner#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60) merged); OP-G2 requested from platform edge; OP-G3 after OP-G2 — OP-G1 platform realm per environment; OP-G3 `OPERATOR_REALM_ISSUER` once OP-G2 (platform-edge) ships
- [ ] A-3: OP-G4 joint run per environment
- [ ] OP-G6: give Commercial a target date for OP-G1 to OP-G4
- [x] A-4: confirm `billing-audit-q` receives `TenantQuotaAddOnChanged`
- [x] A-5 (#24 merged): add `billing.token_addon.refunded`; update the consumed schema
- [x] A-6 (caveat, see reply): confirm no Entitlements add-on subscription
- [x] A-7: Core `000004` and CronJob before direct signup, per environment
- [x] A-8: confirm I-11, RP-3, RP-4, RP-11 admission for `commercial-billing`
- [ ] A-9 (a): OP-G4 covers B-O16 (tenant in path), allow and deny cases
- [ ] A-9 (b): Audit entry type `billing.invoice.suspension_disposition` (`security_3y`, AL-5 direct-write, producer `commercial-billing`)
- [ ] A-10 (a): platform-realm role `pci_owner_reviewer` (PCI owner group members only), forwarded in `x-tenant-roles` on the operator path
- [ ] A-10 (b): OP-G4 covers B-O17 (no tenant in path), allow and deny cases
- [ ] A-10 (c): Audit entry type `billing.provider.certification_reviewed` (`security_3y`, AL-5 direct-write, producer `commercial-billing`)
