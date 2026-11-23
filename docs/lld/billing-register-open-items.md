# Billing LLD §16.1 Register — Open Items Only

| Field | Value |
|---|---|
| Source | `commercial-lld-billing-service.md` rev 0.27, §16.1 "Billing register" (base HLD v0.65) |
| Date | 2026-11-23 |
| From | Commercial Team (Billing) |
| Scope | Every §16.1 row that is **not Closed**: 13 of the 26 rows. The 13 closed rows (BL-OQ-1, 2, 5, 6, 7, 10, 11, 13, 16, 17, 18, 21, 22) are left out |
| Deployment state | No service is in production; all run in dev only. "Go-live" means promotion through dev → staging → production |

**Closed since this list was first written:**
- BL-OQ-18 (token packs), Product + Finance 2026-11-23: Product and Finance finalised the Phase 1 token-pack catalogue (pack sizes, prices and any caps); the pack records are loaded into the commercial catalogue (`token_packs`, B-O12). Billing's existing add-on purchase design supports it without code or design change.
- BL-OQ-22 (final prices), Product 2026-11-23: Product completed the token-budget validation against PD-LLM-1 R-3 and ENT-D6 and finalised the Phase 1 commercial pricing; prices are loaded into `plan_prices`. Resolved through configuration/data only; no code or design change. Token-pack prices (`token_packs`) are not part of this closure and stay with BL-OQ-18.
- BL-OQ-17 (Stripe UAE terms), Legal 2026-11-23: Legal reviewed the Stripe UAE data-location and processor terms and accepted them without required amendments. Billing may proceed with the planned Stripe UAE account configuration. No additional design, legal or implementation work remains.
- BL-OQ-5 (launch currency), Finance 2026-11-23: USD remains the default currency. Market-specific currencies are configured through `plan_prices`; AED pricing can be introduced via B-O11 without application changes. No further design or implementation work is required.

## Read this first

**None of these items blocks the design sign-off.** All nine design decisions (S-1 to S-9 in `billing-design-signoff-blockers.md`) were approved on 2026-11-23. What is left is one of three kinds (all business decisions and data items are now closed):

| Kind | Meaning | Items |
|---|---|---|
| **Decided, to build** | The decision is made; another team has to implement it | BL-OQ-3, 4, 20 |
| **Go-live gate** | Another team has to build, configure or deploy something before a Billing feature can be switched on. Billing's design already handles "not yet" (a feature flag, a `403` or a `503`) | BL-OQ-8, 9, 12, 14, 19, 23, 24, 25, 26 |
| **Deferred** | Planned for a later phase | BL-OQ-15 |

Each item has a **default**: what Billing does until the item closes. The system is safe in that state. It just can't offer the feature yet.

## Summary

| # | Item in one line | Owner | Kind | Billing default until closed | What it holds back |
|---|---|---|---|---|---|
| BL-OQ-14 | Operator path: gateway `jwt_authn` provider (OP-G2), then issuer config (OP-G3) and joint test (OP-G4) | Platform-edge; IAM | Go-live gate (**external**) | Every operator route answers `403` outside dev/test | **All operator routes**, including the Phase 1 conversion stub and provider certification |
| BL-OQ-26 | IAM role `pci_owner_reviewer`, OP-G4 test and Audit type for B-O17 | IAM; PCI owner group | Go-live gate | B-O17 answers `403`; no provider can be certified | **Taking payments through Stripe** (an uncertified provider takes no new accounts) |
| BL-OQ-9 | Billing's operator `HTTPRoute` lists every operator path | Billing | Go-live gate | Same `403` as BL-OQ-14 | Operator routes |
| BL-OQ-25 | OP-G4 test and Audit type for B-O16 | IAM | Go-live gate | Audit entries wait in `pending_audit_entries` | Operator decision on invoices held during a suspension |
| BL-OQ-8 | Signup BFF aligns to Billing's published contract | Web / growth | Go-live gate | `FEATURE_DIRECT_SIGNUP=false` | Direct paid signup |
| BL-OQ-12 | Notification messages for seat, add-on, downgrade and price-change events | Notification + Commercial | Go-live gate | Only past-due and cancelled emails | Customer emails; notice of price increases |
| BL-OQ-3 | Entitlements: per-resource add-on eligibility and read-only access for Billing | Entitlements | Decided, to build | Add-on purchase fails closed (`503`) | Token add-ons |
| BL-OQ-4 | Entitlements consumes Billing's `TenantQuotaAddOnChanged` and withdraws its own | Entitlements | Decided, to build | Billing emits the event | Token add-ons |
| BL-OQ-20 | Entitlements applies `reason: refund`; corrections stay Entitlements' own | Entitlements | Decided, to build | As written | Token add-on refunds |
| BL-OQ-19 | Model Gateway limit hot reload proven on a real cluster | AI team (+ SRE cluster) | Go-live gate | `FEATURE_TOKEN_ADDONS=false` | Token add-ons |
| BL-OQ-23 | Audit Log release with the `billing.token_addon.refunded` type, deployed per environment | IAM (Audit Log) | Go-live gate | Refunds recorded but labelled as purchases | Correct audit label on add-on refunds |
| BL-OQ-24 | Usage & Metering confirms the `UsagePeriodClosed` payload | Usage & Metering | Go-live gate (known risk) | Records kept for reference only; nothing priced from them | Billing's `usage-billing-q` consumer |
| BL-OQ-15 | Per-tenant provider choice; moving customers off a removed provider | Billing + Finance | Deferred (Phase 3) | One provider; fixed per account | Nothing in Phase 1 |

**What each launch needs:**

| To launch | Needs closed |
|---|---|
| Paid subscriptions through Stripe | BL-OQ-14, BL-OQ-9, BL-OQ-26 (certification) |
| Direct paid signup | The above, plus BL-OQ-8 |
| Token add-ons | The above, plus BL-OQ-3, 4, 19 and 20; BL-OQ-23 for the correct refund audit label |
| Price increases for existing customers | BL-OQ-12 (the renewal price-change message) |

---

## Operator path and IAM

### BL-OQ-14 — Operator path is not live yet (CD-52) — **the main external blocker**

**What it is.** Operators (platform staff) reach Billing's `/api/v1/operator/*` routes through the gateway with a platform-realm login. Four steps must happen in order. The numbering follows AuthZ Enrichment LLD §8.9.1; the HLD uses the same numbering since v0.65.

| Step | What | Owner | State |
|---|---|---|---|
| OP-G1 | Keycloak platform realm `bcbp-platform` | IAM | **Merged** ([iam-realm-provisioner#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60)); created on Realm Provisioner's next deploy. Operator users still to be created by hand |
| OP-G2 | Gateway `jwt_authn` provider for the platform realm | **Platform-edge** | Requested (`op-g2-platform-edge-request.md`), **not started**. The only external blocker |
| OP-G3 | `OPERATOR_REALM_ISSUER` set and deployed | IAM | After OP-G2 |
| OP-G4 | Joint end-to-end test through the gateway, per environment | IAM + Commercial | After OP-G3 |

**Billing's default.** Every operator route answers `403` outside dev and test (`BIL-AUTH-6`). This is the operator path being switched off, not a missing route.

**Why it matters.** Every operator action waits on this: the Phase 1 conversion stub (B-O1), refunds and credits, collection pause, price-book changes, conversion reversal, and **provider certification** (B-O14, B-O17). Without certification, Stripe can't take new accounts, so this blocks paid launch.

**To close:** platform-edge delivers OP-G2, IAM does OP-G3, then the joint OP-G4 run passes in each environment. IAM still owes a target date (OP-G6).

### BL-OQ-26 — PCI reviewer role for B-O17 (from BL-OQ-21)

**What it is.** The PCI owner group approved the certification rule on 2026-11-23. A Billing engineer submits a provider certification run (B-O14), and a different PCI owner group member approves it through B-O17. Billing recognises that person by a new platform-realm role. IAM needs to:

- (a) create the platform-realm role `pci_owner_reviewer`, held only by PCI owner group members, and pass it to Billing in `x-tenant-roles` on the operator path;
- (b) add B-O17 to the OP-G4 test (allowed with both roles; denied without `pci_owner_reviewer`);
- (c) add the Audit entry type `billing.provider.certification_reviewed` (`security_3y`).

The PCI owner group names who holds the role.

**Billing's default.** Until (a), B-O17 answers `403` to everyone, so no provider can be certified. Until (c), audit entries wait in `pending_audit_entries`; none are lost.

**Why it matters.** The registry only lets a provider with a `passed` certification take new accounts. **No certification means no paid launch through Stripe.**

**To close:** IAM action **A-10** in `billing-iam-actions.md` is done in each environment, and the PCI owner group has named the role holders.

### BL-OQ-9 — Billing's operator `HTTPRoute` (remaining part)

**What it is.** There's no IAM-side route list. Each service exposes its own operator paths through its own `HTTPRoute`. IAM's part, OP-G4 tests for B-O1 to B-O15, is merged ([iam-authz-enrichment#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43)).

**Remaining.** Billing's own chart lists every operator path. That's **B-O1 to B-O17** (B-O16 added in LLD rev 0.16, B-O17 in rev 0.21; the LLD row was corrected in rev 0.23). A path missing from the `HTTPRoute` can't be reached through the gateway.

**Billing's default.** Same `403` as BL-OQ-14.

**To close:** Billing writes its Helm chart with the full list (LLD §13.1). The gateway half of OP-G4 runs under BL-OQ-14.

### BL-OQ-25 — IAM coverage for B-O16 (from BL-OQ-1)

**What it is.** Finance approved B-O16, the operator's decision to charge or void an invoice held while a tenant was suspended. IAM needs to (a) add B-O16 to the OP-G4 test list and (b) add the Audit entry type `billing.invoice.suspension_disposition` (`security_3y`).

**Billing's default.** Until (b), audit entries wait in `pending_audit_entries`. B-O16 is reachable only once the operator path is live (BL-OQ-14).

**To close:** IAM action **A-9** in `billing-iam-actions.md`.

---

## Integration with other teams

### BL-OQ-8 — Signup BFF alignment (SGN-2) — Web / growth

**What it is.** Billing published the direct paid signup contract (B-I1, LLD §5.4.2). The Signup BFF has to:
- set `BILLING_TIMEOUT_MS` to at least 15000;
- map its fields: `PaymentToken` → `payment_method_ref`, `Plan` → `plan_code`, `Seats` → `licensed_seats`;
- handle 3-D Secure: a `402 PAYMENT_ACTION_REQUIRED` sends the browser to authenticate, then the BFF retries with the same idempotency key.

**Billing's default.** `FEATURE_DIRECT_SIGNUP=false`. Signup goes through trial conversion only.

**To close:** the BFF aligns, the Pact contract test is green, and the go-live order in LLD §18.7 is followed.

### BL-OQ-12 — Notification messages (NTF-3) — Notification + Commercial

**What it is.** New customer messages for:
- seats added;
- seat reduction applied, including renewal above the target;
- token add-on purchased;
- downgrade blocked at renewal;
- renewal top-up charged;
- **renewal price change scheduled**, showing the current and new unit price and the effective date.

The add-on expiry reminder is driven by Entitlements, not Billing.

**Billing's default.** Only the existing past-due and cancelled emails are sent.

**Why it matters.** Product requires customers to be told before a renewal uses a higher price (BL-OQ-16). **Until this message ships, a price increase for existing customers is shown only on the Billing page**, so price increases for live subscribers should wait for it.

**To close:** Notification and Commercial agree the messages and their triggers, and Notification ships them.

### BL-OQ-24 — `UsagePeriodClosed` contract unverified — Usage & Metering

**What it is.** Billing consumes `UsagePeriodClosed` (fields `period_start`, `period_end`, `tokens_total`, `api_requests_total`, `addon_consumed`, `limits`). The Usage & Metering LLD isn't written yet, so these fields come from the HLD only.

**Billing's default.** The records are reference-only (LLD §8.12): nothing is priced from them. A mismatch would affect operator and Finance views, not charges.

**To close:** Usage & Metering confirms the payload and registers its schema in Glue, before Billing's `usage-billing-q` consumer is switched on. This is a known risk, not a design blocker.

---

## Token add-ons (all behind `FEATURE_TOKEN_ADDONS=false`)

The Commercial TL decided BL-OQ-3, 4 and 20 on 2026-11-19. They're waiting on Entitlements to build them. The details, payloads and "done when" tests are in `billing-open-items-entitlements.md` (E-1, E-2, E-4, E-5).

### BL-OQ-3 — Eligibility and read access — Entitlements

**Decided.** The generic `addon_eligible` flag was rejected: it could let Billing sell tokens to a tenant whose token quota is unlimited. Instead, Entitlements returns per-resource eligibility (`llm_tokens`, `api_requests`) on the effective entitlement set. Billing gets **read-only** access to `GET /api/v1/internal/tenants/:id/entitlements` only, through NetworkPolicy and mTLS for `cluster.local/ns/billing/sa/commercial-billing`.

**Billing's default.** If Entitlements can't be reached or doesn't admit Billing, the purchase fails closed with `503`.

**To close:** Entitlements implements it (E-4, E-5).

### BL-OQ-4 — Single publisher of `TenantQuotaAddOnChanged` — Entitlements

**Decided.** Billing is the only publisher of `TenantQuotaAddOnChanged` on `billing.events`. It carries `purchased_at`, the payment-confirmation time and the basis for the 12-month expiry. Entitlements withdraws its own `tenant_quota_add_on_changed` and consumes Billing's.

**To close:** Entitlements implements it (E-1).

### BL-OQ-20 — Refunds and corrections — Entitlements

**Decided.** Billing owns the `purchase` and `refund` reasons. On a refund, Entitlements removes at most the grant's unused tokens, never going below zero. Goodwill, migration credit, manual correction and expiry are Entitlements' own operator actions, audited through AL-5 with no bus event (confirmed by BL-OQ-23 c and HLD v0.65 D-8).

**To close:** Entitlements implements refund handling (E-2) and states this in its LLD (E-7).

### BL-OQ-19 — Model Gateway hot reload not proven on a cluster — AI team

**What it is.** When a tenant buys tokens, the Model Gateway must pick up the higher limit. The AI team showed a 0.3 s limit reload and Envoy fail-open, but only without a cluster; nothing has run through Gateway v1.2.0.

**Billing's default.** `FEATURE_TOKEN_ADDONS=false`.

**To close.** SRE or the platform team provides a cluster with the Gateway, the rate-limit service and Valkey. Then AI-1, AI-3 and AI-5 must each pass on the pinned Gateway v1.2.0 image (Billing decision, 2026-11-19):

| Test | Shows |
|---|---|
| AI-1 | An entitlement change reaches the Gateway within 60 s |
| AI-3 | Calls continue and the SEV-2 alert fires when the rate-limit service fails |
| AI-5 | A purchased add-on actually raises the tenant's allowance (plan plus add-on) |

AI-5 is required. AI-1 and AI-3 alone would leave the commercial path unproven.

### BL-OQ-23 — Audit label for add-on refunds — IAM (Audit Log)

**What remains.** Parts (a) and (c) are closed. Part (b), the entry type `billing.token_addon.refunded`, is merged ([iam-audit-log#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24)) but takes effect in each environment only once that Audit Log version is deployed (release v0.3.0 suggested).

**Billing's default.** Until then, a refund event is stored as `billing.token_addon.purchased`. It's mislabelled, but nothing is lost: `reason` and the negative `delta` are kept in the metadata.

**To close:** Audit Log at or after commit `0e12f2c` is deployed in each environment.

---

## Deferred

### BL-OQ-15 — More than one payment provider (CD-56 follow-ups) — Billing + Finance, Phase 3

**What it is.** Choosing a provider per tenant once a second provider exists, and moving saved payment methods and live subscriptions when a provider is removed. The design is written (LLD §19.3) but not scheduled.

**Billing's default.** One provider (Stripe). An account's provider is fixed while it is live.

**To close:** a second provider is planned for Phase 3.

---

## Also outside the register

These are listed under §16.1 in the LLD but aren't register rows:

| Item | State |
|---|---|
| SRE-1 | Istio `STRICT` mTLS in staging — open |
| SRE-3 | Core migration `000004` and `parked-events-replay` deployed before direct signup, in each environment — code ready; ordering rule |
| SRE-7 | Remove any `membership-billing-q` — open |
| Commercial IaC | `billing-events`, `tenant-billing-q`, `usage-billing-q` (HLD §16 action 2) — open |

SRE-6 and SRE-8 are closed.

## Tracker

| # | Owner | Kind | Status (2026-11-23) |
|---|---|---|---|
| BL-OQ-14 | Platform-edge; IAM | Go-live gate | OP-G1 merged; **OP-G2 not started** |
| BL-OQ-26 | IAM; PCI owner group | Go-live gate | Requested (A-10) |
| BL-OQ-9 | Billing | Go-live gate | IAM part merged; Billing `HTTPRoute` when the chart is written |
| BL-OQ-25 | IAM | Go-live gate | Requested (A-9) |
| BL-OQ-8 | Web / growth | Go-live gate | Contract published |
| BL-OQ-12 | Notification + Commercial | Go-live gate | Open |
| BL-OQ-24 | Usage & Metering | Go-live gate | Open (known risk) |
| BL-OQ-3 | Entitlements | Decided, to build | Decided 2026-11-19 |
| BL-OQ-4 | Entitlements | Decided, to build | Decided 2026-11-19 |
| BL-OQ-20 | Entitlements | Decided, to build | Decided 2026-11-19 |
| BL-OQ-19 | AI team (+ SRE) | Go-live gate | Not proven; needs a cluster |
| BL-OQ-23 | IAM (Audit Log) | Go-live gate | (b) merged; deploy per environment |
| BL-OQ-15 | Billing + Finance | Deferred | Phase 3 |
