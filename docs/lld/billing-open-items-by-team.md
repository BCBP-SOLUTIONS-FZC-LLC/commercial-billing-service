# Billing Service — Open Items by Team

Source: `commercial-lld-billing-service.md` rev 0.26 (2026-11-23; base HLD v0.65), updated with the team responses, the Commercial TL decisions and the AI team reply (draft) of 2026-11-19 (`billing-open-items-team-response.md` and `billing-reply-ai-team.md`, not yet in this repository), §16.1 register, §16.2 discrepancies, platform/SRE dependencies, and the ENT-D3 / ENT-D6 product memos. Item IDs match the LLD. **Deployment state:** no service is in production; every service, Billing included, runs in dev only. "Deploy" items below are environment-promotion steps, not production fixes. Where several teams share an item, it is listed under each, with the lead team first.

**Status key:** **Decided** = TL decision made; owning team to implement · **Conflict** = a team answered, but the answer contradicts the HLD or Billing's need · **Open** = needs an answer · **Proposed** = Billing has a proposal; needs sign-off · **External** = depends on another team's delivery · **Deferred** = later phase.

## Summary

| Team | Items | Blocking go-live |
|---|---|---|
| IAM — Core (Org & Membership) | SRE-3 (code ready; ships with Core's environment promotion) | SRE-3 (direct signup). BL-OQ-2, D-3, D-4, SRE-6 closed |
| IAM — Audit Log | BL-OQ-23 (SRE-8 filter, refund entry type) | Token add-on refund audit |
| IAM — Operator path / platform-edge | BL-OQ-9 (b) OP-G4 coverage; BL-OQ-14; BL-OQ-25 (B-O16 test + Audit type); BL-OQ-26 (`pci_owner_reviewer` role, B-O17 test + Audit type); OP-G1, OP-G3, OP-G6 (IAM); **OP-G2 (Platform edge)** | Every operator route `403` until OP-G1 to OP-G3; OP-G2 is the external blocker |
| Entitlements | BL-OQ-3, BL-OQ-4, BL-OQ-20, read-only access (all **decided**, to implement) | BL-OQ-3 (add-on purchase needs per-resource eligibility and access) |
| Finance | BL-OQ-15 (deferred) | — |
| Product | BL-OQ-18, 22; PD-LLM-1 and ENT-D6 approval; **PD-LLM-1 R-4 (Opus tier)**; R-1 model list; R-2 evaluation set and pass bar | BL-OQ-18 (token add-ons have no packs) |
| Legal | — (BL-OQ-11, BL-OQ-17 closed 2026-11-23) | — |
| Security | — (BL-OQ-7 closed 2026-11-23) | — |
| PCI owner group | Name the `pci_owner_reviewer` role holders (BL-OQ-26) | First provider certification run (needs BL-OQ-26) |
| Web / growth (Signup BFF) | BL-OQ-8 | Direct paid signup (`FEATURE_DIRECT_SIGNUP=false`) |
| Notification | BL-OQ-12 | Price increases for live subscribers are shown only on the Billing page until the renewal price-change message ships |
| AI team | BL-OQ-19; PD-LLM-1 R-1, R-2, R-4 (none can close without another team) | Token add-ons (`FEATURE_TOKEN_ADDONS=false`) — waits on a cluster |
| Platform / SRE | SRE-1, SRE-3, SRE-7, SRE-8, Commercial IaC, audit DB setup; **cluster for Gateway tests; Bedrock access in `ap-south-1`** | Deploy prerequisites; BL-OQ-19 test cluster |
| Usage & Metering | BL-OQ-24 (`UsagePeriodClosed` contract; LLD not written) | — (reference-only records; known risk) |
| Commercial HLD owner / TL | — (D-1, D-5, D-8, D-9 closed in HLD v0.65) | — (BL-OQ-3/4/20 decided 2026-11-19) |
| Commercial Team (Billing) | BL-OQ-15 (co-owner) | — |

---

## IAM — Core (Org & Membership)

All Core-owned items closed on 2026-11-19.

| ID | Item | Status |
|---|---|---|
| SRE-3 | Core migration `000004` and `parked-events-replay` CronJob are in Core's repo | **Code ready.** All services are dev-only today; SRE-3 is a go-live ordering rule, not a fix: in each environment (dev, staging, production), Core's `000004` and the CronJob must be deployed before Billing's direct signup is enabled there |

## IAM — Operator path / platform-edge

**How the operator path works (IAM HLD v1.59 §4.2, §5.4, §10.9).** The ingress Envoy Gateway, owned by **Platform edge**, is the only way in. It authenticates with `jwt_authn`, then calls AuthZ Enrichment (`ext_authz`). A token whose `iss` equals `OPERATOR_REALM_ISSUER` (the platform realm) skips tenant resolution and is allowed only on `/api/v1/operator/*` with the `platform_operator` realm role. On Allow, AuthZ injects `x-tenant-roles: platform_operator`, `x-user-id` = the operator `sub`, and `x-tenant-id` = the tenant in `/api/v1/operator/tenants/{id}/…` or the nil UUID; every decision is logged as `operator_access`. Admission is by prefix: there is no per-route list in IAM. Each service exposes its paths with its own `HTTPRoute`. **Today the path is off** (`OPERATOR_REALM_ISSUER` empty), so no operator route, IAM's or Commercial's, works through the gateway.

**Go-live items (IAM HLD §17.2, AuthZ Enrichment LLD §8.9.1, AE-46).** Numbering as in IAM's documents; the Commercial HLD uses the same numbering from v0.65 (D-9, closed).

| ID | Item | Owner | Status |
|---|---|---|---|
| OP-G1 | Keycloak platform realm: `platform_operator` role, MFA, short sessions, operator users | IAM (Realm Provisioner) | **Done** — [iam-realm-provisioner#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60) merged: realm `bcbp-platform` created on next deploy; operator users still manual |
| OP-G2 | Gateway `jwt_authn` provider for the platform realm, accepted on `/api/v1/operator/*` only | **Platform edge** | **Open — the external blocker** |
| OP-G3 | Set `OPERATOR_REALM_ISSUER` (and `JWT_OPERATOR_PAYLOAD_KEY` if OP-G2 needs it) per environment; deploy AuthZ Enrichment | IAM | Blocked on OP-G1, OP-G2 |
| OP-G4 | End-to-end test through the gateway over IAM's and Commercial's operator routes | IAM + Commercial | Blocked on OP-G3 |
| OP-G5 | Staff role `commercial_trial_manager` (Entitlements trial-profile only) | IAM + Entitlements | Only if Product asks (CD-51); not a Billing need |
| OP-G6 | Target date for OP-G1 to OP-G4, given to Commercial | IAM | Open — Commercial asked 28 Oct; gates Phase 1 |

**Billing items.**

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-9 | (a) **Billing:** its operator `HTTPRoute` lists every operator path, B-O1 to B-O15 (Billing chart, LLD §13.1). (b) **IAM:** OP-G4 covers only B-O1 to B-O5 (Commercial's 28 Oct list); extend it to B-O6 to B-O15 | `403` until OP-G1 to OP-G3 are done (the path is off, not a missing route) | **Approved; IAM part merged** ([iam-authz-enrichment#43](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/pull/43)). Remaining: Billing's `HTTPRoute` lists B-O1–B-O15 |
| BL-OQ-14 | **CD-52:** Billing's operator routes work only after OP-G1 to OP-G3, then the joint OP-G4 run per environment. Expected denials (IAM HLD §5.4): no `platform_operator` → `403 DENY_ROLE_INSUFFICIENT`; tenant token on an operator route → `403`; malformed tenant in the path → **`401` `DENY_MALFORMED_IDENTITY`** | Operator routes `403 INSUFFICIENT_ROLE` outside dev/test (`BIL-AUTH-6`); no Billing bypass | Open (external) — waits on OP-G2 (Platform edge) and OP-G6 date |
| BL-OQ-25 | **New (from BL-OQ-1, 2026-11-23; IAM action A-9).** (a) AuthZ Enrichment: add B-O16 `POST /api/v1/operator/tenants/:id/invoices/:invoice_id/disposition` (tenant in path) to the OP-G4 test list. (b) Audit Log: new AL-5 entry type `billing.invoice.suspension_disposition` (`security_3y`). Billing adds B-O16 to its operator `HTTPRoute` | AL-5 entries wait in `pending_audit_entries`; route `403` until the operator path is live | Open (go-live gate) |
| BL-OQ-26 | **New (from BL-OQ-21, 2026-11-23; IAM action A-10).** (a) Realm Provisioner / Keycloak: platform-realm role `pci_owner_reviewer`, held only by PCI owner group members; AuthZ Enrichment passes it in `x-tenant-roles` on the operator path. (b) AuthZ Enrichment: OP-G4 covers B-O17 `POST /api/v1/operator/billing/provider-certifications/:id/review` (no tenant in path). (c) Audit Log: AL-5 entry type `billing.provider.certification_reviewed` (`security_3y`) | B-O17 answers `403` until (a); AL-5 entries wait in `pending_audit_entries` until (c) | Open (go-live gate for the first provider certification) |

## Entitlements

**Commercial TL decisions, 2026-11-19.** Principle: Billing owns commercial transactions; Entitlements owns quota state. An event originates in the service that owns the business action. The Entitlements replies of 2026-11-19 are superseded where they differ. Corrections are listed in `billing-open-items-corrections.md`.

| ID | Decision | Entitlements action | Status |
|---|---|---|---|
| BL-OQ-4 | Billing is the only publisher of `TenantQuotaAddOnChanged` on `billing.events` | Withdraw `tenant_quota_add_on_changed`; keep consuming Billing's event on `billing-entitlements-q`, idempotent on `billing_ref` | **Decided** — to implement |
| BL-OQ-20 | Billing owns the `purchase` and `refund` reasons; Entitlements owns correction and expiry | Apply `reason: refund` (negative `delta`, `purchase_ref`): remove at most the grant's unused tokens, never below zero. Record corrections and expiry through AL-5 only: no bus event, never `TenantQuotaAddOnChanged` (BL-OQ-23 c, 2026-11-23) | **Decided** — to implement |
| BL-OQ-3 | Generic `addon_eligible` rejected; eligibility per resource | Replace `addon_eligible: bool` with per-resource eligibility (`llm_tokens`, `api_requests`, each with a reason) | **Decided** — to implement |
| Access | Billing → Entitlements read-only access approved | Admit `cluster.local/ns/billing/sa/commercial-billing` on `GET /api/v1/internal/tenants/:id/entitlements` only (NetworkPolicy + Istio AuthorizationPolicy) | **Decided** — to implement |

## IAM — Audit Log

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-23 (a) / SRE-8 | `billing-audit-q` filter policy adds **Billing's** `TenantQuotaAddOnChanged` (TL decision). Not Entitlements' event | Purchases not audited from the bus | **Closed** — catch-all queue, no change (IAM reply) |
| BL-OQ-23 (b) | Map `TenantQuotaAddOnChanged` with `reason: refund` to new entry type `billing.token_addon.refunded` (`security_3y`); `reason: purchase` stays `billing.token_addon.purchased` | Refund events stored as `billing.token_addon.purchased` (Audit's existing mapping ignores `reason`): mislabelled as a purchase, not lost; the payload (`reason`, negative `delta`) is kept in metadata | **Merged** — [iam-audit-log#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24); live per environment on Audit Log deploy |
| BL-OQ-23 (c) | Do not subscribe to any Entitlements add-on event: Entitlements' corrections and expiry already arrive through AL-5 (`billing.token_addon.granted` / `.adjusted` / `.revoked` / `.expired`) | — | **Closed** (Commercial TL and Entitlements approved option 1, 2026-11-23; LLD rev 0.22): no add-on balance event on `entitlement-events`; AL-5 only |

## Finance

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-5 | **Launch currency.** HLD lists USD; the capability bar needs AED. One currency per market? Both in Phase 1? | `DEFAULT_CURRENCY=USD`; AED rows via B-O11 | **Closed** (Finance, 2026-11-23; LLD rev 0.24): USD default; other currencies as `plan_prices` rows via B-O11, no application change |
| BL-OQ-22 | **Final prices wait on token-budget validation** (PD-LLM-1 R-3: 500K / 5M / 25M quotas vs tier model cost; ENT-D6 trial budgets). Then set `plan_prices` and `token_packs` (with Product) | Phase 1 seed; `token_packs` empty → `ADDON_NOT_AVAILABLE` | **Closed** (Product, 2026-11-23; LLD rev 0.26): plan prices final and loaded into `plan_prices`; pack prices stay with BL-OQ-18 |
| BL-OQ-6 | Proration rule without a provider engine; behaviour below the minimum charge; renewal top-up kept on payment failure | Seconds-based, half-up; `free_until_renewal`; top-up kept | **Closed** (Finance approved 2026-11-23; LLD rev 0.15) |
| BL-OQ-1 | **CD-24:** invoicing during operator suspension — auto-pause collection (`keep_as_draft`), resume on reactivation, operator decides drafts (with Billing) | `AUTO_PAUSE_ON_OPERATOR_SUSPENSION=true` | **Closed** (Finance approved 2026-11-23; LLD rev 0.16; follow-up BL-OQ-25) |
| BL-OQ-7 | **CD-10:** conversion reversal on a fraud dispute or early fraud warning within 7 days, operator-approved (with Billing, Security) | Operator-approved; `AUTO_REVERT_ON_EFW=false` | **Closed** (Finance and Security approved 2026-11-23; LLD rev 0.18) |
| BL-OQ-11 | **CD-14:** financial-record retention per jurisdiction; also sets Workflow history retention (with Legal, Billing) | No financial deletion job | **Closed** (Finance and Legal approved 2026-11-23: keep indefinitely; LLD rev 0.19) |
| BL-OQ-13 | **BIZ-6:** approve the six billing permissions (two added by the LLD) (with Billing) | §5.2 as written | **Closed** (Finance approved 2026-11-23; LLD rev 0.17) |
| BL-OQ-16 | Price changes for live subscriptions: next renewal or grandfather longer (with Product) | Next renewal | **Closed** (Product and Finance approved 2026-11-23; LLD rev 0.20) |
| BL-OQ-17 | Stripe data location and processor terms for the UAE account (with Legal) | — | **Closed** (Legal accepted without amendments, 2026-11-23; LLD rev 0.25) |
| BL-OQ-15 | **CD-56 follow-ups:** per-tenant provider selection; moving payment methods and live subscriptions off a removed provider (with Billing) | One provider | Deferred (Phase 3) |
| PD-LLM-1 R-3 | Validate token quotas per plan against tier model cost | — | **Done** (Product, 2026-11-23; closed BL-OQ-22) |
| ENT-D6 | Validate Trial-Specific and Reduced token budgets | — | Open (memo) |

## Product

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-18 | **CD-55 commercial policy (BIZ-1):** token pack sizes, prices, any monthly cap | `token_packs` empty → `ADDON_NOT_AVAILABLE` | **Open — blocking** token add-on sales |
| BL-OQ-22 | Final plan and pack prices after Finance validation (with Finance) | Phase 1 seed | **Closed** (2026-11-23): plan prices loaded; packs under BL-OQ-18 |
| BL-OQ-16 | Price-change policy for live subscriptions (with Finance) | Next renewal | **Closed** (approved 2026-11-23; LLD rev 0.20) |
| PD-LLM-1 | Approve tiered model profiles (tier names, plan mapping, nine principles) | Seeded: Starter `basic`, Pro `standard`, Enterprise `premium` | Proposed (memo) |
| ENT-D6 | Approve trial profile values (six `plan_trial_profiles` rows, explicit Enterprise limits) | `NULL` profiles inherit paid limits | Proposed (memo) |

No Billing design change follows from either memo (LLD rev 0.6, §8.4.3).

## Legal

| ID | Item | Status |
|---|---|---|
| BL-OQ-11 | Financial-record retention per jurisdiction (with Finance, Billing) | **Closed** (approved 2026-11-23: keep indefinitely; LLD rev 0.19) |
| BL-OQ-17 | Stripe data location and processor terms for the UAE account (with Finance) | **Closed** (accepted without amendments, 2026-11-23; LLD rev 0.25) |

## Security

| ID | Item | Status |
|---|---|---|
| BL-OQ-7 | CD-10 conversion-reversal trigger and SEV-2 handling (with Billing, Finance) | **Closed** (approved 2026-11-23; LLD rev 0.18) |

## PCI owner group

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-21 | Who signs a provider certification run: a Billing engineer submits (B-O14), a PCI owner group member reviews (`reviewed_by`), as for production deploys | Reviewer approves through B-O17 (`pci_owner_reviewer`); `chk_pc_two_people`, `trg_pc_guard` | **Closed** (PCI owner group approved 2026-11-23, review complete; LLD rev 0.21; follow-up BL-OQ-26) |

## Web / growth (Signup BFF)

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-8 | **SGN-2 alignment** with the published B-I1 contract: `BILLING_TIMEOUT_MS ≥ 15000`; map `PaymentToken → payment_method_ref`, `Plan → plan_code`, `Seats → licensed_seats`; 3-D Secure flow (`402 PAYMENT_ACTION_REQUIRED` → browser → same-key retry). Go-live order §18.7 | `FEATURE_DIRECT_SIGNUP=false` | Open — contract published |

## Notification

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-12 | **NTF-3** messages: seats added; reduction applied (incl. renewal above target); add-on purchased; downgrade blocked at renewal; renewal top-up charged; **renewal price change scheduled** (current and new unit price, effective date; Product requires it before a higher price applies, BL-OQ-16, LLD §8.16). The scheduled-downgrade message should state `effective_at` (model tier drops then, §8.4.3) | Only past-due and cancelled emails | Open |

## AI team

Reply received 2026-11-19 (`billing-reply-ai-team.md`, draft for TL review, nothing signed off). No item can be closed by the AI team alone.

| ID | AI team answer | Needs | Billing default until closed | Status |
|---|---|---|---|---|
| BL-OQ-19 | **Not proven.** Without a cluster: a changed per-tenant limit was in force in the rate-limit service within 0.3 s; plain Envoy served calls when the rate-limit service was down (`failure_mode_deny: false`) and counted them. Nothing run through Gateway v1.2.0 (no cluster; standalone Agent Router build stopped on the monthly rule) | Cluster with Gateway, rate-limit service and Valkey (SRE / platform); then AI-1, AI-3, AI-4, AI-5 | `FEATURE_TOKEN_ADDONS=false` | Open (external) |
| PD-LLM-1 R-1 | **Not checked.** The Gateway has never called Bedrock | AWS role with Bedrock read and invoke in `ap-south-1` (later `me-central-1`); the memo's model list per tier | Seeded tiers, unvalidated | Open |
| PD-LLM-1 R-2 | **Not done.** No English or Arabic evaluation set; local models cannot judge quality. Tenant text cannot be used without consent and anonymisation | Written evaluation set in both languages (Product, Tender), pass bar per pair (Product), R-1 | Seeded tiers, unvalidated | Open |
| PD-LLM-1 R-4 | **Rule proposed:** `draft-premium-max` only when the plan allows it **and** the tenant's pack names the generation-profile capability; a user hint alone never selects it; a qualifying call over its token limit is refused, never silently downgraded; a non-qualifying call is served by the plan's default profile with a visible degradation marker | Product: which plans get Opus, any cap, and the price consequence | `draft-premium-max` routed to no plan | Open (Product) |

**BL-OQ-19 go-live evidence — Billing decision (2026-11-19).** Record **AI-1, AI-3 and AI-5**, each passing on the pinned Gateway v1.2.0 image on a named cluster:

| Test | What it demonstrates |
|---|---|
| AI-1 | Entitlement-change propagation: a changed per-tenant limit is in force within the required 60-second window |
| AI-3 | Continued operation and alerting under rate-limit service failure (`failClosed: false`: calls served, SEV-2 signal fires) |
| AI-5 | The end-to-end business outcome: a purchased add-on increases the effective allowance (plan entitlement plus add-on). This is the key validation that sold add-ons work in production |

Limiting the evidence to AI-1 and AI-3, as the AI team proposed, would leave the commercial add-on path unproven. `FEATURE_TOKEN_ADDONS` stays `false` until all three pass. AI-4 (usage feed) is Usage & Metering's gate, not Billing's.

**Noted, no Billing change:** the Gateway's monthly allowance window is not the billing period (Usage & Metering sets the quota reset, CD-58), and fail-open (CD-57a) leaves outage tokens unmetered at the edge until reconciliation (LLD §8.7). If Product makes the Opus tier a paid upsell (R-4), Billing needs a price for it: it would join BL-OQ-22.

## Platform / SRE

| ID | Item | Status |
|---|---|---|
| SRE-1 | Istio `STRICT` mTLS in staging | Open |
| SRE-3 | Core migration `000004` and `parked-events-replay` deployed before direct signup is enabled, in each environment (code in Core's repo; all services dev-only today) | Open — ordering rule per environment |
| SRE-6 | `IAMParkedTenantEventsStale` routed to Billing on-call (`notify_billing: "true"`) | **Closed** (Core) |
| SRE-7 | Remove any `membership-billing-q` (retired by CD-54) | Open |
| SRE-8 | `billing-audit-q` filter adds Billing's `TenantQuotaAddOnChanged` (TL decision; BL-OQ-23 a) | **Closed** — Audit queues are catch-all (IAM reply) |
| IaC | Commercial IaC: SNS `billing-events`, Glue registry `billing-events`, SQS `tenant-billing-q`, `usage-billing-q` (+ DLQs) (HLD §16 action 2) | Open |
| DB | `billing_audit_drainer` role and secret (`PG_AUDIT_DRAINER_*`); `MIGRATION_DATABASE_URL` on the direct RDS endpoint, not PgBouncer (rev 0.5) | Open |
| AI-test cluster | Kubernetes cluster with Model Gateway v1.2.0, the rate-limit service and Valkey, for AI-1, AI-3, AI-4, AI-5 (BL-OQ-19) | Open |
| Bedrock access | AWS account and role with Bedrock read and invoke in `ap-south-1` (and later `me-central-1`) for PD-LLM-1 R-1 | Open |
| Go-live | `platform-audit` `Client.Probe` returns `ProbeAdmitted` from a Billing pod in every environment; `platform-audit` alert rules loaded and routed to Billing on-call | Open |

## Commercial HLD owner

| ID | Item | Status |
|---|---|---|
| D-1 | HLD §9.4 says `tenant_id` rides only the envelope; RP's consumed payload requires `data.tenant_id`. Billing sends both. HLD note requested | **Closed** (accepted 2026-11-23; corrected in HLD v0.65 §17.8) |
| D-5 | HLD §3.3 lists `platform-gincommon` v1.6.0; Billing uses v2 (same RFC 9457 wire format). HLD note requested | **Closed** (accepted 2026-11-23; corrected in HLD v0.65 §17.8) |
| D-8 | HLD §9.4 lists reserved correction reasons on `TenantQuotaAddOnChanged`. After the TL decision Billing's enum is `purchase` \| `refund`; correction reasons move to Entitlements' own operator actions, audited through AL-5 with no bus event (BL-OQ-23 c). HLD note requested | **Closed** (accepted 2026-11-23; corrected in HLD v0.65 §17.8) |
| D-9 | HLD CD-52 row numbers the gateway provider OP-G3 and the end-to-end test OP-G5; AuthZ Enrichment LLD §8.9.1 (source) uses OP-G2 and OP-G4. Billing LLD follows the AuthZ LLD from rev 0.11. HLD note requested | **Closed** (accepted 2026-11-23; corrected in HLD v0.65 §17.8) |

## Usage & Metering

| ID | Item | Billing default until closed | Status |
|---|---|---|---|
| BL-OQ-24 | Confirm the `UsagePeriodClosed` payload Billing consumes (§7.1: `period_start`, `period_end`, `tokens_total`, `api_requests_total`, `addon_consumed`, `limits`) and register its schema, before Billing's `usage-billing-q` consumer is enabled. The Usage & Metering LLD is not written yet | Reference-only `usage_period_records`; nothing priced from them | Open (known risk) |

## Commercial Team (Billing) — co-owned items

BL-OQ-15: Billing drafted each proposal and needs the partner team's sign-off listed above.

## Closed since the last revision

| ID | Closed by |
|---|---|
| BL-OQ-10 | Audit Log rev 0.34: Billing admitted to AL-5; all seven Billing entry types added |
| BL-OQ-2 | IAM Core: separate `tenants.seats_last_event_at` watermark (migration `000010`); EVT-14 split by event type; tie rule `time ==` → skip; regression test added. Billing keeps `BIL-EVT-5/6` as defence in depth |
| D-3 | IAM Core: I-18 auth documented as `RequireInternalCaller()` |
| D-4 | IAM Core: `over_cap` removed from I-11 / P-27 in the Core LLD |
| SRE-6 | IAM Core: `notify_billing: "true"` on `IAMParkedTenantEventsStale` (app alerts and Helm PrometheusRule) |
| D-7 | LLD rev 0.5: `platform-audit` v0.1.2 adopted; no in-repo AL-5 adapter |
