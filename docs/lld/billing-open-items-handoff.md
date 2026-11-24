# Billing LLD — Open Items Hand-off by Team

| Field | Value |
|---|---|
| Document | Hand-off of every open item in the Billing LLD, by owning team and service |
| Billing LLD | `commercial-lld-billing-service.md` rev 0.30 (base HLD v0.65) |
| Date | 2026-11-23 |
| From | Commercial Team (Billing) |
| Deployment state | No service is in production; all run in dev only. "Done" means merged **and** promoted with each environment (dev → staging → production) |
| Supersedes for hand-off purposes | Nothing. Detailed working documents stay as they are and are referenced per item |

## 1. Where things stand

| Area | State |
|---|---|
| **Design decisions** | All nine partner-team decisions approved on 2026-11-23 (S-1 to S-9, `billing-design-signoff-blockers.md`). No design question is open |
| **HLD alignment** | All four LLD/HLD differences accepted and written into HLD v0.65 (§17.8). v0.65 is not merged yet (§3.1) |
| **Final sign-off** | Not given yet. Two steps remain: §3.1 and §3.2 (§3.3 done) |
| **Go-live** | Blocked by items owned by other teams (§4). Billing's design already behaves safely until each closes: a feature flag stays off, a route returns `403`, or a call fails closed with `503` |

**How to read this document.** §2 is the one-page view. §3 lists what stands between the LLD and its signature. §4 has one section per team. Each item gives:
- **What:** the item in plain words.
- **Why it matters:** what it holds back.
- **Billing meanwhile:** the safe default the design runs with until it closes.
- **Action:** numbered steps for the owning team and service.
- **Done when:** the evidence that closes it.
- **Depends on:** anything that must happen first.

**Status words:** **Open** = needs an answer or work · **Requested** = asked of the team, no reply yet · **Claimed, unverified** = the team says done, but it can't be seen in their repository · **Decided** = the decision is made and needs building · **Blocked** = waits on another item · **Deferred** = later phase.

---

**Closed since first issue:**
- BL-OQ-9 (a) (operator `HTTPRoute`), Commercial Team 2026-11-23: Billing's operator `HTTPRoute` now lists the complete operator surface, B-O1 to B-O17, as specified in §13.1 (Commercial Team, 2026-11-23). Gateway reachability is proven by the OP-G4 joint run once OP-G2 and OP-G3 are live (BL-OQ-14).
- Product decision memos: Product approved PD-LLM-1 (model tiers) and Entitlements approved ENT-D6 (trial profiles) on 2026-11-23. Billing continues to send plan selections only; no design or implementation change. The approvals validate the assumptions behind pricing and entitlement configuration.
- BL-OQ-18 (token packs), Product + Finance 2026-11-23: Product and Finance finalised the Phase 1 token-pack catalogue (pack sizes, prices and any caps); the pack records are loaded into the commercial catalogue (`token_packs`, B-O12). Billing's existing add-on purchase design supports it without code or design change.
- BL-OQ-22 (final prices), Product 2026-11-23: Product completed the token-budget validation against PD-LLM-1 R-3 and ENT-D6 and finalised the Phase 1 commercial pricing; prices are loaded into `plan_prices`. Resolved through configuration/data only; no code or design change. Token-pack prices (`token_packs`) are not part of this closure and stay with BL-OQ-18.
- BL-OQ-17 (Stripe UAE terms), Legal 2026-11-23: Legal reviewed the Stripe UAE data-location and processor terms and accepted them without required amendments. Billing may proceed with the planned Stripe UAE account configuration. No additional design, legal or implementation work remains.
- BL-OQ-5 (launch currency), Finance 2026-11-23: USD remains the default currency. Market-specific currencies are configured through `plan_prices`; AED pricing can be introduced via B-O11 without application changes. No further design or implementation work is required.

## 2. Summary by team

| Team | Service / repository | Open items | Holds back |
|---|---|---|---|
| **Platform-edge** | Envoy Gateway (platform ingress) | BL-OQ-14: OP-G2 | **Every operator route**, so paid launch too |
| **IAM: Realm Provisioner / Keycloak** | `iam-realm-provisioner` | OP-G1 rollout and operator users; A-10 (a) role `pci_owner_reviewer` | Operator sign-in; provider certification |
| **IAM: AuthZ Enrichment** | `iam-authz-enrichment` | OP-G3; OP-G4 runs (A-3); A-9 (a); A-10 (a, b); OP-G6 date | Operator routes per environment |
| **IAM: Audit Log** | `iam-audit-log` | A-9 (b); A-10 (c); A-5 deploy (BL-OQ-23 b); confirm one metadata field | Correct audit for B-O16, B-O17 and add-on refunds |
| **IAM: Core** | `iam-org-membership` | SRE-3 ordering per environment (with SRE) | Direct paid signup |
| **PCI owner group** | — | Re-confirm §4 and §5.4.4 (rev 0.21); name the role holders | **Final sign-off**; first provider certification |
| **Commercial Entitlements** | `commercial-entitlements-service` (`commercial` / `commercial-entitlements`) | E-1, E-2, E-4, E-5, E-7 (**claimed, unverified**); Billing consumer event names; E-6 | **Token add-ons**; plan changes reaching Entitlements |
| **Commercial IaC / Platform-SRE** | Commercial infrastructure-as-code; clusters | Topics, registry and queues; SRE-1, SRE-3, SRE-7; Gateway test cluster | Billing events in any environment; token add-ons |
| **AI team** | Model Gateway (Envoy AI Gateway) | BL-OQ-19 (AI-1, AI-3, AI-5) | Token add-ons |
| **Web / growth** | Signup BFF | BL-OQ-8 | Direct paid signup |
| **Notification** | `hld-notification-service` | BL-OQ-12 | Customer emails; price increases for existing customers |
| **Usage & Metering** | `commercial-usage-metering-service` | BL-OQ-24 | Billing's usage-record consumer |
| **Finance** | — | — (acknowledgements received 2026-11-23) | — |
| **Product** | — | — (acknowledgement received 2026-11-23) | — |
| **Legal** | — | — (acknowledgement received 2026-11-23) | — |
| **Commercial HLD owner** | `commercial-hld-docs` | Merge and tag HLD v0.65 | **Final sign-off** |
| **Commercial Team (Billing)** | `commercial-billing` (`billing` / `commercial-billing`) | W-8 to W-10; contract-test gap; LLD housekeeping; push and PR; BL-OQ-15 (deferred) | Final sign-off (as approver); its own build |

---

## 3. Steps to final sign-off

The LLD is signed by the **Commercial Team** (whole document, CD-4) and the **PCI owner group** (§4, §5.4.4, §10). These three steps come first.

### 3.1 Commercial HLD owner — merge and tag HLD v0.65

- **What:** HLD v0.65 corrects the four differences the LLD had with the HLD (D-1, D-5, D-8, D-9). It exists only on the unpushed branch `docs/hld-v0.65-billing-lld-notes` in `commercial-hld-docs`.
- **Why it matters:** the LLD now names v0.65 as its base, and the signed HLD version is a tagged commit on `main` (CD-3a). The LLD can't be signed against an HLD that hasn't been merged.
- **Action:**
  1. Review `hld-commercial-domain-v0.65.md` (text-only; the changelog row and §17.8 list every change).
  2. Merge to `main`.
  3. Tag it if HLD versions are signed by tag.
- **Done when:** v0.65 is on `main`, and tagged if that's the practice.

### 3.2 PCI owner group — re-confirm §4 and §5.4.4

- **What:** the PCI review (S-7) covered rev 0.20. To meet PCI's own two recommendations, rev 0.21 then changed sections in PCI's scope:
  - §4.2.24: new statuses `pending_review` and `rejected`, review columns, trigger `trg_pc_guard`, constraint `chk_pc_two_people`.
  - §5.4.4: B-O14 now only records a run; new route **B-O17** for the reviewer's decision.
- **Why it matters:** a PCI sign-off should cover the text actually being signed. The changes tighten the control; they don't loosen it.
- **Action:** read `billing-design-signoff-blockers.md` S-7 ("What the Commercial Team changed") and LLD §4.2.24, B-O14, B-O17 and `BIL-PROV-4`, then reply "confirmed" or with changes.
- **Done when:** the PCI owner group confirms rev 0.21 or later.

### 3.3 Partner teams — acknowledge four Commercial Team interpretations — ✅ done

Partner teams approved S-2, S-3, S-5 and S-6. Applying those approvals needed four interpretations or additions they haven't seen. Each is written in the sign-off document as "correct us if you meant otherwise". One acknowledgement per team closes them.

| Item | What the Commercial Team added or interpreted | Acknowledge |
|---|---|---|
| S-2 | New operator route **B-O16** to charge or void invoices held during a suspension. Automatic retries on invoices already open are stopped while paused | Finance |
| S-3 | "New contractual commitment" means B-P2, B-P4, B-P5 and B-P14. Tenant admins keep seat and add-on purchases | Finance |
| S-5 | "No minimisation job" covers financial records. Contact name and email are still cleared by `pii-minimisation` (90 days after offboarding; 30 days after an abandoned checkout) | Finance + Legal |
| S-6 | A `renewal-repricing` job moves subscribers to a new price at renewal (Stripe otherwise keeps the old price forever). No minimum notice period | Product + Finance |

- **Done when:** each team replies "acknowledged" or with a correction.
- **Result:** ✅ all four acknowledged by Finance, Product and Legal on 2026-11-23, without correction (LLD rev 0.29).

---

## 4. Open items by team

### 4.1 Platform-edge

#### BL-OQ-14 (OP-G2) — Gateway `jwt_authn` provider for the platform realm — **the main external blocker**

- **What:** operators reach `/api/v1/operator/*` through Envoy Gateway with a platform-realm login. The gateway needs a `jwt_authn` provider for realm `bcbp-platform` (issuer `https://auth.bcbpsolutions.com/realms/bcbp-platform`).
- **Why it matters:** until this exists, every operator route in every Commercial service answers `403`. That includes:
  - the Phase 1 conversion stub (B-O1), refunds, credits, collection pause and the price book;
  - **provider certification** (B-O14, B-O17). Without certification, Stripe takes no new accounts, so there's no paid launch.
- **Billing meanwhile:** operator routes answer `403` outside dev and test (`BIL-AUTH-6`).
- **Action:** build and deploy the provider per environment, as specified in `op-g2-platform-edge-request.md`.
- **Done when:** a platform-realm token passes the gateway on `/api/v1/operator/*`, and a tenant-realm token is refused, in each environment.
- **Depends on:** OP-G1 (IAM, merged).

### 4.2 IAM

Detailed specs and test cases: `billing-iam-actions.md` (A-1 to A-10).

#### Realm Provisioner / Keycloak (`iam-realm-provisioner`)

| # | What | Action | Done when |
|---|---|---|---|
| OP-G1 rollout | Platform realm is merged ([#60](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pull/60)) and is created on the next deploy | Deploy per environment; create operator users (manual today) | Operators can sign in to `bcbp-platform` in each environment |
| A-10 (a) | Role **`pci_owner_reviewer`** (BL-OQ-26) | Create the platform-realm role; grant it only to people the PCI owner group names | Role exists per environment, held only by named PCI members |

#### AuthZ Enrichment (`iam-authz-enrichment`)

| # | What | Action | Done when |
|---|---|---|---|
| OP-G3 | Point AuthZ at the platform realm | Set `OPERATOR_REALM_ISSUER` and deploy, after OP-G2 | Operator tokens are accepted and `x-tenant-roles: platform_operator` is injected |
| A-3 / OP-G4 | Joint end-to-end operator test through the gateway | Run with Commercial (Billing **and** Entitlements, E-6) in each environment | Allow and deny cases pass for every Billing operator route |
| A-9 (a) | B-O16 in the OP-G4 list (BL-OQ-25) | Add `POST /api/v1/operator/tenants/:id/invoices/:invoice_id/disposition` (tenant in path) | Allow and deny cases pass |
| A-10 (a, b) | Pass `pci_owner_reviewer` to Billing; B-O17 in OP-G4 (BL-OQ-26) | Forward the role in `x-tenant-roles` on the operator path; add `POST /api/v1/operator/billing/provider-certifications/:id/review` | With both roles → allowed; without `pci_owner_reviewer` → Billing returns `403` |
| OP-G6 | Target date | Give Commercial a date for OP-G1 to OP-G4 | Date published |

**Please also confirm** whether AuthZ already forwards extra platform-realm roles in `x-tenant-roles`, or whether A-10 needs a code change.

#### Audit Log (`iam-audit-log`)

| # | What | Action | Done when |
|---|---|---|---|
| A-9 (b) | Entry type `billing.invoice.suspension_disposition` (`security_3y`, AL-5) | Add to the taxonomy. Also confirm `billing.collection.paused` accepts an extra metadata field `paused` | Type released |
| A-10 (c) | Entry type `billing.provider.certification_reviewed` (`security_3y`, AL-5) | Add to the taxonomy | Type released |
| A-5 / BL-OQ-23 (b) | `billing.token_addon.refunded`, merged ([#24](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pull/24), `0e12f2c`) | Deploy a release containing it in each environment (v0.3.0 suggested) | Refund events stored as `refunded`, not `purchased` |

- **Billing meanwhile:** AL-5 entries wait in `pending_audit_entries` and drain once the type exists, so nothing is lost. Add-on refunds are stored as `purchased`, with `reason` kept in the metadata.

#### Core (`iam-org-membership`) — with Platform-SRE

- **SRE-3:** in each environment, deploy migration `000004` and the `parked-events-replay` CronJob **before** direct paid signup is switched on. The code is ready in Core's repo.
- **Done when:** confirmed for each environment before `FEATURE_DIRECT_SIGNUP=true` there.

### 4.3 PCI owner group

| # | What | Action | Done when |
|---|---|---|---|
| Sign-off | Re-confirm §4 and §5.4.4 at rev 0.21 or later | See §3.2 | Confirmed |
| BL-OQ-26 | Who holds `pci_owner_reviewer` | Name the members to IAM | Role granted to named members |
| First certification | Approve the first Stripe certification run | After `cmd/certify` runs and B-O14 records the run, a member other than the submitter approves it through B-O17 | Stripe's record is `passed`; the registry enables Stripe |

### 4.4 Commercial Entitlements

**Service:** Entitlements (`commercial-entitlements-service`; namespace `commercial`, ServiceAccount `commercial-entitlements`). **Team:** Commercial Team, Entitlements service.

Detail: `billing-decided-items-entitlements-build.md` (W-1 to W-7) and `billing-open-items-entitlements.md` (E-1 to E-8).

**Verification status.** Entitlements' reply of 2026-11-23 (`billing-gap-closure.md`) reports E-1, E-2, E-4, E-5 and E-7 closed. The only pushed code, branch `initial-commit` (last commit 2026-11-19), shows the **opposite**:
- it still produces `tenant_quota_add_on_changed` in three places;
- it still returns `addon_eligible: bool`;
- it has no purchase or refund handling for Billing's event, no `billing_ref`, and no Istio or NetworkPolicy templates.

The work is probably unpushed. **These items stay open until the code can be seen.**

**Second reply, 2026-11-24.** Entitlements returned `billing-decided-items-entitlements-build.md` with W-1 to W-7 ticked, naming functions, files, migration `000002_addon_billing_ref` and LLD rev 1.18. Rechecked on 2026-11-24: still nothing is pushed after `b5ba492` (2026-11-19), and none of the named items exist. The LLD in the repository is rev 1.15. **Still claimed, unverified.** The per-item comparison and what closes each item are in that document, under "Reply of 2026-11-24 and verification". The ask is unchanged: push the branch and open a PR.

| # | Billing ID | What Entitlements must do | Done when |
|---|---|---|---|
| **Push** | — | Push the branch with the described work and open a PR | Code reviewable |
| **Consumer event names** | — | The pushed Billing consumer handles `BillingSubscriptionActivated`, `…Updated`, `…Suspended`, `…Reactivated`, `BillingTrialStarted`, `BillingTrialExpired`. **Billing publishes none of these.** Handle `TenantPlanChanged`, `TenantSubscriptionCancelled`, `TenantReactivated` and `TenantQuotaAddOnChanged` (HLD §9.4, Billing LLD §7.3). Unknown types are acknowledged and dropped today, so every Billing event would be lost | Each of the four types is applied in a test |
| E-1 / W-3, W-4 | BL-OQ-4 | Stop producing `tenant_quota_add_on_changed`. Consume `reason: purchase`; grant idempotent on `billing_ref`; expiry from Billing's `purchased_at` | No add-on event produced; a purchase raises the balance exactly once, including on replay |
| E-2 / W-5 | BL-OQ-20 | Apply `reason: refund`: find the grant whose **`billing_ref` equals the event's `purchase_ref`**. That is Billing's purchase id, not Entitlements' grant UUID; the reply's lookup "by addon UUID" would never match. Remove at most the unused tokens; idempotent on `billing_ref` (the refund id), not only the event id | A refund lowers the right grant, never below zero; replays do nothing |
| E-4 / W-1 | BL-OQ-3 | Replace `addon_eligible` with `addon_eligibility` per resource (`trial`, `not_active`, `unlimited_quota`) | Response matches the agreed shape |
| E-5 / W-2 | BL-OQ-3 | Istio AuthorizationPolicy and NetworkPolicy admitting `commercial-billing` on `GET /api/v1/internal/tenants/*/entitlements` only | A Billing pod gets `200` there and `403` elsewhere, **in each environment** |
| E-7 / W-6, W-7 | BL-OQ-20, D-8 | Write in the Entitlements **LLD document** (not only code comments): consumed `reason` is `purchase` \| `refund`; corrections and expiry go through AL-5 only; no add-on event on `entitlement-events`. Fix the code comment that still says Billing must not emit the event | LLD and comment match the TL decision |
| E-6 | BL-OQ-14 | Join the OP-G4 run for Entitlements' own operator routes | After OP-G2 and OP-G3 |

**Further review points on the described design** (apply once pushed):
1. A malformed purchase event must go to the dead-letter queue and alert, not be logged and acknowledged. Otherwise a customer pays and gets no tokens, silently.
2. A failed recompute after a grant needs a retry or reconcile, so the tokens become usable.
3. Expiry is "12 months": use calendar months (`AddDate(1,0,0)`), not 365 days.
4. `billing_ref` arriving for a different tenant fails the global unique index. Treat it as an error that alerts, not as a retry loop.
5. A refund that arrives before its purchase is applied (queues aren't strictly ordered) must be retried, not acknowledged.

**Billing meanwhile:** `FEATURE_TOKEN_ADDONS=false`. Without eligibility or access, an add-on purchase fails closed (`503`).

### 4.5 Commercial IaC / Platform-SRE

| # | What | Action | Done when |
|---|---|---|---|
| Commercial IaC | SNS `billing-events` with Glue registry `billing-events`; SQS `tenant-billing-q` and `usage-billing-q` with DLQs (HLD §16 action 2; LLD §7.3.9) | Create per environment | Billing can publish and consume in each environment |
| W-11 | `billing-entitlements-q` + DLQ + SNS subscription to `billing-events` | Create per environment | Entitlements receives Billing events |
| SRE-1 | Istio `STRICT` mTLS in staging | Enable | Staging mesh is strict |
| SRE-3 | Core `000004` + `parked-events-replay` before direct signup (with Core) | Order per environment | Confirmed per environment |
| SRE-7 | Remove any `membership-billing-q` (retired by CD-54) | Delete if present | Queue absent |
| BL-OQ-19 support | A cluster with Model Gateway v1.2.0, the rate-limit service and Valkey; Bedrock access in `ap-south-1` | Provide for the AI team's tests | AI team can run AI-1, AI-3, AI-5 |

### 4.6 AI team

#### BL-OQ-19 — Model Gateway limit hot reload, proven on a cluster

- **What:** when a tenant buys tokens, the Gateway must raise that tenant's limit. So far this has only been shown without a cluster (0.3 s reload, Envoy fail-open); nothing has run through Gateway v1.2.0.
- **Billing meanwhile:** `FEATURE_TOKEN_ADDONS=false`.
- **Action:** on the cluster from §4.5, run on the pinned v1.2.0 image:
  - **AI-1:** an entitlement change reaches the Gateway within 60 s;
  - **AI-3:** calls continue and the SEV-2 alert fires when the rate-limit service fails;
  - **AI-5:** a purchased add-on raises the tenant's allowance.

  AI-5 is required: AI-1 and AI-3 alone don't prove the commercial path.
- **Done when:** all three pass, with evidence.

#### PD-LLM-1 R-1, R-2, R-4 — not Billing items

- **What:** the model list (R-1, needs Bedrock access), the evaluation set and pass bar (R-2), and the Opus-tier rule (R-4) for the tiered model profiles.
- **For Billing:** nothing pending. Product approved PD-LLM-1 and Entitlements approved ENT-D6 on 2026-11-23; final plan and pack prices are loaded (BL-OQ-22, BL-OQ-18). Billing sends plan selections only. Whatever remains of R-1, R-2 and R-4 is the AI team's own delivery work for the Model Gateway, tracked outside this hand-off.

### 4.7 Web / growth — Signup BFF

#### BL-OQ-8 — Align the BFF to Billing's published contract (SGN-2)

- **What:** direct paid signup calls Billing's B-I1 (LLD §5.4.2).
- **Billing meanwhile:** `FEATURE_DIRECT_SIGNUP=false`; signup goes through trial conversion only.
- **Action:**
  1. Set `BILLING_TIMEOUT_MS` to at least 15000.
  2. Map `PaymentToken` → `payment_method_ref`, `Plan` → `plan_code`, `Seats` → `licensed_seats`.
  3. Handle 3-D Secure: `402 PAYMENT_ACTION_REQUIRED` → browser authenticates → retry with the **same** idempotency key.
  4. Follow the go-live order in LLD §18.7.
- **Done when:** the Pact contract test is green and the Test-Clock end-to-end run passes in staging.
- **Depends on:** SRE-3.

### 4.8 Notification

#### BL-OQ-12 — Billing messages (NTF-3)

- **What:** messages for:
  - seats added;
  - seat reduction applied (including renewal above target);
  - token add-on purchased;
  - downgrade blocked at renewal;
  - renewal top-up charged;
  - **renewal price change scheduled** (current and new unit price, effective date).
- **Why it matters:** Product requires customers to be told before a renewal uses a higher price (BL-OQ-16). Until this message ships, the change is shown only on the Billing page.
- **Billing meanwhile:** only the past-due and cancelled emails are sent.
- **Action:** agree the triggers with the Commercial Team (event or outbox message per case), then build the templates. Notification resolves recipients itself; Billing puts no personal data on the bus.
- **Done when:** each message is sent in staging from a real Billing trigger. **Don't add price increases for live subscribers until the price-change message is live.**

### 4.9 Usage & Metering

#### BL-OQ-24 — Confirm the `UsagePeriodClosed` contract

- **What:** Billing consumes `period_start`, `period_end`, `tokens_total`, `api_requests_total`, `addon_consumed` and `limits` (LLD §7.1). These are taken from the HLD because the Usage & Metering LLD isn't written.
- **Billing meanwhile:** records are reference-only; nothing is priced from them. A mismatch would affect operator and Finance views, not charges.
- **Action:** confirm the payload in the Usage & Metering LLD and register the schema in Glue `usage-events`.
- **Done when:** the schema is registered, and Billing's contract test passes against it before `usage-billing-q` is switched on.

### 4.10 Finance

| # | What | Billing meanwhile | Action | Done when |
|---|---|---|---|---|
| Acks | S-2, S-3, S-5, S-6 interpretations (§3.3) | — | — | ✅ Received 2026-11-23 |

### 4.11 Product

| # | What | Billing meanwhile | Action | Done when |
|---|---|---|---|---|
| Ack | S-6 renewal-repricing job and no minimum notice (§3.3) | — | — | ✅ Received 2026-11-23 |

### 4.12 Legal

| # | What | Action | Done when |
|---|---|---|---|
| Ack | S-5: contact-data minimisation continues for personal data (§3.3) | — | ✅ Received 2026-11-23 |

### 4.13 Commercial Team (Billing) — own items

**Service:** Billing (`commercial-billing`; namespace `billing`, ServiceAccount `commercial-billing`).

| # | What | Action | Done when |
|---|---|---|---|
| W-8 | Publish `TenantQuotaAddOnChanged` (`purchase`, `refund`) through the outbox; register the schema in Glue `billing-events` | Build as LLD §7.3.6 (`BIL-ADD-2`, `BIL-ADD-4`) | Exactly one event per purchase and per refund; schema registered per environment |
| W-9, W-10 | Read Entitlements eligibility (fresh on purchase, 60 s cache for the pack list, `503` fail-closed); egress to Entitlements on that `GET` only | Build as LLD §8.7 and §10 | Works against Entitlements in each environment |
| Contract test | The CI contract test covers Core, Realm Provisioner and Audit consumed schemas, **not Entitlements** | Add Entitlements' consumed `TenantQuotaAddOnChanged` schema once pushed | CI fails if Billing and Entitlements disagree |
| E-6 signal | Joint OP-G4 run | Tell Entitlements when OP-G2 and OP-G3 are ready | Run scheduled |
| Housekeeping | §1.2 cites `billing-open-items-team-response.md` and `billing-reply-ai-team.md`, which aren't in the repo. `BIL-PAY-1` is stated twice (lines 1436, 2426) | Add the files or say where they're held; make the second `BIL-PAY-1` a reference | Clean LLD |
| Publish | 17+ commits on `docs/billing-lld-rev-0.14-signoff`, not pushed | Push and open a PR, so reviewers see rev 0.23 | PR open |
| Approve | Final sign-off (CD-4) | After §3.1 to §3.3 | Signed |
| BL-OQ-15 | **Deferred (Phase 3).** Per-tenant provider choice; moving customers off a removed provider (design in §19.3). With Finance | None now | Phase 3 plan |

---

## 5. What each launch needs

| Launch | Needs closed |
|---|---|
| **Final sign-off** | §3.1 HLD v0.65 merged · §3.2 PCI re-confirmation (§3.3 acknowledgements received) |
| **Any Billing event in an environment** | Commercial IaC topic, registry and queues · Entitlements consumer event names (or Entitlements misses every plan change) |
| **Paid subscriptions through Stripe** | OP-G1 rollout · **OP-G2** · OP-G3 · OP-G4 · A-10 role + named holders · first certification approved (B-O17) |
| **Direct paid signup** | All of the above · BL-OQ-8 · SRE-3 |
| **Token add-ons** (`FEATURE_TOKEN_ADDONS=true`) | Stripe launch · Entitlements E-1, E-2, E-4, E-5 verified · W-8 to W-11 · BL-OQ-19 (AI-1, AI-3, AI-5) · A-5 deployed |
| **Price increases for live subscribers** | BL-OQ-12 price-change message |
| **Usage-record consumer** | BL-OQ-24 |
| **Correct audit labels** | A-9 (b), A-10 (c), A-5 deployed |

## 6. Critical path

```
OP-G1 rollout ─► OP-G2 (platform-edge, not started) ─► OP-G3 ─► OP-G4 joint run ─┐
A-10 role + PCI names holders ───────────────────────────────────────────────────┴─► first certification (B-O17) ─► Stripe live
```

**OP-G2 is the longest pole.** It hasn't started, and nothing operator-side can be proven without it. Ask IAM for the OP-G6 date and escalate OP-G2 first.

## 7. Tracker

| # | Item | Team | Service | Status (2026-11-23) |
|---|---|---|---|---|
| 3.1 | Merge and tag HLD v0.65 | Commercial HLD owner | `commercial-hld-docs` | Open |
| 3.2 | Re-confirm §4, §5.4.4 | PCI owner group | — | Open |
| 3.3 | Acknowledge S-2, S-3, S-5, S-6 interpretations | Finance, Product, Legal | — | **Done** 2026-11-23 |
| BL-OQ-14 | OP-G2 gateway provider | Platform-edge | Envoy Gateway | **Not started** |
| BL-OQ-14 | OP-G1 rollout, operator users | IAM | Realm Provisioner | Merged; rollout open |
| BL-OQ-14 | OP-G3, OP-G4, OP-G6 | IAM | AuthZ Enrichment | Blocked on OP-G2 |
| BL-OQ-25 | A-9: B-O16 test + Audit type | IAM | AuthZ Enrichment, Audit Log | Requested |
| BL-OQ-26 | A-10: role, B-O17 test, Audit type; name holders | IAM; PCI owner group | Realm Provisioner, AuthZ, Audit Log | Requested |
| BL-OQ-23 (b) | A-5 refund type deployed | IAM | Audit Log | Merged; deploy open |
| SRE-3 | Core `000004` before direct signup | IAM Core + SRE | `iam-org-membership` | Code ready |
| BL-OQ-3 | Eligibility + access (E-4, E-5) | Entitlements | `commercial-entitlements` | **Claimed, unverified** |
| BL-OQ-4 | Single publisher, purchase consumer (E-1) | Entitlements | `commercial-entitlements` | **Claimed, unverified** |
| BL-OQ-20 | Refunds, corrections, LLD text (E-2, E-7) | Entitlements | `commercial-entitlements` | **Claimed, unverified**; refund key wrong as described |
| — | Billing consumer event names | Entitlements | `commercial-entitlements` | Open (found 2026-11-23) |
| E-6 | Joint OP-G4 run | Entitlements + IAM | — | Blocked on OP-G2/3 |
| IaC | Topic, registry, queues incl. `billing-entitlements-q` | Commercial IaC / SRE | IaC | Open |
| SRE-1, SRE-7 | Istio strict in staging; remove old queue | Platform-SRE | — | Open |
| BL-OQ-19 | AI-1, AI-3, AI-5 on a cluster | AI team (+ SRE cluster) | Model Gateway | Not proven |
| BL-OQ-8 | BFF alignment | Web / growth | Signup BFF | Contract published |
| BL-OQ-12 | NTF-3 messages | Notification + Commercial | Notification | Open |
| BL-OQ-24 | `UsagePeriodClosed` contract | Usage & Metering | `commercial-usage-metering` | Open (known risk) |
| BL-OQ-5 | Launch currency | Finance | — | **Closed** 2026-11-23: USD default; others via `plan_prices` / B-O11 |
| BL-OQ-22 | Final prices | Finance + Product | — | **Closed** 2026-11-23: plan prices loaded |
| BL-OQ-17 | UAE Stripe terms | Finance + Legal | — | **Closed** 2026-11-23: accepted without amendments |
| BL-OQ-18 | Token packs | Product + Finance | — | **Closed** 2026-11-23: catalogue loaded into `token_packs` |
| Memos | PD-LLM-1, ENT-D6 approval | Product; Entitlements | — | **Approved** 2026-11-23 |
| BL-OQ-9 (a) | `HTTPRoute` B-O1 to B-O17 | Commercial Team (Billing) | `commercial-billing` | **Closed** 2026-11-23 |
| W-8 to W-10 | Event publish, eligibility read, egress | Commercial Team (Billing) | `commercial-billing` | Designed; to build |
| — | Contract test incl. Entitlements schema | Commercial Team (Billing) | `commercial-billing` | Open |
| — | LLD housekeeping; push and PR | Commercial Team (Billing) | `commercial-billing` | Open |
| BL-OQ-15 | Multi-provider follow-ups | Billing + Finance | `commercial-billing` | Deferred (Phase 3) |

## 8. Reference documents

| Document | Use it for |
|---|---|
| `commercial-lld-billing-service.md` | The design; §16.1 register, §16.2 discrepancies |
| `billing-design-signoff-blockers.md` | S-1 to S-9 decisions, reasons and Commercial Team additions |
| `billing-register-open-items.md` | §16.1 open rows explained |
| `billing-iam-actions.md` | IAM actions A-1 to A-10 with test cases and Audit entry specs |
| `billing-open-items-entitlements.md` | Entitlements items E-1 to E-8 |
| `billing-decided-items-entitlements-build.md` | BL-OQ-3, 4, 20 by owner (W-1 to W-12), with RACI |
| `billing-open-items-by-team.md` | Full by-team register, including closed items |
| `op-g2-platform-edge-request.md` | The OP-G2 request to platform-edge |
| `commercial-hld-docs/hld-commercial-domain-v0.65.md` | HLD with D-1, D-5, D-8, D-9 applied (§17.8) |
